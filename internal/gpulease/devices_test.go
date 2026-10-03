package gpulease

// Card-scoped leases (lease record v2), plan P2.
//
// A device lease names its cards by GPU UUID, writes e/<epoch>.json and one
// cards/<uuid>.claim per card, and never writes meta.json. A whole-node lease keeps the
// legacy meta.json as its real record. Two leases conflict when either is whole-node or
// their card sets intersect. Device ids in these tests are synthetic.

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	card0 = "gpu-test-0"
	card1 = "gpu-test-1"
	card2 = "gpu-test-2"
	card3 = "gpu-test-3"
)

// scopedManager is newTestManager with the device writer enabled and a fake clock.
func scopedManager(t *testing.T) (*Manager, *time.Time) {
	t.Helper()
	m, now := newTestManager(t)
	m.SetCardScoped(true)
	return m, now
}

// scopedRealClock is realClockManager with the device writer enabled.
func scopedRealClock(t *testing.T) *Manager {
	t.Helper()
	m := realClockManager(t)
	m.SetCardScoped(true)
	return m
}

// asPID is a view of m that acts as another process (same root, same clock).
func asPID(m *Manager, pid int) *Manager {
	c := *m
	c.pid = pid
	return &c
}

// deadPIDs makes exactly the listed pids read as gone; every other pid is alive.
func deadPIDs(t *testing.T, dead ...int) {
	t.Helper()
	set := map[int]bool{}
	for _, p := range dead {
		set[p] = true
	}
	orig := pidAliveFn
	pidAliveFn = func(pid int) bool { return !set[pid] }
	t.Cleanup(func() { pidAliveFn = orig })
}

func mustDevices(t *testing.T, m *Manager, devs ...string) *Lease {
	t.Helper()
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "test", Devices: devs, TTL: time.Hour})
	if err != nil {
		t.Fatalf("acquire %v: %v", devs, err)
	}
	return l
}

func epochsOf(infos []Info) []uint64 {
	var out []uint64
	for _, i := range infos {
		out = append(out, i.Epoch)
	}
	return out
}

func claimFiles(t *testing.T, m *Manager) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(m.leaseDir(), "cards"))
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		b, rerr := os.ReadFile(filepath.Join(m.leaseDir(), "cards", e.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		out[strings.TrimSuffix(e.Name(), ".claim")] = strings.TrimSpace(string(b))
	}
	return out
}

func recordFiles(t *testing.T, m *Manager) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(m.leaseDir(), "e"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// ---- the writer is flag-gated ------------------------------------------------------

func TestDeviceWriteRefusedWhenFlagOff(t *testing.T) {
	m, _ := newTestManager(t) // flag off: the default
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "x", Devices: []string{card0}})
	if !errors.Is(err, ErrCardScopedOff) {
		t.Fatalf("a device lease on a host with card-scoped leases off must be refused with ErrCardScopedOff, got %v", err)
	}
	if _, err := m.Acquire(ClassMedia, Options{Reason: "x", Devices: []string{card0}, Wait: 5 * time.Second}); !errors.Is(err, ErrCardScopedOff) {
		t.Fatalf("Acquire must refuse at once, not queue for a lease it can never write: %v", err)
	}
	if got := recordFiles(t, m); len(got) != 0 {
		t.Fatalf("refused acquisition left records behind: %v", got)
	}
	if exists(filepath.Join(m.leaseDir(), "cards")) && len(claimFiles(t, m)) != 0 {
		t.Fatal("refused acquisition left card claims behind")
	}
	if exists(m.metaPath()) {
		t.Fatal("refused acquisition wrote meta.json")
	}
	if exists(filepath.Join(m.gpuDir(), "FORMAT")) {
		t.Fatal("a refused device write must not mark the directory FORMAT=2")
	}
	if exists(m.waitersDir()) {
		t.Fatal("a refused Acquire registered as a waiter: it would sit in other waiters' line for a lease that can never be written")
	}
}

func TestFlagOffWholeNodeRecordCarriesNoV2Fields(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "render"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(m.metaPath())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"devices", "group", "state", "wrapper_version"} {
		if _, ok := raw[k]; ok {
			t.Errorf("whole-node record carries v2 key %q with nothing set: %s", k, b)
		}
	}
	if exists(filepath.Join(m.leaseDir(), "e")) || exists(filepath.Join(m.leaseDir(), "cards")) {
		t.Error("a whole-node acquisition created the v2 directories")
	}
}

func TestWholeNodeRecordCarriesWrapperVersionAndGroupWhenGiven(t *testing.T) {
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "r", WrapperVersion: " 1.2.3 ", Group: "g1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	meta, err := m.readMeta()
	if err != nil || meta.WrapperVersion != "1.2.3" || meta.Group != "g1" || len(meta.Devices) != 0 {
		t.Fatalf("record = %+v, %v", meta, err)
	}
	if info := m.Inspect(); info.WrapperVersion != "1.2.3" || info.Group != "g1" {
		t.Fatalf("Inspect drops the v2 fields: %+v", info)
	}
}

func TestErrHeldNamesTheCardsOfADeviceLease(t *testing.T) {
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card1, card0)
	defer func() { _ = l.Release() }()
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "x", Devices: []string{card0}})
	if err == nil || !strings.Contains(err.Error(), "cards "+card0+","+card1) {
		t.Fatalf("the refusal must say which cards are held, got %v", err)
	}
}

func TestEmptyDevicesMeansWholeNode(t *testing.T) {
	m, _ := scopedManager(t)
	for _, devs := range [][]string{nil, {}} {
		l, err := m.TryAcquire(ClassMedia, Options{Reason: "whole", Devices: devs})
		if err != nil {
			t.Fatalf("whole-node acquire with Devices=%#v: %v", devs, err)
		}
		if !exists(m.metaPath()) {
			t.Fatal("a whole-node lease must write the legacy meta.json as its real record")
		}
		if len(recordFiles(t, m)) != 0 {
			t.Fatal("a whole-node lease wrote a v2 record")
		}
		if info := m.Inspect(); len(info.Devices) != 0 {
			t.Fatalf("whole-node lease reports devices %v", info.Devices)
		}
		_ = l.Release()
	}
}

func TestBlankDeviceIsAnErrorNotAWholeNodeLease(t *testing.T) {
	m, _ := scopedManager(t)
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "x", Devices: []string{" "}}); err == nil {
		t.Fatal("a blank device id must be refused, not quietly widened to the whole node")
	}
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "x", Devices: []string{"../evil"}}); err == nil {
		t.Fatal("a device id that is not a plain token must be refused (it names a file)")
	}
}

func TestDeviceIdsAreNormalisedSoOneCardHasOneClaimFile(t *testing.T) {
	m, _ := scopedManager(t)
	l := mustDevices(t, m, "GPU-TEST-1", " gpu-test-0 ", "gpu-test-1")
	defer func() { _ = l.Release() }()
	got := claimFiles(t, m)
	if len(got) != 2 || got[card0] == "" || got[card1] == "" {
		t.Fatalf("claims = %v, want exactly gpu-test-0 and gpu-test-1", got)
	}
	// The same card in another spelling conflicts with itself.
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "x", Devices: []string{"GPU-TEST-0"}}); !errors.As(err, new(*ErrHeld)) {
		t.Fatalf("a different spelling of a held card must conflict, got %v", err)
	}
}

// ---- conflict rule -----------------------------------------------------------------

