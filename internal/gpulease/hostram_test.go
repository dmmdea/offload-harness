package gpulease

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The host-RAM term of the grant (hostram.go). The numbers follow the incident the term exists for:
// a box with 100 GiB of RAM (round, so the sums read at a glance), 8 GiB of headroom, and jobs that
// stream tens of GiB from RAM into a card that cannot hold them.

// fakeHost is a host whose memory a test moves, and the processes below a holder, which hold
// whatever the test says they hold.
type fakeHost struct {
	mu       sync.Mutex
	mem      gpuprobe.HostMemory
	memOK    bool
	reads    atomic.Int32
	heldByPD map[int]float64
}

func newFakeHost(commit float64) *fakeHost {
	return &fakeHost{mem: gpuprobe.HostMemory{PhysicalGiB: 100, AvailableGiB: 100 - commit/2, CommitUsedGiB: commit, CommitLimitGiB: 160}, memOK: true, heldByPD: map[int]float64{}}
}

func (f *fakeHost) setCommit(commit float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mem.CommitUsedGiB = commit
}

func (f *fakeHost) setHeld(pid int, gib float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heldByPD[pid] = gib
}

// arm installs the fake on m: memory reader and the holder-memory reader.
func (f *fakeHost) arm(m *Manager) {
	m.SetHostRAMHeadroom(8)
	m.hostMem = func() (gpuprobe.HostMemory, bool) {
		f.reads.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.mem, f.memOK
	}
	m.workload = func(pid int) (float64, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.heldByPD[pid], true
	}
}

func ramScoped(t *testing.T, commit float64) (*Manager, *fakeHost) {
	t.Helper()
	m := scopedRealClock(t)
	f := newFakeHost(commit)
	f.arm(m)
	return m, f
}

func asHostRAM(t *testing.T, err error) *ErrHostRAM {
	t.Helper()
	var short *ErrHostRAM
	if !errors.As(err, &short) {
		t.Fatalf("want *ErrHostRAM, got %T: %v", err, err)
	}
	return short
}

func epochNow(t *testing.T, m *Manager) uint64 {
	t.Helper()
	var n uint64
	if err := m.withEpochLock(func() error {
		var rerr error
		n, rerr = m.readEpoch()
		return rerr
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// A card-scoped grant is refused when the host cannot take what it declares, with the line the
// operator reads, and the refusal leaves nothing behind: no record, no claim, no epoch spent.
func TestACardScopedGrantIsRefusedWhenTheHostCannotTakeItsDeclaredNeed(t *testing.T) {
	m, _ := ramScoped(t, 70) // committed 70 of 100: 70 + 30 = 100 > 100 - 8
	before := epochNow(t, m)
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "krea2", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30})
	short := asHostRAM(t, err)
	want := "waiting for host RAM: needs 30.0 GiB, committed 70.0 of 100.0 GiB physical, 8.0 GiB headroom"
	if err.Error() != want {
		t.Fatalf("refusal text\n got: %s\nwant: %s", err.Error(), want)
	}
	if short.Impossible {
		t.Fatal("a shortage that waiting can cure is not impossible")
	}
	if got := epochNow(t, m); got != before {
		t.Fatalf("a refused grant spent an epoch (%d -> %d)", before, got)
	}
	if recs, claims := recordFiles(t, m), claimFiles(t, m); len(recs) != 0 || len(claims) != 0 {
		t.Fatalf("a refused grant left records %v claims %v", recs, claims)
	}
	if info := m.Inspect(); info.Held {
		t.Fatalf("a refused grant holds the card: %+v", info)
	}
}

// The same rule on the whole-node path, and it is checked before an epoch is spent there too.
func TestAWholeNodeGrantIsRefusedWhenTheHostCannotTakeItsDeclaredNeed(t *testing.T) {
	m, _ := ramScoped(t, 70)
	before := epochNow(t, m)
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "whole node", TTL: time.Hour, HostRAMGiB: 30})
	asHostRAM(t, err)
	if got := epochNow(t, m); got != before {
		t.Fatalf("a refused whole-node grant spent an epoch (%d -> %d)", before, got)
	}
	if exists(m.metaPath()) {
		t.Fatal("a refused whole-node grant left a claim")
	}
}

