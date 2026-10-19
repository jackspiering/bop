package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// APIError is an error response from the control listener.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

// Client talks to the control listener of a running bop.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a client for the control address (BOP_CONTROL_ADDR).
// An empty or wildcard host is dialled as 127.0.0.1.
func NewClient(addr string) *Client {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return &Client{base: "http://" + addr, http: &http.Client{Timeout: 5 * time.Second}}
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return &Client{
		base: "http://" + net.JoinHostPort(host, port),
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

// Set sends a mode command ("crash exit=3 after=10s", "clear", ...).
func (c *Client) Set(ctx context.Context, spec string) (State, error) {
	body, _ := json.Marshal(ModeRequest{Mode: spec})
	return c.do(ctx, http.MethodPost, "/mode", body)
}

// Status fetches the current state.
func (c *Client) Status(ctx context.Context) (State, error) {
	return c.do(ctx, http.MethodGet, "/status", nil)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (State, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return State{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return State{}, fmt.Errorf("cannot reach the bop control listener at %s (is bop serving in this container?): %w", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var eb ErrorBody
		if json.NewDecoder(resp.Body).Decode(&eb) != nil || eb.Error == "" {
			eb.Error = resp.Status
		}
		return State{}, &APIError{Status: resp.StatusCode, Message: eb.Error}
	}
	var st State
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return State{}, fmt.Errorf("decode control response: %w", err)
	}
	return st, nil
}
