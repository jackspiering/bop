package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackspiering/bop/internal/mode"
	"github.com/jackspiering/bop/internal/oom"
	"github.com/jackspiering/bop/internal/server"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestRunUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{
		{"explode"},
		{"crash", "exit=999"},
		{"healthy", "after=1s"},
		{"serve", "extra"},
		{"healthcheck", "extra"},
		{"healthcheck", "--bogus"},
		{"sidecar"},
		{"sidecar", "--target=not-a-url"},
		{"status", "x"},
		{"status", "sticky=true"},
		{"version", "x"},
		{"--version", "x"},
		{""},
		{"clear", "sticky=true"},
		{"clear", "sticky=false"},
	} {
		var out, errb bytes.Buffer
		// An unreachable control address: a network call would fail with 1, not 2.
		if code := run(args, &out, &errb, env(map[string]string{"BOP_CONTROL_ADDR": "127.0.0.1:1"})); code != 2 {
			t.Errorf("run(%v) = %d, want 2 (stderr %q)", args, code, errb.String())
		}
		if errb.Len() == 0 {
			t.Errorf("run(%v) printed nothing to stderr", args)
		}
	}
}

func TestRunUsageMentionsModes(t *testing.T) {
	var out, errb bytes.Buffer
	run([]string{"explode"}, &out, &errb, env(nil))
	for _, want := range []string{"unknown mode", "Usage:", "crash-loop", "BOP_ON_SIGTERM", "--interval", "bop help"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("usage message lacks %q:\n%s", want, errb.String())
		}
	}
}

func TestRunVersionAndHelp(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb, env(nil)); code != 0 || out.String() != "bop dev\n" {
		t.Fatalf("version = %d %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"help"}, &out, &errb, env(nil)); code != 0 || !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("help = %d", code)
	}
	// help stays lenient: anything after it still prints the usage and exits 0.
	for _, args := range [][]string{{"help", "crash"}, {"-h", "x"}, {"--help", "x", "y"}} {
		out.Reset()
		if code := run(args, &out, &errb, env(nil)); code != 0 || !strings.Contains(out.String(), "Usage:") {
			t.Errorf("run(%v) = %d, want 0 with usage", args, code)
		}
	}
	// -h on a subcommand is a request for help too: flag help on stderr, exit 0.
	for _, args := range [][]string{{"healthcheck", "-h"}, {"sidecar", "--help"}} {
		errb.Reset()
		if code := run(args, &out, &errb, env(nil)); code != 0 || !strings.Contains(errb.String(), "Usage of") {
			t.Errorf("run(%v) = %d, want 0 with flag help (stderr %q)", args, code, errb.String())
		}
	}
}

func TestRunExecWithoutServerExits1(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"unhealthy"}, &out, &errb, env(map[string]string{"BOP_CONTROL_ADDR": "127.0.0.1:1"}))
	if code != 1 || !strings.Contains(errb.String(), "control listener") {
		t.Fatalf("code = %d, stderr %q", code, errb.String())
	}
}

