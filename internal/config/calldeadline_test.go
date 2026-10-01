package config

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCallDeadlineResolution pins the three-way convention every wait knob in
// this file follows (see PlacementWait): 0 takes the built-in default, a negative
// value switches the feature off, and a positive value is that many seconds.
func TestCallDeadlineResolution(t *testing.T) {
	cases := []struct {
		name string
		sec  int
		want time.Duration
	}{
		{"unset takes the built-in default", 0, time.Duration(DefaultCallDeadlineSec) * time.Second},
		{"negative switches the deadline off", -1, 0},
		{"any negative switches it off", -300, 0},
		{"positive is that many seconds", 900, 900 * time.Second},
		{"one second (a compressed test clock)", 1, time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Config{AgentCallDeadlineSec: tc.sec}).CallDeadline(); got != tc.want {
				t.Fatalf("AgentCallDeadlineSec=%d resolved to %s, want %s", tc.sec, got, tc.want)
			}
		})
	}
	if got := Default().CallDeadline(); got != DefaultCallDeadlineSec*time.Second {
		t.Fatalf("Default().CallDeadline() = %s, want the built-in %ds: the deadline must be ON by default", got, DefaultCallDeadlineSec)
	}
}

// TestCallDeadlineFindingsNameTheValuesThatVoidTheDeadline: the key is accepted as written
// (any negative means "no deadline", a positive value is taken as it comes), and two shapes
// quietly void the protection the key exists for — a value at or above the MCP client's
// abort (the call is dropped with its finished results before it can return), and a negative
// that was meant as a number (-1500 for 1500). doctor and the startup line name them; the
// documented settings (unset, an in-range number, -1) stay silent.
func TestCallDeadlineFindingsNameTheValuesThatVoidTheDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		sec  int
		want []string // substrings of the one finding; nil = no finding
	}{
		{"unset takes the built-in default", 0, nil},
		{"the built-in default written out", DefaultCallDeadlineSec, nil},
		{"one second under the client's abort", 1799, nil},
		{"exactly the client's abort", 1800, []string{"agent_call_deadline_sec 1800", "abort", "below"}},
		{"well above it", 3600, []string{"agent_call_deadline_sec 3600", "abort"}},
		{"the documented way to switch it off", -1, nil},
		{"a negative that was meant as a number", -1500, []string{"agent_call_deadline_sec -1500", "OFF", "-1", "1500"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CallDeadlineFindings(Config{AgentCallDeadlineSec: tc.sec})
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("agent_call_deadline_sec=%d produced findings %q, want none", tc.sec, got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("agent_call_deadline_sec=%d produced %d finding(s) %q, want exactly 1", tc.sec, len(got), got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got[0], w) {
					t.Errorf("finding %q must contain %q", got[0], w)
				}
			}
		})
	}
}

// captureStderr runs f with os.Stderr redirected and returns what was written.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	f()
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// TestLoadAndDoctorReportACallDeadlineThatVoidsItself: the finding rides the loaded value
// (doctor reads Findings()) and is printed once at load — a config that loads, runs and then
// loses a finished result to the client's abort is the silent state this warns about.
func TestLoadAndDoctorReportACallDeadlineThatVoidsItself(t *testing.T) {
	var cfg Config
	var err error
	stderr := captureStderr(t, func() {
		cfg, err = Load(writeShapeCfg(t, `{"agent_call_deadline_sec":2400}`))
	})
	if err != nil {
		t.Fatalf("a deadline at or above the client's abort must LOAD (warn, never refuse): %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Findings(), "\n"), "agent_call_deadline_sec 2400") {
		t.Fatalf("Findings() = %q, want doctor to carry the finding", cfg.Findings())
	}
	if !strings.Contains(stderr, "warning: agent_call_deadline_sec 2400") {
		t.Fatalf("stderr at load = %q, want the startup warning", stderr)
	}
	if f := Default().Findings(); len(f) != 0 {
		t.Fatalf("the default config must produce no finding; got %q", f)
	}
}
