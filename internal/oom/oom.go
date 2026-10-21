// Package oom allocates memory until the cgroup kills the process.
package oom

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	cgroupRoot     = "/sys/fs/cgroup"
	procSelfCgroup = "/proc/self/cgroup"
	meminfoPath    = "/proc/meminfo"
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

// Check refuses to run unless the cgroup whose memory.max is at path bounds
// oom below host memory: the limit must parse, be below MemTotal, and swap must
// be limited too (memory.swap.max not "max") when the host has swap. A missing
// memory.swap.max is allowed, because swap accounting can be disabled. The
// caller may skip the check (BOP_OOM_UNSAFE=1).
func Check(path string) error {
	return check(path, filepath.Join(filepath.Dir(path), "memory.swap.max"), meminfoPath)
}

func check(memMaxPath, swapMaxPath, meminfoPath string) error {
	b, err := os.ReadFile(memMaxPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w; set BOP_OOM_UNSAFE=1 to run anyway", memMaxPath, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "max" {
		return fmt.Errorf("%w: %s is \"max\", so oom would consume host memory; "+
			"start the container with a memory limit (--memory / mem_limit) or set BOP_OOM_UNSAFE=1", ErrNoLimit, memMaxPath)
	}
	limit, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("cannot parse %s (%q); set BOP_OOM_UNSAFE=1 to run anyway", memMaxPath, v)
	}
	memTotal, swapTotal, err := meminfo(meminfoPath)
	if err != nil {
		return fmt.Errorf("%w; set BOP_OOM_UNSAFE=1 to run anyway", err)
	}
	if limit >= memTotal {
		return fmt.Errorf("%w: %s is %d bytes, at least the host's MemTotal of %d bytes, so oom would consume host memory; "+
			"start the container with a lower memory limit or set BOP_OOM_UNSAFE=1", ErrNoLimit, memMaxPath, limit, memTotal)
	}
	b, err = os.ReadFile(swapMaxPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("cannot read %s: %w; set BOP_OOM_UNSAFE=1 to run anyway", swapMaxPath, err)
	}
	if strings.TrimSpace(string(b)) == "max" && swapTotal > 0 {
		return fmt.Errorf("%w: %s is \"max\", so oom would fill host swap before the cgroup kill; "+
			"set --memory-swap equal to --memory (memswap_limit = mem_limit) or set BOP_OOM_UNSAFE=1", ErrNoLimit, swapMaxPath)
	}
	return nil
}

// meminfo returns MemTotal and SwapTotal in bytes.
func meminfo(path string) (memTotal, swapTotal int64, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("cannot read %s: %w", path, err)
	}
	found := 0
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) < 2 || (f[0] != "MemTotal:" && f[0] != "SwapTotal:") {
			continue
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("cannot parse %s in %s: %w", f[0], path, err)
		}
		if f[0] == "MemTotal:" {
			memTotal = kb * 1024
		} else {
			swapTotal = kb * 1024
		}
		found++
	}
	if memTotal == 0 || found < 2 {
		return 0, 0, fmt.Errorf("no MemTotal and SwapTotal in %s", path)
	}
	return memTotal, swapTotal, nil
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
