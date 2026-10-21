// Command bop (Broken on Purpose) is a container that fails on demand.
// Not for production.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackspiering/bop/internal/mode"
	"github.com/jackspiering/bop/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `bop (Broken on Purpose): a container that fails on demand. Not for production.

Usage:
  bop [serve]                      run the main process (configured by BOP_* env)
  bop <mode> [key=value ...]       switch the running process (docker exec <c> bop ...)
  bop clear                        back to healthy and remove the sticky mode
  bop status                       print the running process's state
  bop healthcheck [--url=URL]      exit 0 if URL (default: own /healthz) returns 2xx
  bop sidecar --target=URL         watch another bop's /info; binds nothing
  bop version

Modes:
  healthy
  unhealthy   [after=<dur>]
  crash       [exit=<code>] [after=<dur>]
  crash-loop  [interval=<dur>] [exit=<code>]    boot mode or sticky=true only
  oom         [rate=<MiB/s>]                    needs a memory limit
  hang
Every mode accepts sticky=true: it lasts as long as BOP_STATE_DIR (by default
the writable layer: survives docker restart, not a recreate).

Environment (boot only):
  BOP_MODE         initial mode, same grammar (default healthy)
  BOP_START_DELAY  don't bind the data listener for this long
  BOP_ON_SIGTERM   exit:<code> | ignore | delay:<dur> (default exit:0)
  BOP_ADDR         data listener (default :8080)
  BOP_CONTROL_ADDR control listener (default 127.0.0.1:9901)
  BOP_STATE_DIR    sticky state directory (default /tmp/bop)
  BOP_OOM_UNSAFE   1 allows oom without a memory limit
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		return serve(stderr, getenv)
	}
	switch args[0] {
	case "serve":
		if len(args) > 1 {
			return usageError(stderr, "serve takes no arguments; configure it with BOP_* environment variables")
		}
		return serve(stderr, getenv)
	case "status":
		return execCommand(stdout, stderr, getenv, "")
	case "healthcheck":
		return healthcheck(args[1:], stderr, getenv)
	case "sidecar":
		return sidecar(args[1:], stderr)
	case "version", "--version":
		fmt.Fprintf(stdout, "bop %s\n", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	return execCommand(stdout, stderr, getenv, strings.Join(args, " "))
}

func usageError(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "bop: "+format+"\n\n%s", append(args, usage)...)
	return 2
}

// execCommand sends spec to the running process. An empty spec asks for status.
func execCommand(stdout, stderr io.Writer, getenv func(string) string, spec string) int {
	if spec != "" {
		if _, err := mode.ParseCommand(spec); err != nil {
			return usageError(stderr, "%v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := server.NewClient(envOr(getenv, "BOP_CONTROL_ADDR", defaultControlAddr))
	var (
		st  server.State
		err error
	)
	if spec == "" {
		st, err = client.Status(ctx)
	} else {
		st, err = client.Set(ctx, spec)
	}
	if err != nil {
		var ae *server.APIError
		if errors.As(err, &ae) && ae.Status == 400 {
			fmt.Fprintf(stderr, "bop: %v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "bop: %v\n", err)
		return 1
	}
	printState(stdout, st)
	return 0
}

func printState(w io.Writer, st server.State) {
	// The machine counts hang as healthy because guard does the blocking, but
	// the container is not: /healthz never answers.
	health := "healthy"
	switch {
	case modeName(st.Mode) == mode.Hang:
		health = "hanging (/healthz and /info accept connections but never answer)"
	case !st.Healthy:
		health = "unhealthy (" + st.Reason + ")"
	}
	fmt.Fprintf(w, "mode:        %s\n", st.Mode)
	fmt.Fprintf(w, "health:      %s\n", health)
	fmt.Fprintf(w, "sticky:      %t\n", st.Sticky)
	fmt.Fprintf(w, "instance_id: %s\n", st.InstanceID)
	fmt.Fprintf(w, "pid:         %d\n", st.PID)
	fmt.Fprintf(w, "started_at:  %s\n", st.StartedAt)
	fmt.Fprintf(w, "version:     %s\n", st.Version)
}

// modeName returns the mode name from a Mode.String() value such as
// "hang sticky=true".
func modeName(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

const (
	defaultAddr        = ":8080"
	defaultControlAddr = "127.0.0.1:9901"
	defaultStateDir    = "/tmp/bop"
)

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}
