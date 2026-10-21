// Package server implements the data listener (/healthz, /info) and the
// control listener used by the bop CLI.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/jackspiering/bop/internal/mode"
)

// ExecDelay is how long a mode switched over the control listener waits
// before acting, so the exec'd CLI receives its response before PID 1 dies.
const ExecDelay = 300 * time.Millisecond

// Info is the body of GET /info.
type Info struct {
	InstanceID string `json:"instance_id"`
	PID        int    `json:"pid"`
	Hostname   string `json:"hostname"`
	StartedAt  string `json:"started_at"`
	Mode       string `json:"mode"`
	Sticky     bool   `json:"sticky"`
	Version    string `json:"version"`
}

// State is the body of every control response.
type State struct {
	Info
	Healthy bool   `json:"healthy"`
	Reason  string `json:"reason,omitempty"`
}

// ModeRequest is the body of POST /mode.
type ModeRequest struct {
	Mode string `json:"mode"`
}

// ErrorBody is returned with every non-2xx control response.
type ErrorBody struct {
	Error string `json:"error"`
}

// Server holds the state shared by both listeners.
type Server struct {
	Machine *mode.Machine
	Store   mode.Store
	Log     *slog.Logger
	Version string

	instanceID string
	hostname   string
	startedAt  time.Time
}

// New creates a server with a fresh instance ID.
func New(m *mode.Machine, store mode.Store, log *slog.Logger, version string) *Server {
	var id [8]byte
	_, _ = rand.Read(id[:])
	host, _ := os.Hostname()
	return &Server{
		Machine:    m,
		Store:      store,
		Log:        log,
		Version:    version,
		instanceID: hex.EncodeToString(id[:]),
		hostname:   host,
		startedAt:  time.Now().UTC(),
	}
}

// InstanceID returns the random per-process ID.
func (s *Server) InstanceID() string { return s.instanceID }

func (s *Server) state() State {
	snap := s.Machine.Snapshot()
	return State{
		Info: Info{
			InstanceID: s.instanceID,
			PID:        os.Getpid(),
			Hostname:   s.hostname,
			StartedAt:  s.startedAt.Format(time.RFC3339),
			Mode:       snap.Mode.String(),
			Sticky:     snap.Mode.Sticky,
			Version:    s.Version,
		},
		Healthy: snap.Healthy,
		Reason:  snap.Reason,
	}
}

// DataHandler serves /healthz and /info. In hang mode both block until the
// mode changes. A client that closes or half-closes its side meanwhile gets
// the connection closed with no response.
func (s *Server) DataHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.guard(func(w http.ResponseWriter, _ *http.Request) {
		st := s.state()
		if !st.Healthy {
			http.Error(w, st.Reason, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	}))
	mux.HandleFunc("GET /info", s.guard(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.state().Info)
	}))
	return mux
}

// guard blocks the request while the machine is in hang mode. If the client
// goes away first it aborts the request, so nothing is ever written.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for s.Machine.Snapshot().Mode.Name == mode.Hang {
			changed := s.Machine.Changed()
			// Re-check after taking the channel so a switch in between is not missed.
			if s.Machine.Snapshot().Mode.Name != mode.Hang {
				break
			}
			select {
			case <-changed:
			case <-r.Context().Done():
				// The client closed or half-closed its side. Returning normally
				// would make net/http send an implicit 200 OK; abort instead.
				panic(http.ErrAbortHandler)
			}
		}
		next(w, r)
	}
}

// ControlHandler serves the CLI API: GET /status and POST /mode
// ({"mode":"<command>"}, including "clear").
// addr is BOP_CONTROL_ADDR. Anything that shares the network namespace can
// call it, but requests from browsers are rejected (see cliOnly).
func (s *Server) ControlHandler(addr string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.state())
	})
	mux.HandleFunc("POST /mode", func(w http.ResponseWriter, r *http.Request) {
		var req ModeRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errors.New("invalid request body: "+err.Error()))
			return
		}
		m, err := mode.ParseCommand(req.Mode)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.apply(m); err != nil {
			status := http.StatusInternalServerError
			var ue *mode.UsageError
			if errors.As(err, &ue) {
				status = http.StatusBadRequest
			}
			writeError(w, status, err)
			return
		}
		writeJSON(w, http.StatusOK, s.state())
	})
	return cliOnly(addr, mux)
}

// cliOnly rejects requests the bop CLI never sends, so a web page cannot use
// the browser to reach the control listener. Browsers attach Origin to every
// non-GET request and Sec-Fetch-* to most; a DNS-rebinding page is caught by
// its Host. Every route, present and future, goes through it.
func cliOnly(addr string, next http.Handler) http.Handler {
	configured, _, err := net.SplitHostPort(addr)
	if err != nil {
		configured = addr
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || hasSecFetch(r.Header) {
			writeError(w, http.StatusForbidden, errors.New("the control listener only accepts requests from the bop CLI"))
			return
		}
		if !allowedHost(r.Host, configured) {
			writeError(w, http.StatusForbidden, errors.New("the control listener only accepts requests from the bop CLI (unexpected Host header)"))
			return
		}
		if r.Method == http.MethodPost {
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, errors.New("content type must be application/json"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func hasSecFetch(h http.Header) bool {
	for k := range h {
		if strings.HasPrefix(k, "Sec-Fetch-") {
			return true
		}
	}
	return false
}

// allowedHost reports whether a request's Host names the control listener:
// an IP literal, localhost, or the host part of BOP_CONTROL_ADDR (which the
// CLI dials as given).
func allowedHost(hostport, configured string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return strings.EqualFold(host, "localhost") || (configured != "" && strings.EqualFold(host, configured))
}

// apply validates, persists (if sticky) and activates m.
func (s *Server) apply(m mode.Mode) error {
	if m.Name == mode.Clear {
		if err := s.Store.Clear(); err != nil {
			return err
		}
		return s.Machine.Set(mode.Mode{Name: mode.Healthy}, ExecDelay)
	}
	if err := m.ValidateRuntime(); err != nil {
		return err
	}
	if m.Name == mode.OOM {
		// Fail before persisting anything.
		if err := s.Machine.CheckOOM(); err != nil {
			return &mode.UsageError{Msg: err.Error()}
		}
	}
	switch {
	case m.Sticky && m.Name == mode.Healthy:
		// healthy is the default; sticky healthy means "forget the sticky mode".
		if err := s.Store.Clear(); err != nil {
			return err
		}
		m.Sticky = false
	case m.Sticky:
		if err := s.Store.Save(m); err != nil {
			return err
		}
	}
	if err := s.Machine.Set(m, ExecDelay); err != nil {
		return &mode.UsageError{Msg: err.Error()}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, ErrorBody{Error: err.Error()})
}
