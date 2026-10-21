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
	const meminfo16G = "MemTotal:       16777216 kB\nMemFree:         1000000 kB\nSwapTotal:       2097152 kB\n"
	const meminfoNoSwap = "MemTotal:       16777216 kB\nSwapTotal:             0 kB\n"
	tests := []struct {
		name    string
		memMax  string // "" means missing
		swapMax string // "" means missing
		meminfo string // "" means missing
		noLimit bool   // refused with ErrNoLimit
		errHas  string // refused with this message ("" means allowed)
	}{
		{name: "max", memMax: "max\n", meminfo: meminfo16G, noLimit: true, errHas: "memory limit"},
		{name: "limited", memMax: "67108864\n", swapMax: "0\n", meminfo: meminfo16G},
		{name: "1 TiB", memMax: "1099511627776\n", swapMax: "0\n", meminfo: meminfo16G, noLimit: true, errHas: "MemTotal"},
		{name: "equal to MemTotal", memMax: "17179869184\n", swapMax: "0\n", meminfo: meminfo16G, noLimit: true, errHas: "MemTotal"},
		{name: "just below MemTotal", memMax: "17179865088\n", swapMax: "0\n", meminfo: meminfo16G},
		{name: "unparsable", memMax: "lots\n", meminfo: meminfo16G, errHas: "cannot parse"},
		{name: "empty", memMax: "\n", meminfo: meminfo16G, errHas: "cannot parse"},
		{name: "missing memory.max", meminfo: meminfo16G, errHas: "cannot read"},
		{name: "swap max with host swap", memMax: "67108864\n", swapMax: "max\n", meminfo: meminfo16G, noLimit: true, errHas: "--memory-swap"},
		{name: "swap max without host swap", memMax: "67108864\n", swapMax: "max\n", meminfo: meminfoNoSwap},
		{name: "swap limited", memMax: "67108864\n", swapMax: "67108864\n", meminfo: meminfo16G},
		{name: "missing memory.swap.max", memMax: "67108864\n", meminfo: meminfo16G},
		{name: "missing meminfo", memMax: "67108864\n", swapMax: "0\n", errHas: "cannot read"},
		{name: "meminfo without MemTotal", memMax: "67108864\n", swapMax: "0\n", meminfo: "MemFree: 1 kB\n", errHas: "no MemTotal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := func(name, content string) string {
				p := filepath.Join(dir, name)
				if content != "" {
					if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return p
			}
			err := check(path("memory.max", tt.memMax), path("memory.swap.max", tt.swapMax), path("meminfo", tt.meminfo))
			if tt.errHas == "" {
				if err != nil {
					t.Fatalf("check = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.errHas) || !strings.Contains(err.Error(), "BOP_OOM_UNSAFE") {
				t.Fatalf("check = %v, want an error containing %q and BOP_OOM_UNSAFE", err, tt.errHas)
			}
			if errors.Is(err, ErrNoLimit) != tt.noLimit {
				t.Fatalf("check = %v, errors.Is(ErrNoLimit) = %v, want %v", err, !tt.noLimit, tt.noLimit)
			}
		})
	}
}

// Check reads the sibling memory.swap.max of the resolved memory.max.
func TestCheckReadsSiblingSwapMax(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"memory.max": "4096\n", "memory.swap.max": "max\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, swap, err := meminfo(meminfoPath); err != nil || swap == 0 {
		t.Skipf("host has no swap (%v)", err)
	}
	if err := Check(filepath.Join(dir, "memory.max")); err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "memory.swap.max")) {
		t.Fatalf("Check = %v, want a refusal naming the sibling memory.swap.max", err)
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
			meminfo := filepath.Join(t.TempDir(), "meminfo")
			if err := os.WriteFile(meminfo, []byte("MemTotal: 16777216 kB\nSwapTotal: 0 kB\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			err = check(got, filepath.Join(filepath.Dir(got), "memory.swap.max"), meminfo)
			if tt.noLimit != (err != nil) || (tt.noLimit && !errors.Is(err, ErrNoLimit)) {
				t.Fatalf("Check = %v, want ErrNoLimit: %v", err, tt.noLimit)
			}
		})
	}
}
