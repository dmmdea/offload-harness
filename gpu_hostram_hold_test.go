package main

// F82, the hold bound (production incident of 2026-10-14 on the reference box).
//
// `gpu reserve --wait 8h --drain --unload-seat --class media --for 40m -- <a command that is not a render helper>` declared
// the media class default (46.8 GiB). F82 takes the cards, drains, unloads the seat and only then asks the host, still holding
// the cards, for the whole --wait. The seat was not even loaded, the host had 8 GiB of headroom, the need could not be met, and
// the lease held all three cards idle for about 50 minutes while a production lease queued behind it.
//
// What these tests pin: the hold after the unload is bounded (config gpu_hostram_hold_max_sec, default 600 s, `--ram-hold-max`
// overrides, 0 = the old unbounded wait inside --wait); at the bound the lease is RELEASED (the seat warmed back as for any
// release), the command never runs and the reserve exits non-zero with one line that names the need, the host's figures, how
// long the cards were held and what to do; it never silently re-queues; the wait line says when the unload freed nothing; a
// figure that is only the class default is warned about; and `gpu status` says how long the cards have been held and when the
// bound releases them. The host these tests stand in is useSeatBackedHost's: 95 of 100 GiB committed while the fake seat is
// loaded, the second figure once it is not. 70 never fits a 30 GiB need (70 + 30 > 92) and always fits the 21 GiB warm-back
// of the seat (70 + 21 <= 92), so the warm-back is observable.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// holdBoundReserve is an unload-seat reserve of the wrapper form on the seat-backed host that never has the room.
func holdBoundReserve(t *testing.T, loaded, keepFloor bool, extra ...string) (cfgPath string, m *gpulease.Manager, f *orderedSwap, epochFile string, args []string) {
	t.Helper()
	f = &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(loaded)
	cfgPath, m = handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 70)
	if !keepFloor {
		// Only the incident's own test keeps the production 2 minute drain floor (the bound must beat it). The others
		// shorten it, so a build that ignores the bound is red in the wait it asked for and not after two minutes each.
		shortFloor(t, time.Second)
	}
	args, epochFile = unloadSeatReserve(t, cfgPath, append([]string{"--wait", "30s", "--drain", "--unload-seat", "--ram", "30"}, extra...)...)
	return
}

// THE INCIDENT. The unload frees too little (70 stay committed), so the host never has the room; the bound passes. The lease
// is released and the seat warmed back, the command never ran, and the line names everything the operator needs.
func TestTheHoldBoundReleasesTheCardsAndRunsNothingWhenTheHostNeverHasTheRoom(t *testing.T) {
	_, m, f, epochFile, args := holdBoundReserve(t, true, true, "--ram-hold-max", "200ms")
	start := time.Now()
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil {
		t.Fatalf("the host never had the room: the reserve must exit non-zero\n%s", stderr)
	}
	// The wait would have run to --wait (30s, and never under the 2 minute drain floor): the bound ended it.
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("the bound did not end the wait: the reserve ran %s", took)
	}
	msg := err.Error()
	t.Logf("the failure the operator reads:\n%s", msg)
	if strings.ContainsAny(msg, "\r\n") {
		t.Errorf("the failure is one line:\n%s", msg)
	}
	for _, want := range []string{
		"hold bound",                  // what ended it
		"30.0 GiB",                    // the need
		"committed 70.0 of 100.0 GiB", // the host's committed and physical figures
		"8.0 GiB headroom",            // and its headroom
		"held the cards for",          // how long
		"--ram <GiB>",                 // what to do about a figure that was a guess
		"released",                    // what happened to the lease
		"--ram-hold-max",              // the lever that holds longer
		"gpu_hostram_hold_max_sec",    // and its config key
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure must say %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "pass a longer --wait") {
		t.Errorf("--wait is not what ended this wait, so it is not the lever to name:\n%s", msg)
	}
	if fileExists(epochFile) {
		t.Errorf("the command ran although the host never admitted its RAM\n%s", stderr)
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("the lease must be released at the bound, not kept: %+v", info)
	}
	if w := m.Waiters(); len(w) != 0 {
		t.Errorf("the reserve must not silently re-queue: %+v", w)
	}
	if strings.Contains(stderr, "queueing for the card again") {
		t.Errorf("the reserve re-queued after the bound:\n%s", stderr)
	}
	if n := f.unloads.Load(); n != 1 {
		t.Errorf("the seat was unloaded once before the host was asked: unloads=%d", n)
	}
	if n := f.warms.Load(); n != 1 || m.SeatWarmOwed() != "" {
		t.Errorf("the seat is warmed back as for any release of a lease that unloaded it, and the debt is paid: warms=%d owed=%q\n%s", n, m.SeatWarmOwed(), stderr)
	}
}

