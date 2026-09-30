package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// a re-pack stall on a seat with no measured rate names no arithmetic (there
// is none: the flat bound sized it), rather than a meaningless "N expected tok /
// 0.0 tok/s" clause.
func TestMonitorRepackStallWithoutARateNamesNoArithmetic(t *testing.T) {
	pol := StallPolicy{Floor: 40 * time.Millisecond, Repack: 40 * time.Millisecond, Slack: 10 * time.Millisecond}
	ctx, m := NewMonitor(context.Background(), pol, 5*time.Second)
	defer m.Stop()
	m.Phase(PhaseRepack, 800)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("silent re-pack was not stalled")
	}
	var se *StallError
	if !errors.As(context.Cause(ctx), &se) || se.Phase != PhaseRepack {
		t.Fatalf("cause = %v, want a re-pack stall", context.Cause(ctx))
	}
	if strings.Contains(se.Error(), "expected tok") {
		t.Fatalf("stall reason %q names arithmetic for a seat with no known rate", se.Error())
	}
	// It says what sized the allowance instead: the flat bound, because nothing was
	// measured for this seat.
	if !strings.Contains(se.Error(), "flat re-pack bound") || !strings.Contains(se.Error(), "no measured decode rate") {
		t.Fatalf("stall reason %q, want the flat bound and why it is the flat bound named", se.Error())
	}
}

// The same for a seat WITH a rate but no expected size: the arithmetic has nothing
// to work on, so the flat bound sized it and the reason says that.
func TestMonitorRepackStallWithoutASizeNamesTheFlatBound(t *testing.T) {
	pol := StallPolicy{Floor: 40 * time.Millisecond, Repack: 40 * time.Millisecond, Slack: 10 * time.Millisecond, TokS: 100}
	ctx, m := NewMonitor(context.Background(), pol, 5*time.Second)
	defer m.Stop()
	m.Phase(PhaseRepack, 0)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("silent re-pack was not stalled")
	}
	var se *StallError
	if !errors.As(context.Cause(ctx), &se) || se.Phase != PhaseRepack {
		t.Fatalf("cause = %v, want a re-pack stall", context.Cause(ctx))
	}
	if !strings.Contains(se.Error(), "flat re-pack bound") || !strings.Contains(se.Error(), "expected size unknown") {
		t.Fatalf("stall reason %q, want the flat bound and why named", se.Error())
	}
}