func TestRunExecUsesServeAddrGrammar(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"mode":"healthy","healthy":true}`))
	}))
	defer ts.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(ts.URL, "http://"), ":")
	var out, errb bytes.Buffer
	// serve binds ":port" on all interfaces; the CLI must find it on loopback.
	if code := run([]string{"status"}, &out, &errb, env(map[string]string{"BOP_CONTROL_ADDR": ":" + port})); code != 0 {
		t.Fatalf("status with BOP_CONTROL_ADDR=:%s = %d, stderr %q", port, code, errb.String())
	}
	// serve refuses a bare control port, so the CLI does too.
	if code := run([]string{"status"}, &out, &errb, env(map[string]string{"BOP_CONTROL_ADDR": port})); code != 2 {
		t.Fatalf("status with BOP_CONTROL_ADDR=%s = %d, want 2", port, code)
	}
	if code := run([]string{"status"}, &out, &errb, env(map[string]string{"BOP_CONTROL_ADDR": "nope"})); code != 2 {
		t.Fatalf("status with BOP_CONTROL_ADDR=nope = %d, want 2", code)
	}
}

func TestParseControlAddr(t *testing.T) {
	for _, in := range []string{"127.0.0.1:9901", ":9902", "[::1]:9902", "localhost:9902"} {
		if got, err := parseControlAddr(in); err != nil || got != in {
			t.Errorf("parseControlAddr(%q) = %q, %v; want it unchanged", in, got, err)
		}
	}
	for _, in := range []string{"9902", "nope", "127.0.0.1:70000"} {
		_, err := parseControlAddr(in)
		var ue *mode.UsageError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "BOP_CONTROL_ADDR") {
			t.Errorf("parseControlAddr(%q) = %v, want a usage error naming BOP_CONTROL_ADDR", in, err)
		}
	}
	// Only a bare port gets the hint; other junk gets the plain grammar error.
	if _, err := parseControlAddr("nope"); err == nil || strings.Contains(err.Error(), "127.0.0.1:nope") {
		t.Errorf("parseControlAddr(\"nope\") = %v, want no bare-port hint", err)
	}
	// The error says how to keep the listener local.
	if _, err := parseControlAddr("9902"); err == nil || !strings.Contains(err.Error(), "127.0.0.1:9902") {
		t.Errorf("parseControlAddr(\"9902\") = %v, want a hint naming 127.0.0.1:9902", err)
	}
}

func TestDefaultHealthURL(t *testing.T) {
	for in, want := range map[string]string{
		":8080":          "http://127.0.0.1:8080/healthz",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/healthz",
		"[::]:9000":      "http://127.0.0.1:9000/healthz",
		"10.1.2.3:81":    "http://10.1.2.3:81/healthz",
		"localhost:7000": "http://localhost:7000/healthz",
		"9000":           "http://127.0.0.1:9000/healthz", // a bare port, as the listener accepts
	} {
		if got, err := defaultHealthURL(in); err != nil || got != want {
			t.Errorf("defaultHealthURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// What the listener rejects, the healthcheck rejects too.
	for _, in := range []string{":http", "nope", ":99999", "::1:9000"} {
		if got, err := defaultHealthURL(in); err == nil {
			t.Errorf("defaultHealthURL(%q) = %q, want an error", in, got)
		}
	}
	var errb bytes.Buffer
	if code := healthcheck(nil, &errb, env(map[string]string{"BOP_ADDR": "nope"})); code != 2 {
		t.Errorf("healthcheck with BOP_ADDR=nope = %d, want 2", code)
	}
}

func TestParseListenAddr(t *testing.T) {
	for in, want := range map[string]string{
		":8080":           ":8080",
		"9000":            ":9000",
		"0":               ":0",
		"127.0.0.1:9901":  "127.0.0.1:9901",
		"[::1]:9000":      "[::1]:9000",
		"[::]:9000":       "[::]:9000",
		"localhost:7000":  "localhost:7000",
		"bop.local:65535": "bop.local:65535",
	} {
		if got, err := parseListenAddr("BOP_ADDR", in); err != nil || got != want {
			t.Errorf("parseListenAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", ":", "nope", ":http", ":99999", "127.0.0.1:70000", "::1:9000", ":-1", "1.2.3.4:"} {
		_, err := parseListenAddr("BOP_ADDR", in)
		var ue *mode.UsageError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "BOP_ADDR") {
			t.Errorf("parseListenAddr(%q) = %v, want a usage error naming BOP_ADDR", in, err)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	var status = http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer ts.Close()

	var errb bytes.Buffer
	if code := healthcheck([]string{"--url=" + ts.URL}, &errb, env(nil)); code != 0 {
		t.Fatalf("200 -> %d", code)
	}
	status = http.StatusServiceUnavailable
	if code := healthcheck([]string{"--url=" + ts.URL}, &errb, env(nil)); code != 1 {
		t.Fatalf("503 -> %d", code)
	}
	// Default URL follows BOP_ADDR.
	addr := strings.TrimPrefix(ts.URL, "http://")
	status = http.StatusOK
	if code := healthcheck(nil, &errb, env(map[string]string{"BOP_ADDR": addr})); code != 0 {
		t.Fatalf("BOP_ADDR=%s -> %d (%s)", addr, code, errb.String())
	}
	if code := healthcheck([]string{"--url=http://127.0.0.1:1/"}, &errb, env(nil)); code != 1 {
		t.Fatalf("refused -> %d", code)
	}
}

func TestHealthcheckTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer ts.Close()
	start := time.Now()
	var errb bytes.Buffer
	if code := healthcheck([]string{"--url=" + ts.URL}, &errb, env(nil)); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("healthcheck took %s, want ~1.5s", d)
	}
}

func TestLoadConfig(t *testing.T) {
	cfg, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.boot.Name != mode.Healthy || cfg.addr != ":8080" || cfg.controlAddr != "127.0.0.1:9901" ||
		cfg.store.Dir != "/tmp/bop" || cfg.sigterm.Kind != mode.SigtermExit || cfg.startDelay != 0 {
		t.Fatalf("defaults = %+v", cfg)
	}

	cfg, err = loadConfig(env(map[string]string{
		"BOP_MODE": "crash exit=3 after=10s", "BOP_START_DELAY": "2s", "BOP_ON_SIGTERM": "exit:7",
		"BOP_ADDR": ":9000", "BOP_CONTROL_ADDR": "127.0.0.1:9902", "BOP_STATE_DIR": "/x", "BOP_OOM_UNSAFE": "1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.boot.Exit != 3 || cfg.startDelay != 2*time.Second || cfg.sigterm.Code != 7 || cfg.addr != ":9000" ||
		cfg.controlAddr != "127.0.0.1:9902" || cfg.store.Dir != "/x" || !cfg.oomUnsafe {
		t.Fatalf("config = %+v", cfg)
	}

	// A blank BOP_MODE means healthy, like an unset one.
	for _, blank := range []string{"", " ", "\t"} {
		cfg, err = loadConfig(env(map[string]string{"BOP_MODE": blank}))
		if err != nil || cfg.boot.Name != mode.Healthy {
			t.Errorf("BOP_MODE=%q: boot=%q, %v", blank, cfg.boot.Name, err)
		}
	}

	// A bare data port means all interfaces.
	cfg, err = loadConfig(env(map[string]string{"BOP_ADDR": "9000"}))
	if err != nil || cfg.addr != ":9000" {
		t.Errorf("bare BOP_ADDR: addr=%q, %v", cfg.addr, err)
	}

	for _, bad := range []map[string]string{
		{"BOP_MODE": "explode"},
		{"BOP_MODE": "clear"},
		{"BOP_START_DELAY": "soon"},
		{"BOP_START_DELAY": "-1s"},
		{"BOP_ON_SIGTERM": "reboot"},
		{"BOP_ADDR": ":99999"},
		{"BOP_ADDR": "nope"},
		{"BOP_ADDR": "::1:9000"},
		{"BOP_ADDR": ":http"},
		{"BOP_CONTROL_ADDR": "nope"},
		{"BOP_CONTROL_ADDR": "9902"}, // would expose the control API; needs a host
		{"BOP_CONTROL_ADDR": "127.0.0.1:70000"},
		{"BOP_ADDR": "9901"}, // collides with the default control address 127.0.0.1:9901
		{"BOP_ADDR": "127.0.0.1:9000", "BOP_CONTROL_ADDR": "127.0.0.1:9000"},
		{"BOP_ADDR": ":9000", "BOP_CONTROL_ADDR": "[::1]:9000"}, // wildcard overlaps
	} {
		_, err := loadConfig(env(bad))
		var ue *mode.UsageError
		if !errors.As(err, &ue) {
			t.Errorf("loadConfig(%v) = %v, want a usage error", bad, err)
		}
	}

	// Different specific hosts, and port 0 (other tests rely on it), are accepted.
	for _, ok := range []map[string]string{
		{"BOP_ADDR": "10.0.0.1:9000", "BOP_CONTROL_ADDR": "127.0.0.1:9000"},
		{"BOP_ADDR": "127.0.0.1:0", "BOP_CONTROL_ADDR": "127.0.0.1:0"},
	} {
		if _, err := loadConfig(env(ok)); err != nil {
			t.Errorf("loadConfig(%v) = %v, want nil", ok, err)
		}
	}
}

func TestServeInvalidStickyFile(t *testing.T) {
	for name, write := range map[string]func(path string) error{
		"invalid":    func(p string) error { return os.WriteFile(p, []byte("explode\n"), 0o644) },
		"unreadable": func(p string) error { return os.Mkdir(p, 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := write(filepath.Join(dir, "mode")); err != nil {
				t.Fatal(err)
			}
			var errb bytes.Buffer
			// serve returns before either listener serves.
			code := serve(&errb, env(map[string]string{
				"BOP_STATE_DIR": dir, "BOP_CONTROL_ADDR": "127.0.0.1:0", "BOP_ADDR": "127.0.0.1:0",
			}))
			if code != 2 {
				t.Fatalf("serve = %d, want 2 (stderr %q)", code, errb.String())
			}
			// bop clear needs the control listener, which never serves.
			for _, bad := range []string{"bop clear", "Usage:"} {
				if strings.Contains(errb.String(), bad) {
					t.Errorf("stderr mentions %q:\n%s", bad, errb.String())
				}
			}
			for _, want := range []string{filepath.Join(dir, "mode"), "recreate"} {
				if !strings.Contains(errb.String(), want) {
					t.Errorf("stderr lacks %q:\n%s", want, errb.String())
				}
			}
		})
	}
}

func TestHandleSignals(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	send := func(n int) chan os.Signal {
		c := make(chan os.Signal, n)
		for range n {
			c <- syscall.SIGTERM
		}
		return c
	}

	if code := handleSignals(log, mode.Sigterm{Kind: mode.SigtermExit, Code: 7}, send(1)); code != 7 {
		t.Fatalf("exit:7 -> %d", code)
	}

	// delay exits 0 once the delay has passed, measured from the first signal:
	// a later signal neither restarts the timer nor exits early.
	const delay = 500 * time.Millisecond
	sigs := send(1)
	start := time.Now()
	delayed := make(chan int, 1)
	go func() { delayed <- handleSignals(log, mode.Sigterm{Kind: mode.SigtermDelay, Delay: delay}, sigs) }()
	time.Sleep(350 * time.Millisecond)
	sigs <- syscall.SIGINT
	select {
	case code := <-delayed:
		if code != 0 {
			t.Fatalf("delay -> %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delay never exited")
	}
	// A timer restarted by the second signal would return at about 850ms.
	if d := time.Since(start); d < delay || d > delay+250*time.Millisecond {
		t.Fatalf("delay exited after %s, want %s from the first signal", d, delay)
	}

	// ignore never returns.
	done := make(chan int, 1)
	go func() { done <- handleSignals(log, mode.Sigterm{Kind: mode.SigtermIgnore}, send(3)) }()
	select {
	case code := <-done:
		t.Fatalf("ignore returned %d", code)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestInfoURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:8080":      "http://127.0.0.1:8080/info",
		"http://127.0.0.1:8080/":     "http://127.0.0.1:8080/info",
		"http://127.0.0.1:8080/info": "http://127.0.0.1:8080/info",
		"http://svc/custom":          "http://svc/custom",
	} {
		got, err := infoURL(in)
		if err != nil || got != want {
			t.Errorf("infoURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := infoURL("127.0.0.1:8080"); err == nil {
		t.Error("infoURL without a scheme should fail")
	}
}

func TestWatcherLogsOnlyChanges(t *testing.T) {
	var buf bytes.Buffer
	w := &watcher{log: slog.New(slog.NewTextHandler(&buf, nil)), target: "t"}
	steps := []struct {
		id      string
		err     error
		wantLog string
	}{
		{"a", nil, "target reachable"},
		{"a", nil, ""},
		{"", errors.New("refused"), "target unreachable"},
		{"", errors.New("refused"), ""},
		{"a", nil, "target reachable again"},
		{"b", nil, "instance_id changed"},
		{"b", nil, ""},
	}
	for i, s := range steps {
		buf.Reset()
		w.observe(s.id, s.err)
		if s.wantLog == "" && buf.Len() != 0 {
			t.Errorf("step %d logged %q, want silence", i, buf.String())
		}
		if s.wantLog != "" && !strings.Contains(buf.String(), s.wantLog) {
			t.Errorf("step %d logged %q, want %q", i, buf.String(), s.wantLog)
		}
	}
}

// A restart usually shows up as an outage followed by a new instance_id, so
// the first poll after the outage must log the change as well.
func TestWatcherLogsIDChangeAfterOutage(t *testing.T) {
	var buf bytes.Buffer
	w := &watcher{log: slog.New(slog.NewTextHandler(&buf, nil)), target: "t"}
	w.observe("a", nil)
	w.observe("", errors.New("refused"))
	buf.Reset()
	w.observe("b", nil)
	for _, want := range []string{"target reachable again", "target instance_id changed", "old=a", "new=b"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("logged %q, want %q", buf.String(), want)
		}
	}
}

func TestPoll(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"instance_id":"abc"}`))
	}))
	defer ts.Close()
	id, err := poll(context.Background(), http.DefaultClient, ts.URL+"/info")
	if err != nil || id != "abc" {
		t.Fatalf("poll = %q, %v", id, err)
	}
}

