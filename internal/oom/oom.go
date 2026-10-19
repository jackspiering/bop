// Package oom allocates memory until the cgroup kills the process.
package oom

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// MemoryMaxPath is the cgroup v2 limit of the container.
const MemoryMaxPath = "/sys/fs/cgroup/memory.max"

const (
	mib      = 1 << 20
	pageSize = 4096
)

// ErrNoLimit means the container has no memory limit.
var ErrNoLimit = errors.New("no memory limit")

// Check refuses to run unless the cgroup at path has a memory limit. The
// caller may skip the check (BOP_OOM_UNSAFE=1).
func Check(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s (cgroup v2 required): %w; set BOP_OOM_UNSAFE=1 to run anyway", path, err)
	}
	if strings.TrimSpace(string(b)) == "max" {
		return fmt.Errorf("%w: %s is \"max\", so oom would consume host memory; "+
			"start the container with a memory limit (--memory / mem_limit) or set BOP_OOM_UNSAFE=1", ErrNoLimit, path)
	}
	return nil
}

// Run allocates and touches rate MiB per second until stop is closed or the
// kernel kills the process. Released memory is left to the garbage collector.
func Run(rate int, stop <-chan struct{}) {
	const ticks = 10
	perTick := max(rate*mib/ticks, pageSize)
	held := make([][]byte, 0, 1024)
	t := time.NewTicker(time.Second / ticks)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			held = append(held, touch(perTick))
		}
	}
}

// touch returns n bytes with every page written, so the memory is resident.
func touch(n int) []byte {
	b := make([]byte, n)
	for i := 0; i < len(b); i += pageSize {
		b[i] = 1
	}
	return b
}
