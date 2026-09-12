package main

import (
	"strings"
	"testing"
)

// A DETACHED holder releases the card at --for whether or not the work has
// finished. With the old 45-minute default a multi-hour job silently lost its
// reservation partway through, the next render claimed the card and unloaded
// the seat on top of the running work, and the node was left advertising a
// seat that was not there (2026-09-07 audit). So --detach must state its
// window; the wrapper form, which ties the hold to a process, still needs none.
func TestGPUReserveDetachRequiresAnExplicitFor(t *testing.T) {
	err := runGPUReserve([]string{"--class", "text", "--reason", "training", "--detach"})
	if err == nil {
		t.Fatal("--detach without --for must be refused: the default window would free the card mid-job")
	}
	for _, want := range []string{"--detach requires an explicit --for", "wrapper form"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must say %q and point at the alternative; got: %v", want, err)
		}
	}
}

// The two shapes that were already correct stay correct: an explicit window on
// --detach, and the wrapper form with no window at all. Neither may be
// rejected by the new guard, so these assert we get PAST it — any later error
// (no command to wrap, an unwritable lease dir) is not this check.
//
// HERMETIC since 0.116.1: the first draft passed "\x00nonexistent" as --config,
// which config.Load maps to built-in DEFAULTS (IsNotExist → defaults, nil error)
// — i.e. the machine's REAL lease directory. On a box whose card was held by a
// render the wrapper form queued behind it for the default 8 h wait and the
// whole root package timed out (2026-09-12, the ReadyPep render's media lease);
// on an idle box the --detach case would have taken a real 8 h "training"
// lease. Both calls now arbitrate a temp lease root (leaseFixture) with a
// fail-fast wait, and the --detach case is steered into the LATER
// "mutually exclusive" refusal so it proves the guard was passed without ever
// acquiring anything.
func TestGPUReserveAcceptsAnExplicitWindowAndTheWrapperForm(t *testing.T) {
	cfg, _ := leaseFixture(t)
	err := runGPUReserve([]string{"--class", "text", "--for", "8h", "--reason", "training", "--detach", "--config", cfg, "--wait", "0", "--", "true"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("an explicit --for must satisfy the guard and fall through to the later mutual-exclusion refusal; got: %v", err)
	}
	// The wrapper form declares its window implicitly by wrapping a process.
	err = runGPUReserve(append([]string{"--class", "text", "--reason", "training", "--config", cfg, "--wait", "0"}, helperCmd()...))
	if err != nil && strings.Contains(err.Error(), "--detach requires an explicit --for") {
		t.Fatalf("the wrapper form must never hit the --detach guard: %v", err)
	}
	// And the pre-existing pairing rule is untouched.
	err = runGPUReserve([]string{"--unload-seat", "--for", "1h", "--detach"})
	if err == nil || !strings.Contains(err.Error(), "--unload-seat requires --drain") {
		t.Fatalf("--unload-seat without --drain must still be refused; got %v", err)
	}
}
