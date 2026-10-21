package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackspiering/bop/internal/mode"
	"github.com/jackspiering/bop/internal/oom"
	"github.com/jackspiering/bop/internal/server"
)

// checkOOMLimit is the oom guard without the BOP_OOM_UNSAFE bypass; tests
// replace it.
var checkOOMLimit = oom.CheckOwnCgroup

// config is the boot-time configuration, parsed from BOP_* variables.
type config struct {
	boot        mode.Mode
	startDelay  time.Duration
	sigterm     mode.Sigterm
	addr        string
	controlAddr string
	store       mode.Store
	oomUnsafe   bool
}

func loadConfig(getenv func(string) string) (config, error) {
	var c config
	var err error
	// A blank BOP_MODE means healthy, like an unset one.
	bootSpec := strings.TrimSpace(getenv("BOP_MODE"))
	if bootSpec == "" {
		bootSpec = mode.Healthy
	}
	if c.boot, err = mode.Parse(bootSpec); err != nil {
		return c, fmt.Errorf("BOP_MODE: %w", err)
	}
	if v := getenv("BOP_START_DELAY"); v != "" {
		if c.startDelay, err = time.ParseDuration(v); err != nil || c.startDelay < 0 {
			return c, &mode.UsageError{Msg: fmt.Sprintf("invalid BOP_START_DELAY=%q: expected a non-negative duration", v)}
		}
	}
	if c.sigterm, err = mode.ParseSigterm(getenv("BOP_ON_SIGTERM")); err != nil {
		return c, err
	}
	if c.addr, err = parseListenAddr("BOP_ADDR", envOr(getenv, "BOP_ADDR", defaultAddr)); err != nil {
		return c, err
	}
	if c.controlAddr, err = parseControlAddr(envOr(getenv, "BOP_CONTROL_ADDR", defaultControlAddr)); err != nil {
		return c, err
	}
	c.store = mode.Store{Dir: envOr(getenv, "BOP_STATE_DIR", defaultStateDir)}
	c.oomUnsafe = getenv("BOP_OOM_UNSAFE") == "1"
	return c, nil
}

// parseListenAddr checks a listen address without resolving it, so that a
// typo fails at boot rather than when the data listener binds after
// BOP_START_DELAY. It accepts [host]:port with a numeric port, and a bare port
// ("9000"), which it normalises to ":9000". The listeners and the
// healthcheck's default URL both use it, so they accept the same forms.
func parseListenAddr(name, v string) (string, error) {
	addr := v
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	_, port, err := net.SplitHostPort(addr)
	if err == nil {
		_, err = strconv.ParseUint(port, 10, 16)
	}
	if err != nil {
		return "", &mode.UsageError{Msg: fmt.Sprintf("invalid %s=%q: expected [host]:port or a bare port", name, v)}
	}
	return addr, nil
}

// parseControlAddr is parseListenAddr without the bare-port shorthand: the
// control API switches modes, so binding it to all interfaces must be spelled
// out (":9902"), not implied by "9902". serve and the exec CLI both use it.
func parseControlAddr(v string) (string, error) {
	if _, err := strconv.ParseUint(v, 10, 16); err == nil {
		return "", &mode.UsageError{Msg: fmt.Sprintf("invalid BOP_CONTROL_ADDR=%q: expected [host]:port; "+
			"use 127.0.0.1:%s to keep the control listener local (or :%s for all interfaces)", v, v, v)}
	}
	return parseListenAddr("BOP_CONTROL_ADDR", v)
}

