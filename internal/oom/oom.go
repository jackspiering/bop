// Package oom allocates memory until the cgroup kills the process.
package oom

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	cgroupRoot     = "/sys/fs/cgroup"
	procSelfCgroup = "/proc/self/cgroup"
)

const (
	mib      = 1 << 20
	pageSize = 4096
)

// ErrNoLimit means the container has no memory limit.
var ErrNoLimit = errors.New("no memory limit")

// CheckOwnCgroup checks the memory limit of the cgroup this process runs in.
// The caller may skip the check (BOP_OOM_UNSAFE=1).
func CheckOwnCgroup() error {
	path, err := resolve(cgroupRoot, procSelfCgroup)
	if err != nil {
		return err
	}
	return Check(path)
}

// resolve returns the memory.max of the cgroup v2 path in procSelfCgroup. With a
// private cgroup namespace that is root/memory.max; with a host namespace root
// is the host's root cgroup and the limit is in the process's own cgroup below
// it. Only the leaf counts: an ancestor's limit is shared with other processes.
func resolve(root, procSelfCgroup string) (string, error) {
	b, err := os.ReadFile(procSelfCgroup)
	if err != nil {
		return "", fmt.Errorf("cannot read %s (cgroup v2 required): %w; set BOP_OOM_UNSAFE=1 to run anyway", procSelfCgroup, err)
	}
	var rel string
	found := false
	for line := range strings.Lines(string(b)) {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			rel, found = p, true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("no cgroup v2 entry in %s (cgroup v2 required); set BOP_OOM_UNSAFE=1 to run anyway", procSelfCgroup)
	}
	candidates := []string{filepath.Join(root, "memory.max")}
	if rel != "/" && !strings.HasPrefix(rel, "/..") {
		candidates = append([]string{filepath.Join(root, rel, "memory.max")}, candidates...)
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	missing := candidates[0] + " does not exist"
	if len(candidates) == 2 {
		missing = "neither " + candidates[0] + " nor " + candidates[1] + " exists"
	}
	return "", fmt.Errorf("memory controller unavailable: %s; set BOP_OOM_UNSAFE=1 to run anyway", missing)
}

// Check refuses to run unless the cgroup at path has a memory limit. The
// caller may skip the check (BOP_OOM_UNSAFE=1).
func Check(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w; set BOP_OOM_UNSAFE=1 to run anyway", path, err)
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
