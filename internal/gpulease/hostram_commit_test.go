package gpulease

// The two quantities of the host-RAM grant (hostram.go, gpuprobe/hostmemory.go): the working set a lease TOUCHES is admitted
// against the memory that is available, and the commit it is CHARGED against the OS commit limit. A lease declares them
// apart (Options.HostRAMGiB and Options.HostRAMCommitGiB); a lease that declares one figure declares it for both.
//
// The host these tests stand in for: 100 GiB of RAM, a 160 GiB commit limit (a 60 GiB page file), 8 GiB of headroom, so
// the commit bound is 152 GiB and a touched set must leave 8 GiB of the memory that is available.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func commitHostMem(available, commitUsed float64) gpuprobe.HostMemory {
	return gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: available, CommitUsedGiB: commitUsed, CommitLimitGiB: 160}
}

func commitScoped(t *testing.T, mem gpuprobe.HostMemory) (*Manager, *fakeHost) {
	t.Helper()
	m := scopedRealClock(t)
	f := newFakeHostMem(mem)
	f.arm(m)
	return m, f
}

// THE POINT OF THE CHANGE, in the grant. 10 GiB touched fits the 80 available; 60 GiB of commit does not fit the commit
// limit (130 + 60 = 190 > 152). Refused on commit alone, with the commit sentence, and nothing is left behind.
func TestAGrantIsRefusedOnItsCommitAlthoughItsTouchedSetFits(t *testing.T) {
	m, _ := commitScoped(t, commitHostMem(80, 130))
	before := epochNow(t, m)
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "coder arm", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 10, HostRAMCommitGiB: 60})
	short := asHostRAM(t, err)
	if !short.CommitShort || short.TouchShort || short.Impossible {
		t.Fatalf("want a refusal on commit alone, got commitShort=%v touchShort=%v impossible=%v (%s)", short.CommitShort, short.TouchShort, short.Impossible, err)
	}
	want := "waiting for host RAM: needs 60.0 GiB of commit, committed 130.0 of 160.0 GiB commit limit, 8.0 GiB headroom"
	if err.Error() != want {
		t.Fatalf("refusal text\n got: %s\nwant: %s", err.Error(), want)
	}
	if got := epochNow(t, m); got != before {
		t.Fatalf("a refused grant spent an epoch (%d -> %d)", before, got)
	}
	if recs, claims := recordFiles(t, m), claimFiles(t, m); len(recs) != 0 || len(claims) != 0 {
		t.Fatalf("a refused grant left records %v claims %v", recs, claims)
	}
}

// And the other direction: the commit fits, the touched set does not fit what is available.
func TestAGrantIsRefusedOnItsTouchedSetAlthoughItsCommitFits(t *testing.T) {
	m, _ := commitScoped(t, commitHostMem(20, 40))
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "krea2", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 15, HostRAMCommitGiB: 5})
	short := asHostRAM(t, err)
	if !short.TouchShort || short.CommitShort {
		t.Fatalf("want a refusal on the touched set alone, got touchShort=%v commitShort=%v (%s)", short.TouchShort, short.CommitShort, err)
	}
	want := "waiting for host RAM: needs 15.0 GiB, only 20.0 GiB of 100.0 GiB physical is available, 8.0 GiB headroom"
	if err.Error() != want {
		t.Fatalf("refusal text\n got: %s\nwant: %s", err.Error(), want)
	}
}

// Both fit: admitted, and the record carries BOTH declarations (the card record and the whole-node record).
func TestAGrantThatFitsBothConditionsCarriesBothDeclarations(t *testing.T) {
	m, _ := commitScoped(t, commitHostMem(80, 40))
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "coder arm", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 10, HostRAMCommitGiB: 60})
	if err != nil {
		t.Fatalf("10 touched of 80 available and 60 committed on 40 of a 152 bound fit: %v", err)
	}
	defer func() { _ = l.Release() }()
	info := m.InspectFor([]string{card0})
	if info.HostRAMGiB != 10 || info.HostRAMCommitGiB != 60 {
		t.Fatalf("the record must carry both declarations: touched %.1f commit %.1f", info.HostRAMGiB, info.HostRAMCommitGiB)
	}
	raw, rerr := os.ReadFile(epochRecordPath(m.leaseDir(), l.Epoch()))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var rec Meta
	if json.Unmarshal(raw, &rec) != nil || rec.HostRAMGiB != 10 || rec.HostRAMCommitGiB != 60 {
		t.Fatalf("e/<epoch>.json host_ram_gib = %v, host_ram_commit_gib = %v, want 10 and 60 (%s)", rec.HostRAMGiB, rec.HostRAMCommitGiB, raw)
	}

	m2, _ := commitScoped(t, commitHostMem(80, 40))
	w, err := m2.TryAcquire(ClassMedia, Options{Reason: "whole", TTL: time.Hour, HostRAMGiB: 12, HostRAMCommitGiB: 50})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Release() }()
	if got := m2.Inspect(); got.HostRAMGiB != 12 || got.HostRAMCommitGiB != 50 {
		t.Fatalf("a whole-node record must carry both declarations, got touched %.1f commit %.1f", got.HostRAMGiB, got.HostRAMCommitGiB)
	}
}

