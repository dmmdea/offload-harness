package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func fourCardTable() []gpuprobe.Card {
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 3, UUID: "GPU-dddd0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}, "")
	return cards
}

// --cards N that must wait queues on the cards that are free NOW plus the first claimed
// ones, never on claimed cards alone while an idle card sits unused.
func TestPickAutoCardsQueuesOnTheFreeCardPlusTheFirstClaimedOne(t *testing.T) {
	cards := fourCardTable()
	build := func() (gpulease.AllocInput, error) {
		return gpulease.AllocInput{Cards: cards, HostFreeOK: true, HostFreeGiB: 64,
			Claimed: map[string]bool{"gpu-aaaa0000-x": true, "gpu-bbbb0000-x": true, "gpu-cccc0000-x": true}}, nil
	}
	var out bytes.Buffer
	ids, free, err := pickAutoCards(devicePlan{Auto: true, Min: 2, Max: 2}, time.Minute, build, &out, func(time.Duration) { t.Error("a place in line is registered, not polled for") }, time.Now)
	if err != nil || free {
		t.Fatalf("no two cards are free: queue (free=%v): %v", free, err)
	}
	if got := strings.Join(ids, ","); got != "gpu-dddd0000-x,gpu-aaaa0000-x" {
		t.Fatalf("hold the free card plus the first claimed one, never two claimed cards: %s", got)
	}
}

// One card free and one claimed, two wanted: that is a place in line on both, not a poll
// that never registers one (a later --devices or whole-node request could jump it).
func TestPickAutoCardsQueuesWhenOneCardIsFreeAndOneIsClaimed(t *testing.T) {
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}, "")
	build := func() (gpulease.AllocInput, error) {
		return gpulease.AllocInput{Cards: cards, HostFreeOK: true, HostFreeGiB: 64, Claimed: map[string]bool{"gpu-cccc0000-x": true}}, nil
	}
	var out bytes.Buffer
	ids, free, err := pickAutoCards(devicePlan{Auto: true, Min: 2, Max: 2}, 10*time.Second, build, &out, func(time.Duration) { t.Error("must queue, not poll") }, time.Now)
	if err != nil || free || strings.Join(ids, ",") != "gpu-aaaa0000-x,gpu-cccc0000-x" {
		t.Fatalf("queue on both: %v free=%v %v", ids, free, err)
	}
	if !strings.Contains(out.String(), "queueing") {
		t.Errorf("say so: %q", out.String())
	}
}

// What waiting for a lease cannot fix (a display card here) is polled for and then refused
// with its reason, never queued on.
func TestPickAutoCardsPollsWhenTheShortfallIsNotALiveLease(t *testing.T) {
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16, DisplayActive: true},
	}, "")
	build := func() (gpulease.AllocInput, error) {
		return gpulease.AllocInput{Cards: cards, HostFreeOK: true, HostFreeGiB: 64}, nil
	}
	clock := time.Unix(1000, 0)
	polls := 0
	sleep := func(d time.Duration) { polls++; clock = clock.Add(d) }
	var out bytes.Buffer
	_, _, err := pickAutoCards(devicePlan{Auto: true, Min: 2, Max: 2}, 6*time.Second, build, &out, sleep, func() time.Time { return clock })
	if err == nil || !strings.Contains(err.Error(), "display") || polls == 0 {
		t.Fatalf("the display card is not a lease to wait for: polled %d times, then refused with its reason: %v", polls, err)
	}
}

// ---------------------------------------------------------------------------
// Allocate-and-claim: two reserves must not pick the same card
// ---------------------------------------------------------------------------

