package main

// F82, the parts of the reorder that gpu_reserve_unload_hostram_test.go reaches only through a whole reserve: how a
// need is split between the grant and the wait after the unload, what the hidden holder is told and what it does
// with it, what the wait says, how long it may run, and what `gpu status` shows while a lease holds its cards and
// has not been admitted yet.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// hostRAMSplit is the one decision of the reorder: a lease that unloads the seat defers every GiB it declares, any other
// lease (and a need of 0) compares in the grant as before.
func TestHostRAMSplitDefersTheNeedOfALeaseThatUnloadsTheSeatAndOnlyThat(t *testing.T) {
	for _, tc := range []struct {
		name         string
		unload       bool
		need         float64
		gate, awaits float64
	}{
		{"unload with a need", true, 30, 0, 30},
		{"no unload with a need", false, 30, 30, 0},
		{"unload with no need", true, 0, 0, 0},
		{"no unload with no need", false, 0, 0, 0},
	} {
		gate, later := hostRAMSplit(tc.unload, tc.need)
		if gate != tc.gate || later != tc.awaits {
			t.Errorf("%s: gate %.1f later %.1f, want gate %.1f later %.1f", tc.name, gate, later, tc.gate, tc.awaits)
		}
	}
}

// THE HIDDEN HOLDER'S ARGV. A lease that unloads the seat reaches the holder as --ram 0 --ram-wait N: the holder
// compares nothing in its grant, and the record carries the figure as a wait. Without it the holder would compare the
// full figure in its own Acquire and wait for the memory the parent's unload frees: --detach would deadlock exactly as
// the wrapper did.
func TestHoldArgsCarryTheWaitingRamOfALeaseThatUnloadsTheSeat(t *testing.T) {
	word := func(args []string, flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return "<absent>"
	}
	deferred := holdArgs("media", time.Hour, time.Minute, gpulease.Options{Devices: []string{"gpu-aaaa0000-x"}, HostRAMWaitGiB: 30}, nil, "")
	if got := word(deferred, "--ram"); got != "0" {
		t.Errorf("the holder declares nothing in its grant: --ram %s (%v)", got, deferred)
	}
	if got := word(deferred, "--ram-wait"); got != "30" {
		t.Errorf("the figure travels as a wait: --ram-wait %s (%v)", got, deferred)
	}
	// An allocated request carries the wait too, beside the allocator's own (zero) host term.
	auto := holdArgs("media", time.Hour, time.Minute, gpulease.Options{HostRAMWaitGiB: 32.8}, &reserveDeviceFlags{cards: "1", vramGiB: 4, ramGiB: 0}, "")
	if word(auto, "--ram") != "0" || word(auto, "--ram-wait") != "32.8" {
		t.Errorf("an allocated request defers its figure the same way: %v", auto)
	}
	plain := holdArgs("media", time.Hour, time.Minute, gpulease.Options{Devices: []string{"gpu-aaaa0000-x"}, HostRAMGiB: 30}, nil, "")
	if got := word(plain, "--ram-wait"); got != "<absent>" {
		t.Errorf("a lease that does not unload the seat has nothing to wait for after it: --ram-wait %s (%v)", got, plain)
	}
}

