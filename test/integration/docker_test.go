//go:build integration

// Package integration drives real Docker through the docker CLI and asserts
// on Docker's own view (docker inspect), not just on bop's output.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	image        = "bop:test"
	sidecarImage = "bop:test-sidecar"
)

// fastHealth shortens the probe timings while keeping the image's own test
// command (docker merges these flags with the image HEALTHCHECK).
var fastHealth = []string{"--health-interval=1s", "--health-timeout=2s", "--health-retries=2", "--health-start-period=0s"}

func TestMain(m *testing.M) {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if out, err := dockerCtx(10*time.Minute, "build", "-q", "-t", image, root); err != nil {
		fmt.Fprintf(os.Stderr, "docker build failed: %v\n%s\n", err, out)
		os.Exit(1)
	}
	// distroless has no shell, and `docker run --health-cmd` always uses one,
	// so the sidecar's exec-form healthcheck comes from a derived image.
	derived := "FROM " + image + "\n" +
		`HEALTHCHECK --interval=1s --timeout=2s --start-period=0s --retries=2 CMD ["/bop","healthcheck","--url=http://127.0.0.1:8080/info"]` + "\n"
	cmd := exec.Command("docker", "build", "-q", "-t", sidecarImage, "-")
	cmd.Stdin = strings.NewReader(derived)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "docker build (sidecar) failed: %v\n%s\n", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		if dir == filepath.Dir(dir) {
			return "", fmt.Errorf("go.mod not found above %s", wd)
		}
	}
}

func dockerCtx(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func docker(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return dockerCtx(2*time.Minute, args...)
}

func mustDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := docker(t, args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

var nameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func uniqueName(t *testing.T) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	base := strings.ToLower(nameUnsafe.ReplaceAllString(t.Name(), "-"))
	return "bop-it-" + base + "-" + hex.EncodeToString(b[:])
}

// run starts a detached container and removes it when the test ends.
func run(t *testing.T, args ...string) string {
	t.Helper()
	return runNamed(t, uniqueName(t), args...)
}

func runNamed(t *testing.T, name string, args ...string) string {
	t.Helper()
	t.Cleanup(func() { _, _ = dockerCtx(time.Minute, "rm", "-f", "-v", name) })
	all := append([]string{"run", "-d", "--name", name}, args...)
	mustDocker(t, all...)
	return name
}

func inspect(t *testing.T, name, format string) string {
	t.Helper()
	return mustDocker(t, "inspect", "-f", format, name)
}

func logs(name string) string {
	out, _ := dockerCtx(30*time.Second, "logs", name)
	return out
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, name string, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s\nstate: %s\nlogs:\n%s", timeout, what,
		strings.TrimSpace(mustDockerQuiet("inspect", "-f", "{{json .State}}", name)), logs(name))
}

func mustDockerQuiet(args ...string) string {
	out, _ := dockerCtx(30*time.Second, args...)
	return out
}

func health(t *testing.T, name string) string {
	t.Helper()
	return inspect(t, name, "{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}")
}

func waitHealth(t *testing.T, name, want string, timeout time.Duration) {
	t.Helper()
	waitFor(t, name, timeout, "health "+want, func() bool { return health(t, name) == want })
}

func waitExited(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	waitFor(t, name, timeout, "container exit", func() bool { return inspect(t, name, "{{.State.Status}}") == "exited" })
}

