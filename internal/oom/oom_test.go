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