// A grant that fits is admitted, and the declared need rides the record: the lease directory is how
// the next grant (and `gpu status`) learns what this one is still to load.
func TestAGrantThatFitsIsAdmittedAndCarriesItsDeclaredNeed(t *testing.T) {
	m, _ := ramScoped(t, 40)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "krea2", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30})
	if err != nil {
		t.Fatalf("40 committed + 30 declared is under 92: %v", err)
	}
	defer func() { _ = l.Release() }()
	info := m.InspectFor([]string{card0})
	if !info.Held || info.HostRAMGiB != 30 {
		t.Fatalf("the record must carry the declared need: %+v", info)
	}
	raw, rerr := os.ReadFile(epochRecordPath(m.leaseDir(), l.Epoch()))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var rec Meta
	if json.Unmarshal(raw, &rec) != nil || rec.HostRAMGiB != 30 {
		t.Fatalf("e/<epoch>.json host_ram_gib = %v, want 30 (%s)", rec.HostRAMGiB, raw)
	}
	// Whole-node leases carry it in meta.json.
	m2, _ := ramScoped(t, 40)
	w, err := m2.TryAcquire(ClassMedia, Options{Reason: "whole", TTL: time.Hour, HostRAMGiB: 12})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Release() }()
	if got := m2.Inspect().HostRAMGiB; got != 12 {
		t.Fatalf("a whole-node record must carry the declared need, got %v", got)
	}
}