func exitCode(t *testing.T, name string) int {
	t.Helper()
	n, err := strconv.Atoi(inspect(t, name, "{{.State.ExitCode}}"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// execCode runs a command in the container and returns its exit code and output.
// dieExitCodes returns the exit code of every exit so far. State.ExitCode
// is reset to 0 when a restart policy starts the container again, so a
// crash-looping container's codes are read from its die events.
func dieExitCodes(t *testing.T, name string) []int {
	t.Helper()
	out := mustDocker(t, "events", "--since", inspect(t, name, "{{.Created}}"), "--until", "0s",
		"--filter", "container="+name, "--filter", "event=die", "--format", "{{.Actor.Attributes.exitCode}}")
	var codes []int
	for _, f := range strings.Fields(out) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("die event exit code %q: %v", f, err)
		}
		codes = append(codes, n)
	}
	return codes
}

// assertDieExitCodes waits until a die event is visible (it can lag behind
// RestartCount) and fails unless every exit so far used want.
func assertDieExitCodes(t *testing.T, name string, want int) {
	t.Helper()
	var codes []int
	waitFor(t, name, 30*time.Second, "a die event", func() bool {
		codes = dieExitCodes(t, name)
		return len(codes) > 0
	})
	for _, c := range codes {
		if c != want {
			t.Fatalf("exit codes = %v, want all %d", codes, want)
		}
	}
}

func execCode(t *testing.T, name string, args ...string) (int, string) {
	t.Helper()
	out, err := docker(t, append([]string{"exec", name}, args...)...)
	if err == nil {
		return 0, out
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out
	}
	t.Fatalf("docker exec %v: %v", args, err)
	return -1, out
}

func mustExec(t *testing.T, name string, args ...string) string {
	t.Helper()
	code, out := execCode(t, name, args...)
	if code != 0 {
		t.Fatalf("docker exec %s %s = %d\n%s", name, strings.Join(args, " "), code, out)
	}
	return out
}

func env(k, v string) []string { return []string{"-e", k + "=" + v} }

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestImageMetadata(t *testing.T) {
	t.Parallel()
	for format, want := range map[string]string{
		"{{.Config.User}}":                                                "65532:65532",
		"{{json .Config.Entrypoint}}":                                     `["/bop"]`,
		"{{json .Config.ExposedPorts}}":                                   `{"8080/tcp":{}}`,
		`{{index .Config.Labels "org.opencontainers.image.title"}}`:       "bop",
		`{{index .Config.Labels "org.opencontainers.image.description"}}`: "Deliberately failing test image. Not for production.",
		`{{index .Config.Labels "org.opencontainers.image.source"}}`:      "https://github.com/jackspiering/bop",
		`{{index .Config.Labels "org.opencontainers.image.licenses"}}`:    "MIT",
		"{{json .Config.Healthcheck.Test}}":                               `["CMD","/bop","healthcheck"]`,
		"{{.Config.Healthcheck.Interval}}":                                "5s",
		"{{.Config.Healthcheck.Timeout}}":                                 "2s",
		"{{.Config.Healthcheck.StartPeriod}}":                             "10s",
		"{{.Config.Healthcheck.StartInterval}}":                           "1s",
		"{{.Config.Healthcheck.Retries}}":                                 "3",
	} {
		if got := mustDocker(t, "image", "inspect", "-f", format, image); got != want {
			t.Errorf("%s = %q, want %q", format, got, want)
		}
	}
	for _, key := range []string{"revision", "created", "version"} {
		if got := mustDocker(t, "image", "inspect", "-f", `{{index .Config.Labels "org.opencontainers.image.`+key+`"}}`, image); got == "" {
			t.Errorf("label %s is empty", key)
		}
	}
}

// The image's own HEALTHCHECK, with no overrides.
func TestHealthyUsesImageHealthcheck(t *testing.T) {
	t.Parallel()
	c := run(t, image)
	if got := inspect(t, c, "{{json .Config.Healthcheck.Test}}"); got != `["CMD","/bop","healthcheck"]` {
		t.Fatalf("container healthcheck = %s", got)
	}
	waitHealth(t, c, "healthy", 30*time.Second)
	if out := mustExec(t, c, "bop", "version"); !strings.HasPrefix(out, "bop ") {
		t.Fatalf("bop on PATH: %q", out)
	}
}

func TestHealthyUnhealthyHealthy(t *testing.T) {
	t.Parallel()
	c := run(t, cat(fastHealth, []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)

	out := mustExec(t, c, "bop", "unhealthy")
	if !strings.Contains(out, "unhealthy") {
		t.Fatalf("exec output should show the new state:\n%s", out)
	}
	waitHealth(t, c, "unhealthy", 30*time.Second)
	if inspect(t, c, "{{.State.Running}}") != "true" {
		t.Fatal("container must keep running while unhealthy")
	}

	mustExec(t, c, "bop", "healthy")
	waitHealth(t, c, "healthy", 30*time.Second)
}

func TestUnhealthyAfter(t *testing.T) {
	t.Parallel()
	c := run(t, cat(fastHealth, env("BOP_MODE", "unhealthy after=4s"), []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	started := time.Now()
	waitHealth(t, c, "unhealthy", 30*time.Second)
	// The flip is at ~4s after start; two failed 1s probes then mark it.
	if d := time.Since(started); d > 20*time.Second {
		t.Fatalf("flip took %s", d)
	}
	if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "unhealthy after=4s") {
		t.Fatalf("status:\n%s", out)
	}
}

func TestCrashAtBoot(t *testing.T) {
	t.Parallel()
	c := run(t, cat(env("BOP_MODE", "crash exit=3"), []string{image})...)
	waitExited(t, c, 30*time.Second)
	if got := exitCode(t, c); got != 3 {
		t.Fatalf("exit code = %d, want 3", got)
	}
}

func TestCrashAfter(t *testing.T) {
	t.Parallel()
	c := run(t, cat(env("BOP_MODE", "crash exit=4 after=3s"), []string{image})...)
	time.Sleep(time.Second)
	if inspect(t, c, "{{.State.Status}}") != "running" {
		t.Fatal("crashed before after=3s")
	}
	waitExited(t, c, 30*time.Second)
	if got := exitCode(t, c); got != 4 {
		t.Fatalf("exit code = %d, want 4", got)
	}
}

func TestCrashViaExec(t *testing.T) {
	t.Parallel()
	c := run(t, cat(fastHealth, []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	// The exec succeeds because PID 1 responds first and exits afterwards.
	mustExec(t, c, "bop", "crash", "exit=3")
	waitExited(t, c, 30*time.Second)
	if got := exitCode(t, c); got != 3 {
		t.Fatalf("exit code = %d, want 3", got)
	}
}

func TestCrashLoop(t *testing.T) {
	t.Parallel()
	c := run(t, cat([]string{"--restart=always"}, env("BOP_MODE", "crash-loop interval=1s exit=1"), []string{image})...)
	waitFor(t, c, 60*time.Second, "RestartCount >= 2", func() bool {
		n, _ := strconv.Atoi(inspect(t, c, "{{.RestartCount}}"))
		return n >= 2
	})
	assertDieExitCodes(t, c, 1)
}

func TestCrashLoopWaitsForInterval(t *testing.T) {
	t.Parallel()
	c := run(t, cat([]string{"--restart=always"}, env("BOP_MODE", "crash-loop interval=3s exit=5"), []string{image})...)
	time.Sleep(time.Second)
	if n := inspect(t, c, "{{.RestartCount}}"); n != "0" {
		t.Fatalf("RestartCount = %s one second in, want 0 before interval=3s", n)
	}
	if got := inspect(t, c, "{{.State.Status}}"); got != "running" {
		t.Fatalf("status = %s one second in, want running", got)
	}
	if codes := dieExitCodes(t, c); len(codes) != 0 {
		t.Fatalf("exited before interval=3s with %v", codes)
	}
	waitFor(t, c, 60*time.Second, "RestartCount >= 1", func() bool {
		n, _ := strconv.Atoi(inspect(t, c, "{{.RestartCount}}"))
		return n >= 1
	})
	assertDieExitCodes(t, c, 5)
}

func TestCrashLoopRejectedWhenNotSticky(t *testing.T) {
	t.Parallel()
	c := run(t, image)
	waitHealth(t, c, "healthy", 30*time.Second)
	code, out := execCode(t, c, "bop", "crash-loop")
	if code != 2 || !strings.Contains(out, "sticky=true") {
		t.Fatalf("exec crash-loop = %d\n%s", code, out)
	}
	if health(t, c) != "healthy" || inspect(t, c, "{{.State.Running}}") != "true" {
		t.Fatal("rejected command must not affect the container")
	}
}

func TestCrashLoopViaStickyExec(t *testing.T) {
	t.Parallel()
	c := run(t, cat([]string{"--restart=always"}, []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	mustExec(t, c, "bop", "crash-loop", "interval=1s", "exit=6", "sticky=true")
	waitFor(t, c, 60*time.Second, "RestartCount >= 2", func() bool {
		n, _ := strconv.Atoi(inspect(t, c, "{{.RestartCount}}"))
		return n >= 2
	})
	assertDieExitCodes(t, c, 6)
}

func TestInvalidUsageExits2(t *testing.T) {
	t.Parallel()
	c := run(t, cat(env("BOP_MODE", "explode"), []string{image})...)
	waitExited(t, c, 30*time.Second)
	if got := exitCode(t, c); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
	if out := logs(c); !strings.Contains(out, "unknown mode") || !strings.Contains(out, "Usage:") {
		t.Fatalf("expected a usage message, got:\n%s", out)
	}
}

// A malformed address is a usage error at boot, not a data-listener failure
// once BOP_START_DELAY has passed.
func TestInvalidAddrExits2BeforeStartDelay(t *testing.T) {
	t.Parallel()
	start := time.Now()
	c := run(t, cat(env("BOP_ADDR", "nope"), env("BOP_START_DELAY", "20s"), []string{image})...)
	waitExited(t, c, 30*time.Second)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("exited after %s, want immediately (not after the 20s start delay)", d)
	}
	if got := exitCode(t, c); got != 2 {
		t.Fatalf("exit code = %d, want 2", got)
	}
	if out := logs(c); !strings.Contains(out, `invalid BOP_ADDR="nope"`) || !strings.Contains(out, "Usage:") {
		t.Fatalf("expected a usage message, got:\n%s", out)
	}
}

func oomFlags() []string { return []string{"--memory=64m", "--memory-swap=64m"} }

// expectOOMKilled starts a container with start, waits for it to die and
// asserts exit 137 and State.OOMKilled. Docker occasionally misses the cgroup
// OOM event when the container dies quickly (the exit is recorded with
// OOMKilled=false although the kernel killed it; seen about 1 in 30 runs), so
// that one outcome is retried with a fresh container. Any other outcome fails.
func expectOOMKilled(t *testing.T, start func(t *testing.T) string) {
	t.Helper()
	const attempts = 3
	for attempt := 1; ; attempt++ {
		c := start(t)
		waitExited(t, c, 60*time.Second)
		if got := exitCode(t, c); got != 137 {
			t.Fatalf("exit code = %d, want 137\n%s", got, logs(c))
		}
		killed := false
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !killed; time.Sleep(250 * time.Millisecond) {
			killed = inspect(t, c, "{{.State.OOMKilled}}") == "true"
		}
		if killed {
			return
		}
		if attempt == attempts {
			t.Fatalf("exit 137 but State.OOMKilled=false in %d attempts\n%s", attempts, logs(c))
		}
		t.Logf("attempt %d: exit 137 without OOMKilled (Docker missed the OOM event), retrying", attempt)
	}
}

func TestOOMAtBoot(t *testing.T) {
	t.Parallel()
	expectOOMKilled(t, func(t *testing.T) string {
		return run(t, cat(oomFlags(), env("BOP_MODE", "oom rate=32"), []string{image})...)
	})
}

func TestOOMViaExec(t *testing.T) {
	t.Parallel()
	expectOOMKilled(t, func(t *testing.T) string {
		c := run(t, cat(oomFlags(), []string{image})...)
		waitHealth(t, c, "healthy", 30*time.Second)
		mustExec(t, c, "bop", "oom", "rate=32")
		return c
	})
}

func TestOOMRefusedWithoutLimit(t *testing.T) {
	t.Parallel()
	c := run(t, image)
	waitHealth(t, c, "healthy", 30*time.Second)
	code, out := execCode(t, c, "bop", "oom")
	if code != 2 || !strings.Contains(out, "memory limit") {
		t.Fatalf("exec oom = %d\n%s", code, out)
	}
	if health(t, c) != "healthy" {
		t.Fatal("refused oom must leave the container healthy")
	}

	boot := run(t, cat(env("BOP_MODE", "oom"), []string{image})...)
	waitExited(t, boot, 30*time.Second)
	if got := exitCode(t, boot); got != 2 {
		t.Fatalf("boot oom without limit: exit code = %d, want 2", got)
	}
	if inspect(t, boot, "{{.State.OOMKilled}}") != "false" {
		t.Fatal("must not have been OOM killed")
	}

	// A refused sticky oom persists nothing, so it cannot come back.
	sticky := run(t, cat(env("BOP_MODE", "oom sticky=true"), []string{image})...)
	waitExited(t, sticky, 30*time.Second)
	if got := exitCode(t, sticky); got != 2 {
		t.Fatalf("boot oom sticky=true without limit: exit code = %d, want 2", got)
	}
	if hasStickyFile(t, sticky) {
		t.Fatal("a refused oom sticky=true left /tmp/bop/mode behind")
	}
}

func TestOOMStickyAtBootPersists(t *testing.T) {
	t.Parallel()
	var c string // the last attempt's container
	expectOOMKilled(t, func(t *testing.T) string {
		c = run(t, cat(oomFlags(), env("BOP_MODE", "oom rate=32 sticky=true"), []string{image})...)
		return c
	})
	if !hasStickyFile(t, c) {
		t.Fatal("oom sticky=true with a memory limit did not persist /tmp/bop/mode")
	}
}

// hasStickyFile reports whether the container (running or not) has a sticky file.
func hasStickyFile(t *testing.T, c string) bool {
	t.Helper()
	_, err := docker(t, "cp", c+":/tmp/bop/mode", "-")
	return err == nil
}

func TestOOMRefusedWithLimitAboveHostMemory(t *testing.T) {
	t.Parallel()
	c := run(t, cat([]string{"--memory=1t"}, env("BOP_MODE", "oom rate=1"), []string{image})...)
	waitExited(t, c, 30*time.Second)
	if got := exitCode(t, c); got != 2 {
		t.Fatalf("exit code = %d, want 2\n%s", got, logs(c))
	}
	if out := logs(c); !strings.Contains(out, "MemTotal") || !strings.Contains(out, "BOP_OOM_UNSAFE") {
		t.Fatalf("expected a refusal naming MemTotal and BOP_OOM_UNSAFE:\n%s", out)
	}

	// BOP_OOM_UNSAFE=1 still bypasses every check. At 1 MiB/s for a few
	// seconds this allocates almost nothing before the container is removed.
	unsafe := run(t, cat([]string{"--memory=1t"}, env("BOP_MODE", "oom rate=1"), env("BOP_OOM_UNSAFE", "1"), []string{image})...)
	waitFor(t, unsafe, 30*time.Second, "oom to start", func() bool { return strings.Contains(logs(unsafe), "allocating memory") })
	time.Sleep(time.Second)
	if inspect(t, unsafe, "{{.State.Running}}") != "true" {
		t.Fatalf("BOP_OOM_UNSAFE=1 must bypass the guard\n%s", logs(unsafe))
	}
	mustDocker(t, "rm", "-f", unsafe)
}

func TestOOMRefusedWithUnlimitedSwap(t *testing.T) {
	t.Parallel()
	// Assumes the Docker daemon runs on this host.
	if swapTotal(t) == 0 {
		t.Skip("host has no swap")
	}
	c := run(t, cat([]string{"--memory=64m", "--memory-swap=-1"}, env("BOP_MODE", "oom rate=1"), []string{image})...)
	waitExited(t, c, 30*time.Second)
	if got := exitCode(t, c); got != 2 {
		t.Fatalf("exit code = %d, want 2\n%s", got, logs(c))
	}
	if out := logs(c); !strings.Contains(out, "memory.swap.max") || !strings.Contains(out, "--memory-swap") {
		t.Fatalf("expected a refusal about swap:\n%s", out)
	}
}

func swapTotal(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		t.Skip(err)
	}
	for line := range strings.Lines(string(b)) {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "SwapTotal:" {
			n, _ := strconv.Atoi(f[1])
			return n
		}
	}
	return 0
}

// With a host cgroup namespace /sys/fs/cgroup is the host's root cgroup, so the
// limit must be read from the process's own cgroup.
func TestOOMHostCgroupns(t *testing.T) {
	t.Parallel()
	expectOOMKilled(t, func(t *testing.T) string {
		return run(t, cat(oomFlags(), []string{"--cgroupns=host"}, env("BOP_MODE", "oom rate=32"), []string{image})...)
	})

	c := run(t, cat([]string{"--cgroupns=host"}, env("BOP_MODE", "oom"), []string{image})...)
	waitExited(t, c, 30*time.Second)
	if got := exitCode(t, c); got != 2 {
		t.Fatalf("host cgroupns without limit: exit code = %d, want 2\n%s", got, logs(c))
	}
	if out := logs(c); !strings.Contains(out, "BOP_OOM_UNSAFE") {
		t.Fatalf("refusal must mention BOP_OOM_UNSAFE:\n%s", out)
	}
}

func TestHang(t *testing.T) {
	t.Parallel()
	c := run(t, cat(fastHealth, []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)

	mustExec(t, c, "bop", "hang")
	waitHealth(t, c, "unhealthy", 30*time.Second)
	if inspect(t, c, "{{.State.Running}}") != "true" {
		t.Fatal("container must keep running while hung")
	}
	// The control listener still answers.
	if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "mode:        hang") || !strings.Contains(out, "health:      hanging") {
		t.Fatalf("status:\n%s", out)
	}

	mustExec(t, c, "bop", "healthy")
	waitHealth(t, c, "healthy", 30*time.Second)
}

func TestStartDelay(t *testing.T) {
	t.Parallel()
	const delay = 8 * time.Second
	t0 := time.Now() // before docker run, so the container starts at or after t0
	c := run(t, cat(env("BOP_START_DELAY", delay.String()), []string{image})...)
	waitHealth(t, c, "starting", 5*time.Second)
	// The control listener is up while the data listener is not.
	if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "mode:        healthy") {
		t.Fatalf("status during delay:\n%s", out)
	}
	code, out := execCode(t, c, "bop", "healthcheck")
	l := logs(c)
	if time.Since(t0) >= delay {
		t.Fatalf("checks finished after BOP_START_DELAY=%s; host too slow to observe the delay", delay)
	}
	// Not bound at all (refused), not merely bound and not serving (timeout).
	if code != 1 || !strings.Contains(out, "connection refused") {
		t.Fatalf("data listener during the delay: healthcheck = %d\n%s", code, out)
	}
	if strings.Contains(l, "listener=data") {
		t.Fatalf("data listener bound during the delay:\n%s", l)
	}
	waitHealth(t, c, "healthy", 40*time.Second)
	// The bind happens at process start + delay >= t0 + delay.
	if d := time.Since(t0); d < delay {
		t.Fatalf("healthy after %s, before BOP_START_DELAY=%s", d, delay)
	}
}

// The boot mode is set at process start, so its timers include
// BOP_START_DELAY (documented under BOP_START_DELAY in the README).
func TestStartDelayTimersCountFromProcessStart(t *testing.T) {
	t.Parallel()
	const delay = 8 * time.Second
	t.Run("unhealthy after", func(t *testing.T) {
		t.Parallel()
		t0 := time.Now()
		c := run(t, cat(env("BOP_START_DELAY", delay.String()), env("BOP_MODE", "unhealthy after=3s"), []string{image})...)
		waitFor(t, c, delay, "unhealthy in bop status", func() bool {
			_, out := execCode(t, c, "bop", "status")
			return strings.Contains(out, "health:      unhealthy")
		})
		code, out := execCode(t, c, "bop", "healthcheck")
		if time.Since(t0) >= delay {
			t.Skip("host too slow to observe the delay")
		}
		if code != 1 || !strings.Contains(out, "connection refused") {
			t.Fatalf("data listener should still be unbound: healthcheck = %d\n%s", code, out)
		}
	})
	t.Run("crash after", func(t *testing.T) {
		t.Parallel()
		t0 := time.Now()
		c := run(t, cat(env("BOP_START_DELAY", delay.String()), env("BOP_MODE", "crash exit=3 after=2s"), []string{image})...)
		waitExited(t, c, 30*time.Second)
		if d := time.Since(t0); d >= delay {
			t.Fatalf("crash after=2s exited after %s, want before BOP_START_DELAY=%s", d, delay)
		}
		if got := exitCode(t, c); got != 3 {
			t.Fatalf("exit code = %d, want 3", got)
		}
		if strings.Contains(logs(c), "listener=data") {
			t.Fatal("the data listener bound before the crash")
		}
	})
}

func stop(t *testing.T, c string, seconds int) time.Duration {
	t.Helper()
	start := time.Now()
	mustDocker(t, "stop", "-t", strconv.Itoa(seconds), c)
	return time.Since(start)
}

func TestSigtermDefaultExits0(t *testing.T) {
	t.Parallel()
	c := run(t, image)
	waitHealth(t, c, "healthy", 30*time.Second)
	if d := stop(t, c, 20); d > 10*time.Second {
		t.Fatalf("docker stop took %s", d)
	}
	if got := exitCode(t, c); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}
}

func TestSigtermExitCode(t *testing.T) {
	t.Parallel()
	c := run(t, cat(env("BOP_ON_SIGTERM", "exit:7"), []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	stop(t, c, 20)
	if got := exitCode(t, c); got != 7 {
		t.Fatalf("exit code = %d, want 7", got)
	}
}

func TestSigtermIgnoreIsKilled(t *testing.T) {
	t.Parallel()
	c := run(t, cat(env("BOP_ON_SIGTERM", "ignore"), []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	if d := stop(t, c, 3); d < 3*time.Second {
		t.Fatalf("docker stop returned after %s, before the 3s timeout", d)
	}
	if got := exitCode(t, c); got != 137 {
		t.Fatalf("exit code = %d, want 137", got)
	}
}

func TestSigtermDelay(t *testing.T) {
	t.Parallel()
	c := run(t, cat(env("BOP_ON_SIGTERM", "delay:3s"), []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	d := stop(t, c, 20)
	if d < 3*time.Second || d > 15*time.Second {
		t.Fatalf("docker stop took %s, want about 3s", d)
	}
	if got := exitCode(t, c); got != 0 {
		t.Fatalf("exit code = %d, want 0 (a clean exit after the delay)", got)
	}
}

func TestSigintFollowsPolicy(t *testing.T) {
	t.Parallel()
	c := run(t, cat(env("BOP_ON_SIGTERM", "exit:7"), []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	mustDocker(t, "kill", "-s", "INT", c)
	waitExited(t, c, 10*time.Second)
	if got := exitCode(t, c); got != 7 {
		t.Fatalf("exit code after SIGINT = %d, want 7", got)
	}
}

func TestSigtermDuringStartDelay(t *testing.T) {
	t.Parallel()
	c := run(t, cat(env("BOP_START_DELAY", "60s"), env("BOP_ON_SIGTERM", "exit:5"), []string{image})...)
	waitFor(t, c, 20*time.Second, "control listener", func() bool {
		code, _ := execCode(t, c, "bop", "status")
		return code == 0
	})
	if code, out := execCode(t, c, "bop", "healthcheck"); code != 1 {
		t.Fatalf("healthcheck during the start delay = %d, want 1\n%s", code, out)
	}
	if d := stop(t, c, 20); d > 10*time.Second {
		t.Fatalf("docker stop took %s", d)
	}
	if got := exitCode(t, c); got != 5 {
		t.Fatalf("exit code = %d, want 5", got)
	}
}

func TestSidecarStopExits0(t *testing.T) {
	t.Parallel()
	c := run(t, image, "sidecar", "--target=http://127.0.0.1:1", "--interval=200ms")
	// Not "sidecar starting": it is the first poll's result that proves the
	// sidecar is past startup.
	waitFor(t, c, 20*time.Second, "target unreachable", func() bool {
		return strings.Contains(logs(c), "target unreachable")
	})
	if d := stop(t, c, 20); d > 5*time.Second {
		t.Fatalf("docker stop took %s", d)
	}
	if got := exitCode(t, c); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}
	if l := logs(c); !strings.Contains(l, "sidecar stopping") {
		t.Fatalf("no 'sidecar stopping' log:\n%s", l)
	}
}

func TestSticky(t *testing.T) {
	t.Parallel()
	name := uniqueName(t)
	args := cat(fastHealth, []string{image})
	runNamed(t, name, args...)
	waitHealth(t, name, "healthy", 30*time.Second)
	id1 := instanceID(t, name)

	mustExec(t, name, "bop", "unhealthy", "sticky=true")
	waitHealth(t, name, "unhealthy", 30*time.Second)

	// Restart keeps the writable layer, so the sticky mode survives.
	mustDocker(t, "restart", "-t", "5", name)
	waitHealth(t, name, "unhealthy", 30*time.Second)
	if id2 := instanceID(t, name); id2 == id1 {
		t.Fatal("instance_id should change across a restart")
	}
	if out := mustExec(t, name, "bop", "status"); !strings.Contains(out, "sticky:      true") {
		t.Fatalf("status:\n%s", out)
	}

	// A recreate starts from a fresh writable layer.
	mustDocker(t, "rm", "-f", name)
	runNamed(t, name, args...)
	waitHealth(t, name, "healthy", 30*time.Second)
}

func TestStickyCleared(t *testing.T) {
	t.Parallel()
	for _, clear := range [][]string{{"bop", "healthy", "sticky=true"}, {"bop", "clear"}} {
		t.Run(strings.Join(clear[1:], "-"), func(t *testing.T) {
			t.Parallel()
			c := run(t, cat(fastHealth, []string{image})...)
			waitHealth(t, c, "healthy", 30*time.Second)
			mustExec(t, c, "bop", "hang", "sticky=true")
			waitHealth(t, c, "unhealthy", 30*time.Second)

			mustExec(t, c, clear...)
			waitHealth(t, c, "healthy", 30*time.Second)
			mustDocker(t, "restart", "-t", "5", c)
			waitHealth(t, c, "healthy", 30*time.Second)
		})
	}
}

func TestNonStickySwitchKeepsSticky(t *testing.T) {
	t.Parallel()
	c := run(t, cat(fastHealth, []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	mustExec(t, c, "bop", "unhealthy", "sticky=true")
	waitHealth(t, c, "unhealthy", 30*time.Second)

	// A non-sticky heal does not touch the file...
	if out := mustExec(t, c, "bop", "healthy"); !strings.Contains(out, "sticky:      false") {
		t.Fatalf("status:\n%s", out)
	}
	waitHealth(t, c, "healthy", 30*time.Second)

	// ...so the sticky fault returns on restart.
	mustDocker(t, "restart", "-t", "5", c)
	waitHealth(t, c, "unhealthy", 30*time.Second)
	if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "sticky:      true") {
		t.Fatalf("status:\n%s", out)
	}
}

func TestStickyHealthyDoesNotChangeBootMode(t *testing.T) {
	t.Parallel()
	c := run(t, cat(fastHealth, env("BOP_MODE", "unhealthy"), []string{image})...)
	waitHealth(t, c, "unhealthy", 30*time.Second)
	// healthy+sticky removes nothing here, but a sticky healthy request still
	// switches the live mode; BOP_MODE only applies again after a recreate.
	mustExec(t, c, "bop", "healthy", "sticky=true")
	waitHealth(t, c, "healthy", 30*time.Second)
	mustDocker(t, "restart", "-t", "5", c)
	waitHealth(t, c, "unhealthy", 30*time.Second) // BOP_MODE applies again
}

func TestStickyOnReadOnlyRootFails(t *testing.T) {
	t.Parallel()
	c := run(t, cat(fastHealth, []string{"--read-only"}, []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	code, out := execCode(t, c, "bop", "unhealthy", "sticky=true")
	if code == 0 || !strings.Contains(out, "read-only") {
		t.Fatalf("exec = %d\n%s", code, out)
	}
	if health(t, c) != "healthy" {
		t.Fatal("a failed sticky write must not change the mode")
	}
	// Non-sticky still works on a read-only root.
	mustExec(t, c, "bop", "unhealthy")
	waitHealth(t, c, "unhealthy", 30*time.Second)
}

// ownMountWarning is logged at boot when BOP_STATE_DIR is not on the root
// filesystem, so a sticky mode lasts as long as that mount, not the container.
const ownMountWarning = "BOP_STATE_DIR is on its own mount"

func TestStickyOnTmpfsIsLostOnRestart(t *testing.T) {
	t.Parallel()
	// The usual read-only hardening puts a tmpfs on /tmp: sticky writes
	// succeed, but the tmpfs is emptied on every restart.
	c := run(t, cat(fastHealth, []string{"--read-only", "--tmpfs", "/tmp"}, []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	if !strings.Contains(logs(c), ownMountWarning) {
		t.Fatalf("no %q warning at boot:\n%s", ownMountWarning, logs(c))
	}
	if out := mustExec(t, c, "bop", "unhealthy", "sticky=true"); !strings.Contains(out, "sticky:      true") {
		t.Fatalf("status:\n%s", out)
	}
	waitHealth(t, c, "unhealthy", 30*time.Second)

	mustDocker(t, "restart", "-t", "5", c)
	waitHealth(t, c, "healthy", 30*time.Second)
	if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "sticky:      false") {
		t.Fatalf("status:\n%s", out)
	}
}

func TestNoOwnMountWarningByDefault(t *testing.T) {
	t.Parallel()
	c := run(t, cat(fastHealth, []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	if l := logs(c); strings.Contains(l, ownMountWarning) {
		t.Fatalf("unexpected warning on the writable layer:\n%s", l)
	}
}

func TestStickyAnyUID(t *testing.T) {
	t.Parallel()
	// Kubernetes runAsUser and OpenShift run the image as an arbitrary UID.
	c := run(t, cat(fastHealth, []string{"--user", "1000:1000"}, []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	mustExec(t, c, "bop", "unhealthy", "sticky=true")
	waitHealth(t, c, "unhealthy", 30*time.Second)
	mustDocker(t, "restart", "-t", "5", c)
	waitHealth(t, c, "unhealthy", 30*time.Second)
	if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "sticky:      true") {
		t.Fatalf("status:\n%s", out)
	}
}

func TestClearOnReadOnlyRoot(t *testing.T) {
	t.Parallel()
	// Linux returns EROFS, not ENOENT, when removing a missing name on a
	// read-only mount; there is nothing to clear, so these must still work.
	for _, clear := range [][]string{{"bop", "clear"}, {"bop", "healthy", "sticky=true"}} {
		t.Run(strings.Join(clear[1:], "-"), func(t *testing.T) {
			t.Parallel()
			c := run(t, cat(fastHealth, []string{"--read-only"}, []string{image})...)
			waitHealth(t, c, "healthy", 30*time.Second)
			mustExec(t, c, "bop", "unhealthy")
			waitHealth(t, c, "unhealthy", 30*time.Second)

			mustExec(t, c, clear...)
			waitHealth(t, c, "healthy", 30*time.Second)
			if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "sticky:      false") {
				t.Fatalf("status:\n%s", out)
			}
		})
	}
}

// hasFile reports whether path exists in container c (distroless has no
// shell, so ask docker cp).
func hasFile(c, path string) bool {
	_, err := dockerCtx(30*time.Second, "cp", c+":"+path, "-")
	return err == nil
}

func TestStateDir(t *testing.T) {
	t.Parallel()
	c := run(t, cat(fastHealth, env("BOP_STATE_DIR", "/tmp/bop/custom"), []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	mustExec(t, c, "bop", "unhealthy", "sticky=true")
	waitHealth(t, c, "unhealthy", 30*time.Second)
	if !hasFile(c, "/tmp/bop/custom/mode") || hasFile(c, "/tmp/bop/mode") {
		t.Fatal("the sticky file must be at $BOP_STATE_DIR/mode and only there")
	}

	mustDocker(t, "restart", "-t", "5", c)
	waitHealth(t, c, "unhealthy", 30*time.Second)
	if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "sticky:      true") {
		t.Fatalf("status:\n%s", out)
	}

	mustExec(t, c, "bop", "clear")
	waitHealth(t, c, "healthy", 30*time.Second)
	if hasFile(c, "/tmp/bop/custom/mode") {
		t.Fatal("bop clear left $BOP_STATE_DIR/mode behind")
	}
}

func TestStateDirUnwritable(t *testing.T) {
	t.Parallel()
	// uid 65532 cannot create a directory in /.
	c := run(t, cat(fastHealth, env("BOP_STATE_DIR", "/bop-state"), []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	code, out := execCode(t, c, "bop", "unhealthy", "sticky=true")
	if code == 0 || !strings.Contains(out, "/bop-state") || !strings.Contains(out, "BOP_STATE_DIR") {
		t.Fatalf("exec = %d\n%s", code, out)
	}
	if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "mode:        healthy") {
		t.Fatalf("a failed sticky write must not change the mode:\n%s", out)
	}
}

// TestExecBopInRunningContainer: `docker exec <c> bop` (looking for help) runs
// a second serve. It must fail on the busy control address before it logs,
// writes or chowns anything, even as root, so the next start is unaffected.
func TestExecBopInRunningContainer(t *testing.T) {
	t.Parallel()
	c := run(t, cat(fastHealth, env("BOP_MODE", "unhealthy sticky=true"), []string{image})...)
	waitHealth(t, c, "unhealthy", 30*time.Second)
	mustExec(t, c, "bop", "clear")
	waitHealth(t, c, "healthy", 30*time.Second)
	if hasStickyFile(t, c) {
		t.Fatal("bop clear left the sticky file")
	}

	out, err := docker(t, "exec", "-u", "0", c, "bop")
	if err == nil {
		t.Fatalf("docker exec bop succeeded:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1:9901 is already in use") || strings.Contains(out, "msg=starting") {
		t.Fatalf("docker exec bop output:\n%s", out)
	}
	if hasStickyFile(t, c) {
		t.Fatal("docker exec bop wrote a sticky file")
	}

	// The next start comes up normally (BOP_MODE recreates its sticky file).
	mustDocker(t, "restart", "-t", "5", c)
	waitHealth(t, c, "unhealthy", 30*time.Second)
	if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "sticky:      true") {
		t.Fatalf("status after restart:\n%s", out)
	}
}

func instanceID(t *testing.T, c string) string {
	t.Helper()
	for _, line := range strings.Split(mustExec(t, c, "bop", "status"), "\n") {
		if v, ok := strings.CutPrefix(line, "instance_id: "); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatal("no instance_id in status")
	return ""
}

func TestCustomAddresses(t *testing.T) {
	t.Parallel()
	c := run(t, cat(fastHealth, env("BOP_ADDR", ":9000"), env("BOP_CONTROL_ADDR", "127.0.0.1:9902"), []string{image})...)
	// The image's healthcheck follows BOP_ADDR, and exec inherits BOP_CONTROL_ADDR.
	waitHealth(t, c, "healthy", 30*time.Second)
	mustExec(t, c, "bop", "unhealthy")
	waitHealth(t, c, "unhealthy", 30*time.Second)
	if code, _ := execCode(t, c, "bop", "healthcheck", "--url=http://127.0.0.1:8080/healthz"); code != 1 {
		t.Fatalf("nothing should listen on the default port, healthcheck = %d", code)
	}
	if out := logs(c); !strings.Contains(out, "addr=[::]:9000") || !strings.Contains(out, "addr=127.0.0.1:9902") {
		t.Fatalf("listeners not on the configured addresses:\n%s", out)
	}
}

// A bare BOP_ADDR port is shorthand for ":port"; the listener and the image's
// healthcheck must agree on it. A bare BOP_CONTROL_ADDR port is refused, so
// the control API is never exposed by accident.
func TestBarePortAddresses(t *testing.T) {
	t.Parallel()
	t.Run("data", func(t *testing.T) {
		t.Parallel()
		c := run(t, cat(fastHealth, env("BOP_ADDR", "9000"), []string{image})...)
		waitHealth(t, c, "healthy", 30*time.Second)
		mustExec(t, c, "bop", "unhealthy")
		waitHealth(t, c, "unhealthy", 30*time.Second)
		if out := logs(c); !strings.Contains(out, "addr=[::]:9000") || !strings.Contains(out, "addr=127.0.0.1:9901") {
			t.Fatalf("listeners not on the expected addresses:\n%s", out)
		}
	})
	t.Run("control", func(t *testing.T) {
		t.Parallel()
		c := run(t, cat(env("BOP_CONTROL_ADDR", "9902"), []string{image})...)
		waitExited(t, c, 30*time.Second)
		if got := exitCode(t, c); got != 2 {
			t.Fatalf("exit code = %d, want 2", got)
		}
		if out := logs(c); !strings.Contains(out, "use 127.0.0.1:9902") {
			t.Fatalf("expected a hint to keep it local, got:\n%s", out)
		}
	})
}

func TestSidecarSharesParentNetworkNamespace(t *testing.T) {
	t.Parallel()
	parent := run(t, cat(fastHealth, []string{image})...)
	waitHealth(t, parent, "healthy", 30*time.Second)

	sidecar := run(t, "--network", "container:"+parent, sidecarImage, "sidecar", "--target=http://127.0.0.1:8080", "--interval=500ms")
	waitHealth(t, sidecar, "healthy", 30*time.Second)
	waitFor(t, sidecar, 20*time.Second, "sidecar to see the parent", func() bool {
		return strings.Contains(logs(sidecar), "target reachable")
	})

	// Restarting the parent gives it a new network namespace; the sidecar
	// keeps the old, empty one.
	mustDocker(t, "restart", "-t", "5", parent)
	waitHealth(t, sidecar, "unhealthy", 40*time.Second)
	if inspect(t, sidecar, "{{.State.Running}}") != "true" {
		t.Fatal("sidecar must keep running")
	}
	waitFor(t, sidecar, 20*time.Second, "unreachable log", func() bool {
		return strings.Contains(logs(sidecar), "target unreachable")
	})
}

func TestSidecarLogsInstanceIDChange(t *testing.T) {
	t.Parallel()
	netName := uniqueName(t)
	mustDocker(t, "network", "create", netName)
	t.Cleanup(func() { _, _ = dockerCtx(time.Minute, "network", "rm", netName) })

	parent := run(t, "--network", netName, "--network-alias", "parent", image)
	waitHealth(t, parent, "healthy", 30*time.Second)
	sidecar := run(t, "--network", netName, image, "sidecar", "--target=http://parent:8080", "--interval=300ms")
	waitFor(t, sidecar, 20*time.Second, "target reachable", func() bool {
		return strings.Contains(logs(sidecar), "target reachable")
	})

	// Same name, same address, new process: a new instance_id.
	mustDocker(t, "restart", "-t", "5", parent)
	waitFor(t, sidecar, 30*time.Second, "instance_id change", func() bool {
		l := logs(sidecar)
		return strings.Contains(l, "instance_id changed") || strings.Contains(l, "target reachable again")
	})
}

func TestExamplesComposeConfig(t *testing.T) {
	t.Parallel()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, "examples", "*", "compose.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	for _, f := range files {
		name := filepath.Base(filepath.Dir(f))
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := dockerCtx(time.Minute, "compose", "-f", f, "config", "-q")
			if err != nil {
				t.Fatalf("docker compose config: %v\n%s", err, out)
			}
		})
	}
}
