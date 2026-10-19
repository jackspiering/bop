package mode

import (
	"sync"
	"time"
)

// Hooks are the side effects the machine triggers. All are optional.
type Hooks struct {
	// Exit terminates the process with code.
	Exit func(code int)
	// OOM starts allocating memory at rate MiB/s until stop is closed.
	OOM func(rate int, stop <-chan struct{})
	// CheckOOM reports whether the oom mode may run here.
	CheckOOM func() error
	// OnChange is called after the mode changes and when a scheduled health
	// flip happens, for logging.
	OnChange func(Snapshot)
}

// Snapshot is a point-in-time view of the machine.
type Snapshot struct {
	Mode    Mode
	SetAt   time.Time
	Healthy bool
	Reason  string
}

// Machine holds the current mode and runs its scheduled actions.
type Machine struct {
	hooks Hooks
	now   func() time.Time

	mu      sync.Mutex
	cur     Mode
	setAt   time.Time
	gen     uint64
	timer   *time.Timer
	stopOOM chan struct{}
	changed chan struct{}
}

// NewMachine returns a machine in the healthy mode.
func NewMachine(h Hooks) *Machine {
	return newMachine(h, time.Now)
}

func newMachine(h Hooks, now func() time.Time) *Machine {
	return &Machine{
		hooks:   h,
		now:     now,
		cur:     Mode{Name: Healthy},
		setAt:   now(),
		changed: make(chan struct{}),
	}
}

// Set switches to m. Timed actions (crash, crash-loop, oom) fire after at
// least minDelay, so a caller can send its response before the process dies.
func (mc *Machine) Set(m Mode, minDelay time.Duration) error {
	if m.Name == OOM && mc.hooks.CheckOOM != nil {
		if err := mc.hooks.CheckOOM(); err != nil {
			return err
		}
	}

	mc.mu.Lock()
	mc.gen++
	gen := mc.gen
	if mc.timer != nil {
		mc.timer.Stop()
		mc.timer = nil
	}
	if mc.stopOOM != nil {
		close(mc.stopOOM)
		mc.stopOOM = nil
	}
	mc.cur = m
	mc.setAt = mc.now()
	close(mc.changed)
	mc.changed = make(chan struct{})

	switch m.Name {
	case Crash:
		mc.schedule(gen, max(m.After, minDelay), func() { mc.exit(m.Exit) })
	case CrashLoop:
		mc.schedule(gen, max(m.Interval, minDelay), func() { mc.exit(m.Exit) })
	case Unhealthy:
		if m.After > 0 {
			mc.schedule(gen, m.After, func() {
				if mc.hooks.OnChange != nil {
					mc.hooks.OnChange(mc.Snapshot())
				}
			})
		}
	case OOM:
		stop := make(chan struct{})
		mc.stopOOM = stop
		mc.schedule(gen, minDelay, func() {
			if mc.hooks.OOM != nil {
				mc.hooks.OOM(m.Rate, stop)
			}
		})
	}
	mc.mu.Unlock()

	if mc.hooks.OnChange != nil {
		mc.hooks.OnChange(mc.Snapshot())
	}
	return nil
}

// schedule must be called with mu held.
func (mc *Machine) schedule(gen uint64, d time.Duration, fn func()) {
	mc.timer = time.AfterFunc(d, func() {
		mc.mu.Lock()
		stale := mc.gen != gen
		mc.mu.Unlock()
		if !stale {
			fn()
		}
	})
}

func (mc *Machine) exit(code int) {
	if mc.hooks.Exit != nil {
		mc.hooks.Exit(code)
	}
}

// Snapshot returns the current state.
func (mc *Machine) Snapshot() Snapshot {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	s := Snapshot{Mode: mc.cur, SetAt: mc.setAt, Healthy: true}
	if mc.cur.Name == Unhealthy && mc.now().Sub(mc.setAt) >= mc.cur.After {
		s.Healthy = false
		s.Reason = "unhealthy mode is active"
	}
	return s
}

// Changed returns a channel that is closed on the next mode change.
func (mc *Machine) Changed() <-chan struct{} {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return mc.changed
}

// CheckOOM reports whether the oom mode may run, or nil if there is no check.
func (mc *Machine) CheckOOM() error {
	if mc.hooks.CheckOOM == nil {
		return nil
	}
	return mc.hooks.CheckOOM()
}
