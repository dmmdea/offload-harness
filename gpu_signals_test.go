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
	// The card names the signal and gives the cause as LIKELY: a SIGTERM is also what a plain kill sends, and nothing in the
	// wrapper can tell which it was (0.180 final check). Text that named the console close as the cause claimed what it
	// could not know.
	got := signalWhy(syscall.SIGTERM)
	for _, want := range []string{"SIGTERM", "likely", "console close", "plain kill"} {
		if !strings.Contains(got, want) {
			t.Errorf("SIGTERM text %q lacks %q", got, want)
		}
	}
	for _, claim := range []string{"its console was closed", "the session is ending", "the machine is shutting down", "terminated:"} {
		if strings.Contains(got, claim) {
			t.Errorf("SIGTERM text %q states a cause as fact (%q)", got, claim)
		}
	}
	if n := len([]rune(signalWhy(syscall.SIGTERM) + " exit status 1")); n > 140-20 {
		t.Errorf("the card clause is %d characters; the card shows 140 in all and the exit status goes before it", n)
	}
	for _, claim := range []string{"its console was closed", "the session is ending", "the machine is shutting down"} {
		if strings.Contains(warmSkippedOnTermSentence, claim) {
			t.Errorf("the warm-skipped sentence states a cause as fact (%q)", claim)
		}
	}
	if !strings.Contains(warmSkippedOnTermSentence, "SIGTERM") || !strings.Contains(warmSkippedOnTermSentence, "plain kill") {
		t.Errorf("the warm-skipped sentence must name the signal and say a plain kill sends it too: %s", warmSkippedOnTermSentence)
	}
	if other := signalWhy(syscall.SIGHUP); !strings.Contains(other, "signal") {
		t.Errorf("an unknown signal is named as a signal: %q", other)
	}
}

// The wrapper's loop is long and drives a real command, so the wiring is pinned where it is made: the
// signals it asks for are wrapperSignals (not a literal os.Interrupt that would drop SIGTERM again), they are asked
// for FIRST (before the lease is opened: the handler used to be registered after the queue wait and the drain) and the
// card's text comes from the signal that arrived.
func TestTheWrapperLoopUsesTheSignalSetAndTheSignalsOwnText(t *testing.T) {
	read := func(name string) string {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(src), "\r\n", "\n")
	}
	cmd, sigs := read("gpu_cmd.go"), read("gpu_signals.go")
	for _, want := range []string{"sw = watchWrapperSignals()", "why = signalWhy(sig)"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("gpu_cmd.go no longer contains %q", want)
		}
	}
	if !strings.Contains(sigs, "signal.Notify(w.sigc, wrapperSignals...)") {
		t.Error("the watch no longer asks for wrapperSignals")
	}
	if strings.Contains(cmd+sigs, "signal.Notify(sigc, os.Interrupt)") || strings.Contains(cmd+sigs, "Notify(w.sigc, os.Interrupt)") {
		t.Error("the wrapper asks for os.Interrupt alone again")
	}
	// The handler is registered before the lease directory is opened, so no stage runs unhandled.
	body := cmd[strings.Index(cmd, "func runGPUReserve("):]
	if w, o := strings.Index(body, "sw = watchWrapperSignals()"), strings.Index(body, "m, err := openLease(fs)"); w < 0 || o < 0 || w > o {
		t.Error("the signal handler must be registered before openLease: every stage of the wrapper runs after it")
	}
}