// The bound comes from the config key when no flag is given.
func TestTheHoldBoundComesFromTheConfigKey(t *testing.T) {
	cfgPath, m, _, epochFile, args := holdBoundReserve(t, true, false)
	setConfigKey(t, cfgPath, "gpu_hostram_hold_max_sec", 1)
	start := time.Now()
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "hold bound") {
		t.Fatalf("a configured bound of 1 s ends the wait with the bound's failure, got %v\n%s", err, stderr)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("the configured bound did not end the wait: %s", took)
	}
	if fileExists(epochFile) || m.Inspect().Held {
		t.Errorf("nothing ran and nothing is held after the bound\n%s", stderr)
	}
}

// BOUND 0 IS THE OLD BEHAVIOUR. The wait runs to --wait (never under the drain floor), the cards stay held until then, and it
// ends with the host-RAM refusal and the flag that waits longer, not with the hold bound's failure.
func TestAHoldBoundOfZeroKeepsTheWholeWait(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		cfg   any // gpu_hostram_hold_max_sec, nil = leave the key out
		wait  string
	}{
		{"flag 0", []string{"--ram-hold-max", "0"}, nil, "2500ms"},
		{"config 0", nil, 0, "2500ms"},
		// A configured bound of 1 s would end this wait well before --wait (6 s); the flag's 0 is a value and beats it.
		{"flag 0 beats a configured bound", []string{"--ram-hold-max", "0"}, 1, "6s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &orderedSwap{drainSwap: &drainSwap{}}
			f.loaded.Store(true)
			cfgPath, m := handoffFixture(t, f)
			useSeatBackedHost(t, f.loaded.Load, 95, 70)
			shortFloor(t, 400*time.Millisecond)
			if tc.cfg != nil {
				setConfigKey(t, cfgPath, "gpu_hostram_hold_max_sec", tc.cfg)
			}
			args, epochFile := unloadSeatReserve(t, cfgPath, append([]string{"--wait", tc.wait, "--drain", "--unload-seat", "--ram", "30"}, tc.flags...)...)
			start := time.Now()
			var err error
			stderr := captureStderr(t, func() { err = runGPUReserve(args) })
			if err == nil {
				t.Fatalf("the host never had the room\n%s", stderr)
			}
			waitFor, _ := time.ParseDuration(tc.wait)
			if took := time.Since(start); took < waitFor-500*time.Millisecond {
				t.Errorf("with no bound the cards are held for the whole --wait (%s), the reserve ended after %s: %v", waitFor, took, err)
			}
			for _, want := range []string{"waiting for host RAM: needs 30.0 GiB, committed 70.0 of 100.0 GiB physical", "--wait " + waitFor.String(), "pass a longer --wait"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the old refusal says %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "hold bound") {
				t.Errorf("no bound was in force, so none may be named: %v", err)
			}
			if fileExists(epochFile) || m.Inspect().Held {
				t.Errorf("the lease ends released and the command never ran\n%s", stderr)
			}
		})
	}
}