func TestPrintState(t *testing.T) {
	for _, tc := range []struct {
		st   server.State
		want string
	}{
		{server.State{Info: server.Info{Mode: "healthy"}, Healthy: true}, "health:      healthy\n"},
		{server.State{Info: server.Info{Mode: "unhealthy after=0s"}, Reason: "unhealthy mode is active"}, "health:      unhealthy (unhealthy mode is active)\n"},
		{server.State{Info: server.Info{Mode: "unhealthy after=1m0s"}, Healthy: true}, "health:      healthy\n"},
		{server.State{Info: server.Info{Mode: "hang"}, Healthy: true}, "health:      hanging"},
		{server.State{Info: server.Info{Mode: "hang sticky=true", Sticky: true}, Healthy: true}, "health:      hanging"},
	} {
		var out bytes.Buffer
		printState(&out, tc.st)
		if !strings.Contains(out.String(), "mode:        "+tc.st.Mode+"\n") || !strings.Contains(out.String(), tc.want) {
			t.Errorf("printState(%q) lacks %q:\n%s", tc.st.Mode, tc.want, out.String())
		}
	}
}

// TestServeHelperProcess is not a test: TestServe* tests re-run the test
// binary with BOP_TEST_SERVE=1 to get a real `bop serve` process.
func TestServeHelperProcess(t *testing.T) {
	if os.Getenv("BOP_TEST_SERVE") != "1" {
		t.Skip("helper process")
	}
	os.Exit(run(nil, os.Stdout, os.Stderr, os.Getenv))
}

