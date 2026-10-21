package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackspiering/bop/internal/mode"
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
	} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb, env(nil)); code != 2 {
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
	for _, want := range []string{"unknown mode", "Usage:", "crash-loop", "BOP_ON_SIGTERM"} {
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
}

func TestRunExecWithoutServerExits1(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"unhealthy"}, &out, &errb, env(map[string]string{"BOP_CONTROL_ADDR": "127.0.0.1:1"}))
	if code != 1 || !strings.Contains(errb.String(), "control listener") {
		t.Fatalf("code = %d, stderr %q", code, errb.String())
	}
}

func TestDefaultHealthURL(t *testing.T) {
	for in, want := range map[string]string{
		":8080":          "http://127.0.0.1:8080/healthz",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/healthz",
		"[::]:9000":      "http://127.0.0.1:9000/healthz",
		"10.1.2.3:81":    "http://10.1.2.3:81/healthz",
		"localhost:7000": "http://localhost:7000/healthz",
		"9000":           "http://127.0.0.1:9000/healthz",
	} {
		if got := defaultHealthURL(in); got != want {
			t.Errorf("defaultHealthURL(%q) = %q, want %q", in, got, want)
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

	for _, bad := range []map[string]string{
		{"BOP_MODE": "explode"},
		{"BOP_MODE": "clear"},
		{"BOP_START_DELAY": "soon"},
		{"BOP_START_DELAY": "-1s"},
		{"BOP_ON_SIGTERM": "reboot"},
	} {
		_, err := loadConfig(env(bad))
		var ue *mode.UsageError
		if !errors.As(err, &ue) {
			t.Errorf("loadConfig(%v) = %v, want a usage error", bad, err)
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
			// serve returns before binding either listener.
			code := serve(&errb, env(map[string]string{
				"BOP_STATE_DIR": dir, "BOP_CONTROL_ADDR": "127.0.0.1:0", "BOP_ADDR": "127.0.0.1:0",
			}))
			if code != 2 {
				t.Fatalf("serve = %d, want 2 (stderr %q)", code, errb.String())
			}
			// bop clear needs the control listener, which never binds.
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

	// delay exits 0 only after the delay, and further signals do not restart it.
	start := time.Now()
	if code := handleSignals(log, mode.Sigterm{Kind: mode.SigtermDelay, Delay: 150 * time.Millisecond}, send(3)); code != 0 {
		t.Fatalf("delay -> %d", code)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("delay exited early")
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