// A lease that declares one figure declares it for both, and its record does not repeat it. A record from before the field
// reads the same way.
func TestACommitLeftUnsetIsTheTouchedFigure(t *testing.T) {
	m, _ := commitScoped(t, commitHostMem(80, 40))
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "one figure", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if info := m.InspectFor([]string{card0}); info.HostRAMGiB != 30 || info.HostRAMCommitGiB != 30 {
		t.Fatalf("a lease that declared one figure declared it for both: touched %.1f commit %.1f", info.HostRAMGiB, info.HostRAMCommitGiB)
	}
	raw, _ := os.ReadFile(epochRecordPath(m.leaseDir(), l.Epoch()))
	if strings.Contains(string(raw), "host_ram_commit_gib") {
		t.Fatalf("a commit equal to the touched figure is not written: %s", raw)
	}

	var old Meta
	if err := json.Unmarshal([]byte(`{"epoch":7,"class":"media","holder":{"pid":1,"start_time_ms":2},"acquired_at_ms":3,"expires_at_ms":4,"renewed_at_ms":5,"host_ram_gib":12,"host_ram_wait_gib":9}`), &old); err != nil {
		t.Fatal(err)
	}
	if info := infoFrom(&old, time.Now()); info.HostRAMCommitGiB != 12 || info.HostRAMWaitCommitGiB != 9 {
		t.Fatalf("a record without the commit fields reads as one figure: commit %.1f, wait commit %.1f", info.HostRAMCommitGiB, info.HostRAMWaitCommitGiB)
	}
	old.HostRAMCommitGiB, old.HostRAMWaitCommitGiB = 40, 33
	if info := infoFrom(&old, time.Now()); info.HostRAMGiB != 12 || info.HostRAMCommitGiB != 40 || info.HostRAMWaitGiB != 9 || info.HostRAMWaitCommitGiB != 33 {
		t.Fatalf("both declarations are read: %+v", info)
	}
	b, _ := json.Marshal(&old)
	if !strings.Contains(string(b), `"host_ram_commit_gib":40`) || !strings.Contains(string(b), `"host_ram_wait_commit_gib":33`) {
		t.Fatalf("both commit fields are written when declared: %s", b)
	}
}

// The default is applied where the lease is granted, so a caller that never heard of the commit (the media admission, the
// seat warm-back) is judged on commit as strictly as on the touched figure: 70 committed + 30 declared is over a bound of 92.
func TestACallerThatDeclaresOneFigureIsStillJudgedOnCommit(t *testing.T) {
	m, _ := ramScoped(t, 70) // limit == physical here: the bound is 92
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "krea2", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30})
	if short := asHostRAM(t, err); !short.CommitShort || short.CommitNeedGiB != 30 {
		t.Fatalf("a single declared figure of 30 is a commit of 30: %+v (%s)", short.HostRAMCheck, err)
	}
	chk := m.HostRAMCheck(30)
	if chk.OK || !chk.CommitShort {
		t.Fatalf("the read-only check applies the same default: %+v", chk)
	}
}

