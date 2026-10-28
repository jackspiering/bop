// Command bop (Broken on Purpose) is a container that fails on demand.
// Not for production.
package main

import (
	"context"
	"encoding/json"
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
  bop status [--json]              print the running process's state (--json: one JSON object)
  bop healthcheck [--url=URL]      exit 0 if URL returns 2xx, else 1 (default URL:
                                   /healthz on BOP_ADDR's host and port, 127.0.0.1
                                   for an empty or wildcard host)
  bop sidecar --target=URL [--interval=2s]
                                   watch another bop's /info; binds nothing
  bop version
  bop help
Invalid input exits 2. Probes go direct: no proxy, and a redirect is a failure.

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
  BOP_ADDR         data listener, [host]:port or a bare port (default :8080)
  BOP_CONTROL_ADDR control listener, [host]:port (default 127.0.0.1:9901)
  BOP_STATE_DIR    sticky state directory (default /tmp/bop)
  BOP_OOM_UNSAFE   1 skips the oom guard (memory limit below host RAM, swap limited)
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
		asJSON := len(args) == 2 && args[1] == "--json"
		if len(args) > 1 && !asJSON {
			return usageError(stderr, "status takes no arguments except --json, got %q", args[1:])
		}
		return execCommand(stdout, stderr, getenv, "", asJSON)
	case "healthcheck":
		return healthcheck(args[1:], stderr, getenv)
	case "sidecar":
		return sidecar(args[1:], stderr)
	case "version", "--version":
		if len(args) > 1 {
			return usageError(stderr, "%s takes no arguments, got %q", args[0], args[1:])
		}
		fmt.Fprintf(stdout, "bop %s\n", version)
		return 0
	case "help", "-h", "--help":
		// Deliberately lenient: `bop help <anything>` prints the usage.
		fmt.Fprint(stdout, usage)
		return 0
	}
	spec := strings.Join(args, " ")
	if _, err := mode.ParseCommand(spec); err != nil {
		return usageError(stderr, "%v", err)
	}
	return execCommand(stdout, stderr, getenv, spec, false)
}

func usageError(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "bop: "+format+"\n\n%s", append(args, usage)...)
	return 2
}

// execCommand sends spec, already validated by mode.ParseCommand, to the
// running process. An empty spec asks for status; run never passes an empty
// spec otherwise, because ParseCommand rejects it.
func execCommand(stdout, stderr io.Writer, getenv func(string) string, spec string, asJSON bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Parsed like serve parses it, so a bare port reaches the same listener.
	addr, err := parseControlAddr(envOr(getenv, "BOP_CONTROL_ADDR", defaultControlAddr))
	if err != nil {
		return usageError(stderr, "%v", err)
	}
	client := server.NewClient(addr, newProbeClient(5*time.Second))
	var st server.State
	if spec == "" {
		st, err = client.Status(ctx)
	} else {
		st, err = client.Set(ctx, spec)
	}
	if err != nil {
		var ae *server.APIError
		if errors.As(err, &ae) && (ae.Status == 400 || ae.Status == 413) {
			fmt.Fprintf(stderr, "bop: %v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "bop: %v\n", err)
		return 1
	}
	if asJSON {
		if err := json.NewEncoder(stdout).Encode(st); err != nil {
			fmt.Fprintf(stderr, "bop: %v\n", err)
			return 1
		}
		return 0
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
		health = "hanging (the data port accepts connections but never answers)"
	case !st.Healthy:
		health = "unhealthy (" + st.Reason + ")"
	}
	fmt.Fprintf(w, "mode:        %s\n", st.Mode)
	fmt.Fprintf(w, "health:      %s\n", health)
	stickyMode := st.StickyMode
	if stickyMode == "" {
		stickyMode = "(none)"
	}
	fmt.Fprintf(w, "sticky:      %t\n", st.Sticky)
	fmt.Fprintf(w, "sticky_mode: %s\n", stickyMode)
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
