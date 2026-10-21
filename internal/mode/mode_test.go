package mode

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    Mode
		wantErr string
	}{
		{in: "healthy", want: Mode{Name: Healthy}},
		{in: "  healthy  ", want: Mode{Name: Healthy}},
		{in: "healthy sticky=true", want: Mode{Name: Healthy, Sticky: true}},
		{in: "unhealthy", want: Mode{Name: Unhealthy}},
		{in: "unhealthy after=3s", want: Mode{Name: Unhealthy, After: 3 * time.Second}},
		{in: "crash", want: Mode{Name: Crash, Exit: 1}},
		{in: "crash exit=3 after=10s", want: Mode{Name: Crash, Exit: 3, After: 10 * time.Second}},
		{in: "crash after=1m exit=0", want: Mode{Name: Crash, Exit: 0, After: time.Minute}},
		{in: "crash-loop", want: Mode{Name: CrashLoop, Exit: 1, Interval: 5 * time.Second}},
		{in: "crash-loop interval=250ms exit=42 sticky=true", want: Mode{Name: CrashLoop, Exit: 42, Interval: 250 * time.Millisecond, Sticky: true}},
		{in: "oom", want: Mode{Name: OOM, Rate: 64}},
		{in: "oom rate=8", want: Mode{Name: OOM, Rate: 8}},
		{in: "hang", want: Mode{Name: Hang}},
		{in: "hang sticky=false", want: Mode{Name: Hang}},

		{in: "", wantErr: "empty mode"},
		{in: "   ", wantErr: "empty mode"},
		{in: "explode", wantErr: `unknown mode "explode"`},
		{in: "clear", wantErr: "runtime command"},
		{in: "healthy after=1s", wantErr: `does not accept "after"`},
		{in: "crash interval=1s", wantErr: `does not accept "interval"`},
		{in: "crash exit", wantErr: "expected key=value"},
		{in: "crash =3", wantErr: "expected key=value"},
		{in: "crash exit=3 exit=4", wantErr: "duplicate"},
		{in: "crash exit=256", wantErr: "0 to 255"},
		{in: "crash exit=-1", wantErr: "0 to 255"},
		{in: "crash exit=x", wantErr: "0 to 255"},
		{in: "unhealthy after=soon", wantErr: "invalid after"},
		{in: "unhealthy after=5", wantErr: "invalid after"},
		{in: "unhealthy after=-1s", wantErr: "must be positive"},
		{in: "crash-loop interval=0s", wantErr: "must be positive"},
		{in: "oom rate=0", wantErr: "positive integer"},
		{in: "oom rate=fast", wantErr: "positive integer"},
		{in: "hang sticky=maybe", wantErr: "true or false"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := Parse(tt.in)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse(%q) = %+v, want error containing %q", tt.in, got, tt.wantErr)
				}
				var ue *UsageError
				if !errors.As(err, &ue) {
					t.Fatalf("error %T is not a *UsageError", err)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("Parse(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseCommandClear(t *testing.T) {
	m, err := ParseCommand("clear")
	if err != nil || m.Name != Clear {
		t.Fatalf("ParseCommand(clear) = %+v, %v", m, err)
	}
	for _, in := range []string{"clear after=1s", "clear sticky=true", "clear sticky=false"} {
		if _, err := ParseCommand(in); err == nil {
			t.Errorf("ParseCommand(%q) should fail: clear takes no arguments", in)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, in := range []string{
		"healthy", "unhealthy after=3s", "crash exit=3 after=10s", "crash-loop interval=5s exit=1",
		"oom rate=64", "hang", "hang sticky=true", "crash-loop interval=1s exit=7 sticky=true",
	} {
		m, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got := m.String(); got != in {
			t.Errorf("String() = %q, want %q", got, in)
		}
	}
	// Defaults are rendered explicitly and parse back to the same value.
	m, _ := Parse("crash")
	back, err := Parse(m.String())
	if err != nil || back != m {
		t.Fatalf("round trip of %q: %+v, %v", m.String(), back, err)
	}
}

func TestValidateRuntime(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"healthy", false},
		{"crash exit=3", false},
		{"crash-loop", true},
		{"crash-loop sticky=true", false},
	}
	for _, tt := range tests {
		m, err := Parse(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		err = m.ValidateRuntime()
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateRuntime(%q) = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "sticky=true") {
			t.Errorf("error should explain sticky: %v", err)
		}
	}
}

func TestParseSigterm(t *testing.T) {
	tests := []struct {
		in      string
		want    Sigterm
		wantErr bool
	}{
		{"", Sigterm{Kind: SigtermExit}, false},
		{"exit:0", Sigterm{Kind: SigtermExit}, false},
		{"exit:7", Sigterm{Kind: SigtermExit, Code: 7}, false},
		{"ignore", Sigterm{Kind: SigtermIgnore}, false},
		{"delay:2s", Sigterm{Kind: SigtermDelay, Delay: 2 * time.Second}, false},
		{"exit", Sigterm{}, true},
		{"exit:300", Sigterm{}, true},
		{"exit:x", Sigterm{}, true},
		{"ignore:1", Sigterm{}, true},
		{"delay:soon", Sigterm{}, true},
		{"delay:-1s", Sigterm{}, true},
		{"reboot", Sigterm{}, true},
	}
	for _, tt := range tests {
		got, err := ParseSigterm(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseSigterm(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if err == nil && got != tt.want {
			t.Errorf("ParseSigterm(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}
