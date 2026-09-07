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
func TestGPUReserveAcceptsAnExplicitWindowAndTheWrapperForm(t *testing.T) {
	err := runGPUReserve([]string{"--class", "text", "--for", "8h", "--reason", "training", "--detach", "--config", "\x00nonexistent"})
	if err != nil && strings.Contains(err.Error(), "--detach requires an explicit --for") {
		t.Fatalf("an explicit --for must satisfy the guard: %v", err)
	}
	// The wrapper form declares its window implicitly by wrapping a process.
	err = runGPUReserve([]string{"--class", "text", "--reason", "training", "--config", "\x00nonexistent", "--", "true"})
	if err != nil && strings.Contains(err.Error(), "--detach requires an explicit --for") {
		t.Fatalf("the wrapper form must never hit the --detach guard: %v", err)
	}
	// And the pre-existing pairing rule is untouched.
	err = runGPUReserve([]string{"--unload-seat", "--for", "1h", "--detach"})
	if err == nil || !strings.Contains(err.Error(), "--unload-seat requires --drain") {
		t.Fatalf("--unload-seat without --drain must still be refused; got %v", err)
	}
}
