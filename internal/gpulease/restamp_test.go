package gpulease

import (
	"strings"
	"testing"
	"time"
)

// Restamp flips the holder's own flags in place: draining -> exclusive once a
// drain completes (0.117.0). The epoch never moves, the claim never stops
// existing, and a reader sees the new flags through the one inspection path.
func TestRestampTurnsADrainingLeaseExclusiveWithoutMovingTheEpoch(t *testing.T) {
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassText, Options{Reason: "arm B", Draining: true, Command: "bash bench.sh --arm B", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	before := m.Inspect()
	if !before.Held || !before.Draining || before.Exclusive || before.Command != "bash bench.sh --arm B" {
		t.Fatalf("record at acquire: %+v", before)
	}
	if err := l.Restamp(func(meta *Meta) { meta.Draining = false; meta.Exclusive = true; meta.Epoch = 999 }); err != nil {
		t.Fatal(err)
	}
	after := m.Inspect()
	if !after.Held || after.Draining || !after.Exclusive || after.Epoch != before.Epoch || after.Command != before.Command || after.Reason != "arm B" {
		t.Fatalf("record after restamp: %+v (before %+v)", after, before)
	}
	if err := l.Check(); err != nil {
		t.Fatalf("the holder must still own the lease after a restamp: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if m.Inspect().Held {
		t.Fatal("release after restamp left the lease held")
	}
}

// A restamp from a fenced-out epoch is refused and changes nothing.
func TestRestampRefusesAFencedOutEpoch(t *testing.T) {
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassText, Options{Reason: "live", Draining: true, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	err = m.Restamp(l.Epoch()+1, func(meta *Meta) { meta.Exclusive = true })
	if err == nil || !strings.Contains(err.Error(), "fenced out") {
		t.Fatalf("restamp with a stale epoch must be refused, got %v", err)
	}
	if info := m.Inspect(); info.Exclusive || !info.Draining {
		t.Fatalf("a refused restamp must change nothing: %+v", info)
	}
}

// Draining is recorded for EITHER class (2026-09-22): `gpu reserve --class media
// --drain` must cordon new runs while the runs in flight finish, exactly as a text
// drain does. Dropping the stamp on a media lease — the behaviour this test used to
// pin — made the media class fence those runs from acquire, so the drain waited on
// work it was itself blocking. The command is clipped either way.
func TestDrainingIsRecordedForEitherClassAndTheCommandIsClipped(t *testing.T) {
	m, _ := newTestManager(t)
	long := strings.Repeat("x", 500)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "render", Draining: true, Command: long})
	if err != nil {
		t.Fatal(err)
	}
	if info := m.Inspect(); !info.Draining || info.Exclusive || len([]rune(info.Command)) > commandClip {
		t.Fatalf("media lease: %+v", info)
	}
	_ = l.Release()
}

// InspectDirDetail tells a stale record apart from a free card — the one thing
// Inspect's zero Info cannot say.
func TestInspectDirDetailReportsAStaleRecord(t *testing.T) {
	m, now := newTestManager(t)
	// The detail reader uses the PLATFORM process seams (it has no Manager), so
	// the record must carry no start-time identity and liveness is stubbed.
	m.procStart = func(int) (int64, bool) { return 0, false }
	pidAliveFn = func(int) bool { return true }
	t.Cleanup(func() { pidAliveFn = func(pid int) bool { return pidAliveImpl(pid) } })
	l, err := m.TryAcquire(ClassText, Options{Reason: "gone", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_ = l // never released: the holder "dies"
	dir := m.leaseDir()
	if info, meta, stale := inspectDirDetailAt(dir, *now, m.heartbeatTTL); !info.Held || meta == nil || stale {
		t.Fatalf("live lease misread: held=%v meta=%v stale=%v", info.Held, meta != nil, stale)
	}
	// The holder pid is gone: provably reclaimable, record still on disk.
	pidAliveFn = func(int) bool { return false }
	later := now.Add(2 * time.Hour)
	info, meta, stale := inspectDirDetailAt(dir, later, m.heartbeatTTL)
	if info.Held || meta == nil || !stale || meta.Reason != "gone" {
		t.Fatalf("stale record misread: held=%v meta=%+v stale=%v", info.Held, meta, stale)
	}
	if free, meta, stale := inspectDirDetailAt(t.TempDir(), later, m.heartbeatTTL); free.Held || meta != nil || stale {
		t.Fatalf("an empty dir must read free with no record: %v %v %v", free.Held, meta, stale)
	}
}
