package mode

import (
	"strconv"
	"strings"
	"time"
)

// SigtermKind selects how the process reacts to SIGTERM/SIGINT.
type SigtermKind int

const (
	SigtermExit SigtermKind = iota
	SigtermIgnore
	SigtermDelay
)

// Sigterm is the parsed form of BOP_ON_SIGTERM.
type Sigterm struct {
	Kind  SigtermKind
	Code  int           // SigtermExit
	Delay time.Duration // SigtermDelay: exit 0 after this long
}

// ParseSigterm parses exit:<code>, ignore or delay:<dur>. Empty means exit:0.
func ParseSigterm(s string) (Sigterm, error) {
	if s == "" {
		return Sigterm{Kind: SigtermExit}, nil
	}
	kind, arg, hasArg := strings.Cut(s, ":")
	switch {
	case kind == "ignore" && !hasArg:
		return Sigterm{Kind: SigtermIgnore}, nil
	case kind == "exit" && hasArg:
		n, err := strconv.Atoi(arg)
		if err != nil || n < 0 || n > 255 {
			return Sigterm{}, usagef("invalid BOP_ON_SIGTERM=%q: exit code must be 0 to 255", s)
		}
		return Sigterm{Kind: SigtermExit, Code: n}, nil
	case kind == "delay" && hasArg:
		d, err := time.ParseDuration(arg)
		if err != nil || d < 0 {
			return Sigterm{}, usagef("invalid BOP_ON_SIGTERM=%q: delay must be a non-negative duration", s)
		}
		return Sigterm{Kind: SigtermDelay, Delay: d}, nil
	}
	return Sigterm{}, usagef("invalid BOP_ON_SIGTERM=%q; expected exit:<code>, ignore or delay:<dur>", s)
}
