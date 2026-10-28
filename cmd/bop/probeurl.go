package main

import (
	"fmt"
	"net/url"
)

// parseProbeURL accepts only absolute http or https URLs with a host.
func parseProbeURL(flagName, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid --%s %q: expected an http:// or https:// URL like http://127.0.0.1:8080", flagName, raw)
	}
	return u, nil
}