func TestDisjointDeviceLeasesCoexist(t *testing.T) {
	m, _ := scopedManager(t)
	a := mustDevices(t, m, card0)
	b := mustDevices(t, m, card1)
	if a.Epoch() == b.Epoch() {
		t.Fatal("two leases share an epoch")
	}
	for _, l := range []*Lease{a, b} {
		if err := l.Check(); err != nil {
			t.Fatalf("epoch %d: %v", l.Epoch(), err)
		}
	}
	leases := m.Leases()
	if got := epochsOf(leases); len(got) != 2 || got[0] != a.Epoch() || got[1] != b.Epoch() {
		t.Fatalf("live leases = %v, want [%d %d]", got, a.Epoch(), b.Epoch())
	}
	if exists(m.metaPath()) {
		t.Fatal("a device lease must never write meta.json (no synthetic umbrella record)")
	}
	if got := claimFiles(t, m); got[card0] == "" || got[card1] == "" || len(got) != 2 {
		t.Fatalf("claims = %v", got)
	}
	// A card nobody holds is still free.
	c := mustDevices(t, m, card2)
	_ = c.Release()
}

func TestOverlappingDeviceLeasesQueue(t *testing.T) {
	m, _ := scopedManager(t)
	a := mustDevices(t, m, card0, card1)
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "b", Devices: []string{card1, card2}})
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("overlapping request must be ErrHeld, got %v", err)
	}
	if held.Info.Epoch != a.Epoch() || len(held.Info.Devices) != 2 {
		t.Fatalf("ErrHeld should name the conflicting lease (epoch %d, 2 cards), got %+v", a.Epoch(), held.Info)
	}
	// Nothing of the refused request leaks: card2 stays free.
	if got := claimFiles(t, m); got[card2] != "" {
		t.Fatalf("a refused grant left a claim on %s", card2)
	}
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	b := mustDevices(t, m, card1, card2)
	_ = b.Release()
}

func TestWholeNodeConflictsWithAnyDeviceLease(t *testing.T) {
	m, _ := scopedManager(t)
	a := mustDevices(t, m, card0)
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "whole"}); !errors.As(err, new(*ErrHeld)) {
		t.Fatalf("a whole-node request must wait for a device lease, got %v", err)
	}
	if exists(m.metaPath()) {
		t.Fatal("the refused whole-node request left its claim (meta.json) behind")
	}
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	w, err := m.TryAcquire(ClassMedia, Options{Reason: "whole"})
	if err != nil {
		t.Fatal(err)
	}
	// And the other way round: a held whole-node lease fences every device request.
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "b", Devices: []string{card3}}); !errors.As(err, new(*ErrHeld)) {
		t.Fatalf("a device request must wait for a whole-node lease, got %v", err)
	}
	_ = w.Release()
}

// The whole-node path and the device path arbitrate on different tokens (meta.json and
// the card claims). Raced against each other they must still never both win.
func TestWholeNodeAndDeviceGrantsNeverBothWinConcurrently(t *testing.T) {
	m := scopedRealClock(t)
	for round := 0; round < 150; round++ {
		var start, done sync.WaitGroup
		start.Add(1)
		var wholeErr, devErr error
		var whole, dev *Lease
		done.Add(2)
		go func() {
			defer done.Done()
			start.Wait()
			whole, wholeErr = m.TryAcquire(ClassMedia, Options{Reason: "whole"})
		}()
		go func() {
			defer done.Done()
			start.Wait()
			dev, devErr = m.TryAcquire(ClassMedia, Options{Reason: "dev", Devices: []string{card0}})
		}()
		start.Done()
		done.Wait()
		if wholeErr == nil && devErr == nil {
			t.Fatalf("round %d: a whole-node lease and a device lease were both granted", round)
		}
		for _, e := range []error{wholeErr, devErr} {
			if e != nil && !errors.As(e, new(*ErrHeld)) {
				t.Fatalf("round %d: unexpected error %v", round, e)
			}
		}
		if whole != nil {
			_ = whole.Release()
		}
		if dev != nil {
			_ = dev.Release()
		}
		if exists(m.metaPath()) {
			t.Fatalf("round %d: meta.json left behind", round)
		}
	}
}

// ---- the fence is per epoch --------------------------------------------------------

