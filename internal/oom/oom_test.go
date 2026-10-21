package oom

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	write := func(content string) string {
		p := filepath.Join(t.TempDir(), "memory.max")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := Check(write("max\n")); !errors.Is(err, ErrNoLimit) || !strings.Contains(err.Error(), "BOP_OOM_UNSAFE") {
		t.Fatalf("unlimited: %v", err)
	}
	if err := Check(write("67108864\n")); err != nil {
		t.Fatalf("limited: %v", err)
	}
	if err := Check(filepath.Join(t.TempDir(), "missing")); err == nil || errors.Is(err, ErrNoLimit) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestTouchFillsEveryPage(t *testing.T) {
	b := touch(3*pageSize + 10)
	for i := 0; i < len(b); i += pageSize {
		if b[i] != 1 {
			t.Fatalf("page at %d not written", i)
		}
	}
}

func TestRunStops(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { Run(1, stop); close(done) }()
	time.Sleep(250 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestCheckResolvesOwnCgroup(t *testing.T) {
	tests := []struct {
		name    string
		proc    string            // content of /proc/self/cgroup
		files   map[string]string // memory.max files, relative to the cgroup root
		want    string            // resolved path, relative to the cgroup root
		noLimit bool              // Check refuses with ErrNoLimit
		errHas  string            // resolve refuses with this message
	}{
		{name: "private namespace", proc: "0::/\n", files: map[string]string{"memory.max": "67108864\n"}, want: "memory.max"},
		{
			name:  "host namespace, nested",
			proc:  "0::/system.slice/docker-abc.scope\n",
			files: map[string]string{"system.slice/docker-abc.scope/memory.max": "67108864\n"},
			want:  "system.slice/docker-abc.scope/memory.max",
		},
		{
			name:    "host namespace, max leaf",
			proc:    "0::/system.slice/docker-abc.scope\n",
			files:   map[string]string{"system.slice/docker-abc.scope/memory.max": "max\n"},
			want:    "system.slice/docker-abc.scope/memory.max",
			noLimit: true,
		},
		{
			name:  "nested path missing, falls back to the root",
			proc:  "0::/sub\n",
			files: map[string]string{"memory.max": "67108864\n"},
			want:  "memory.max",
		},
		{name: "path outside the namespace", proc: "0::/../../x.scope\n", files: map[string]string{"memory.max": "67108864\n"}, want: "memory.max"},
		{name: "cgroup v1 only", proc: "12:memory:/docker/abc\n1:name=systemd:/docker/abc\n", errHas: "cgroup v2 required"},
		{name: "no memory controller", proc: "0::/system.slice/x.scope\n", errHas: "memory controller"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for rel, content := range tt.files {
				p := filepath.Join(root, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			proc := filepath.Join(t.TempDir(), "cgroup")
			if err := os.WriteFile(proc, []byte(tt.proc), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := resolve(root, proc)
			if tt.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errHas) || !strings.Contains(err.Error(), "BOP_OOM_UNSAFE") {
					t.Fatalf("resolve = %q, %v; want an error containing %q and BOP_OOM_UNSAFE", got, err, tt.errHas)
				}
				return
			}
			if err != nil || got != filepath.Join(root, tt.want) {
				t.Fatalf("resolve = %q, %v; want %q", got, err, filepath.Join(root, tt.want))
			}
			err = Check(got)
			if tt.noLimit != (err != nil) || (tt.noLimit && !errors.Is(err, ErrNoLimit)) {
				t.Fatalf("Check = %v, want ErrNoLimit: %v", err, tt.noLimit)
			}
		})
	}
}
