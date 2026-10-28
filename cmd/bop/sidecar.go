package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// watcher turns poll results into log lines, one per state change.
type watcher struct {
	log         *slog.Logger
	target      string
	seen        bool // a poll has succeeded at least once
	unreachable bool
	instanceID  string
}

func (w *watcher) observe(instanceID string, err error) {
	switch {
	case err != nil:
		if !w.unreachable {
			w.unreachable = true
			w.log.Warn("target unreachable", "target", w.target, "err", err)
		}
	case !w.seen:
		w.log.Info("target reachable", "target", w.target, "instance_id", instanceID)
	case w.unreachable:
		w.log.Info("target reachable again", "target", w.target, "instance_id", instanceID)
	}
	if err != nil {
		return
	}
	if w.seen && instanceID != w.instanceID {
		w.log.Warn("target instance_id changed", "target", w.target, "old", w.instanceID, "new", instanceID)
	}
	w.seen, w.unreachable, w.instanceID = true, false, instanceID
}

// infoURL appends /info to a bare base URL and leaves explicit paths alone.
func infoURL(target string) (string, error) {
	u, err := parseProbeURL("target", target)
	if err != nil {
		return "", err
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/info"
	}
	return u.String(), nil
}

func poll(ctx context.Context, client *http.Client, infoURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %s", resp.Status)
	}
	var info struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&info); err != nil {
		return "", fmt.Errorf("decode /info: %w", err)
	}
	return info.InstanceID, nil
}

func sidecar(args []string, stderr io.Writer) int {
	// Same reason as serve: install the handler before flag parsing and the
	// first poll, so a stop during startup follows the process exit path
	// instead of the runtime's 143/130. A residual window remains during init.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	fs := flag.NewFlagSet("sidecar", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", "", "base URL of the bop to watch, e.g. http://127.0.0.1:8080 (required)")
	interval := fs.Duration("interval", 2*time.Second, "poll interval")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *target == "" || fs.NArg() > 0 || *interval <= 0 {
		return usageError(stderr, "sidecar needs --target=<url> and a positive --interval")
	}
	u, err := infoURL(*target)
	if err != nil {
		return usageError(stderr, "%v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	log.Info("sidecar starting", "version", version, "target", u, "interval", interval.String())

	client := newProbeClient(healthcheckTimeout)
	w := &watcher{log: log, target: u}
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		id, err := poll(ctx, client, u)
		if ctx.Err() != nil {
			log.Info("sidecar stopping")
			return 0
		}
		w.observe(id, err)
		select {
		case <-ctx.Done():
			log.Info("sidecar stopping")
			return 0
		case <-t.C:
		}
	}
}