// THE HOLDER ITSELF, on a host the seat holds at 95 of 100 GiB. Given --ram 0 --ram-wait 30 it takes its cards and
// declares nothing (the parent drains, unloads and asks the host afterwards); given the figure as --ram it is held
// back by the grant, as it always was, which is why the parent must not pass it that way.
func TestTheDetachedHolderOfAnUnloadSeatLeaseTakesItsCardsAndComparesNothing(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	useSeatBackedHost(t, func() bool { return true }, 95, 40)

	args := holdArgs("media", 3*time.Second, 0, gpulease.Options{Reason: "film", Devices: []string{boxCard2}, HostRAMWaitGiB: 30}, nil, cfg)
	done := make(chan error, 1)
	go func() { done <- runGPUHold(args[2:]) }()
	ls := waitForLeases(t, m, 1)
	if ls[0].HostRAMGiB != 0 || ls[0].HostRAMWaitGiB != 30 {
		t.Errorf("the holder's record declares nothing and carries the wait: host_ram %.1f, wait %.1f", ls[0].HostRAMGiB, ls[0].HostRAMWaitGiB)
	}
	releaseAll(t, m)
	if err := <-done; err != nil {
		t.Fatalf("the holder takes its cards although the host reads 95 of 100 GiB: %v", err)
	}

	// Control: the same figure declared in the grant is refused by it, on this host, so the argv above is the whole fix.
	declared := holdArgs("media", 3*time.Second, 0, gpulease.Options{Reason: "film", Devices: []string{boxCard2}, HostRAMGiB: 30}, nil, cfg)
	err := runGPUHold(declared[2:])
	var short *gpulease.ErrHostRAM
	if !errors.As(err, &short) {
		t.Fatalf("a holder that declares 30 GiB in its grant is held back by the host, got %v", err)
	}
	releaseAll(t, m)
}

// What the wait after the unload says and returns, with a fake lease in place of the real one.
func TestAwaitHostRAMHeldTellsOnceAndAdmits(t *testing.T) {
	short := &gpulease.ErrHostRAM{}
	short.Why = "waiting for host RAM: needs 30.0 GiB, committed 90.0 of 100.0 GiB physical, 8.0 GiB headroom"
	var gotDeadline time.Time
	await := func(o gpulease.AwaitHostRAMOptions) error {
		gotDeadline = o.Deadline
		o.OnWait(short)
		return nil
	}
	deadline := time.Now().Add(time.Hour)
	var out bytes.Buffer
	if err := awaitHostRAMHeld(context.Background(), await, deadline, time.Hour, &out); err != nil {
		t.Fatalf("an admitted wait returns nil: %v", err)
	}
	if !gotDeadline.Equal(deadline) {
		t.Errorf("the deadline reaches the wait: %v", gotDeadline)
	}
	text := out.String()
	for _, want := range []string{short.Why, "the seat is cleared and the cards are held", "the host has the room"} {
		if !strings.Contains(text, want) {
			t.Errorf("the wait says %q:\n%s", want, text)
		}
	}
	if n := strings.Count(text, short.Why); n != 1 {
		t.Errorf("the shortage is told once, not per poll: %d times", n)
	}

	// A host that has the room at the first look says nothing at all.
	out.Reset()
	if err := awaitHostRAMHeld(context.Background(), func(gpulease.AwaitHostRAMOptions) error { return nil }, deadline, time.Hour, &out); err != nil || out.Len() != 0 {
		t.Errorf("an immediate admission is silent: err=%v out=%q", err, out.String())
	}
}

// A wait that ends on its deadline comes back as the refusal the grant would have given, with the flag that waits longer;
// an impossible figure and a cancelled wait pass through as what they are.
func TestAwaitHostRAMHeldReturnsTheGrantsRefusalWithTheFlagThatWaitsLonger(t *testing.T) {
	short := &gpulease.ErrHostRAM{}
	short.Why = "waiting for host RAM: needs 30.0 GiB, committed 90.0 of 100.0 GiB physical, 8.0 GiB headroom"
	var out bytes.Buffer
	err := awaitHostRAMHeld(context.Background(), func(gpulease.AwaitHostRAMOptions) error { return short }, time.Now(), 4*time.Second, &out)
	var got *gpulease.ErrHostRAM
	if !errors.As(err, &got) {
		t.Fatalf("the refusal stays an *ErrHostRAM: %v", err)
	}
	for _, want := range []string{short.Why, "--wait 4s", "pass a longer --wait"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal says %q: %v", want, err)
		}
	}
	if strings.Contains(out.String(), "the host has the room") {
		t.Errorf("a refused wait does not claim an admission:\n%s", out.String())
	}

	impossible := &gpulease.ErrHostRAM{}
	impossible.Why, impossible.Impossible = "can never admit 99.0 GiB: pass --ram", true
	if err := awaitHostRAMHeld(context.Background(), func(gpulease.AwaitHostRAMOptions) error { return impossible }, time.Now(), time.Minute, &out); !errors.Is(err, impossible) {
		t.Errorf("an impossible figure is returned as it is: %v", err)
	}
	if err := awaitHostRAMHeld(context.Background(), func(gpulease.AwaitHostRAMOptions) error { return gpulease.ErrCancelled }, time.Now(), time.Minute, &out); !errors.Is(err, gpulease.ErrCancelled) {
		t.Errorf("a cancelled wait stays ErrCancelled so the caller can say which signal ended it: %v", err)
	}
}

