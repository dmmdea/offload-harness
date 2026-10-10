package main

// C02 (2026-10-09): five card-scoped leases and two queued ones died in the same second while their
// renders were 7 to 9 hours in; the renders exited 0xC000013A (STATUS_CONTROL_C_EXIT, a console close) and
// the wrappers were gone, so the PAIR cards stayed open until a fleet-node sweep closed them "harness
// process exited before the job finished" and the leases were found stale. The wrapper asked Go for
// os.Interrupt only. Go on Windows delivers CTRL_CLOSE_EVENT, CTRL_LOGOFF_EVENT and CTRL_SHUTDOWN_EVENT as
// syscall.SIGTERM to a program that asked for it, and the process is ended only after its handler has had
// its grace: a wrapper that asks gets to close its card with the true cause and release its lease. (A
// taskkill /F gives no signal at all; nothing in-process can answer it, and the sweep's text covers it.)
// A console close cannot be driven from a test, so these pin what the wrapper asks for and what it says.

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestWrapperAsksForTheSignalsAClosedConsoleDelivers(t *testing.T) {
	have := map[os.Signal]bool{}
	for _, s := range wrapperSignals {
		have[s] = true
	}
	if !have[os.Interrupt] {
		t.Error("Ctrl-C / Ctrl-Break (os.Interrupt) must still end the wrapper cleanly")
	}
	if !have[syscall.SIGTERM] {
		t.Error("syscall.SIGTERM is how Go on Windows delivers a console close, a logoff and a shutdown: without it the wrapper is ended mid-job with its card open and its lease to be found stale")
	}
}

func TestTheCardSaysWhichSignalEndedTheCommand(t *testing.T) {
	if got := signalWhy(os.Interrupt); got != " (interrupted)" {
		t.Errorf("interrupt = %q, the text every existing reader knows", got)
	}
	got := signalWhy(syscall.SIGTERM)
	for _, want := range []string{"terminated", "console was closed", "session"} {
		if !strings.Contains(got, want) {
			t.Errorf("SIGTERM text %q lacks %q", got, want)
		}
	}
	if other := signalWhy(syscall.SIGHUP); !strings.Contains(other, "signal") {
		t.Errorf("an unknown signal is named as a signal: %q", other)
	}
}

// The wrapper's loop is long and drives a real command, so the wiring is pinned where it is made: the
// signals it asks for are wrapperSignals (not a literal os.Interrupt that would drop SIGTERM again) and
// the card's text comes from the signal that arrived.
func TestTheWrapperLoopUsesTheSignalSetAndTheSignalsOwnText(t *testing.T) {
	src, err := os.ReadFile("gpu_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	for _, want := range []string{"signal.Notify(sigc, wrapperSignals...)", "why = signalWhy(sig)"} {
		if !strings.Contains(text, want) {
			t.Errorf("gpu_cmd.go no longer contains %q", want)
		}
	}
	if strings.Contains(text, "signal.Notify(sigc, os.Interrupt)") {
		t.Error("the wrapper asks for os.Interrupt alone again")
	}
}