func TestSecondDeviceLeaseSurvivesFirstEpochRelease(t *testing.T) {
	m, _ := scopedManager(t)
	a := mustDevices(t, m, card0)
	b := mustDevices(t, m, card1)
	if err := b.Renew(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(m.leaseDir(), "unloaded."+itoa(b.Epoch()))
	if err := os.WriteFile(marker, []byte("1"), 0o666); err != nil {
		t.Fatal(err)
	}
	hbB := m.heartbeatPath(b.Epoch())
	if !exists(hbB) {
		t.Fatal("setup: no heartbeat for the second lease")
	}

	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	if err := b.Check(); err != nil {
		t.Fatalf("releasing the FIRST lease fenced out the second: %v", err)
	}
	if err := b.Renew(); err != nil {
		t.Fatalf("second lease could not renew after the first released: %v", err)
	}
	if !exists(hbB) {
		t.Fatal("releasing one lease deleted another lease's heartbeat (clearUnloadMarkers sweeps every hb.*)")
	}
	if !exists(marker) {
		t.Fatal("releasing one lease deleted another lease's unload marker")
	}
	if got := epochsOf(m.Leases()); len(got) != 1 || got[0] != b.Epoch() {
		t.Fatalf("live leases after the first released = %v, want [%d]", got, b.Epoch())
	}
	if info := m.Inspect(); !info.Held || !info.HoldsEpoch(b.Epoch()) {
		t.Fatalf("Inspect lost the surviving lease: %+v", info)
	}
	_ = b.Release()
}

// The lowest epoch is only the PRIMARY of a directory; a higher-epoch lease must pass
// every fence a lower one passes.
func TestHigherEpochDeviceLeaseIsNeverFencedByALowerOne(t *testing.T) {
	m, _ := scopedManager(t)
	low := mustDevices(t, m, card0)
	high := mustDevices(t, m, card1)
	if high.Epoch() <= low.Epoch() {
		t.Fatalf("setup: epochs %d, %d", low.Epoch(), high.Epoch())
	}
	for i := 0; i < 3; i++ { // the wrapper's 15 s tick: Check then Renew, repeatedly
		if err := high.Renew(); err != nil {
			t.Fatalf("tick %d: higher epoch fenced out by the lower: %v", i, err)
		}
	}
	if !m.EpochIsCurrent(high.Epoch()) || !m.EpochIsCurrent(low.Epoch()) {
		t.Fatal("EpochIsCurrent must pass for every live epoch")
	}
	if m.EpochIsCurrent(high.Epoch() + 7) {
		t.Fatal("EpochIsCurrent passed for an epoch nobody holds")
	}
	info := m.Inspect()
	if !info.HoldsEpoch(high.Epoch()) || !info.HoldsEpoch(low.Epoch()) || info.HoldsEpoch(high.Epoch()+7) {
		t.Fatalf("Info.HoldsEpoch disagrees with the live set: %+v", info)
	}
	if info.Epoch != low.Epoch() {
		t.Fatalf("the primary epoch is the lowest live one: got %d want %d", info.Epoch, low.Epoch())
	}
}

func TestCheckIsFencedWhenACardClaimIsStolenOrGone(t *testing.T) {
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0, card1)
	claim := filepath.Join(m.leaseDir(), "cards", card1+".claim")

	if err := os.WriteFile(claim, []byte(`{"epoch":999999}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("a claim that names another epoch must fence the lease out, got %v", err)
	}
	if err := l.Renew(); err == nil {
		t.Fatal("Renew must fail once fenced")
	}
	_ = os.Remove(claim)
	if err := l.Check(); err == nil {
		t.Fatal("a missing card claim must fail the fence")
	}
	// Release of a fenced lease must not remove the claim that now belongs to someone else.
	if err := os.WriteFile(claim, []byte(`{"epoch":999999}`), 0o666); err != nil {
		t.Fatal(err)
	}
	_ = l.Release()
	if got := claimFiles(t, m); !strings.Contains(got[card1], "999999") {
		t.Fatalf("a fenced-out release deleted another holder's claim: %v", got)
	}
}

func TestCheckIsFencedWhenTheRecordIsGone(t *testing.T) {
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0)
	if err := os.Remove(filepath.Join(m.leaseDir(), "e", itoa(l.Epoch())+".json")); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); err == nil {
		t.Fatal("a lease whose record is gone must fail the fence")
	}
}

// ---- reading -----------------------------------------------------------------------

func TestInspectReportsADeviceOnlyDirectoryAsHeld(t *testing.T) {
	m, _ := scopedManager(t)
	if info := m.Inspect(); info.Held {
		t.Fatalf("empty dir reads held: %+v", info)
	}
	a := mustDevices(t, m, card1, card0)
	b := mustDevices(t, m, card2)
	for name, info := range map[string]Info{"Inspect": m.Inspect()} {
		if !info.Held || info.Epoch != a.Epoch() || info.Class != ClassMedia {
			t.Fatalf("%s: %+v", name, info)
		}
		if got := strings.Join(info.Devices, ","); got != card0+","+card1 {
			t.Fatalf("%s: devices %q, want the primary's sorted cards", name, got)
		}
		if len(info.Epochs) != 2 || info.Epochs[0] != a.Epoch() || info.Epochs[1] != b.Epoch() {
			t.Fatalf("%s: epochs %v", name, info.Epochs)
		}
	}
	info, meta, reclaimable := m.InspectDetail()
	if !info.Held || meta == nil || reclaimable || meta.Epoch != a.Epoch() || meta.Holder.PID != os.Getpid() {
		t.Fatalf("InspectDirDetail: %+v %+v %v", info, meta, reclaimable)
	}
	_ = a.Release()
	_ = b.Release()
	if info := m.Inspect(); info.Held {
		t.Fatalf("released dir still reads held: %+v", info)
	}
}

func TestInspectForNarrowsToTheConflictingLease(t *testing.T) {
	m, _ := scopedManager(t)
	a := mustDevices(t, m, card0)
	b := mustDevices(t, m, card1)
	if info := m.InspectFor([]string{card1}); !info.Held || info.Epoch != b.Epoch() {
		t.Fatalf("InspectFor(card1) = %+v, want epoch %d", info, b.Epoch())
	}
	if info := m.InspectFor([]string{card2}); info.Held {
		t.Fatalf("InspectFor(card2) = %+v, want free", info)
	}
	if info := m.InspectFor(nil); !info.Held || info.Epoch != a.Epoch() {
		t.Fatalf("InspectFor(whole node) = %+v, want the lowest live lease", info)
	}
}

func TestRecordCarriesTheV2Fields(t *testing.T) {
	m, _ := scopedManager(t)
	l, err := m.TryAcquire(ClassText, Options{Reason: "bench", Devices: []string{card2, card0}, Group: "batch-7", WrapperVersion: "9.9.9", Command: "x", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	b, err := os.ReadFile(filepath.Join(m.leaseDir(), "e", itoa(l.Epoch())+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec Meta
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.State != "active" || rec.Group != "batch-7" || rec.WrapperVersion != "9.9.9" ||
		strings.Join(rec.Devices, ",") != card0+","+card2 || rec.Holder.PID != os.Getpid() || rec.Epoch != l.Epoch() {
		t.Fatalf("record = %+v", rec)
	}
	b, err = os.ReadFile(filepath.Join(m.gpuDir(), "FORMAT"))
	if err != nil || strings.TrimSpace(string(b)) != "2" {
		t.Fatalf("gpu/FORMAT = %q, %v; want 2 after the first device grant", b, err)
	}
}

func TestLegacyMetaWithoutDevicesIsWholeNode(t *testing.T) {
	m, _ := scopedManager(t)
	epoch, err := legacyTryAcquire(t, m, ClassMedia)
	if err != nil {
		t.Fatal(err)
	}
	info := m.Inspect()
	if !info.Held || info.Epoch != epoch || len(info.Devices) != 0 || !info.HoldsEpoch(epoch) {
		t.Fatalf("a legacy record (no devices field) must read as a held whole-node lease: %+v", info)
	}
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "dev", Devices: []string{card0}}); !errors.As(err, new(*ErrHeld)) {
		t.Fatalf("a legacy whole-node lease must fence every device request, got %v", err)
	}
	if !m.EpochIsCurrent(epoch) {
		t.Fatal("EpochIsCurrent must keep honouring a legacy lease")
	}
}

// ---- cross-version: what a pre-v2 reader makes of a v2 directory --------------------

// THE HAZARD, PINNED. There is no synthetic meta.json over a device lease (the
// umbrella is a deviation from plan rev 2 pending acceptance: see the P2 notes in
// docs/systems/gpu-lease.md), so a binary that predates this format reads a v2-only
// directory as a FREE card. What keeps that from ever mattering is the writer gate: it
// stays off on a host until the reader audit is green. The test pins both halves, so a
// change that starts writing devices by default fails here.
func TestOldReaderFixtureSeesFreeOverV2DirAndTheFlagIsWhatGatesIt(t *testing.T) {
	m, now := scopedManager(t)
	l := mustDevices(t, m, card0)
	defer func() { _ = l.Release() }()

	if info, meta, _ := legacyInspect(m.leaseDir(), *now, m.heartbeatTTL, m.procStart); info.Held || meta != nil {
		t.Fatalf("the pre-v2 reader is expected to see nothing in a v2-only directory (this is why the writer is flag-gated), got %+v", info)
	}
	// The current reader sees it.
	if !m.Inspect().Held {
		t.Fatal("the current reader must see the device lease")
	}
	// And the default writer never produces such a directory.
	off, _ := newTestManager(t)
	if _, err := off.TryAcquire(ClassMedia, Options{Reason: "x", Devices: []string{card0}}); !errors.Is(err, ErrCardScopedOff) {
		t.Fatalf("default config must not write device leases: %v", err)
	}
}

// The current reader parses what the pre-v2 writer wrote, whole-node.
func TestCurrentReaderReadsAPreV2Record(t *testing.T) {
	m, now := scopedManager(t)
	epoch, err := legacyTryAcquire(t, m, ClassText)
	if err != nil {
		t.Fatal(err)
	}
	old, _, _ := legacyInspect(m.leaseDir(), *now, m.heartbeatTTL, m.procStart)
	cur := m.Inspect()
	if !old.Held || !cur.Held || old.Epoch != cur.Epoch || old.PID != cur.PID || old.Class != cur.Class || cur.Epoch != epoch {
		t.Fatalf("old reader %+v vs current reader %+v", old, cur)
	}
	if err := legacyCheck(m.leaseDir(), epoch); err != nil {
		t.Fatal(err)
	}
}

// ---- release, reclaim, restamp ------------------------------------------------------

func TestReleaseByEpochTargetsOneLease(t *testing.T) {
	m, _ := scopedManager(t)
	a := mustDevices(t, m, card0)
	b := mustDevices(t, m, card1)
	if released, err := m.ReleaseByEpoch(a.Epoch()); err != nil || !released {
		t.Fatalf("release a: %v %v", released, err)
	}
	if err := b.Check(); err != nil {
		t.Fatalf("releasing epoch %d by number fenced out epoch %d: %v", a.Epoch(), b.Epoch(), err)
	}
	if released, err := m.ReleaseByEpoch(a.Epoch()); err != nil || released {
		t.Fatalf("releasing a released epoch must be a quiet no-op: %v %v", released, err)
	}
	c := mustDevices(t, m, card0)
	// Epoch 0 means "whatever is held": with several leases that is ambiguous, so it fails loud.
	if _, err := m.ReleaseByEpoch(0); err == nil || !strings.Contains(err.Error(), "--epoch") {
		t.Fatalf("epoch 0 with several live leases must name the epochs and refuse, got %v", err)
	}
	_ = c.Release()
	if released, err := m.ReleaseByEpoch(0); err != nil || !released {
		t.Fatalf("epoch 0 with exactly one lease releases it: %v %v", released, err)
	}
	if m.Inspect().Held {
		t.Fatal("still held")
	}
}

func TestRestampRewritesOnlyItsOwnV2Record(t *testing.T) {
	m, _ := scopedManager(t)
	a := mustDevices(t, m, card0)
	b := mustDevices(t, m, card1)
	if err := a.Restamp(func(meta *Meta) { meta.Draining = true }); err != nil {
		t.Fatal(err)
	}
	infos := m.Leases()
	if len(infos) != 2 || !infos[0].Draining || infos[1].Draining {
		t.Fatalf("restamp leaked across leases: %+v", infos)
	}
	// A restamp may change flags; it must never change WHICH cards the lease holds. A record
	// whose card set was emptied would read as a whole-node lease and fence every other card.
	if err := a.Restamp(func(meta *Meta) { meta.Devices = nil }); err != nil {
		t.Fatal(err)
	}
	if got := m.Leases(); len(got) != 2 || len(got[0].Devices) != 1 || got[0].Devices[0] != card0 {
		t.Fatalf("a restamp rewrote the lease's card set: %+v", got)
	}
	if info := m.InspectFor([]string{card2}); info.Held {
		t.Fatalf("a restamp widened a card-scoped lease to the whole node: %+v", info)
	}
	if err := m.Restamp(b.Epoch()+50, func(*Meta) {}); err == nil {
		t.Fatal("restamp of an epoch nobody holds must fail")
	}
	if err := a.Check(); err != nil {
		t.Fatalf("restamp broke the fence: %v", err)
	}
}

func TestReclaimRemovesOnlyThatLeasesClaims(t *testing.T) {
	m, _ := scopedManager(t)
	const deadPID = 7001
	deadPIDs(t)                                          // every pid reads alive while the leases are taken
	a := mustDevices(t, asPID(m, deadPID), card0, card1) // will die
	b := mustDevices(t, m, card2)
	deadPIDs(t, deadPID)

	// A reader never mutates: the dead lease reads free, its files are still there.
	if info := m.InspectFor([]string{card0}); info.Held {
		t.Fatalf("a dead holder's cards must read free: %+v", info)
	}
	if got := claimFiles(t, m); got[card0] == "" {
		t.Fatal("a read removed a claim")
	}

	// The next acquirer sweeps it: only the dead lease's claims and record go.
	c := mustDevices(t, m, card0)
	defer func() { _ = c.Release() }()
	if exists(filepath.Join(m.leaseDir(), "e", itoa(a.Epoch())+".json")) {
		t.Fatal("the dead lease's record survived the reclaim")
	}
	got := claimFiles(t, m)
	if !strings.Contains(got[card0], itoa(c.Epoch())) {
		t.Fatalf("card0 claim = %q, want the new holder's", got[card0])
	}
	if got[card1] != "" {
		t.Fatalf("the dead lease's OTHER claim (card1) survived: %v", got)
	}
	if !strings.Contains(got[card2], itoa(b.Epoch())) {
		t.Fatalf("the live sibling's claim was disturbed: %v", got)
	}
	if err := b.Check(); err != nil {
		t.Fatalf("reclaiming a dead lease fenced out a live sibling: %v", err)
	}
}

// ---- crash debris ------------------------------------------------------------------

func writeDebris(t *testing.T, m *Manager, epoch uint64, state string, at time.Time, devs ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(m.leaseDir(), "e"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(m.leaseDir(), "cards"), 0o777); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		rec, _ := json.Marshal(&Meta{Epoch: epoch, Class: ClassMedia, Holder: Holder{PID: os.Getpid(), StartTimeMs: 4242},
			AcquiredAtMs: at.UnixMilli(), ExpiresAtMs: at.Add(time.Hour).UnixMilli(), RenewedAtMs: at.UnixMilli(), Devices: devs, State: state})
		if err := os.WriteFile(filepath.Join(m.leaseDir(), "e", itoa(epoch)+".json"), rec, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range devs {
		c, _ := json.Marshal(map[string]any{"epoch": epoch, "at_ms": at.UnixMilli()})
		if err := os.WriteFile(filepath.Join(m.leaseDir(), "cards", d+".claim"), c, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.writeEpoch(epoch); err != nil {
		t.Fatal(err)
	}
}

func TestCrashMidGrantLeavesNoPermanentCardClaim(t *testing.T) {
	cases := map[string]func(t *testing.T, m *Manager, now time.Time){
		// died after writing the record, before any claim
		"record only": func(t *testing.T, m *Manager, now time.Time) {
			writeDebris(t, m, 40, "granting", now)
		},
		// died with some of its claims made
		"some claims": func(t *testing.T, m *Manager, now time.Time) {
			writeDebris(t, m, 41, "granting", now, card0)
		},
		// died after the claims but before flipping to active
		"all claims": func(t *testing.T, m *Manager, now time.Time) {
			writeDebris(t, m, 42, "granting", now, card0, card1)
		},
		// a claim whose record is gone (a release that died between its two removals)
		"claim without record": func(t *testing.T, m *Manager, now time.Time) {
			writeDebris(t, m, 43, "", now, card0, card1)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			m, clock := scopedManager(t)
			setup(t, m, *clock)

			// Inside the grace a grant may be in flight: the cards are not stolen.
			if name != "record only" {
				if _, err := m.TryAcquire(ClassMedia, Options{Reason: "n", Devices: []string{card0, card1}}); !errors.As(err, new(*ErrHeld)) {
					t.Fatalf("a fresh grant in flight must hold its cards, got %v", err)
				}
			}
			// Past the grace it is debris and the next acquirer removes it.
			*clock = clock.Add(claimGrace + time.Second)
			l, err := m.TryAcquire(ClassMedia, Options{Reason: "n", Devices: []string{card0, card1}, TTL: time.Hour})
			if err != nil {
				t.Fatalf("debris from a crashed grant held the cards past the grace: %v", err)
			}
			defer func() { _ = l.Release() }()
			if recs := recordFiles(t, m); len(recs) != 1 || recs[0] != itoa(l.Epoch())+".json" {
				t.Fatalf("records after the sweep = %v, want only the live lease", recs)
			}
			for d, c := range claimFiles(t, m) {
				if !strings.Contains(c, itoa(l.Epoch())) {
					t.Fatalf("claim %s still names a dead epoch: %s", d, c)
				}
			}
		})
	}
}

func TestStaleGrantReadsAsFreeToReaders(t *testing.T) {
	m, clock := scopedManager(t)
	writeDebris(t, m, 50, "granting", *clock, card0)
	if !m.InspectFor([]string{card0}).Held {
		t.Fatal("a fresh grant in flight conflicts")
	}
	*clock = clock.Add(claimGrace + time.Second)
	if m.InspectFor([]string{card0}).Held || m.Inspect().Held {
		t.Fatal("a stale granting record must read as free")
	}
	if len(m.Leases()) != 0 {
		t.Fatal("a stale granting record is not a live lease")
	}
}

// A failed grant rolls itself back completely.
func TestFailedGrantLeavesNothingBehind(t *testing.T) {
	m, _ := scopedManager(t)
	// Another process's claim on card1 that this acquirer cannot see in its read-only
	// probe (a record-less claim inside the grace) makes the exclusive create fail
	// part-way through the card list.
	now := m.now()
	if err := os.MkdirAll(filepath.Join(m.leaseDir(), "cards"), 0o777); err != nil {
		t.Fatal(err)
	}
	c, _ := json.Marshal(map[string]any{"epoch": 77, "at_ms": now.UnixMilli()})
	if err := os.WriteFile(filepath.Join(m.leaseDir(), "cards", card1+".claim"), c, 0o666); err != nil {
		t.Fatal(err)
	}
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "x", Devices: []string{card0, card1}})
	if !errors.As(err, new(*ErrHeld)) {
		t.Fatalf("got %v", err)
	}
	if got := claimFiles(t, m); got[card0] != "" {
		t.Fatalf("the part-made grant left a claim on card0: %v", got)
	}
	for _, r := range recordFiles(t, m) {
		t.Fatalf("the failed grant left a record: %s", r)
	}
}

// ---- the invariant: a card never has two holders --------------------------------------

func TestNoCardEverHasTwoClaims(t *testing.T) {
	m := scopedRealClock(t)
	cards := []string{card0, card1, card2, card3}
	var owner [4]atomic.Int64
	var violations atomic.Int64
	var wg sync.WaitGroup
	const workers, iters = 6, 25
	for w := 1; w <= workers; w++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			seed := uint64(id)*2654435761 + 12345
			next := func(n int) int {
				seed = seed*6364136223846793005 + 1442695040888963407
				return int((seed >> 33) % uint64(n))
			}
			for i := 0; i < iters; i++ {
				var devs []string
				var idx []int
				switch next(5) {
				case 0: // whole node
				default:
					for c := 0; c < 4; c++ {
						if next(3) == 0 {
							devs = append(devs, cards[c])
							idx = append(idx, c)
						}
					}
					if len(devs) == 0 {
						devs, idx = []string{cards[next(4)]}, nil
						for c := range cards {
							if cards[c] == devs[0] {
								idx = []int{c}
							}
						}
					}
				}
				l, err := m.Acquire(ClassMedia, Options{Reason: "w", Devices: devs, Wait: 20 * time.Second, TTL: time.Hour})
				if err != nil {
					t.Errorf("worker %d: %v", id, err)
					return
				}
				held := idx
				if len(devs) == 0 {
					held = []int{0, 1, 2, 3}
				}
				for _, c := range held {
					if !owner[c].CompareAndSwap(0, id) {
						violations.Add(1)
					}
				}
				time.Sleep(time.Duration(next(3)) * time.Millisecond)
				for _, c := range held {
					owner[c].Store(0)
				}
				if err := l.Release(); err != nil {
					t.Errorf("release: %v", err)
				}
			}
		}(int64(w))
	}
	wg.Wait()
	if v := violations.Load(); v != 0 {
		t.Fatalf("%d grants overlapped a card another lease held", v)
	}
	if left := claimFiles(t, m); len(left) != 0 {
		t.Fatalf("claims left behind: %v", left)
	}
	if exists(m.metaPath()) || len(recordFiles(t, m)) != 0 {
		t.Fatal("records left behind")
	}
}

// ---- multi-process ----------------------------------------------------------------

// TestMain lets the test binary double as the contending process.
func TestMain(m *testing.M) {
	if os.Getenv("GPULEASE_HELPER_ROOT") != "" {
		os.Exit(contendHelper())
	}
	os.Exit(m.Run())
}

func contendHelper() int {
	root := os.Getenv("GPULEASE_HELPER_ROOT")
	id := os.Getenv("GPULEASE_HELPER_ID")
	iters, _ := strconv.Atoi(os.Getenv("GPULEASE_HELPER_ITERS"))
	m := &Manager{root: root, heartbeatTTL: DefaultHeartbeatTTL, now: time.Now, sleep: time.Sleep,
		pollEvery: 2 * time.Millisecond, procStart: processStart, pid: os.Getpid()}
	m.SetCardScoped(true)
	ownersDir := filepath.Join(root, "owners")
	_ = os.MkdirAll(ownersDir, 0o777)
	cards := []string{card0, card1, card2, card3}
	seed := uint64(os.Getpid())*2654435761 + 99
	next := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int((seed >> 33) % uint64(n))
	}
	for i := 0; i < iters; i++ {
		var devs, held []string
		if next(5) != 0 {
			for _, c := range cards {
				if next(3) == 0 {
					devs = append(devs, c)
				}
			}
			if len(devs) == 0 {
				devs = []string{cards[next(4)]}
			}
			held = devs
		} else {
			held = cards
		}
		l, err := m.Acquire(ClassMedia, Options{Reason: "proc " + id, Devices: devs, Wait: 60 * time.Second, TTL: time.Hour})
		if err != nil {
			os.Stderr.WriteString("acquire: " + err.Error() + "\n")
			return 3
		}
		var made []string
		for _, c := range held {
			p := filepath.Join(ownersDir, c)
			f, oerr := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
			if oerr != nil {
				os.Stderr.WriteString("DOUBLE GRANT on " + c + " (process " + id + ")\n")
				return 4
			}
			_ = f.Close()
			made = append(made, p)
		}
		time.Sleep(time.Duration(next(3)) * time.Millisecond)
		for _, p := range made {
			_ = removeClaim(p)
		}
		if err := l.Release(); err != nil {
			os.Stderr.WriteString("release: " + err.Error() + "\n")
			return 5
		}
	}
	return 0
}

func newHelperCmd(self, root string, i int) *exec.Cmd {
	cmd := exec.Command(self, "-test.run=^$")
	cmd.Env = append(os.Environ(), "GPULEASE_HELPER_ROOT="+root, "GPULEASE_HELPER_ID="+strconv.Itoa(i), "GPULEASE_HELPER_ITERS=40")
	return cmd
}

func TestNoCardEverHasTwoClaimsAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "gpu"), 0o777); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const procs = 5
	type result struct {
		out []byte
		err error
	}
	results := make([]result, procs)
	var wg sync.WaitGroup
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := newHelperCmd(self, root, i)
			out, rerr := cmd.CombinedOutput()
			results[i] = result{out, rerr}
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		if r.err != nil {
			t.Errorf("process %d: %v\n%s", i, r.err, r.out)
		}
	}
	m := &Manager{root: root, heartbeatTTL: DefaultHeartbeatTTL, now: time.Now, procStart: processStart, pid: os.Getpid()}
	if left := claimFiles(t, m); len(left) != 0 {
		t.Fatalf("claims left behind: %v", left)
	}
	if exists(m.metaPath()) || len(recordFiles(t, m)) != 0 {
		t.Fatal("records left behind")
	}
}

// ---- FIFO ---------------------------------------------------------------------------

func TestWaiterRecordCarriesItsDevices(t *testing.T) {
	m := scopedRealClock(t)
	hold := mustDevices(t, m, card0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l, err := m.Acquire(ClassMedia, Options{Reason: "w", Devices: []string{card1, card0}, Wait: 10 * time.Second})
		if err == nil {
			_ = l.Release()
		}
	}()
	waitAllRegistered(t, m, 1, 3*time.Second)
	ws := m.Waiters()
	sort.Slice(ws, func(i, j int) bool { return ws[i].SinceMs < ws[j].SinceMs })
	if len(ws) != 1 || strings.Join(ws[0].Devices, ",") != card0+","+card1 {
		t.Fatalf("waiters = %+v", ws)
	}
	_ = hold.Release()
	<-done
}

func TestDisjointBackfillPassesAWaitingOverlap(t *testing.T) {
	m := scopedRealClock(t)
	a := mustDevices(t, m, card0)
	bDone := make(chan *Lease, 1)
	go func() {
		l, err := m.Acquire(ClassMedia, Options{Reason: "b", Devices: []string{card0}, Wait: 20 * time.Second})
		if err != nil {
			t.Errorf("b: %v", err)
		}
		bDone <- l
	}()
	waitAllRegistered(t, m, 1, 3*time.Second)

	// C wants a card nobody holds or is queued for: it must not wait behind B.
	start := time.Now()
	c, err := m.Acquire(ClassMedia, Options{Reason: "c", Devices: []string{card1}, Wait: 20 * time.Second})
	if err != nil {
		t.Fatalf("a disjoint request queued behind a waiter it does not conflict with: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("backfill took %s", d)
	}
	select {
	case <-bDone:
		t.Fatal("B was served while A still held its card")
	default:
	}
	_ = c.Release()
	_ = a.Release()
	if l := <-bDone; l != nil {
		_ = l.Release()
	}
}

func TestOverlappingWaitersAreServedInArrivalOrder(t *testing.T) {
	m := scopedRealClock(t)
	hold := mustDevices(t, m, card0, card1)
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			devs := []string{card1}
			if i == 1 {
				devs = []string{card0, card1}
			}
			l, err := m.Acquire(ClassMedia, Options{Reason: "w", Devices: devs, Wait: 20 * time.Second})
			if err != nil {
				t.Errorf("waiter %d: %v", i, err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			_ = l.Release()
		}(i)
	}
	waitAllRegistered(t, m, 3, 3*time.Second)
	_ = hold.Release()
	wg.Wait()
	for i, got := range order {
		if got != i {
			t.Fatalf("order = %v, want arrival order (each waiter conflicts with the one before it)", order)
		}
	}
}

func TestWholeNodeWaiterNotStarvedByLaterDeviceWaiters(t *testing.T) {
	m := scopedRealClock(t)
	a := mustDevices(t, m, card0)

	wholeCh := make(chan *Lease, 1)
	go func() {
		l, err := m.Acquire(ClassMedia, Options{Reason: "whole", Wait: 30 * time.Second})
		if err != nil {
			t.Errorf("whole: %v", err)
		}
		wholeCh <- l
	}()
	waitAllRegistered(t, m, 1, 3*time.Second)

	// D is disjoint from A and the card is free, but the whole-node waiter is a barrier:
	// anything that arrives after it queues behind it.
	devCh := make(chan *Lease, 1)
	go func() {
		l, err := m.Acquire(ClassMedia, Options{Reason: "dev", Devices: []string{card1}, Wait: 30 * time.Second})
		if err != nil {
			t.Errorf("dev: %v", err)
		}
		devCh <- l
	}()
	waitAllRegistered(t, m, 2, 3*time.Second)
	select {
	case l := <-devCh:
		_ = l.Release()
		t.Fatal("a later device waiter jumped a waiting whole-node request (starvation)")
	case <-time.After(400 * time.Millisecond):
	}

	_ = a.Release()
	var whole *Lease
	select {
	case whole = <-wholeCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the whole-node waiter was never served")
	}
	select {
	case l := <-devCh:
		_ = l.Release()
		t.Fatal("the device waiter was served while the whole-node lease was held")
	case <-time.After(300 * time.Millisecond):
	}
	_ = whole.Release()
	select {
	case l := <-devCh:
		_ = l.Release()
	case <-time.After(10 * time.Second):
		t.Fatal("the device waiter was never served after the whole-node lease ended")
	}
}

// The package-level readers (what gpu status, the vision gate, the fleet daemon and the
// pipeline call with only a directory) apply the same rules under the real clock and
// the real process table.
func TestPackageLevelReadersSeeEveryLiveLease(t *testing.T) {
	m := testManagerAt(t, t.TempDir())
	m.SetCardScoped(true)
	a := mustDevices(t, m, card0)
	b := mustDevices(t, m, card1)
	dir := m.leaseDir()

	info := InspectDir(dir)
	if !info.Held || info.Epoch != a.Epoch() || !info.HoldsEpoch(b.Epoch()) {
		t.Fatalf("InspectDir: %+v", info)
	}
	if got := epochsOf(InspectLeases(dir)); len(got) != 2 || got[0] != a.Epoch() || got[1] != b.Epoch() {
		t.Fatalf("InspectLeases = %v", got)
	}
	if !EpochIsCurrent(dir, a.Epoch()) || !EpochIsCurrent(dir, b.Epoch()) || EpochIsCurrent(dir, b.Epoch()+1) {
		t.Fatal("EpochIsCurrent")
	}
	_ = a.Release()
	if got := epochsOf(InspectLeases(dir)); len(got) != 1 || got[0] != b.Epoch() {
		t.Fatalf("after release: %v", got)
	}
	if EpochIsCurrent(dir, a.Epoch()) {
		t.Fatal("a released epoch must not read current")
	}
	_ = b.Release()
	if InspectDir(dir).Held || len(InspectLeases(dir)) != 0 {
		t.Fatal("still held")
	}
}

// ---- the two halves of the whole-node / device arbitration, pinned deterministically ----

// The concurrent test above races the two paths; these pin each half's decision on its
// own, by driving the in-lock step directly with the other side's claim already there.

func TestDeviceGrantRefusesUnderTheLockWhileAWholeNodeClaimIsLive(t *testing.T) {
	m, _ := scopedManager(t)
	if _, err := legacyTryAcquire(t, m, ClassMedia); err != nil { // meta.json appears AFTER any probe
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.v2Dir(), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.cardsDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	var l *Lease
	var held *ErrHeld
	err := m.withEpochLock(func() error {
		var gerr error
		l, held, gerr = m.grantDevicesLocked(ClassMedia, Options{Reason: "x", Devices: []string{card0}}, []string{card0})
		return gerr
	})
	if err != nil || l != nil || held == nil {
		t.Fatalf("a device grant must refuse while a live whole-node record exists: lease=%v held=%v err=%v", l, held, err)
	}
	if len(recordFiles(t, m)) != 0 || len(claimFiles(t, m)) != 0 {
		t.Fatal("the refused grant left files behind")
	}
}

func TestWholeNodeClaimWithdrawsWhenADeviceLeaseIsLive(t *testing.T) {
	m, now := scopedManager(t)
	dev := mustDevices(t, m, card0)
	defer func() { _ = dev.Release() }()

	plant := func(epoch uint64) {
		b, _ := json.Marshal(&Meta{Epoch: epoch, Class: ClassMedia, Holder: Holder{PID: os.Getpid(), StartTimeMs: 4242},
			AcquiredAtMs: now.UnixMilli(), ExpiresAtMs: now.Add(time.Hour).UnixMilli(), RenewedAtMs: now.UnixMilli()})
		if err := os.WriteFile(m.metaPath(), b, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	// Our own whole-node claim, made while a device lease was being granted: withdraw it.
	plant(900)
	err := m.verifyNoDeviceLeases(900)
	var held *ErrHeld
	if !errors.As(err, &held) || held.Info.Epoch != dev.Epoch() {
		t.Fatalf("verify must report the live device lease, got %v", err)
	}
	if exists(m.metaPath()) {
		t.Fatal("the whole-node claim was not withdrawn")
	}
	// A meta.json that is NOT ours (another acquirer's, epoch 901) is never removed.
	plant(901)
	if err := m.verifyNoDeviceLeases(900); !errors.As(err, &held) {
		t.Fatalf("verify: %v", err)
	}
	if !exists(m.metaPath()) {
		t.Fatal("verify removed a claim that was not ours")
	}
	_ = os.Remove(m.metaPath())
	// And with no live device lease it keeps the claim.
	_ = dev.Release()
	plant(902)
	if err := m.verifyNoDeviceLeases(902); err != nil {
		t.Fatalf("no device lease is live, the claim must stand: %v", err)
	}
	if !exists(m.metaPath()) {
		t.Fatal("verify withdrew a claim nothing conflicts with")
	}
}

// The window the deterministic test above cannot reach through TryAcquire: a device
// lease lands after a whole-node acquirer's claim and before its verification. The hook
// plants one by hand (a grant would refuse, correctly, on the claim that is already there).
func TestWholeNodeGrantWithdrawsWhenADeviceLeaseLandsAfterItsClaim(t *testing.T) {
	m, now := scopedManager(t)
	if err := os.MkdirAll(m.v2Dir(), 0o777); err != nil { // the host has had device leases before
		t.Fatal(err)
	}
	m.afterClaimHook = func() {
		writeDebris(t, m, 800, "active", *now, card1)
		if err := os.WriteFile(m.heartbeatPath(800), []byte(itoa(uint64(now.UnixMilli()))), 0o666); err != nil {
			t.Error(err)
		}
	}
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "whole"})
	var held *ErrHeld
	if !errors.As(err, &held) || held.Info.Epoch != 800 {
		t.Fatalf("a whole-node grant must back off for a device lease that appeared after its claim: %v", err)
	}
	if exists(m.metaPath()) {
		t.Fatal("the withdrawn whole-node claim (meta.json) was left behind")
	}
}

func TestEpochIsCurrentAndRestampFailOnAStolenCard(t *testing.T) {
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0)
	if !m.EpochIsCurrent(l.Epoch()) {
		t.Fatal("setup")
	}
	if err := os.WriteFile(filepath.Join(m.leaseDir(), "cards", card0+".claim"), []byte(`{"epoch":999999}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if m.EpochIsCurrent(l.Epoch()) {
		t.Fatal("a lease whose card is claimed by another epoch must not read current")
	}
	if err := l.Restamp(func(meta *Meta) { meta.Draining = true }); err == nil {
		t.Fatal("a fenced-out lease must not restamp its record")
	}
}

func TestSweepNeverTouchesALiveLeasesFilesOrAForeignClaim(t *testing.T) {
	m, clock := scopedManager(t)
	live := mustDevices(t, m, card0)
	defer func() { _ = live.Release() }()
	// An OLD claim with no record on another card is debris; the live lease's claim, though
	// equally old by the clock, is not.
	*clock = clock.Add(claimGrace + time.Hour)
	writeDebris(t, m, 61, "", clock.Add(-time.Hour), card2)
	if err := live.Renew(); err != nil {
		t.Fatal(err)
	}
	other := mustDevices(t, m, card1)
	defer func() { _ = other.Release() }()
	got := claimFiles(t, m)
	if got[card2] != "" {
		t.Fatalf("an old orphan claim survived a sweep: %v", got)
	}
	if !strings.Contains(got[card0], itoa(live.Epoch())) || !strings.Contains(got[card1], itoa(other.Epoch())) {
		t.Fatalf("a live lease's claim was removed: %v", got)
	}
	if err := live.Check(); err != nil {
		t.Fatalf("sweep fenced out a live lease: %v", err)
	}
}

// ---- review fixes (P2 fix pass) ------------------------------------------------------

// stalledDeviceLease is a device lease whose holder is ALIVE but has stopped renewing:
// a suspended or lid-closed session. Its pid answers, its window is long past and its
// heartbeat is stale, so the shared reclaim rule judges it reclaimable.
func stalledDeviceLease(t *testing.T, m *Manager, now *time.Time, devs ...string) *Lease {
	t.Helper()
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "stalled holder", Devices: devs, TTL: time.Minute})
	if err != nil {
		t.Fatalf("acquire %v: %v", devs, err)
	}
	*now = now.Add(3 * time.Hour)
	return l
}