// What a running lease has yet to be CHARGED is its declared commit less the PRIVATE bytes of its processes (not the
// working set): the commit charge now contains what they already hold privately, nothing more.
func TestWhatARunningLeaseHasYetToBeChargedCountsAgainstTheCommitLimit(t *testing.T) {
	m, f := commitScoped(t, commitHostMem(80, 40))
	a, err := m.TryAcquire(ClassMedia, Options{Reason: "lane a", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 10, HostRAMCommitGiB: 100})
	if err != nil {
		t.Fatalf("lane a fits an idle box: %v", err)
	}
	defer func() { _ = a.Release() }()
	lane := func(commit float64) Options {
		return Options{Reason: "lane b", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 5, HostRAMCommitGiB: commit}
	}

	// Nothing of lane a has loaded: 100 GiB of commit are still to be charged. 40 + 20 + 100 = 160 > 152.
	_, err = m.TryAcquire(ClassMedia, lane(20))
	short := asHostRAM(t, err)
	if !short.CommitShort || short.TouchShort || short.CommitPendingGiB != 100 || short.PendingGiB != 10 {
		t.Fatalf("pending: touched %.1f commit %.1f; shortfalls touch=%v commit=%v (%s)", short.PendingGiB, short.CommitPendingGiB, short.TouchShort, short.CommitShort, err)
	}
	if !strings.Contains(err.Error(), "+100.0 GiB still to load by leases already running") {
		t.Fatalf("the refusal says how much commit is still to be charged: %v", err)
	}
	if got := m.HostRAMPendingCommit(); got != 100 {
		t.Fatalf("HostRAMPendingCommit = %.1f, want 100", got)
	}

	// Lane a's processes now hold 10 resident (all of its touched set) but only 20 private: the charge includes those 20 (60),
	// 80 are still to be charged. Counting the working set instead would leave 90.
	f.setCommit(60)
	f.setHeld(os.Getpid(), 10)
	f.setHeldPrivate(os.Getpid(), 20)
	_, err = m.TryAcquire(ClassMedia, lane(20)) // 60 + 20 + 80 = 160 > 152
	if short = asHostRAM(t, err); short.CommitPendingGiB != 80 || short.PendingGiB != 0 {
		t.Fatalf("pending after the load: touched %.1f commit %.1f, want 0 and 80", short.PendingGiB, short.CommitPendingGiB)
	}
	if got, want := m.HostRAMPending(), 0.0; got != want {
		t.Fatalf("HostRAMPending = %.1f, want %.1f (the working set is all loaded)", got, want)
	}
	if got := m.HostRAMPendingCommit(); got != 80 {
		t.Fatalf("HostRAMPendingCommit = %.1f, want 80", got)
	}

	// Lane a has been charged all of it: nothing pending, and a smaller lane fits (140 + 10 = 150 <= 152).
	f.setCommit(140)
	f.setHeldPrivate(os.Getpid(), 100)
	b, err := m.TryAcquire(ClassMedia, lane(10))
	if err != nil {
		t.Fatalf("10 GiB of commit fits beside a fully charged lane: %v", err)
	}
	_ = b.Release()
}

// The commit condition is applied only where the OS enforces the limit.
func TestASoftCommitLimitIsNotAppliedByTheGrant(t *testing.T) {
	mem := commitHostMem(80, 400)
	m, _ := commitScoped(t, mem)
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "hard", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 10, HostRAMCommitGiB: 60})
	if short := asHostRAM(t, err); !short.CommitShort {
		t.Fatalf("a hard limit refuses 460 against a bound of 152: %+v", short.HostRAMCheck)
	}
	mem.CommitLimitSoft = true
	m2, _ := commitScoped(t, mem)
	l, err := m2.TryAcquire(ClassMedia, Options{Reason: "soft", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 10, HostRAMCommitGiB: 60})
	if err != nil {
		t.Fatalf("a soft limit is not applied, and 80 available leaves room for 10: %v", err)
	}
	_ = l.Release()
}

// A commit no state of the box can admit ends the request at once, in the grant and in the read-only check.
func TestACommitAboveTheBoundIsImpossible(t *testing.T) {
	m, _ := commitScoped(t, commitHostMem(80, 10))
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "too big", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 10, HostRAMCommitGiB: 155})
	if short := asHostRAM(t, err); !short.Impossible || !short.CommitShort || !strings.Contains(err.Error(), "--ram-commit") {
		t.Fatalf("155 GiB of commit can never fit 160 less 8: %+v (%s)", short.HostRAMCheck, err)
	}
	if chk := m.HostRAMCheckLoad(gpuprobe.HostLoad{TouchGiB: 10, CommitGiB: 155}); chk.OK || !chk.Impossible {
		t.Fatalf("the read-only check agrees: %+v", chk)
	}
}

