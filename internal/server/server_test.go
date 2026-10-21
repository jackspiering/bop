package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackspiering/bop/internal/mode"
)

type harness struct {
	srv     *Server
	data    *httptest.Server
	control *httptest.Server
	client  *Client
	store   mode.Store
	exited  chan int
	dataLog *syncBuffer
}

// syncBuffer is a bytes.Buffer that is safe to write from server goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newHarness(t *testing.T, hooks mode.Hooks) *harness {
	t.Helper()
	h := &harness{exited: make(chan int, 1), dataLog: &syncBuffer{}}
	hooks.Exit = func(c int) { h.exited <- c }
	h.store = mode.Store{Dir: filepath.Join(t.TempDir(), "bop")}
	h.srv = New(mode.NewMachine(hooks), h.store, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	h.data = httptest.NewUnstartedServer(h.srv.DataHandler())
	h.data.Config.ErrorLog = log.New(h.dataLog, "", 0)
	h.data.Start()
	h.control = httptest.NewServer(h.srv.ControlHandler("127.0.0.1:9901"))
	t.Cleanup(h.data.Close)
	t.Cleanup(h.control.Close)
	h.client = NewClient(strings.TrimPrefix(h.control.URL, "http://"))
	return h
}

func (h *harness) get(t *testing.T, path string) (int, string) {
	t.Helper()
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(h.data.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHealthzAndInfo(t *testing.T) {
	h := newHarness(t, mode.Hooks{})
	if code, body := h.get(t, "/healthz"); code != 200 || strings.TrimSpace(body) != "ok" {
		t.Fatalf("healthz = %d %q", code, body)
	}
	code, body := h.get(t, "/info")
	if code != 200 {
		t.Fatalf("info = %d", code)
	}
	var info Info
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatal(err)
	}
	if info.InstanceID == "" || info.PID != os.Getpid() || info.Mode != "healthy" || info.Sticky || info.Version != "test" || info.StartedAt == "" {
		t.Fatalf("info = %+v", info)
	}
}

func TestInstanceIDDiffersPerServer(t *testing.T) {
	a := newHarness(t, mode.Hooks{})
	b := newHarness(t, mode.Hooks{})
	if a.srv.InstanceID() == b.srv.InstanceID() {
		t.Fatal("instance IDs collided")
	}
}

func TestUnhealthyAndRecover(t *testing.T) {
	h := newHarness(t, mode.Hooks{})
	if _, err := h.client.Set(context.Background(), "unhealthy"); err != nil {
		t.Fatal(err)
	}
	if code, body := h.get(t, "/healthz"); code != 503 || strings.TrimSpace(body) == "" {
		t.Fatalf("healthz = %d %q, want 503 with a reason", code, body)
	}
	if code, _ := h.get(t, "/info"); code != 200 {
		t.Fatalf("info should still answer, got %d", code)
	}
	if _, err := h.client.Set(context.Background(), "healthy"); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.get(t, "/healthz"); code != 200 {
		t.Fatalf("healthz after healthy = %d", code)
	}
}

func TestUnhealthyAfter(t *testing.T) {
	h := newHarness(t, mode.Hooks{})
	if _, err := h.client.Set(context.Background(), "unhealthy after=400ms"); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.get(t, "/healthz"); code != 200 {
		t.Fatalf("before after: %d", code)
	}
	time.Sleep(450 * time.Millisecond)
	if code, _ := h.get(t, "/healthz"); code != 503 {
		t.Fatalf("after after: %d", code)
	}
}

func TestHangBlocksDataButNotControl(t *testing.T) {
	h := newHarness(t, mode.Hooks{})
	if _, err := h.client.Set(context.Background(), "hang"); err != nil {
		t.Fatal(err)
	}
	c := http.Client{Timeout: 300 * time.Millisecond}
	for _, p := range []string{"/healthz", "/info"} {
		if _, err := c.Get(h.data.URL + p); err == nil {
			t.Fatalf("GET %s answered in hang mode", p)
		}
	}
	// The control listener keeps working and recovery releases a pending request.
	type result struct {
		code int
		err  error
	}
	pending := make(chan result, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(h.data.URL + "/healthz")
		if err != nil {
			pending <- result{err: err}
			return
		}
		resp.Body.Close()
		pending <- result{code: resp.StatusCode}
	}()
	time.Sleep(100 * time.Millisecond)
	st, err := h.client.Set(context.Background(), "healthy")
	if err != nil || st.Mode != "healthy" {
		t.Fatalf("recover: %+v %v", st, err)
	}
	select {
	case r := <-pending:
		if r.err != nil || r.code != 200 {
			t.Fatalf("pending request = %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending request was not released by recovery")
	}
	if l := h.dataLog.String(); l != "" {
		t.Fatalf("data server logged:\n%s", l)
	}
}

// A client that half-closes after its request cancels the request context at
// once. In hang mode it must still get no response, not an implicit 200.
func TestHangIgnoresHalfClosedClient(t *testing.T) {
	h := newHarness(t, mode.Hooks{})
	rawGet := func(path string) []byte {
		t.Helper()
		conn, err := net.Dial("tcp", h.data.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, "GET "+path+" HTTP/1.1\r\nHost: bop\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		b, _ := io.ReadAll(conn)
		return b
	}
	// Control: a half-closed client does get an answer when healthy.
	if b := rawGet("/healthz"); !bytes.HasPrefix(b, []byte("HTTP/1.1 200")) {
		t.Fatalf("healthy: %q", b)
	}
	if _, err := h.client.Set(context.Background(), "hang"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/healthz", "/info"} {
		if b := rawGet(p); bytes.HasPrefix(b, []byte("HTTP/")) {
			t.Fatalf("GET %s answered in hang mode: %q", p, b)
		}
	}
	if l := h.dataLog.String(); l != "" {
		t.Fatalf("data server logged:\n%s", l)
	}
}

func TestControlCrashRespondsBeforeExit(t *testing.T) {
	h := newHarness(t, mode.Hooks{})
	st, err := h.client.Set(context.Background(), "crash exit=3")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != "crash exit=3 after=0s" {
		t.Fatalf("mode = %q", st.Mode)
	}
	select {
	case c := <-h.exited:
		t.Fatalf("exited with %d before the response was read", c)
	default:
	}
	select {
	case c := <-h.exited:
		if c != 3 {
			t.Fatalf("exit code = %d", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("crash never fired")
	}
}

func TestControlRejections(t *testing.T) {
	h := newHarness(t, mode.Hooks{CheckOOM: func() error { return errors.New("no memory limit") }})
	tests := []struct {
		spec string
		want string
	}{
		{"explode", "unknown mode"},
		{"crash exit=999", "0 to 255"},
		{"crash-loop", "sticky=true"},
		{"oom", "no memory limit"},
		{"", "empty mode"},
	}
	for _, tt := range tests {
		_, err := h.client.Set(context.Background(), tt.spec)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || !strings.Contains(ae.Message, tt.want) {
			t.Errorf("Set(%q) = %v, want 400 containing %q", tt.spec, err, tt.want)
		}
	}
	if got, _ := h.client.Status(context.Background()); got.Mode != "healthy" {
		t.Fatalf("rejected commands changed the mode to %q", got.Mode)
	}
	if _, ok, _ := h.store.Load(); ok {
		t.Fatal("rejected command was persisted")
	}
}

func TestControlRejectsBrowserRequests(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		host    string
		headers map[string]string
		body    string
		sticky  bool
		want    int
	}{
		{
			name:   "cross-site text/plain POST",
			method: http.MethodPost, path: "/mode",
			headers: map[string]string{"Content-Type": "text/plain;charset=UTF-8", "Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
			body:    `{"mode":"unhealthy"}`,
			want:    http.StatusForbidden,
		},
		{
			name:   "DNS rebinding POST",
			method: http.MethodPost, path: "/mode", host: "evil.example:9901",
			headers: map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example:9901"},
			body:    `{"mode":"unhealthy"}`,
			want:    http.StatusForbidden,
		},
		{
			name:   "cross-site POST /clear",
			method: http.MethodPost, path: "/clear", sticky: true,
			headers: map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
			want:    http.StatusForbidden,
		},
		{
			name:   "text/plain POST without Origin",
			method: http.MethodPost, path: "/mode",
			headers: map[string]string{"Content-Type": "text/plain"},
			body:    `{"mode":"unhealthy"}`,
			want:    http.StatusUnsupportedMediaType,
		},
		{
			name:   "DNS rebinding GET /status",
			method: http.MethodGet, path: "/status", host: "evil.example:9901",
			want: http.StatusForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, mode.Hooks{})
			ctx := context.Background()
			wantMode := "healthy"
			if tt.sticky {
				st, err := h.client.Set(ctx, "unhealthy sticky=true")
				if err != nil {
					t.Fatal(err)
				}
				wantMode = st.Mode
			}
			req, err := http.NewRequestWithContext(ctx, tt.method, h.control.URL+tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if tt.host != "" {
				req.Host = tt.host
			}
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d %s, want %d", resp.StatusCode, strings.TrimSpace(string(b)), tt.want)
			}
			if got, err := h.client.Status(ctx); err != nil || got.Mode != wantMode {
				t.Errorf("mode = %q (%v), want %q", got.Mode, err, wantMode)
			}
			if _, ok, _ := h.store.Load(); ok != tt.sticky {
				t.Errorf("sticky file present = %v, want %v", ok, tt.sticky)
			}
		})
	}
	// The CLI still works.
	h := newHarness(t, mode.Hooks{})
	if _, err := h.client.Set(context.Background(), "unhealthy"); err != nil {
		t.Fatalf("CLI request rejected: %v", err)
	}
}

func TestControlAcceptsCLIHosts(t *testing.T) {
	h := newHarness(t, mode.Hooks{})
	control := httptest.NewServer(h.srv.ControlHandler("Bop.Internal:9902"))
	t.Cleanup(control.Close)
	for host, want := range map[string]int{
		"127.0.0.1:9902":      http.StatusOK,
		"[::1]:9902":          http.StatusOK,
		"localhost:9902":      http.StatusOK,
		"bop.internal:9902":   http.StatusOK,
		"bop.internal":        http.StatusOK,
		"other.example:9902":  http.StatusForbidden,
		"localhost.evil:9902": http.StatusForbidden,
	} {
		req, err := http.NewRequest(http.MethodGet, control.URL+"/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("Host %q: status = %d, want %d", host, resp.StatusCode, want)
		}
	}
}

func TestControlClearIsOnlyAMode(t *testing.T) {
	h := newHarness(t, mode.Hooks{})
	ctx := context.Background()
	if _, err := h.client.Set(ctx, "unhealthy sticky=true"); err != nil {
		t.Fatal(err)
	}
	// Shaped like a CLI request, so only the route decides the answer.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.control.URL+"/clear", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /clear = %d, want 404", resp.StatusCode)
	}
	if _, ok, _ := h.store.Load(); !ok {
		t.Fatal("POST /clear removed the sticky file")
	}
	st, err := h.client.Set(ctx, "clear")
	if err != nil || st.Mode != "healthy" {
		t.Fatalf("clear: %+v %v", st, err)
	}
	if _, ok, _ := h.store.Load(); ok {
		t.Fatal("sticky file still present after clear")
	}
}

func TestSticky(t *testing.T) {
	h := newHarness(t, mode.Hooks{})
	ctx := context.Background()

	st, err := h.client.Set(ctx, "unhealthy sticky=true")
	if err != nil || !st.Sticky || st.Mode != "unhealthy after=0s sticky=true" {
		t.Fatalf("sticky set: %+v %v", st, err)
	}
	if m, ok, err := h.store.Load(); err != nil || !ok || m.Name != mode.Unhealthy {
		t.Fatalf("stored = %+v %v %v", m, ok, err)
	}

	// healthy + sticky forgets the sticky mode.
	st, err = h.client.Set(ctx, "healthy sticky=true")
	if err != nil || st.Sticky || st.Mode != "healthy" {
		t.Fatalf("healthy sticky: %+v %v", st, err)
	}
	if _, ok, _ := h.store.Load(); ok {
		t.Fatal("sticky file still present after healthy sticky=true")
	}

	if _, err := h.client.Set(ctx, "hang sticky=true"); err != nil {
		t.Fatal(err)
	}
	st, err = h.client.Set(ctx, "clear")
	if err != nil || st.Mode != "healthy" {
		t.Fatalf("clear: %+v %v", st, err)
	}
	if _, ok, _ := h.store.Load(); ok {
		t.Fatal("sticky file still present after clear")
	}

	// crash-loop is allowed when sticky.
	if _, err := h.client.Set(ctx, "crash-loop interval=1h sticky=true"); err != nil {
		t.Fatal(err)
	}
}

func TestStickyWriteFailureIsReported(t *testing.T) {
	h := newHarness(t, mode.Hooks{})
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h.srv.Store = mode.Store{Dir: filepath.Join(blocker, "bop")}
	_, err := h.client.Set(context.Background(), "unhealthy sticky=true")
	var ae *APIError
	if !errors.As(err, &ae) || !strings.Contains(ae.Message, "read-only") {
		t.Fatalf("err = %v, want read-only explanation", err)
	}
	if got, _ := h.client.Status(context.Background()); got.Mode != "healthy" {
		t.Fatalf("failed sticky write still changed the mode to %q", got.Mode)
	}
}

func TestClientUnreachable(t *testing.T) {
	c := NewClient("127.0.0.1:1")
	if _, err := c.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "control listener") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewClientWildcardHost(t *testing.T) {
	for in, want := range map[string]string{
		":9901":          "http://127.0.0.1:9901",
		"0.0.0.0:9901":   "http://127.0.0.1:9901",
		"[::]:9901":      "http://127.0.0.1:9901",
		"127.0.0.1:9901": "http://127.0.0.1:9901",
	} {
		if got := NewClient(in).base; got != want {
			t.Errorf("NewClient(%q).base = %q, want %q", in, got, want)
		}
	}
}