// assertStalledHolderFenced is the property the fence exists for: after another holder
// was granted the node, the stalled holder's own Check and Renew refuse, its files are
// gone (so its Renew cannot revive a heartbeat), and Node's fence refuses too.
func assertStalledHolderFenced(t *testing.T, m *Manager, stalled *Lease, newEpoch uint64) {
	t.Helper()
	if newEpoch <= stalled.Epoch() {
		t.Fatalf("setup: the new lease (epoch %d) must outrank the stalled one (%d)", newEpoch, stalled.Epoch())
	}
	if err := stalled.Check(); err == nil {
		t.Fatalf("FENCE HOLE: a stalled device lease still passes Check() after the node was granted to epoch %d", newEpoch)
	}
	if err := stalled.Renew(); err == nil {
		t.Fatal("FENCE HOLE: a stalled device lease can still Renew() after the node was granted to someone else")
	}
	if got := recordFiles(t, m); len(got) != 0 {
		t.Fatalf("the stalled lease's record survived the whole-node grant: %v", got)
	}
	if got := claimFiles(t, m); len(got) != 0 {
		t.Fatalf("the stalled lease's card claims survived the whole-node grant: %v", got)
	}
	if exists(m.heartbeatPath(stalled.Epoch())) {
		t.Fatal("the stalled lease's heartbeat survived (a Renew would have revived it)")
	}
	if m.EpochIsCurrent(stalled.Epoch()) {
		t.Fatal("EpochIsCurrent still honours the stalled lease")
	}
	if res := runNode(t, m.leaseDir(), stalled.Epoch()); res.FencePasses {
		t.Fatal("Node's fence (checkInheritedLease) still passes for the stalled lease")
	}
}