func serveHelper(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeHelperProcess$")
	cmd.Env = append(os.Environ(), append([]string{"BOP_TEST_SERVE=1",
		"BOP_ADDR=127.0.0.1:0", "BOP_CONTROL_ADDR=127.0.0.1:0"}, env...)...)
	return cmd
}

func TestServeSigtermDuringBootFollowsPolicy(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "mode")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	cmd := serveHelper(t, "BOP_STATE_DIR="+dir, "BOP_ON_SIGTERM=exit:7")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Opening the FIFO for writing returns once bop has opened it to read
	// the sticky file, i.e. while it is blocked mid-boot.
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
		}
		opened <- f
	}()
	var w *os.File
	select {
	case w = <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("bop never opened the sticky file")
	}
	if w == nil {
		t.FailNow()
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the signal land before boot continues
	_, _ = w.WriteString("healthy\n")
	w.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 7 {
			t.Fatalf("SIGTERM during boot: %v, want exit status 7 (BOP_ON_SIGTERM=exit:7)\n%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bop did not exit after SIGTERM")
	}
}

func TestServeControlAddrInUseHasNoSideEffects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dir := t.TempDir()
	var out, errb bytes.Buffer
	code := run(nil, &out, &errb, env(map[string]string{
		"BOP_CONTROL_ADDR": ln.Addr().String(), "BOP_ADDR": "127.0.0.1:0",
		"BOP_STATE_DIR": dir, "BOP_MODE": "unhealthy sticky=true",
	}))
	if code == 0 {
		t.Fatal("serve with its control address in use exited 0")
	}
	if _, err := os.Stat(filepath.Join(dir, "mode")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serve that could not start left a sticky file behind: %v", err)
	}
	if !strings.Contains(errb.String(), ln.Addr().String()) || !strings.Contains(errb.String(), "bop status") {
		t.Fatalf("stderr lacks a hint naming the control address:\n%s", errb.String())
	}
}