// A lease that declares nothing is never read against the host: it adds no memory, so a box that is
// over is not made to wait for it, and the host's counters are not even read.
func TestALeaseThatDeclaresNoNeedIsNeverReadAgainstTheHost(t *testing.T) {
	m, f := ramScoped(t, 400) // far over
	l, err := m.TryAcquire(ClassText, Options{Reason: "text", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatalf("a lease that declares no host RAM must be admitted on an over-committed host: %v", err)
	}
	_ = l.Release()
	w, err := m.TryAcquire(ClassText, Options{Reason: "whole", TTL: time.Hour})
	if err != nil {
		t.Fatalf("whole-node, no declared need: %v", err)
	}
	_ = w.Release()
	if n := f.reads.Load(); n != 0 {
		t.Fatalf("a lease that declares nothing read the host %d times", n)
	}
}

// THE POINT OF THE PENDING TERM. Lease A was granted for 40 GiB and has loaded none of it, so the
// commit charge does not show it yet. Without the not-yet-loaded part, lease B reads 20 committed,
// sees room for 40 more, and both go: the incident, replayed. With it, B is refused until A's
// memory is either in the commit charge (and A's tree holds it) or A is gone.
func TestTheNotYetLoadedPartOfAGrantedLeaseCounts(t *testing.T) {
	m, f := ramScoped(t, 20)
	a, err := m.TryAcquire(ClassMedia, Options{Reason: "lane a", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 40})
	if err != nil {
		t.Fatalf("lane a fits on an idle box: %v", err)
	}
	defer func() { _ = a.Release() }()

	_, err = m.TryAcquire(ClassMedia, Options{Reason: "lane b", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 40})
	short := asHostRAM(t, err)
	if short.PendingGiB != 40 {
		t.Fatalf("lane a has loaded nothing, so all 40 GiB are still to load, got %.1f", short.PendingGiB)
	}
	if !strings.Contains(err.Error(), "+40.0 GiB still to load by leases already running") {
		t.Fatalf("the refusal must say how much of the running leases is still to load: %v", err)
	}

	// Lane a's processes now hold 25 of its 40 GiB; the commit charge includes them (45), and 15 GiB
	// are still to load: 45 + 40 + 15 = 100 > 92.
	f.setCommit(45)
	f.setHeld(os.Getpid(), 25)
	_, err = m.TryAcquire(ClassMedia, Options{Reason: "lane b", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 40})
	if short = asHostRAM(t, err); short.PendingGiB != 15 {
		t.Fatalf("pending = declared 40 - held 25 = 15, got %.1f", short.PendingGiB)
	}

	// Lane a has loaded all of it: nothing pending, but its 40 GiB are in the commit charge (60):
	// 60 + 40 = 100 > 92, still refused.
	f.setCommit(60)
	f.setHeld(os.Getpid(), 40)
	_, err = m.TryAcquire(ClassMedia, Options{Reason: "lane b", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 40})
	if short = asHostRAM(t, err); short.PendingGiB != 0 {
		t.Fatalf("a fully loaded lease has nothing pending, got %.1f", short.PendingGiB)
	}

	// A smaller second lane fits once the first is fully loaded: 60 + 20 = 80 <= 92.
	b, err := m.TryAcquire(ClassMedia, Options{Reason: "small lane", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 20})
	if err != nil {
		t.Fatalf("20 GiB fits beside a loaded 40: %v", err)
	}
	_ = b.Release()
}

// Two grants on different cards, racing, cannot both take the last headroom: the check and the
// record that carries the declared need are one critical section. Exactly one of four racers fits
// (commit 50 + 30 = 80 <= 92, a second would be 50 + 30 + 30 = 110), whichever the scheduler picks,
// round after round.
func TestConcurrentGrantsOnDifferentCardsCannotBothTakeTheLastHeadroom(t *testing.T) {
	cards := []string{card0, card1, card2, card3}
	for round := 0; round < 25; round++ {
		m, _ := ramScoped(t, 50)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var granted, refused atomic.Int32
		leases := make(chan *Lease, len(cards))
		for _, c := range cards {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				l, err := m.TryAcquire(ClassMedia, Options{Reason: "racer", TTL: time.Hour, Devices: []string{c}, HostRAMGiB: 30})
				switch {
				case err == nil:
					granted.Add(1)
					leases <- l
				default:
					var short *ErrHostRAM
					if errors.As(err, &short) {
						refused.Add(1)
					} else {
						t.Errorf("round %d: unexpected error %v", round, err)
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		close(leases)
		for l := range leases {
			_ = l.Release()
		}
		if granted.Load() != 1 || refused.Load() != 3 {
			t.Fatalf("round %d: %d granted and %d refused; exactly one of four may take the last headroom", round, granted.Load(), refused.Load())
		}
	}
}

// A request that waits keeps its place in the SAME line, marked as waiting for host RAM; a request
// behind it for the same cards stays behind it; the line is told once; and it is granted the moment
// the host has room.
func TestAcquireKeepsAHostRAMWaiterInTheSameLineAndGrantsWhenTheHostRecovers(t *testing.T) {
	m, f := ramScoped(t, 80) // 80 + 30 > 92
	var told atomic.Int32
	type result struct {
		l   *Lease
		err error
	}
	done := make(chan result, 1)
	go func() {
		l, err := m.Acquire(ClassMedia, Options{
			Reason: "krea2 lane", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30, Wait: 20 * time.Second, WaitOut: true,
			OnHostRAMWait: func(e *ErrHostRAM) {
				told.Add(1)
				if !strings.Contains(e.Error(), "waiting for host RAM") {
					t.Errorf("the one line it prints is the refusal text, got %q", e.Error())
				}
			},
		})
		done <- result{l, err}
	}()

	deadline := time.Now().Add(5 * time.Second)
	var rec Waiter
	for time.Now().Before(deadline) {
		if ws := m.Waiters(); len(ws) == 1 && ws[0].WaitingFor == WaitHostRAM {
			rec = ws[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec.WaitingFor != WaitHostRAM || rec.HostRAMGiB != 30 {
		t.Fatalf("the waiter's record must say it waits on host RAM and what it declared, got %+v", rec)
	}

	// A later request for the same card, which declares nothing and so needs no RAM, is still behind it.
	time.Sleep(5 * time.Millisecond)
	_, err := m.Acquire(ClassMedia, Options{Reason: "behind", TTL: time.Hour, Devices: []string{card0}})
	if !errors.Is(err, ErrStillQueued) {
		t.Fatalf("a request behind the host-RAM waiter for the same card must queue behind it, got %v", err)
	}
	// A later request for ANOTHER card is not held back by a waiter that wants other cards.
	other, err := m.Acquire(ClassMedia, Options{Reason: "other card", TTL: time.Hour, Devices: []string{card1}})
	if err != nil {
		t.Fatalf("a waiter on card0 is no reason to wait on card1: %v", err)
	}
	_ = other.Release()

	f.setCommit(40) // the host recovers
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("granted when the host recovered, got %v", r.err)
		}
		defer func() { _ = r.l.Release() }()
		if got := m.InspectFor([]string{card0}).HostRAMGiB; got != 30 {
			t.Fatalf("the lease it was finally granted carries its need, got %v", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the waiter was never granted after the host recovered")
	}
	if n := told.Load(); n != 1 {
		t.Fatalf("the wait is announced once per request, not per tick; announced %d times", n)
	}
}

// --wait 0 is one gated attempt: it refuses at once with the host-RAM text, not with a queue message.
func TestAcquireWithNoWaitRefusesWithTheHostRAMText(t *testing.T) {
	m, _ := ramScoped(t, 80)
	var told atomic.Int32
	_, err := m.Acquire(ClassMedia, Options{Reason: "krea2", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30,
		OnHostRAMWait: func(*ErrHostRAM) { told.Add(1) }})
	asHostRAM(t, err)
	if want := "waiting for host RAM: needs 30.0 GiB, committed 80.0 of 100.0 GiB physical, 8.0 GiB headroom"; err.Error() != want {
		t.Fatalf("refusal text\n got: %s\nwant: %s", err.Error(), want)
	}
	if told.Load() != 0 {
		t.Fatal("a request that does not wait has no wait to announce")
	}
	if len(m.Waiters()) != 0 {
		t.Fatal("a refused request leaves no waiter record behind")
	}
}

// A need no state of this box admits ends the request at once, however long it was willing to wait.
func TestAnImpossibleNeedEndsTheRequestAtOnce(t *testing.T) {
	m, _ := ramScoped(t, 10)
	start := time.Now()
	_, err := m.Acquire(ClassMedia, Options{Reason: "too big", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 95, Wait: time.Minute, WaitOut: true})
	short := asHostRAM(t, err)
	if !short.Impossible {
		t.Fatalf("95 GiB on a 100 GiB box with 8 GiB of headroom can never be admitted: %+v", short)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("an impossible need waited %s instead of ending at once", time.Since(start))
	}
	if !strings.Contains(err.Error(), "--ram") || !strings.Contains(err.Error(), "gpu_host_ram_headroom_gib") {
		t.Fatalf("the refusal must say what to change: %v", err)
	}
}

// The host's memory unreadable, on a platform that has a reader, makes a lease that declares a need
// wait (it cannot be shown to fit); the wait is the same host-RAM wait.
func TestAnUnreadableHostMakesADeclaringLeaseWait(t *testing.T) {
	if !gpuprobe.HostMemorySupported {
		t.Skip("this platform has no host memory reader, so the rule cannot judge and admits")
	}
	m, f := ramScoped(t, 10)
	f.mu.Lock()
	f.memOK = false
	f.mu.Unlock()
	_, err := m.TryAcquire(ClassMedia, Options{Reason: "krea2", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 20})
	if short := asHostRAM(t, err); !short.Unreadable {
		t.Fatalf("want an unreadable refusal, got %+v", short)
	}
}

// The additive field: absent on a record from before it existed, omitted when zero.
func TestHostRAMOnTheRecordIsAdditive(t *testing.T) {
	var old Meta
	if err := json.Unmarshal([]byte(`{"epoch":7,"class":"media","holder":{"pid":1,"start_time_ms":2},"acquired_at_ms":3,"expires_at_ms":4,"renewed_at_ms":5}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.HostRAMGiB != 0 || infoFrom(&old, time.Now()).HostRAMGiB != 0 {
		t.Fatal("a record without the field declares nothing")
	}
	b, _ := json.Marshal(&old)
	if strings.Contains(string(b), "host_ram_gib") {
		t.Fatalf("a zero declaration must not appear on the record: %s", b)
	}
	old.HostRAMGiB = 12.5
	b, _ = json.Marshal(&old)
	if !strings.Contains(string(b), `"host_ram_gib":12.5`) {
		t.Fatalf("a declared need must be written: %s", b)
	}
}

// pendingGiB pools leases that share a holder (the pipeline's one server process holds one lease per
// card, and all their runners are below it), never goes below zero, and counts the whole declared
// need of a holder whose memory cannot be read.
func TestPendingPoolsLeasesByHolder(t *testing.T) {
	held := func(m map[int]float64, unreadable ...int) func(int) (float64, bool) {
		bad := map[int]bool{}
		for _, p := range unreadable {
			bad[p] = true
		}
		return func(pid int) (float64, bool) { return m[pid], !bad[pid] }
	}
	live := []Info{
		{Epoch: 1, PID: 100, HostRAMGiB: 20},
		{Epoch: 2, PID: 100, HostRAMGiB: 20}, // same holder: the runners below pid 100 are both jobs
		{Epoch: 3, PID: 200, HostRAMGiB: 10},
		{Epoch: 4, PID: 300, HostRAMGiB: 0}, // declares nothing
		{Epoch: 5, PID: 400, HostRAMGiB: 8}, // holder unreadable
	}
	got := pendingGiB(live, held(map[int]float64{100: 25, 200: 30}, 400))
	// holder 100: 40 declared - 25 held = 15; holder 200: 10 - 30 -> 0; holder 400 unreadable: all 8.
	if got != 15+0+8 {
		t.Fatalf("pending = %.1f, want 23", got)
	}
	if pendingGiB(nil, held(nil)) != 0 {
		t.Fatal("no leases, nothing pending")
	}
}

// HostRAMPending is that arithmetic over the leases live in the directory, the figure the card
// allocator's pre-filter and `gpu status` read.
func TestHostRAMPendingReadsTheLiveLeases(t *testing.T) {
	m, f := ramScoped(t, 20)
	a, err := m.TryAcquire(ClassMedia, Options{Reason: "a", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Release() }()
	if got := m.HostRAMPending(); got != 30 {
		t.Fatalf("nothing loaded: pending = %.1f, want 30", got)
	}
	f.setHeld(os.Getpid(), 12)
	if got := m.HostRAMPending(); got != 18 {
		t.Fatalf("12 of 30 loaded: pending = %.1f, want 18", got)
	}
	if got := DeclaredHostRAMGiB(m.Leases()); got != 30 {
		t.Fatalf("declared = %.1f, want 30", got)
	}
}
