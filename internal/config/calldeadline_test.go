package config

import (
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
