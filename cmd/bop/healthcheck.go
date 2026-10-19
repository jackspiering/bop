package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const healthcheckTimeout = 1500 * time.Millisecond

// defaultHealthURL points at the local data listener derived from BOP_ADDR.
func defaultHealthURL(addr string) string {
	if !strings.Contains(addr, ":") {
		addr = ":" + addr // a bare port
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1:8080/healthz"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}

func healthcheck(args []string, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "URL to probe (default: http://127.0.0.1:<port of BOP_ADDR>/healthz)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return usageError(stderr, "healthcheck takes no arguments, got %q", fs.Args())
	}
	if *url == "" {
		*url = defaultHealthURL(envOr(getenv, "BOP_ADDR", defaultAddr))
	}

	client := http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(*url)
	if err != nil {
		fmt.Fprintf(stderr, "bop healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintf(stderr, "bop healthcheck: %s returned %s\n", *url, resp.Status)
		return 1
	}
	return 0
}