func TestServeStickyOOMRefusedBeforePersist(t *testing.T) {
	memMax := filepath.Join(t.TempDir(), "memory.max")
	if err := os.WriteFile(memMax, []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := checkOOMLimit
	checkOOMLimit = func() error { return oom.Check(memMax) }
	t.Cleanup(func() { checkOOMLimit = old })

	dir := t.TempDir()
	var out, errb bytes.Buffer
	code := run(nil, &out, &errb, env(map[string]string{
		"BOP_CONTROL_ADDR": "127.0.0.1:0", "BOP_ADDR": "127.0.0.1:0",
		"BOP_STATE_DIR": dir, "BOP_MODE": "oom sticky=true",
	}))
	// A missing limit is a configuration mistake, so the usage text follows.
	if code != 2 || !strings.Contains(errb.String(), "no memory limit") || !strings.Contains(errb.String(), "Usage:") {
		t.Fatalf("code = %d, want 2 with the oom refusal and usage\n%s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "mode")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused oom sticky=true persisted a sticky file: %v", err)
	}
}

// An oom guard that cannot read the cgroup is an environment problem, not a
// usage mistake: exit 2 with the remedy and no usage text.
func TestServeOOMGuardFailureHasNoUsage(t *testing.T) {
	old := checkOOMLimit
	checkOOMLimit = func() error { return oom.Check(filepath.Join(t.TempDir(), "missing", "memory.max")) }
	t.Cleanup(func() { checkOOMLimit = old })

	for _, bootMode := range []string{"oom", "oom sticky=true"} {
		dir := t.TempDir()
		var out, errb bytes.Buffer
		code := run(nil, &out, &errb, env(map[string]string{
			"BOP_CONTROL_ADDR": "127.0.0.1:0", "BOP_ADDR": "127.0.0.1:0",
			"BOP_STATE_DIR": dir, "BOP_MODE": bootMode,
		}))
		if code != 2 || !strings.Contains(errb.String(), "BOP_OOM_UNSAFE") || strings.Contains(errb.String(), "Usage:") {
			t.Errorf("BOP_MODE=%q: code = %d, want 2 with the remedy and no usage\n%s", bootMode, code, errb.String())
		}
	}
}

func TestBootMode(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	noLimit := func() error { return fmt.Errorf("%w: memory.max is \"max\"", oom.ErrNoLimit) }
	for _, tc := range []struct {
		name     string
		stored   string // file content; "" means no file
		env      string // BOP_MODE
		checkOOM func() error
		want     string // resulting mode, "" when an error is expected
		wantFile string // file content afterwards; "" means no file
	}{
		{name: "no file, healthy", env: "healthy", want: "healthy"},
		{name: "no file, sticky BOP_MODE writes it", env: "hang sticky=true",
			want: "hang sticky=true", wantFile: "hang sticky=true\n"},
		{name: "no file, healthy sticky persists nothing", env: "healthy sticky=true", want: "healthy"},
		{name: "stored file overrides a sticky BOP_MODE", stored: "crash exit=3 after=1h0m0s sticky=true\n",
			env: "unhealthy sticky=true", want: "crash exit=3 after=1h0m0s sticky=true",
			wantFile: "crash exit=3 after=1h0m0s sticky=true\n"},
		{name: "stored file is sticky even if written without it", stored: "hang\n", env: "healthy",
			want: "hang sticky=true", wantFile: "hang\n"},
		{name: "invalid stored file", stored: "bogus\n", env: "healthy", wantFile: "bogus\n"},
		{name: "sticky oom without a limit persists nothing", env: "oom sticky=true", checkOOM: noLimit},
		{name: "sticky oom with a limit persists", env: "oom sticky=true", checkOOM: func() error { return nil },
			want: "oom rate=64 sticky=true", wantFile: "oom rate=64 sticky=true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mode")
			if tc.stored != "" {
				if err := os.WriteFile(path, []byte(tc.stored), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			env, err := mode.Parse(tc.env)
			if err != nil {
				t.Fatal(err)
			}
			check := tc.checkOOM
			if check == nil {
				check = func() error { t.Error("unexpected oom check"); return nil }
			}

			got, err := bootMode(mode.Store{Dir: dir}, env, check, log)
			switch {
			case tc.want == "" && err == nil:
				t.Errorf("bootMode = %q, want an error", got.String())
			case tc.want != "" && err != nil:
				t.Errorf("bootMode: %v", err)
			case tc.want != "" && got.String() != tc.want:
				t.Errorf("bootMode = %q, want %q", got.String(), tc.want)
			}
			b, err := os.ReadFile(path)
			if tc.wantFile == "" && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("sticky file = %q, %v; want none", b, err)
			}
			if tc.wantFile != "" && string(b) != tc.wantFile {
				t.Errorf("sticky file = %q, %v; want %q", b, err, tc.wantFile)
			}
		})
	}

	// A sticky BOP_MODE that cannot be saved is an error (here Dir is a file).
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := bootMode(mode.Store{Dir: notDir}, mode.Mode{Name: mode.Hang, Sticky: true}, nil, log); err == nil {
		t.Error("bootMode with an unwritable state dir succeeded")
	}
}