// THE DETACHED FORM. A hold that no command wraps is the one that can sit on the cards the longest, so the bound releases it
// too, with the warm-back a release owes; the failure is non-zero. (A wait that --wait ends first keeps the lease held, as
// TestADetachedUnloadSeatLeaseThatTheHostNeverAdmitsFailsAndSaysTheLeaseIsHeld pins.)
func TestTheHoldBoundReleasesADetachedHoldToo(t *testing.T) {
	f := &orderedSwap{drainSwap: &drainSwap{}}
	f.loaded.Store(true)
	cfgPath, m := handoffFixture(t, f)
	useSeatBackedHost(t, f.loaded.Load, 95, 70)
	shortFloor(t, time.Second)
	var held *gpulease.Lease
	oldFn := detachHolderFn
	detachHolderFn = func(_ *flag.FlagSet, class string, _, _ time.Duration, opts gpulease.Options, _ *reserveDeviceFlags, _ bool, m *gpulease.Manager) (uint64, error) {
		l, err := m.TryAcquire(gpulease.Class(class), opts)
		if err != nil {
			return 0, err
		}
		held = l
		return l.Epoch(), nil
	}
	t.Cleanup(func() {
		detachHolderFn = oldFn
		if held != nil {
			_ = held.Release()
		}
	})
	args := []string{"--config", cfgPath, "--detach", "--for", "1h", "--wait", "30s", "--drain", "--unload-seat", "--ram", "30", "--ram-hold-max", "200ms", "--reason", "film"}
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "hold bound") {
		t.Fatalf("a detached hold whose bound passed fails with the bound's failure, got %v\n%s", err, stderr)
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("the bound releases a detached hold: %+v", info)
	}
	if n := f.warms.Load(); n != 1 || m.SeatWarmOwed() != "" {
		t.Errorf("the release warms the seat back (`gpu release --warm-seat`): warms=%d owed=%q\n%s", n, m.SeatWarmOwed(), stderr)
	}
}

// THE WAIT LINE. It says that the unload freed nothing when the seat was not loaded (the 2026-10-14 shape: the unload freed
// nothing, so it could not shorten the wait), still applies the bound (the lease is not failed at once), and names the bound.
func TestTheWaitLineSaysWhenTheUnloadFreedNothingAndStillAppliesTheBound(t *testing.T) {
	_, m, f, epochFile, args := holdBoundReserve(t, false, false, "--ram-hold-max", "300ms")
	// The seat is cold, so the host reads 70 whether or not an unload runs.
	start := time.Now()
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "hold bound") {
		t.Fatalf("the bound ends a wait the unload could not shorten: %v\n%s", err, stderr)
	}
	if took := time.Since(start); took < 250*time.Millisecond {
		t.Errorf("a cold seat is not special-cased into an immediate failure: the reserve ended after %s", took)
	}
	line := lineWith(t, stderr, "the cards are held")
	for _, want := range []string{"freed nothing", "was not loaded", "hold bound", "300ms"} {
		if !strings.Contains(line, want) {
			t.Errorf("the wait line must say %q:\n%s", want, line)
		}
	}
	if !strings.Contains(stderr, "was not loaded; nothing to warm back") {
		t.Errorf("the reserve still says the seat was cold:\n%s", stderr)
	}
	if fileExists(epochFile) || m.Inspect().Held {
		t.Errorf("nothing ran and the lease is released\n%s", stderr)
	}
	if n := f.warms.Load(); n != 0 {
		t.Errorf("a seat that was never loaded is not warmed back: warms=%d", n)
	}
}