// The allocator reads live state; another reserve can take the card it picked before this
// one claims it. Deterministic: the first read is followed by a competitor taking the lowest
// free card, and the acquire must then re-run the allocator and land on the next card
// instead of failing or queueing behind the winner.
func TestAcquireAutoCardsRetriesWhenACompetitorWinsTheRace(t *testing.T) {
	_, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	builds := 0
	var competitor *gpulease.Lease
	build := func() (gpulease.AllocInput, error) {
		in, err := buildAllocInput(context.Background(), m, config.Config{}, reserveDeviceFlags{})
		builds++
		if builds == 1 {
			// The snapshot above is already taken; now another reserve claims the card it picked.
			l, lerr := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "competitor", Devices: []string{"gpu-aaaa0000-x"}, TTL: time.Hour})
			if lerr != nil {
				t.Fatal(lerr)
			}
			competitor = l
		}
		return in, err
	}
	var out bytes.Buffer
	lease, err := acquireAutoCards(m, gpulease.ClassMedia, gpulease.Options{Reason: "ours", TTL: time.Hour}, devicePlan{Auto: true, Min: 1, Max: 1, Source: "--cards 1"},
		0, build, &out, func(time.Duration) { t.Error("must re-pick, not poll") }, time.Now)
	if competitor != nil {
		defer func() { _ = competitor.Release() }()
	}
	if err != nil {
		t.Fatalf("a free card exists (the other non-display one): the loser must re-pick, not fail: %v", err)
	}
	defer func() { _ = lease.Release() }()
	if got := strings.Join(lease.Devices(), ","); got != "gpu-cccc0000-x" {
		t.Fatalf("the winner holds card 0, so this reserve takes card 2: %s", got)
	}
	if builds < 2 {
		t.Errorf("the allocator must have run again after the lost claim: %d reads", builds)
	}
	if strings.Contains(out.String(), "queueing") {
		t.Errorf("a card was free: no queue: %q", out.String())
	}
}

// raceTheFirstTwoAllocations makes the first two allocations in the process read the live
// state before either has claimed anything: the host-RAM read is the last of an allocation's
// reads, so holding it until a second allocation has arrived gives both the same snapshot (the
// start-up shape of a fan-out). Later allocations pass straight through. Call it after
// useCardTable, whose cleanup restores the seam.
func raceTheFirstTwoAllocations() {
	var arrived atomic.Int32
	gate := make(chan struct{})
	hostFreeRAMFn = func() (float64, bool) {
		if n := arrived.Add(1); n <= 2 {
			if n == 2 {
				close(gate)
			}
			select {
			case <-gate:
			case <-time.After(10 * time.Second):
			}
		}
		return 64, true
	}
}

// Many reserves, one card each, over free cards land on DISTINCT cards, through the real
// verb and the real lease directory. Both reserves are held after their reads of live
// state until the other has read too (the start-up shape of a fan-out), so each sees every
// card free; with --wait 0 the one that loses the claim has no queue to fall back on.
func TestGPUReserveConcurrentCardRequestsLandOnDistinctCards(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	raceTheFirstTwoAllocations()
	t.Setenv("LO_HELPER_SLEEP_MS", "2500")
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			done <- runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--cards", "1", "--wait", "0", "--reason", "fan-out"}, helperCmd()...))
		}()
	}
	leases := waitForLeases(t, m, 2)
	got := map[string]bool{}
	for _, l := range leases {
		if len(l.Devices) != 1 {
			t.Fatalf("each reserve holds exactly one card: %+v", l)
		}
		got[l.Devices[0]] = true
	}
	if len(got) != 2 || !got["gpu-aaaa0000-x"] || !got["gpu-cccc0000-x"] {
		t.Fatalf("two reserves over two free non-display cards must hold both, one each: %v", got)
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("reserve %d must not be refused while a card is free: %v", i, err)
		}
	}
}

// When no qualifying set is free the request is queued FIFO on the fixed set the
// allocator named, and takes it when the holder lets go.
func TestAcquireAutoCardsQueuesFIFOWhenNoCardIsFree(t *testing.T) {
	_, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	var holders []*gpulease.Lease
	for _, id := range []string{"gpu-aaaa0000-x", "gpu-cccc0000-x"} {
		l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "holder", Devices: []string{id}, TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		holders = append(holders, l)
	}
	go func() {
		time.Sleep(700 * time.Millisecond)
		for _, l := range holders {
			_ = l.Release()
		}
	}()
	builds := 0
	build := func() (gpulease.AllocInput, error) {
		builds++
		return buildAllocInput(context.Background(), m, config.Config{}, reserveDeviceFlags{})
	}
	var out bytes.Buffer
	start := time.Now()
	lease, err := acquireAutoCards(m, gpulease.ClassMedia, gpulease.Options{Reason: "queued", TTL: time.Hour}, devicePlan{Auto: true, Min: 1, Max: 1, Source: "--cards 1"},
		20*time.Second, build, &out, time.Sleep, time.Now)
	if err != nil {
		t.Fatalf("a busy card is a place in line: %v", err)
	}
	defer func() { _ = lease.Release() }()
	if time.Since(start) < 500*time.Millisecond {
		t.Errorf("acquired after %s, before the holders released", time.Since(start))
	}
	if !strings.Contains(out.String(), "queueing") {
		t.Errorf("say that it queued: %q", out.String())
	}
	if builds != 1 {
		t.Errorf("the allocator says no set is free: that is one read and a queue, not a claim loop (%d reads)", builds)
	}
}