// The wait after the unload runs for the rest of the queue budget, measured from when the reserve began queueing, and
// never under the drain floor: a caller that asked to fail fast (--wait 0) still gets a real window for the memory the
// unload frees to come back.
func TestHostRAMDeadlineIsTheRestOfTheQueueBudgetNeverUnderTheDrainFloor(t *testing.T) {
	queued := time.Now().Add(-time.Hour)
	if d := hostRAMDeadline(queued, 8*time.Hour); d.Sub(queued) < 7*time.Hour+59*time.Minute || d.Sub(queued) > 8*time.Hour+time.Minute {
		t.Errorf("an 8h budget spent one hour leaves seven: deadline %s after queueing", d.Sub(queued))
	}
	if left := time.Until(hostRAMDeadline(time.Now(), 0)); left < drainFloor-time.Second {
		t.Errorf("--wait 0 still gets the drain floor %s, got %s", drainFloor, left)
	}
	if left := time.Until(hostRAMDeadline(time.Now().Add(-9*time.Hour), 8*time.Hour)); left < drainFloor-time.Second {
		t.Errorf("a budget the drain spent whole still leaves the floor %s, got %s", drainFloor, left)
	}
}

// `gpu status` while a lease holds its cards and has not been admitted: the figure it asked for is on the row and in the
// text, and the lease declares nothing yet (the next grant counts nothing for it).
func TestGPUStatusShowsHostRAMAskedAndNotYetAdmitted(t *testing.T) {
	t.Run("card-scoped", func(t *testing.T) {
		cfg, m := scopedLeaseFixture(t)
		useQuietStatus(t, statusCards(), nil)
		useHostCommit(t, newHostCommit(10)) // the host the final admission reads: roomy, whatever box runs the test
		l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, Devices: []string{boxCard2}, HostRAMWaitGiB: 30})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Release() }()
		doc := statusJSON(t, cfg)
		leases, _ := doc["leases"].([]any)
		if len(leases) != 1 {
			t.Fatalf("one lease row: %v", doc["leases"])
		}
		row := leases[0].(map[string]any)
		if row["host_ram_wait_gib"] != 30.0 {
			t.Errorf("the row carries what the lease asked for: %v", row)
		}
		if _, declared := row["host_ram_gib"]; declared {
			t.Errorf("a lease that has not been admitted declares nothing: %v", row)
		}
		out := captureStdout(t, func() {
			if err := runGPUStatus([]string{"--config", cfg}); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, "host RAM 30.0 GiB asked, not yet admitted") {
			t.Errorf("the text status says the lease waits for the host:\n%s", out)
		}
		if err := l.AwaitHostRAM(gpulease.AwaitHostRAMOptions{}); err != nil {
			t.Fatalf("the test host has the room: %v", err)
		}
		doc = statusJSON(t, cfg)
		row = doc["leases"].([]any)[0].(map[string]any)
		if row["host_ram_gib"] != 30.0 {
			t.Errorf("once admitted the lease declares its need: %v", row)
		}
		if _, waiting := row["host_ram_wait_gib"]; waiting {
			t.Errorf("an admitted lease waits for nothing: %v", row)
		}
	})
	t.Run("whole node", func(t *testing.T) {
		cfg, m := scopedLeaseFixture(t)
		useQuietStatus(t, statusCards(), nil)
		l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, HostRAMWaitGiB: 30})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Release() }()
		out := captureStdout(t, func() {
			if err := runGPUStatus([]string{"--config", cfg}); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, "host RAM 30.0 GiB asked, not yet admitted") {
			t.Errorf("the text status says the whole-node lease waits for the host:\n%s", out)
		}
	})
}