// Control: when the unload DID take the seat down the line does not claim it freed nothing, but still names the bound.
func TestTheWaitLineDoesNotClaimNothingWasFreedWhenTheSeatWasUnloaded(t *testing.T) {
	_, _, f, _, args := holdBoundReserve(t, true, false, "--ram-hold-max", "200ms")
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil {
		t.Fatalf("the host never had the room\n%s", stderr)
	}
	if n := f.unloads.Load(); n != 1 {
		t.Fatalf("the seat was loaded and is unloaded once: unloads=%d", n)
	}
	line := lineWith(t, stderr, "the cards are held")
	if strings.Contains(line, "freed nothing") {
		t.Errorf("the unload took the seat down, so the line must not say it freed nothing:\n%s", line)
	}
	if !strings.Contains(line, "hold bound") || !strings.Contains(line, "200ms") {
		t.Errorf("the line names the bound that will release the lease:\n%s", line)
	}
}

func lineWith(t *testing.T, text, want string) string {
	t.Helper()
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, want) {
			return l
		}
	}
	t.Fatalf("no line says %q:\n%s", want, text)
	return ""
}

// THE WARNING. A wrapped command that is not a recognised render helper, with no --ram, declares the media class default; that
// figure is a guess about a command the lease could not read, and with --unload-seat it can hold the cards while the host
// cannot meet it. The line after the declaring one is a WARNING that says so and names --ram. The figure itself is unchanged.
func TestTheClassDefaultLineIsAWarningThatTheFigureMayHoldTheCards(t *testing.T) {
	cfg := classDefaultBox(t)
	lines, need := reserveSays(t, false, 0, gpulease.ClassMedia, []string{"python", "driver.py"}, cfg)
	wantDeclaringFirst(t, lines, need)
	if len(lines) != 2 {
		t.Fatalf("the declaring line and exactly one warning, got %q", lines)
	}
	for _, want := range []string{"gpu reserve: WARNING: ", whyUnrecognised, "may hold the cards while the host cannot meet it", "--ram <GiB>", "--ram 0", "gpu_hostram_hold_max_sec"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("the warning must say %q, got %q", want, lines[1])
		}
	}
	if !strings.HasPrefix(lines[1], "gpu reserve: WARNING: ") {
		t.Errorf("the line is a WARNING line: %q", lines[1])
	}
	// The figure is the class default, as before.
	if need.GiB < 32 {
		t.Errorf("the warning changes no figure; the class default stands, got %v", need)
	}
	// A figure the operator stated gets no warning.
	if lines, _ := reserveSays(t, true, 12, gpulease.ClassMedia, []string{"python", "driver.py"}, cfg); len(lines) != 1 {
		t.Errorf("an explicit --ram prints the declaring line only, got %q", lines)
	}
}