func serve(stderr io.Writer, getenv func(string) string) int {
	// Install the handler before any boot work: until signal.Notify runs, the Go
	// runtime's default handler turns SIGTERM/SIGINT into exit 143/130 and
	// BOP_ON_SIGTERM is ignored. The buffer holds signals that arrive during boot.
	// A residual window remains during runtime init, before main runs.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := loadConfig(getenv)
	if err != nil {
		return usageError(stderr, "%v", err)
	}

	// The control listener binds before anything else has side effects, so a
	// second `bop` started in a running container (docker exec <c> bop) fails
	// here without touching the sticky file. It binds before the start delay.
	controlLn, err := net.Listen("tcp", cfg.controlAddr)
	if errors.Is(err, syscall.EADDRINUSE) {
		fmt.Fprintf(stderr, "bop: %s is already in use; bop may already be running in this container. Use 'bop status' or 'bop help'\n", cfg.controlAddr)
		return 1
	}
	if err != nil {
		log.Error("control listener", "err", err)
		return 1
	}
	served := false
	defer func() {
		if !served { // a boot check failed; serveHTTP owns it otherwise
			controlLn.Close()
		}
	}()

	checkOOM := func() error {
		if cfg.oomUnsafe {
			return nil
		}
		return checkOOMLimit()
	}

	// bop cannot tell a tmpfs (emptied on every restart) from a volume or an
	// emptyDir (which outlive the container), so it only warns.
	if own, err := cfg.store.OnOwnMount(); err == nil && own {
		log.Warn("BOP_STATE_DIR is on its own mount: a sticky mode lasts exactly as long as that mount "+
			"(a tmpfs is emptied on every restart, a volume survives a recreate)", "dir", cfg.store.Dir)
	}

	boot, err := bootMode(cfg.store, cfg.boot, checkOOM, log)
	if err != nil {
		return reportBootError(stderr, err)
	}

	machine := mode.NewMachine(mode.Hooks{
		Exit: func(code int) {
			log.Info("exiting", "code", code)
			os.Exit(code)
		},
		OOM: func(rate int, stop <-chan struct{}) {
			log.Info("allocating memory until the cgroup kills this process", "mib_per_s", rate)
			oom.Run(rate, stop)
		},
		CheckOOM: checkOOM,
		OnChange: func(s mode.Snapshot) {
			log.Info("state", "mode", s.Mode.String(), "healthy", s.Healthy)
		},
	})
	srv := server.New(machine, cfg.store, log, version)
	log.Info("starting", "version", version, "pid", os.Getpid(), "instance_id", srv.InstanceID(),
		"addr", cfg.addr, "control_addr", cfg.controlAddr, "start_delay", cfg.startDelay.String(),
		"on_sigterm", envOr(getenv, "BOP_ON_SIGTERM", "exit:0"))
	served = true
	go serveHTTP(log, "control", controlLn, srv.ControlHandler(cfg.controlAddr))

	if err := machine.Set(boot, 0); err != nil {
		return reportBootError(stderr, oomRefusal(err))
	}

	go func() {
		time.Sleep(cfg.startDelay)
		ln, err := net.Listen("tcp", cfg.addr)
		if err != nil {
			log.Error("data listener", "err", err)
			os.Exit(1)
		}
		serveHTTP(log, "data", ln, srv.DataHandler())
	}()

	return handleSignals(log, cfg.sigterm, sigs)
}

// reportBootError reports a usage mistake with the usage text. Any other boot
// failure (a bad or unwritable sticky file, an oom guard that cannot read the
// cgroup) is reported without it, because the usage text would only bury the
// remedy. Both exit 2 like a configuration error.
func reportBootError(stderr io.Writer, err error) int {
	if ue, ok := err.(*mode.UsageError); ok {
		return usageError(stderr, "%v", ue)
	}
	fmt.Fprintf(stderr, "bop: %v\n", err)
	return 2
}

// oomRefusal classifies an oom guard refusal at boot. A missing or too large
// limit is a configuration mistake, so it becomes a *mode.UsageError; an
// environment the guard cannot check (no cgroup v2, an unreadable memory.max)
// is not.
func oomRefusal(err error) error {
	if errors.Is(err, oom.ErrNoLimit) {
		return &mode.UsageError{Msg: err.Error()}
	}
	return err
}

// bootMode applies the boot precedence: a sticky file overrides BOP_MODE, and
// a sticky BOP_MODE creates the file. `healthy sticky=true` persists nothing,
// and a sticky oom is refused (see oomRefusal) before it is persisted.
func bootMode(store mode.Store, env mode.Mode, checkOOM func() error, log *slog.Logger) (mode.Mode, error) {
	stored, ok, err := store.Load()
	switch {
	case err != nil:
		return mode.Mode{}, err
	case ok:
		log.Info("sticky mode overrides BOP_MODE", "sticky", stored.String(), "BOP_MODE", env.String())
		return stored, nil
	case env.Sticky && env.Name == mode.Healthy:
		env.Sticky = false
	case env.Sticky:
		// Refuse oom before persisting it, as Server.apply does at runtime.
		if env.Name == mode.OOM {
			if err := checkOOM(); err != nil {
				return mode.Mode{}, oomRefusal(err)
			}
		}
		if err := store.Save(env); err != nil {
			return mode.Mode{}, err
		}
	}
	return env, nil
}

func serveHTTP(log *slog.Logger, name string, ln net.Listener, h http.Handler) {
	log.Info("listening", "listener", name, "addr", ln.Addr().String())
	s := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listener stopped", "listener", name, "err", err)
		os.Exit(1)
	}
}

// handleSignals blocks and returns the exit code once SIGTERM/SIGINT is
// handled according to the BOP_ON_SIGTERM policy. The Go runtime, not the
// kernel, handles an uncaught SIGTERM/SIGINT by exiting 143/130, including
// when the process is PID 1. The handler is installed at the start of serve;
// a residual window remains during runtime init, before main runs.
func handleSignals(log *slog.Logger, st mode.Sigterm, sigs <-chan os.Signal) int {
	var delayed <-chan time.Time
	for {
		select {
		case sig := <-sigs:
			switch st.Kind {
			case mode.SigtermIgnore:
				log.Info("ignoring signal", "signal", sig.String())
			case mode.SigtermDelay:
				if delayed == nil {
					log.Info("signal received, exiting after delay", "signal", sig.String(), "delay", st.Delay.String())
					delayed = time.After(st.Delay)
				} else {
					log.Info("ignoring signal during delay", "signal", sig.String())
				}
			default:
				log.Info("signal received, exiting", "signal", sig.String(), "code", st.Code)
				return st.Code
			}
		case <-delayed:
			log.Info("delay elapsed, exiting", "code", 0)
			return 0
		}
	}
}