// The retry loop is bounded: a view of live state that never catches up with the claims
// (here a permanently stale one) ends in the ordinary answer for a busy card, not in a spin.
func TestAcquireAutoCardsBoundsItsRetries(t *testing.T) {
	_, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	holder, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "holder", Devices: []string{"gpu-aaaa0000-x"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16}}, "")
	builds := 0
	stale := func() (gpulease.AllocInput, error) {
		builds++
		return gpulease.AllocInput{Cards: cards, HostFreeOK: true, HostFreeGiB: 64}, nil // never shows the holder's claim
	}
	var out bytes.Buffer
	_, err = acquireAutoCards(m, gpulease.ClassMedia, gpulease.Options{Reason: "ours", TTL: time.Hour}, devicePlan{Auto: true, Min: 1, Max: 1, Source: "--cards 1"},
		0, stale, &out, func(time.Duration) {}, time.Now)
	var held *gpulease.ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("a card that stays held is the ordinary busy-card answer: %v", err)
	}
	if builds != maxAutoClaimRetries+1 {
		t.Errorf("allocate, then at most %d re-allocations, then queue: %d reads", maxAutoClaimRetries, builds)
	}
}

// ---------------------------------------------------------------------------
// The detached holder allocates in its own process
// ---------------------------------------------------------------------------

// A detached `--cards` reserve cannot pick in the parent and claim in the child (the race
// again, with a wider window): the request itself travels to the holder.
func TestHoldArgsForwardAnAllocatedRequestNotAPickedSet(t *testing.T) {
	opts := gpulease.Options{Reason: "r", Origin: "o", Group: "g", Devices: []string{"gpu-aaaa0000-x"}}
	auto := holdArgs("media", time.Hour, 5*time.Minute, opts, &reserveDeviceFlags{cards: "1..2", vramGiB: 8, ramGiB: 12.5}, "cfg.json")
	joined := " " + strings.Join(auto, " ") + " "
	for _, want := range []string{" --cards 1..2 ", " --vram 8 ", " --ram 12.5 ", " --group g ", " --config cfg.json ", " --class media "} {
		if !strings.Contains(joined, want) {
			t.Errorf("the holder must be told %q: %v", want, auto)
		}
	}
	if strings.Contains(joined, "--devices") {
		t.Errorf("an allocated request must not also name cards: %v", auto)
	}
	named := holdArgs("media", time.Hour, 0, opts, nil, "")
	if joined := " " + strings.Join(named, " ") + " "; !strings.Contains(joined, " --devices gpu-aaaa0000-x ") || strings.Contains(joined, "--cards") {
		t.Errorf("a named set travels as --devices: %v", named)
	}
}

// The holder allocates and claims, and two of them over two free cards hold two cards.
func TestGPUHoldCardsAllocatesAndClaimsInTheHolder(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "")
	raceTheFirstTwoAllocations()
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			done <- runGPUHold([]string{"--config", cfg, "--class", "media", "--for", "3s", "--wait", "0", "--cards", "1", "--reason", "hold"})
		}()
	}
	leases := waitForLeases(t, m, 2)
	got := map[string]bool{}
	for _, l := range leases {
		if len(l.Devices) != 1 {
			t.Fatalf("each holder holds one card: %+v", l)
		}
		got[l.Devices[0]] = true
	}
	if len(got) != 2 {
		t.Fatalf("two holders over two free cards hold two different cards: %v", got)
	}
	releaseAll(t, m) // a holder no longer releases at its deadline (plan P9); a release is what ends it
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("holder %d: %v", i, err)
		}
	}
}