func TestWholeNodeGrantOverAStalledDeviceLeaseFencesTheStalledHolder(t *testing.T) {
	m, now := scopedManager(t)
	stalled := stalledDeviceLease(t, m, now, card0)
	whole, err := m.TryAcquire(ClassMedia, Options{Reason: "whole node"})
	if err != nil {
		t.Fatalf("a whole-node acquirer must be granted the node over a reclaimable device lease: %v", err)
	}
	defer func() { _ = whole.Release() }()
	assertStalledHolderFenced(t, m, stalled, whole.Epoch())
	if err := whole.Check(); err != nil {
		t.Fatalf("the new whole-node holder must pass its own fence: %v", err)
	}
}

// The mirror: a whole-node WAITER (the Acquire path) whose claim lands on a stale
// whole-node record, so it reclaims first and verifies afterwards.
func TestWholeNodeWaiterThatReclaimsFencesAStalledDeviceLease(t *testing.T) {
	m, now := scopedManager(t)
	stalled := stalledDeviceLease(t, m, now, card0)
	// A pre-v2 writer sees a v2-only directory as free and takes a whole-node lease
	// (the documented cross-version hazard); it then goes stale too.
	if _, err := legacyTryAcquire(t, m, ClassMedia); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(3 * time.Hour)
	if !exists(m.metaPath()) {
		t.Fatal("setup: the legacy whole-node record is missing")
	}
	whole, err := m.Acquire(ClassMedia, Options{Reason: "whole node waiter", Wait: 2 * time.Second})
	if err != nil {
		t.Fatalf("the waiter must reclaim the stale whole-node record and be granted: %v", err)
	}
	defer func() { _ = whole.Release() }()
	assertStalledHolderFenced(t, m, stalled, whole.Epoch())
}

