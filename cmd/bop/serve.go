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
	"syscall"
	"time"

	"github.com/jackspiering/bop/internal/mode"
	"github.com/jackspiering/bop/internal/oom"
	"github.com/jackspiering/bop/internal/server"
)

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
	if c.boot, err = mode.Parse(envOr(getenv, "BOP_MODE", mode.Healthy)); err != nil {
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
	c.addr = envOr(getenv, "BOP_ADDR", defaultAddr)
	c.controlAddr = envOr(getenv, "BOP_CONTROL_ADDR", defaultControlAddr)
	c.store = mode.Store{Dir: envOr(getenv, "BOP_STATE_DIR", defaultStateDir)}
	c.oomUnsafe = getenv("BOP_OOM_UNSAFE") == "1"
	return c, nil
}

func serve(stderr io.Writer, getenv func(string) string) int {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := loadConfig(getenv)
	if err != nil {
		return usageError(stderr, "%v", err)
	}

	// bop cannot tell a tmpfs (emptied on every restart) from a volume or an
	// emptyDir (which outlive the container), so it only warns.
	if own, err := cfg.store.OnOwnMount(); err == nil && own {
		log.Warn("BOP_STATE_DIR is on its own mount: a sticky mode lasts exactly as long as that mount "+
			"(a tmpfs is emptied on every restart, a volume survives a recreate)", "dir", cfg.store.Dir)
	}

	// A sticky file overrides BOP_MODE; BOP_MODE with sticky=true creates it.
	boot := cfg.boot
	switch stored, ok, err := cfg.store.Load(); {
	case err != nil:
		return bootError(stderr, err)
	case ok:
		log.Info("sticky mode overrides BOP_MODE", "sticky", stored.String(), "BOP_MODE", cfg.boot.String())
		boot = stored
	case boot.Sticky:
		if boot.Name == mode.Healthy {
			boot.Sticky = false
		} else if err := cfg.store.Save(boot); err != nil {
			return bootError(stderr, err)
		}
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
		CheckOOM: func() error {
			if cfg.oomUnsafe {
				return nil
			}
			return oom.Check(oom.MemoryMaxPath)
		},
		OnChange: func(s mode.Snapshot) {
			log.Info("state", "mode", s.Mode.String(), "healthy", s.Healthy)
		},
	})
	srv := server.New(machine, cfg.store, log, version)
	log.Info("starting", "version", version, "pid", os.Getpid(), "instance_id", srv.InstanceID(),
		"addr", cfg.addr, "control_addr", cfg.controlAddr, "start_delay", cfg.startDelay.String(),
		"on_sigterm", envOr(getenv, "BOP_ON_SIGTERM", "exit:0"))

	// The control listener binds immediately, before the start delay.
	controlLn, err := net.Listen("tcp", cfg.controlAddr)
	if err != nil {
		log.Error("control listener", "err", err)
		return 1
	}
	go serveHTTP(log, "control", controlLn, srv.ControlHandler(cfg.controlAddr))

	if err := machine.Set(boot, 0); err != nil {
		return usageError(stderr, "%v", err)
	}

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

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

// bootError reports a boot failure that is not a usage mistake, so the usage
// text would only bury the remedy. It exits 2 like a configuration error.
func bootError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "bop: %v\n", err)
	return 2
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
// handled according to the BOP_ON_SIGTERM policy. PID 1 gets no default
// signal handling from the kernel, so these must be handled explicitly.
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