// redirectingServer returns a server that redirects every request to a
// server that answers 200.
func redirectingServer(t *testing.T) *httptest.Server {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"instance_id":"abc"}`))
	}))
	t.Cleanup(target.Close)
	redir := httptest.NewServer(http.RedirectHandler(target.URL+"/info", http.StatusFound))
	t.Cleanup(redir.Close)
	return redir
}

func TestHealthcheckDoesNotFollowRedirects(t *testing.T) {
	ts := redirectingServer(t)
	var errb bytes.Buffer
	if code := healthcheck([]string{"--url=" + ts.URL}, &errb, env(nil)); code != 1 {
		t.Fatalf("302 -> %d, want 1 (stderr %q)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "302") {
		t.Errorf("stderr should name the 302: %q", errb.String())
	}
}

func TestPollDoesNotFollowRedirects(t *testing.T) {
	ts := redirectingServer(t)
	if id, err := poll(context.Background(), newProbeClient(time.Second), ts.URL+"/info"); err == nil {
		t.Fatalf("poll followed the redirect: id %q", id)
	}
}

func TestProbeClientIgnoresProxyEnvironment(t *testing.T) {
	c := newProbeClient(time.Second)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("probe transport must not use a proxy")
	}
	if tr == http.DefaultTransport {
		t.Fatal("probe transport must be a clone, not http.DefaultTransport")
	}
	if c.Timeout != time.Second {
		t.Fatalf("Timeout = %s", c.Timeout)
	}
}

func TestPrintStateStickyMode(t *testing.T) {
	for want, st := range map[string]server.State{
		"sticky_mode: (none)\n":                         {},
		"sticky_mode: unhealthy after=0s sticky=true\n": {Info: server.Info{StickyMode: "unhealthy after=0s sticky=true"}},
	} {
		var out bytes.Buffer
		printState(&out, st)
		if !strings.Contains(out.String(), want) {
			t.Errorf("printState(%+v) lacks %q:\n%s", st, want, out.String())
		}
	}
}