// A LIVE device lease is untouched by the sweep: only reclaimable ones are removed.
func TestWholeNodeBackOffLeavesALiveDeviceLeaseIntact(t *testing.T) {
	m, now := scopedManager(t)
	dev := mustDevices(t, m, card0)
	defer func() { _ = dev.Release() }()
	if err := os.WriteFile(m.metaPath(), mustJSON(t, &Meta{Epoch: 900, Class: ClassMedia, Holder: Holder{PID: os.Getpid(), StartTimeMs: 4242},
		AcquiredAtMs: now.UnixMilli(), ExpiresAtMs: now.Add(time.Hour).UnixMilli(), RenewedAtMs: now.UnixMilli()}), 0o666); err != nil {
		t.Fatal(err)
	}
	var held *ErrHeld
	if err := m.verifyNoDeviceLeases(900); !errors.As(err, &held) {
		t.Fatalf("verify: %v", err)
	}
	if err := dev.Check(); err != nil {
		t.Fatalf("the sweep inside verify fenced out a LIVE device lease: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- the whole-node arbitration must not fail open ----------------------------------

func TestWholeNodeGrantVerifiesWhenTheV2DirectoryCannotBeStatted(t *testing.T) {
	m, now := scopedManager(t)
	if err := os.MkdirAll(m.v2Dir(), 0o777); err != nil {
		t.Fatal(err)
	}
	m.statHook = func(path string) (os.FileInfo, error) {
		if path == m.v2Dir() {
			return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrPermission} // not ErrNotExist
		}
		return os.Stat(path)
	}
	m.afterClaimHook = func() {
		writeDebris(t, m, 800, "active", *now, card1)
		if err := os.WriteFile(m.heartbeatPath(800), []byte(itoa(uint64(now.UnixMilli()))), 0o666); err != nil {
			t.Error(err)
		}
	}
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "whole"})
	var held *ErrHeld
	if !errors.As(err, &held) || held.Info.Epoch != 800 {
		t.Fatalf("an unreadable e/ must trigger the verification, not skip it: a whole-node grant went through over a live device lease (%v)", err)
	}
	if exists(m.metaPath()) {
		t.Fatal("the whole-node claim was left in place")
	}
}