// A lease that declares only a commit is a lease that declares: it is judged, not skipped.
func TestACommitOnlyDeclarationIsStillJudged(t *testing.T) {
	m, f := commitScoped(t, commitHostMem(80, 130))
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "commit only", TTL: time.Hour, Devices: []string{card0}, HostRAMCommitGiB: 60})
	if short := asHostRAM(t, err); !short.CommitShort {
		t.Fatalf("a declaration of commit alone is refused when the commit does not fit: %+v", short.HostRAMCheck)
	}
	if f.reads.Load() == 0 {
		t.Fatal("a lease that declares a commit reads the host")
	}
	f.setCommit(40)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "commit only", TTL: time.Hour, Devices: []string{card0}, HostRAMCommitGiB: 60})
	if err != nil {
		t.Fatalf("it fits once the commit charge is down: %v", err)
	}
	defer func() { _ = l.Release() }()
	if info := m.InspectFor([]string{card0}); info.HostRAMGiB != 0 || info.HostRAMCommitGiB != 60 || !info.DeclaresHostRAM() {
		t.Fatalf("the record declares a commit and no touched set: %+v", info)
	}
}

// The read-only check takes both figures; the single-figure form declares one for both.
func TestHostRAMCheckLoadPutsBothFigures(t *testing.T) {
	m, _ := commitScoped(t, commitHostMem(80, 130))
	if chk := m.HostRAMCheckLoad(gpuprobe.HostLoad{TouchGiB: 10, CommitGiB: 60, CommitStated: true}); chk.OK || !chk.CommitShort || chk.TouchShort {
		t.Fatalf("10 touched fits, 60 committed over 130 does not: %+v", chk)
	}
	if chk := m.HostRAMCheckLoad(gpuprobe.HostLoad{TouchGiB: 10, CommitGiB: 10, CommitStated: true}); !chk.OK {
		t.Fatalf("10 and 10 fit: %+v", chk)
	}
	if chk := m.HostRAMCheck(30); chk.OK || !chk.CommitShort || chk.CommitStated {
		t.Fatalf("one figure of 30 is a commit of 30, not stated, held to physical RAM (130 + 30 > 92): %+v", chk)
	}
}

// The lease that clears the seat first: BOTH figures wait on the record, the await puts both to the one rule, and
// admission moves both into the declared fields in one step.
func TestADeferredLeaseCarriesBothFiguresThroughTheAwait(t *testing.T) {
	m, f := commitScoped(t, commitHostMem(80, 130))
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 10, HostRAMWaitCommitGiB: 60})
	if err != nil {
		t.Fatalf("a lease that waits for the host after it has cleared the seat is granted its cards: %v", err)
	}
	defer func() { _ = l.Release() }()
	rec := recordOf(t, m, l.Epoch())
	if rec.HostRAMGiB != 0 || rec.HostRAMCommitGiB != 0 || rec.HostRAMWaitGiB != 10 || rec.HostRAMWaitCommitGiB != 60 {
		t.Fatalf("the record declares nothing yet and carries both waits: %+v", rec)
	}

	err = l.AwaitHostRAM(AwaitHostRAMOptions{})
	if short := asHostRAM(t, err); !short.CommitShort || short.TouchShort {
		t.Fatalf("the await refuses on commit alone (130 + 60 > 152): %+v (%s)", short.HostRAMCheck, err)
	}
	if rec = recordOf(t, m, l.Epoch()); rec.HostRAMWaitGiB != 10 || rec.HostRAMWaitCommitGiB != 60 || rec.HostRAMGiB != 0 {
		t.Fatalf("a refused await changes nothing: %+v", rec)
	}

	f.setCommit(40) // the seat is gone
	if err := l.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
		t.Fatalf("the host has the room: %v", err)
	}
	rec = recordOf(t, m, l.Epoch())
	if rec.HostRAMGiB != 10 || rec.HostRAMCommitGiB != 60 || rec.HostRAMWaitGiB != 0 || rec.HostRAMWaitCommitGiB != 0 {
		t.Fatalf("an admitted lease declares both figures and waits for nothing: %+v", rec)
	}
}

// A deferred lease whose wait carries a commit only (no touched set) is still awaited.
func TestADeferredCommitOnlyFigureIsAwaited(t *testing.T) {
	m, f := commitScoped(t, commitHostMem(80, 130))
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitCommitGiB: 60})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if short := asHostRAM(t, l.AwaitHostRAM(AwaitHostRAMOptions{})); !short.CommitShort {
		t.Fatalf("the commit is awaited: %+v", short.HostRAMCheck)
	}
	f.setCommit(40)
	if err := l.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec := recordOf(t, m, l.Epoch()); rec.HostRAMCommitGiB != 60 || rec.HostRAMWaitCommitGiB != 0 {
		t.Fatalf("admitted: %+v", rec)
	}
}
