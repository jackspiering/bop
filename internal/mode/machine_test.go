package mode

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestMachineHealth(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	mc := newMachine(Hooks{}, clock.now)

	if s := mc.Snapshot(); !s.Healthy || s.Mode.Name != Healthy {
		t.Fatalf("initial snapshot = %+v", s)
	}

	tests := []struct {
		name    string
		mode    Mode
		elapsed time.Duration
		healthy bool
	}{
		{"unhealthy immediately", Mode{Name: Unhealthy}, 0, false},
		{"unhealthy before after", Mode{Name: Unhealthy, After: 3 * time.Second}, 2 * time.Second, true},
		{"unhealthy at after", Mode{Name: Unhealthy, After: 3 * time.Second}, 3 * time.Second, false},
		{"hang is not unhealthy by itself", Mode{Name: Hang}, time.Hour, true},
		{"healthy", Mode{Name: Healthy}, time.Hour, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := mc.Set(tt.mode, 0); err != nil {
				t.Fatal(err)
			}
			clock.advance(tt.elapsed)
			s := mc.Snapshot()
			if s.Healthy != tt.healthy {
				t.Fatalf("Healthy = %v, want %v", s.Healthy, tt.healthy)
			}
			if !s.Healthy && s.Reason == "" {
				t.Fatal("unhealthy snapshot needs a reason")
			}
		})
	}
}

func TestMachineChangedBroadcast(t *testing.T) {
	mc := NewMachine(Hooks{})
	ch := mc.Changed()
	select {
	case <-ch:
		t.Fatal("Changed closed before any Set")
	default:
	}
	if err := mc.Set(Mode{Name: Hang}, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("Changed not closed by Set")
	}
	if next := mc.Changed(); next == ch {
		t.Fatal("Changed should return a fresh channel after a Set")
	}
}

func TestMachineCrashFires(t *testing.T) {
	codes := make(chan int, 1)
	mc := NewMachine(Hooks{Exit: func(c int) { codes <- c }})
	start := time.Now()
	if err := mc.Set(Mode{Name: Crash, Exit: 3, After: 50 * time.Millisecond}, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-codes:
		if c != 3 {
			t.Fatalf("exit code = %d, want 3", c)
		}
		if time.Since(start) < 50*time.Millisecond {
			t.Fatal("crash fired before after")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("crash did not fire")
	}
}

func TestMachineMinDelayDefersImmediateCrash(t *testing.T) {
	codes := make(chan int, 1)
	mc := NewMachine(Hooks{Exit: func(c int) { codes <- c }})
	start := time.Now()
	if err := mc.Set(Mode{Name: Crash, Exit: 1}, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	<-codes
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("exit was not deferred by minDelay")
	}
}

func TestMachineSwitchCancelsPendingCrash(t *testing.T) {
	codes := make(chan int, 1)
	mc := NewMachine(Hooks{Exit: func(c int) { codes <- c }})
	if err := mc.Set(Mode{Name: Crash, Exit: 3, After: 80 * time.Millisecond}, 0); err != nil {
		t.Fatal(err)
	}
	if err := mc.Set(Mode{Name: Healthy}, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-codes:
		t.Fatalf("cancelled crash fired with %d", c)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestMachineCrashLoopUsesInterval(t *testing.T) {
	codes := make(chan int, 1)
	mc := NewMachine(Hooks{Exit: func(c int) { codes <- c }})
	if err := mc.Set(Mode{Name: CrashLoop, Exit: 9, Interval: 50 * time.Millisecond}, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-codes:
		if c != 9 {
			t.Fatalf("exit code = %d", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("crash-loop did not fire")
	}
}

func TestMachineOOM(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		mc := NewMachine(Hooks{CheckOOM: func() error { return errors.New("no limit") }})
		if err := mc.Set(Mode{Name: OOM, Rate: 1}, 0); err == nil {
			t.Fatal("expected refusal")
		}
		if got := mc.Snapshot().Mode.Name; got != Healthy {
			t.Fatalf("refused oom changed the mode to %s", got)
		}
	})
	t.Run("started and stopped on switch", func(t *testing.T) {
		started := make(chan (<-chan struct{}), 1)
		mc := NewMachine(Hooks{OOM: func(rate int, stop <-chan struct{}) {
			if rate != 7 {
				t.Errorf("rate = %d", rate)
			}
			started <- stop
		}})
		if err := mc.Set(Mode{Name: OOM, Rate: 7}, 0); err != nil {
			t.Fatal(err)
		}
		var stop <-chan struct{}
		select {
		case stop = <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("OOM hook did not run")
		}
		if err := mc.Set(Mode{Name: Healthy}, 0); err != nil {
			t.Fatal(err)
		}
		select {
		case <-stop:
		case <-time.After(time.Second):
			t.Fatal("OOM stop channel not closed on switch")
		}
	})
}

func TestMachineOnChange(t *testing.T) {
	got := make(chan Snapshot, 4)
	mc := NewMachine(Hooks{OnChange: func(s Snapshot) { got <- s }})
	if err := mc.Set(Mode{Name: Unhealthy, After: 50 * time.Millisecond}, 0); err != nil {
		t.Fatal(err)
	}
	first := <-got
	if !first.Healthy {
		t.Fatal("first notification should still be healthy")
	}
	select {
	case second := <-got:
		if second.Healthy {
			t.Fatal("scheduled flip should report unhealthy")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification at the scheduled flip")
	}
}