func TestWholeNodeGrantSkipsVerificationOnlyWhenTheV2DirectoryIsAbsent(t *testing.T) {
	m, _ := scopedManager(t)
	calls := 0
	m.statHook = func(path string) (os.FileInfo, error) {
		if path == m.v2Dir() {
			calls++
		}
		return os.Stat(path)
	}
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "whole"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if calls == 0 {
		t.Fatal("the whole-node grant never asked whether e/ exists")
	}
	if exists(m.v2Dir()) {
		t.Fatal("a whole-node grant on a host that never had device leases must not create e/")
	}
}

// ---- a failed removal is a failed release -------------------------------------------

func TestReleaseOfADeviceLeaseReportsAFailedRemovalAndCanBeRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(path string) bool
	}{
		{"record", func(p string) bool { return filepath.Base(filepath.Dir(p)) == "e" }},
		{"claim", func(p string) bool { return strings.HasSuffix(p, ".claim") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := scopedManager(t)
			l := mustDevices(t, m, card0)
			broken := true
			m.removeHook = func(p string) error {
				if broken && tc.fail(p) {
					return &os.PathError{Op: "remove", Path: p, Err: os.ErrPermission}
				}
				return removeClaim(p)
			}
			err := l.Release()
			if err == nil || !strings.Contains(err.Error(), "releasing lease") {
				t.Fatalf("a release whose %s removal failed reported success (%v): the caller believes the card is free while it is still held", tc.name, err)
			}
			broken = false
			if err := l.Release(); err != nil {
				t.Fatalf("a failed release must be retryable: %v", err)
			}
			if len(recordFiles(t, m)) != 0 || len(claimFiles(t, m)) != 0 {
				t.Fatalf("the retry left files behind: %v %v", recordFiles(t, m), claimFiles(t, m))
			}
		})
	}
}

