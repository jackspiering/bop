// Package mode implements the BoP mode grammar (`<mode> [key=value ...]`) and
// the state machine that drives steady-state misbehaviour.
package mode

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Mode names.
const (
	Healthy   = "healthy"
	Unhealthy = "unhealthy"
	Crash     = "crash"
	CrashLoop = "crash-loop"
	OOM       = "oom"
	Hang      = "hang"

	// Clear is accepted by ParseCommand only. It resets to healthy and removes
	// the sticky file.
	Clear = "clear"
)

// Names lists every mode, in documentation order.
var Names = []string{Healthy, Unhealthy, Crash, CrashLoop, OOM, Hang}

// Mode is a parsed mode with its arguments. Arguments that do not apply to the
// mode are left zero.
type Mode struct {
	Name     string
	After    time.Duration // unhealthy, crash
	Exit     int           // crash, crash-loop
	Interval time.Duration // crash-loop
	Rate     int           // oom, MiB/s
	Sticky   bool
}

// UsageError reports invalid user input. The CLI maps it to exit status 2.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func usagef(format string, args ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}

// allowed lists the keys each mode accepts (besides sticky).
var allowed = map[string][]string{
	Healthy:   {},
	Unhealthy: {"after"},
	Crash:     {"exit", "after"},
	CrashLoop: {"interval", "exit"},
	OOM:       {"rate"},
	Hang:      {},
	Clear:     {},
}

// Parse parses a boot-time mode string such as "crash exit=3 after=10s".
func Parse(s string) (Mode, error) {
	m, err := ParseCommand(s)
	if err != nil {
		return Mode{}, err
	}
	if m.Name == Clear {
		return Mode{}, usagef("%q is a runtime command, not a mode", Clear)
	}
	return m, nil
}

// ParseCommand parses a runtime command: any mode, or "clear".
func ParseCommand(s string) (Mode, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return Mode{}, usagef("empty mode; expected one of: %s", strings.Join(Names, ", "))
	}
	name := fields[0]
	keys, ok := allowed[name]
	if !ok {
		return Mode{}, usagef("unknown mode %q; expected one of: %s", name, strings.Join(Names, ", "))
	}
	if name == Clear && len(fields) > 1 {
		return Mode{}, usagef("%q takes no arguments, got %q", Clear, fields[1:])
	}

	m := Mode{Name: name}
	switch name {
	case Crash:
		m.Exit = 1
	case CrashLoop:
		m.Exit = 1
		m.Interval = 5 * time.Second
	case OOM:
		m.Rate = 64
	}

	seen := map[string]bool{}
	for _, f := range fields[1:] {
		k, v, found := strings.Cut(f, "=")
		if !found || k == "" {
			return Mode{}, usagef("invalid argument %q; expected key=value", f)
		}
		if seen[k] {
			return Mode{}, usagef("duplicate argument %q", k)
		}
		seen[k] = true

		if k == "sticky" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return Mode{}, usagef("invalid sticky=%q; expected true or false (1, 0, t, f and other strconv.ParseBool spellings are accepted too)", v)
			}
			m.Sticky = b
			continue
		}
		if !contains(keys, k) {
			return Mode{}, usagef("mode %q does not accept %q; accepts: %s", name, k, acceptList(keys))
		}
		if err := m.set(k, v); err != nil {
			return Mode{}, err
		}
	}
	return m, nil
}

func (m *Mode) set(k, v string) error {
	switch k {
	case "after", "interval":
		d, err := time.ParseDuration(v)
		if err != nil {
			return usagef("invalid %s=%q: %v", k, v, err)
		}
		if d < 0 {
			return usagef("invalid %s=%q: must not be negative", k, v)
		}
		if k == "interval" && d == 0 {
			return usagef("invalid %s=%q: must be positive", k, v)
		}
		if k == "after" {
			m.After = d
		} else {
			m.Interval = d
		}
	case "exit":
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 255 {
			return usagef("invalid exit=%q: must be an integer from 0 to 255", v)
		}
		m.Exit = n
	case "rate":
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1<<20 {
			return usagef("invalid rate=%q: must be a positive integer (MiB/s)", v)
		}
		m.Rate = n
	}
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func acceptList(keys []string) string {
	k := append([]string{"sticky"}, keys...)
	sort.Strings(k)
	return strings.Join(k, ", ")
}

// String renders the canonical form, which Parse accepts again.
func (m Mode) String() string {
	parts := []string{m.Name}
	switch m.Name {
	case Unhealthy:
		parts = append(parts, "after="+m.After.String())
	case Crash:
		parts = append(parts, "exit="+strconv.Itoa(m.Exit), "after="+m.After.String())
	case CrashLoop:
		parts = append(parts, "interval="+m.Interval.String(), "exit="+strconv.Itoa(m.Exit))
	case OOM:
		parts = append(parts, "rate="+strconv.Itoa(m.Rate))
	}
	if m.Sticky {
		parts = append(parts, "sticky=true")
	}
	return strings.Join(parts, " ")
}

// ValidateRuntime applies the rules for modes sent to a running process.
func (m Mode) ValidateRuntime() error {
	if m.Name == CrashLoop && !m.Sticky {
		return usagef("crash-loop only makes sense as the boot mode (BOP_MODE) or with sticky=true; " +
			"a non-sticky exec would exit once and the restarted process would not loop")
	}
	return nil
}
