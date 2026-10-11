package main

// C02 follow-up (2026-10-10): the wrapper closes its card and releases its lease "inside the grace" a console
// close gives it, but the house command (`--drain --unload-seat`) ran the seat's warm-back, a reload that takes
// minutes, BEFORE the release, on every exit path. A wrapper ended on SIGTERM (the signal a console close, a
// logoff and a shutdown reach Go on Windows as) therefore stood in the warm-back until the OS ended it, with its
// lease held, and a model was being reloaded while the machine went down. Now the lease is released first and
// the warm-back stays owed (the marker); a Ctrl-C, which has no such deadline, still warms the seat back.
//
// The wrapper ends its own process with the command's exit code, so these tests drive it in a child process (this
// test binary, TestHelperWrapperDriver) against a fake llama-swap in the parent, and the signal is delivered
// through the wrapper's test seam: a console close cannot be driven from a test.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHelperWrapperDriver runs `gpu reserve` with the arguments in LO_WRAP_ARGS (one per line) and delivers
// LO_WRAP_SIGNAL (term | int) to it LO_WRAP_SIGNAL_MS after it is listening. A no-op unless LO_WRAP_ARGS is set.
func TestHelperWrapperDriver(t *testing.T) {
	raw := os.Getenv("LO_WRAP_ARGS")
	if raw == "" {
		return
	}
	var sig os.Signal = syscall.SIGTERM
	if os.Getenv("LO_WRAP_SIGNAL") == "int" {
		sig = os.Interrupt
	}
	ms, _ := strconv.Atoi(os.Getenv("LO_WRAP_SIGNAL_MS"))
	// The handler is registered first now (before the queue and the drain), so the clock starts when the command does:
	// these tests are about a command ended inside the grace, not a drain or a wait.
	var feed chan<- os.Signal
	ready := make(chan struct{})
	wrapperSignalFeed = func(c chan<- os.Signal) { feed = c; close(ready) }
	wrapperCommandStarted = func() {
		<-ready
		go func() {
			time.Sleep(time.Duration(ms) * time.Millisecond)
			feed <- sig
		}()
	}
	if err := runGPUReserve(strings.Split(raw, "\n")); err != nil {
		fmt.Fprintln(os.Stderr, "driver: reserve:", err)
		os.Exit(3)
	}
}

// signalledReserve runs the house command (--drain --unload-seat) in a child, ends its wrapped command with sig,
// and returns the fake seat, the lease manager and what the wrapper said.
func signalledReserve(t *testing.T, sig string) (f *orderedSwap, owed string, held bool, out string) {
	t.Helper()
	f = &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	args := []string{"--config", cfgPath, "--wait", "30s", "--drain", "--unload-seat", "--reason", "grace"}
	args = append(args, helperCmdFor("TestHelperSleepMs")...)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWrapperDriver$", "-test.timeout=90s")
	cmd.Env = append(os.Environ(),
		"LO_WRAP_ARGS="+strings.Join(args, "\n"),
		"LO_WRAP_SIGNAL="+sig,
		"LO_WRAP_SIGNAL_MS=700",
		"LO_HELPER_SLEEP_MS=60000", // the command outlives the test unless the signal ends it
	)
	done := make(chan struct{})
	var combined []byte
	go func() {
		combined, _ = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(80 * time.Second):
		t.Fatal("the wrapper never ended after the signal")
	}
	info := m.Inspect()
	return f, m.SeatWarmOwed(), info.Held, string(combined)
}

func TestWrapperEndedOnTermReleasesAtOnceAndLeavesTheWarmOwed(t *testing.T) {
	f, owed, held, out := signalledReserve(t, "term")
	if held {
		t.Errorf("the lease must be released on SIGTERM:\n%s", out)
	}
	if n := f.warms.Load(); n != 0 {
		t.Errorf("the seat was warmed back %d time(s) on SIGTERM: a reload of minutes inside a grace of seconds\n%s", n, out)
	}
	if owed == "" {
		t.Errorf("a skipped warm-back leaves the debt on record (the seat marker), so the next holder pays it\n%s", out)
	}
	if n := f.unloads.Load(); n != 1 {
		t.Errorf("the seat was unloaded %d times, want 1 (the wrapper ran its drain and unload first)\n%s", n, out)
	}
	for _, want := range []string{"ended on SIGTERM", "likely", "NOT warming the seat back", "the warm stays owed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the wrapper's output lacks %q:\n%s", want, out)
		}
	}
}

// Ctrl-C has no five-second deadline: the seat is warmed back before the release, as it always was.
func TestWrapperEndedOnCtrlCStillWarmsTheSeatBack(t *testing.T) {
	f, owed, held, out := signalledReserve(t, "int")
	if held {
		t.Errorf("the lease must be released:\n%s", out)
	}
	if n := f.warms.Load(); n != 1 || owed != "" {
		t.Errorf("a Ctrl-C pays the warm-back once and clears the marker: warms=%d owed=%q\n%s", n, owed, out)
	}
	if strings.Contains(out, "NOT warming the seat back") {
		t.Errorf("a Ctrl-C does not skip the warm-back:\n%s", out)
	}
}