func TestReleaseByEpochReportsAFailedRemoval(t *testing.T) {
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0)
	m.removeHook = func(p string) error {
		if strings.HasSuffix(p, ".claim") {
			return &os.PathError{Op: "remove", Path: p, Err: os.ErrPermission}
		}
		return removeClaim(p)
	}
	if released, err := m.ReleaseByEpoch(l.Epoch()); err == nil || released {
		t.Fatalf("ReleaseByEpoch swallowed a failed removal: released=%v err=%v", released, err)
	}
}

// ---- Info describes every live lease, not just the lowest ----------------------------

func TestInfoCarriesEachLiveLeaseWhenSeveralAreLive(t *testing.T) {
	m, _ := scopedManager(t)
	a := mustDevices(t, m, card0)
	defer func() { _ = a.Release() }()
	if one := m.Inspect(); len(one.Leases) != 0 || len(one.Each()) != 1 || one.Each()[0].Epoch != a.Epoch() {
		t.Fatalf("one live lease: Leases must be nil and Each() the lease itself, got %+v", one)
	}
	b, err := m.TryAcquire(ClassText, Options{Reason: "text", Devices: []string{card1}, Exclusive: true, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Release() }()

	for name, info := range map[string]Info{"Inspect": m.Inspect(), "InspectDetail": func() Info { i, _, _ := m.InspectDetail(); return i }()} {
		if info.Epoch != a.Epoch() {
			t.Fatalf("%s: the summary is the lowest epoch, got %d", name, info.Epoch)
		}
		each := info.Each()
		if len(each) != 2 {
			t.Fatalf("%s: Each() must list both live leases, got %d: %+v", name, len(each), each)
		}
		if each[0].Epoch != a.Epoch() || each[0].Class != ClassMedia || len(each[0].Devices) != 1 || each[0].Devices[0] != card0 || each[0].Exclusive {
			t.Fatalf("%s: lease A mis-described: %+v", name, each[0])
		}
		if each[1].Epoch != b.Epoch() || each[1].Class != ClassText || !each[1].Exclusive || len(each[1].Devices) != 1 || each[1].Devices[0] != card1 {
			t.Fatalf("%s: lease B mis-described: %+v", name, each[1])
		}
		for _, l := range each {
			if len(l.Leases) != 0 || !l.HoldsEpoch(l.Epoch) || len(l.Epochs) != 1 {
				t.Fatalf("%s: a per-lease Info must describe that one lease only: %+v", name, l)
			}
		}
	}
}

// ---- the writer switch needs a green reader audit (plan I7, mechanism not policy) ----

func writeAudit(t *testing.T, m *Manager, body string) {
	t.Helper()
	if err := os.MkdirAll(m.gpuDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.readerAuditPath(), []byte(body), 0o666); err != nil {
		t.Fatal(err)
	}
}

func TestApplyCardScopedConfigNeedsAGreenReaderAudit(t *testing.T) {
	devWrite := func(m *Manager) error {
		l, err := m.TryAcquire(ClassMedia, Options{Reason: "x", Devices: []string{card0}, TTL: time.Hour})
		if err == nil {
			_ = l.Release()
		}
		return err
	}

	// Config off: nothing to apply, nothing refused, writer stays off.
	m, _ := newTestManager(t)
	if err := m.ApplyCardScopedConfig(false); err != nil {
		t.Fatalf("config off must apply cleanly: %v", err)
	}
	if err := devWrite(m); !errors.Is(err, ErrCardScopedOff) {
		t.Fatalf("writer must stay off: %v", err)
	}

	// Config on, no audit: refused, and the writer stays OFF (the older readers on this host
	// have not been looked at).
	m, _ = newTestManager(t)
	err := m.ApplyCardScopedConfig(true)
	if !errors.Is(err, ErrReaderAuditMissing) {
		t.Fatalf("config on with no audit must report ErrReaderAuditMissing, got %v", err)
	}
	if !strings.Contains(err.Error(), "reader-audit.json") || !strings.Contains(err.Error(), "gpu doctor") {
		t.Fatalf("the refusal must name the marker file and `gpu doctor`: %v", err)
	}
	if err := devWrite(m); !errors.Is(err, ErrCardScopedOff) {
		t.Fatalf("a refused switch must leave the writer off, got %v", err)
	}

	// An audit that is not green, unreadable or empty is no audit.
	for name, body := range map[string]string{
		"red":     `{"result":"red"}`,
		"garbage": `not json`,
		"empty":   ``,
		"missing": `{"at":"2026-10-02"}`,
	} {
		m, _ = newTestManager(t)
		writeAudit(t, m, body)
		if err := m.ApplyCardScopedConfig(true); !errors.Is(err, ErrReaderAuditMissing) {
			t.Fatalf("audit %s must not enable the writer, got %v", name, err)
		}
		if err := devWrite(m); !errors.Is(err, ErrCardScopedOff) {
			t.Fatalf("audit %s: writer must stay off, got %v", name, err)
		}
	}

	// A green audit enables it.
	m, _ = newTestManager(t)
	writeAudit(t, m, `{"result":"green"}`)
	if err := m.ApplyCardScopedConfig(true); err != nil {
		t.Fatalf("a green audit must enable the writer: %v", err)
	}
	if err := devWrite(m); err != nil {
		t.Fatalf("writer on after a green audit: %v", err)
	}
	// Turning the config off afterwards turns the writer off whatever the marker says.
	if err := m.ApplyCardScopedConfig(false); err != nil {
		t.Fatal(err)
	}
	if err := devWrite(m); !errors.Is(err, ErrCardScopedOff) {
		t.Fatalf("config off must switch the writer off: %v", err)
	}
}
