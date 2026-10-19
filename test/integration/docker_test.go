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
	mustExec(t, c, "bop", "crash-loop", "interval=1s", "sticky=true")
	waitFor(t, c, 60*time.Second, "RestartCount >= 2", func() bool {
		n, _ := strconv.Atoi(inspect(t, c, "{{.RestartCount}}"))
		return n >= 2
	})
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

func oomFlags() []string { return []string{"--memory=64m", "--memory-swap=64m"} }

func TestOOMAtBoot(t *testing.T) {
	t.Parallel()
	c := run(t, cat(oomFlags(), env("BOP_MODE", "oom rate=32"), []string{image})...)
	waitExited(t, c, 60*time.Second)
	if inspect(t, c, "{{.State.OOMKilled}}") != "true" {
		t.Fatalf("OOMKilled = false\n%s", logs(c))
	}
	if got := exitCode(t, c); got != 137 {
		t.Fatalf("exit code = %d, want 137", got)
	}
}

func TestOOMViaExec(t *testing.T) {
	t.Parallel()
	c := run(t, cat(oomFlags(), []string{image})...)
	waitHealth(t, c, "healthy", 30*time.Second)
	mustExec(t, c, "bop", "oom", "rate=32")
	waitExited(t, c, 60*time.Second)
	if inspect(t, c, "{{.State.OOMKilled}}") != "true" || exitCode(t, c) != 137 {
		t.Fatalf("OOMKilled=%s exit=%d", inspect(t, c, "{{.State.OOMKilled}}"), exitCode(t, c))
	}
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
	if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "mode:        hang") {
		t.Fatalf("status:\n%s", out)
	}

	mustExec(t, c, "bop", "healthy")
	waitHealth(t, c, "healthy", 30*time.Second)
}

func TestStartDelay(t *testing.T) {
	t.Parallel()
	c := run(t, cat(env("BOP_START_DELAY", "8s"), []string{image})...)
	waitHealth(t, c, "starting", 5*time.Second)
	// The control listener is up while the data listener is not.
	if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "mode:        healthy") {
		t.Fatalf("status during delay:\n%s", out)
	}
	if health(t, c) != "starting" {
		t.Fatalf("health during the delay = %s, want starting", health(t, c))
	}
	waitHealth(t, c, "healthy", 40*time.Second)
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
