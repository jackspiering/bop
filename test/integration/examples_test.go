//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// composeProject is one example brought up under a unique project name.
type composeProject struct {
	t     *testing.T
	args  []string // -p <name> -f example [-f override]
	env   []string
	files []string
}

// upExample starts examples/<name> with host port publishing removed (so
// examples can run in parallel and never collide with something on the
// machine) and the locally built image.
func upExample(t *testing.T, name string, portServices ...string) *composeProject {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	project := uniqueName(t)
	args := []string{"compose", "-p", project, "-f", filepath.Join(root, "examples", name, "compose.yaml")}
	if len(portServices) > 0 {
		var b strings.Builder
		b.WriteString("services:\n")
		for _, s := range portServices {
			b.WriteString("  " + s + ":\n    ports: !reset []\n")
		}
		override := filepath.Join(t.TempDir(), "override.yaml")
		if err := os.WriteFile(override, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-f", override)
	}
	p := &composeProject{t: t, args: args, env: append(os.Environ(), "BOP_IMAGE="+image)}
	p.requirePortsReset(portServices)
	t.Cleanup(func() { _, _ = p.run(2*time.Minute, "down", "-v", "-t", "1") })
	if out, err := p.run(2*time.Minute, "up", "-d"); err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}
	return p
}

// requirePortsReset fails fast when Compose ignored the !reset override (it
// needs Compose 2.18+), instead of letting parallel examples collide on 8080.
func (p *composeProject) requirePortsReset(services []string) {
	p.t.Helper()
	if len(services) == 0 {
		return
	}
	out, err := p.run(time.Minute, "config", "--format", "json")
	if err != nil {
		p.t.Fatalf("compose config: %v\n%s", err, out)
	}
	var cfg struct {
		Services map[string]struct {
			Ports []json.RawMessage `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		p.t.Fatalf("decode compose config: %v\n%s", err, out)
	}
	for _, s := range services {
		if n := len(cfg.Services[s].Ports); n > 0 {
			p.t.Fatalf("service %s still publishes %d port(s) after `ports: !reset []`; the example tests need Docker Compose 2.18 or later", s, n)
		}
	}
}

func (p *composeProject) run(timeout time.Duration, args ...string) (string, error) {
	cmd := exec.Command("docker", append(append([]string{}, p.args...), args...)...)
	cmd.Env = p.env
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		err = os.ErrDeadlineExceeded
	}
	return strings.TrimSpace(string(out)), err
}

// container returns the ID of a service's container.
func (p *composeProject) container(service string) string {
	p.t.Helper()
	out, err := p.run(time.Minute, "ps", "-a", "-q", service)
	if err != nil || out == "" {
		p.t.Fatalf("compose ps %s: %v %q", service, err, out)
	}
	return strings.Fields(out)[0]
}

// TestExamplesRun brings each example up and checks its behaviour from
// Docker's point of view. Long timers are not waited out: the docker-run tests
// cover that behaviour, here the aim is to prove the example is wired right.
// Every examples/<name>/compose.yaml needs an entry in exampleChecks.
func TestExamplesRun(t *testing.T) {
	t.Parallel()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, "examples", "*", "compose.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	dirs := map[string]bool{}
	for _, f := range files {
		name := filepath.Base(filepath.Dir(f))
		dirs[name] = true
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			check, ok := exampleChecks[name]
			if !ok {
				t.Fatalf("examples/%s has no TestExamplesRun check; add one to exampleChecks", name)
			}
			check(t)
		})
	}
	for name := range exampleChecks {
		if !dirs[name] {
			t.Errorf("exampleChecks[%q] has no examples/%s/compose.yaml", name, name)
		}
	}
}

// exampleChecks holds one check per example, keyed by its directory name.
var exampleChecks = map[string]func(t *testing.T){
	"healthy": func(t *testing.T) {
		c := upExample(t, "healthy", "bop").container("bop")
		waitHealth(t, c, "healthy", 40*time.Second)
	},
	"unhealthy": func(t *testing.T) {
		c := upExample(t, "unhealthy", "bop").container("bop")
		waitHealth(t, c, "healthy", 40*time.Second)
		if out := mustExec(t, c, "bop", "status"); !strings.Contains(out, "unhealthy after=30s") {
			t.Fatalf("status:\n%s", out)
		}
	},
	"crash": func(t *testing.T) {
		c := upExample(t, "crash").container("bop")
		waitExited(t, c, 60*time.Second)
		if got := exitCode(t, c); got != 3 {
			t.Fatalf("exit code = %d, want 3", got)
		}
	},
	"crash-loop": func(t *testing.T) {
		c := upExample(t, "crash-loop").container("bop")
		waitFor(t, c, 60*time.Second, "a restart", func() bool {
			return inspect(t, c, "{{.RestartCount}}") != "0"
		})
		assertDieExitCodes(t, c, 1)
	},
	"oom": func(t *testing.T) {
		c := upExample(t, "oom").container("bop")
		waitExited(t, c, 60*time.Second)
		if got := exitCode(t, c); got != 137 {
			t.Fatalf("exit code = %d, want 137\n%s", got, logs(c))
		}
		// The limit comes from the example, not from this test.
		if got := inspect(t, c, "{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}"); got != "134217728 134217728" {
			t.Fatalf("memory limits = %q", got)
		}
	},
	"hang": func(t *testing.T) {
		c := upExample(t, "hang", "bop").container("bop")
		// Unhealthy once the image's start period (10s) has passed.
		waitHealth(t, c, "unhealthy", 60*time.Second)
		if inspect(t, c, "{{.State.Running}}") != "true" {
			t.Fatal("hung container must keep running")
		}
		mustExec(t, c, "bop", "healthy")
		waitHealth(t, c, "healthy", 30*time.Second)
	},
	"start-delay": func(t *testing.T) {
		c := upExample(t, "start-delay", "bop").container("bop")
		waitHealth(t, c, "starting", 10*time.Second)
		mustExec(t, c, "bop", "status")
		// The example's delay is 20s, so the data port is still unbound here.
		if code, out := execCode(t, c, "bop", "healthcheck"); code != 1 || !strings.Contains(out, "connection refused") {
			t.Fatalf("data listener during the delay: healthcheck = %d\n%s", code, out)
		}
		waitHealth(t, c, "healthy", 60*time.Second)
	},
	"sigterm": func(t *testing.T) {
		p := upExample(t, "sigterm")
		c := p.container("bop")
		waitFor(t, c, 30*time.Second, "running", func() bool { return inspect(t, c, "{{.State.Running}}") == "true" })
		time.Sleep(time.Second) // let bop install its signal handler
		start := time.Now()
		if out, err := p.run(time.Minute, "stop"); err != nil {
			t.Fatalf("compose stop: %v\n%s", err, out)
		}
		if d := time.Since(start); d < 5*time.Second {
			t.Fatalf("stop returned after %s, before the 5s grace period", d)
		}
		if got := exitCode(t, c); got != 137 {
			t.Fatalf("exit code = %d, want 137", got)
		}
	},
	"custom-addr": func(t *testing.T) {
		c := upExample(t, "custom-addr", "bop").container("bop")
		waitHealth(t, c, "healthy", 40*time.Second)
		mustExec(t, c, "bop", "status")
	},
	"sidecar": func(t *testing.T) {
		p := upExample(t, "sidecar", "parent")
		parent, sidecar := p.container("parent"), p.container("sidecar")
		waitHealth(t, parent, "healthy", 40*time.Second)
		waitHealth(t, sidecar, "healthy", 60*time.Second)
		mustDocker(t, "restart", "-t", "5", parent)
		waitHealth(t, sidecar, "unhealthy", 90*time.Second)
	},
	"restart-vs-recreate": func(t *testing.T) {
		p := upExample(t, "restart-vs-recreate", "bop")
		c := p.container("bop")
		// The example's start_period replaces the image's 10s (0s would not).
		if got := inspect(t, c, "{{.Config.Healthcheck.StartPeriod}}"); got != "1ms" {
			t.Fatalf("StartPeriod = %s, want 1ms", got)
		}
		waitHealth(t, c, "healthy", 40*time.Second)
		mustExec(t, c, "bop", "unhealthy", "sticky=true")
		waitHealth(t, c, "unhealthy", 30*time.Second)

		mustDocker(t, "restart", "-t", "5", c)
		waitHealth(t, c, "unhealthy", 30*time.Second)

		if out, err := p.run(2*time.Minute, "up", "-d", "--force-recreate"); err != nil {
			t.Fatalf("recreate: %v\n%s", err, out)
		}
		waitHealth(t, p.container("bop"), "healthy", 40*time.Second)
	},
	"state-dir": func(t *testing.T) {
		p := upExample(t, "state-dir", "bop")
		c := p.container("bop")
		if got := inspect(t, c, "{{.Config.Healthcheck.StartPeriod}}"); got != "1ms" {
			t.Fatalf("StartPeriod = %s, want 1ms", got)
		}
		waitHealth(t, c, "healthy", 40*time.Second)
		mustExec(t, c, "bop", "unhealthy", "sticky=true")
		waitHealth(t, c, "unhealthy", 30*time.Second)

		// The volume outlives the container, so the mode survives a recreate.
		if out, err := p.run(2*time.Minute, "up", "-d", "--force-recreate"); err != nil {
			t.Fatalf("recreate: %v\n%s", err, out)
		}
		c2 := p.container("bop")
		if c2 == c {
			t.Fatal("compose did not recreate the container")
		}
		waitHealth(t, c2, "unhealthy", 40*time.Second)
		if out := mustExec(t, c2, "bop", "status"); !strings.Contains(out, "sticky:      true") {
			t.Fatalf("status:\n%s", out)
		}
	},
}