// `gpu status` for a lease that holds its cards and has not been admitted: how long it has held, and when the bound releases it.
func TestGPUStatusSaysHowLongAHeldLeaseHasHeldAndWhenTheBoundReleasesIt(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	useHostCommit(t, newHostCommit(10))
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, Devices: []string{boxCard2}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	statusText := func() string {
		return captureStdout(t, func() {
			if err := runGPUStatus([]string{"--config", cfg}); err != nil {
				t.Fatal(err)
			}
		})
	}

	// Before the wait for the host begins no bound is running: the old sentence stands, and the hold time is added.
	out := statusText()
	for _, want := range []string{"host RAM 30.0 GiB asked, not yet admitted", "the seat is cleared first, then the host is asked", "held the cards for"} {
		if !strings.Contains(out, want) {
			t.Errorf("status of a lease that has not begun to wait must say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hold bound") || strings.Contains(out, "is released at") {
		t.Errorf("no bound is running yet, so none is named:\n%s", out)
	}

	until := time.Now().Add(7 * time.Minute)
	if err := m.Restamp(l.Epoch(), func(mt *gpulease.Meta) { mt.HostRAMHoldUntilMs = until.UnixMilli() }); err != nil {
		t.Fatal(err)
	}
	out = statusText()
	for _, want := range []string{"host RAM 30.0 GiB asked, not yet admitted", "held the cards for", "the lease is released at " + until.Format(time.Kitchen), "if the host still lacks the room"} {
		if !strings.Contains(out, want) {
			t.Errorf("status of a lease waiting under a bound must say %q:\n%s", want, out)
		}
	}
	row := statusJSON(t, cfg)["leases"].([]any)[0].(map[string]any)
	got, _ := row["host_ram_hold_until"].(string)
	parsed, perr := time.Parse(time.RFC3339, got)
	if perr != nil || parsed.Sub(until).Abs() > 2*time.Second {
		t.Errorf("the lease row carries the bound as host_ram_hold_until (RFC 3339): %v (parse error %v)", row["host_ram_hold_until"], perr)
	}

	// Once the stamped moment has passed and the lease is still there, the one who will release it is the HOLDER, not the reserve
	// that may be gone: say what is then true, and the way to free it now.
	past := time.Now().Add(-90 * time.Second)
	if err := m.Restamp(l.Epoch(), func(mt *gpulease.Meta) { mt.HostRAMHoldUntilMs = past.UnixMilli() }); err != nil {
		t.Fatal(err)
	}
	out = statusText()
	for _, want := range []string{"the wait ended at " + past.Format(time.Kitchen), "the holder releases the lease at its next poll", "gpu release --warm-seat --epoch 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("status of a lease whose stamped wait has ended must say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "the reserve that holds the lease releases it") {
		t.Errorf("the reserve may be gone: status must not promise its release:\n%s", out)
	}
}

// `--ram-hold-max` follows the same ceiling as the config key (7 days): a duration above it is refused before anything is
// acquired, by name, and one at it is not.
func TestARamHoldMaxAboveTheCeilingIsRefusedBeforeAnythingIsTaken(t *testing.T) {
	cfgPath, m, f, epochFile, args := holdBoundReserve(t, true, false, "--ram-hold-max", "200h")
	var err error
	stderr := captureStderr(t, func() { err = runGPUReserve(args) })
	if err == nil || !strings.Contains(err.Error(), "--ram-hold-max") || !strings.Contains(err.Error(), "7 days") {
		t.Fatalf("200h is above the 7 day ceiling and is refused by name, got %v\n%s", err, stderr)
	}
	if m.Inspect().Held || fileExists(epochFile) || f.unloads.Load() != 0 {
		t.Errorf("a refused flag takes nothing, runs nothing and unloads nothing: held=%v unloads=%d", m.Inspect().Held, f.unloads.Load())
	}
	_ = cfgPath
}

// Review finding: the CLI turns a failed stamp into ONE stderr line, however many times the library reports it (it retries at every
// look), and says what it costs (the status line, and the hidden holder's own enforcement of the bound). There is no clear to
// fail: the stamp leaves the record only with admission.
func TestAFailedStampIsSaidOnce(t *testing.T) {
	short := &gpulease.ErrHostRAM{}
	short.Why = "waiting for host RAM: needs 30.0 GiB, committed 70.0 of 100.0 GiB physical, 8.0 GiB headroom"
	await := func(o gpulease.AwaitHostRAMOptions) error {
		if o.OnStampError == nil {
			return nil // the CLI wired nothing: the failures below would be swallowed
		}
		for i := 0; i < 3; i++ {
			o.OnStampError(errors.New("disk full"))
		}
		return nil
	}
	var out bytes.Buffer
	if err := awaitHostRAMBounded(context.Background(), await, time.Now().Add(time.Hour), time.Hour, hostRAMHold{max: time.Minute, heldSince: time.Now()}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if n := strings.Count(text, "could not stamp"); n != 1 {
		t.Errorf("one line for the stamp that could not be written, got %d:\n%s", n, text)
	}
	for _, want := range []string{"gpu reserve: warning:", "disk full", "hidden holder", "gpu status"} {
		if !strings.Contains(text, want) {
			t.Errorf("the lines must say %q:\n%s", want, text)
		}
	}
}
