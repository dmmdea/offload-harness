package gpulease

// F82: a lease that clears the agent seat for itself is compared to the host AFTER the clearing (Manager.AwaitHostRAM),
// by the one rule the grant applies, and the figure it waits for is not a claim until that comparison admits it.
//
// What these tests pin, and why each one is a different failure:
//
//   - the grant takes the cards without comparing the figure (it is what lets the seat be unloaded), and the record
//     carries it as a WAIT, declaring nothing;
//   - the await is the grant's comparison, not a looser one: over a table of hosts, needs and running leases it
//     admits and refuses exactly where the grant does, with the grant's sentence;
//   - the comparison and the stamp that makes the figure the lease's declared need are one critical section, so two
//     leases cannot both take the last headroom (the same property as TestConcurrentGrantsOnDifferentCards...);
//   - two leases that wait do not wait for each other: a host that fits one of them admits one, and the other is
//     admitted when the first ends (a design that kept the figure declared from the grant on would have each count the
//     other's as still to load, and admit neither);
//   - what cannot ever be admitted ends at once, and a wait that is cancelled or whose lease is gone says so.

import (
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordOf is the live record of a lease, as every other reader of the lease directory sees it.
func recordOf(t *testing.T, m *Manager, epoch uint64) Info {
	t.Helper()
	for _, l := range m.Leases() {
		if l.Epoch == epoch {
			return l
		}
	}
	t.Fatalf("no live lease with epoch %d: %+v", epoch, m.Leases())
	return Info{}
}

// THE GRANT TAKES THE CARDS THE HOST WOULD REFUSE, DECLARING NOTHING; THE AWAIT ADMITS IT WHEN THE HOST HAS THE ROOM.
func TestADeferredLeaseTakesItsCardsWhereTheHostRefusesAndIsAdmittedWhenTheHostHasTheRoom(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"card-scoped", Options{Devices: []string{card0}}},
		{"whole node", Options{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := ramScoped(t, 70) // 70 + 30 > 92: the grant itself would refuse a declared 30
			opts := tc.opts
			opts.Reason, opts.TTL, opts.HostRAMWaitGiB = "film", time.Hour, 30
			// Control: the same lease declaring its 30 GiB is refused by the grant.
			ctl := opts
			ctl.HostRAMGiB, ctl.HostRAMWaitGiB = 30, 0
			asHostRAM(t, func() error { _, err := m.TryAcquire(ClassMedia, ctl); return err }())

			l, err := m.TryAcquire(ClassMedia, opts)
			if err != nil {
				t.Fatalf("a lease that waits for the host after it has cleared the seat is granted its cards: %v", err)
			}
			defer func() { _ = l.Release() }()
			if rec := recordOf(t, m, l.Epoch()); rec.HostRAMGiB != 0 || rec.HostRAMWaitGiB != 30 {
				t.Fatalf("the record declares nothing yet and carries the wait: host_ram %.1f, wait %.1f", rec.HostRAMGiB, rec.HostRAMWaitGiB)
			}

			// A single attempt while the host is short: the grant's sentence, and the record unchanged.
			err = l.AwaitHostRAM(AwaitHostRAMOptions{})
			short := asHostRAM(t, err)
			want := "waiting for host RAM: needs 30.0 GiB of commit, committed 70.0 of 100.0 GiB commit limit, 8.0 GiB headroom"
			if err.Error() != want || short.Impossible {
				t.Fatalf("the await refuses with the grant's words\n got: %s\nwant: %s (impossible=%v)", err.Error(), want, short.Impossible)
			}
			if rec := recordOf(t, m, l.Epoch()); rec.HostRAMGiB != 0 || rec.HostRAMWaitGiB != 30 {
				t.Fatalf("a refused await changes nothing: host_ram %.1f, wait %.1f", rec.HostRAMGiB, rec.HostRAMWaitGiB)
			}

			f.setCommit(40) // the seat is gone: 40 + 30 <= 92
			if err := l.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
				t.Fatalf("the host has the room: %v", err)
			}
			if rec := recordOf(t, m, l.Epoch()); rec.HostRAMGiB != 30 || rec.HostRAMWaitGiB != 0 {
				t.Fatalf("an admitted lease declares its need and waits for nothing: host_ram %.1f, wait %.1f", rec.HostRAMGiB, rec.HostRAMWaitGiB)
			}
			// Admitted already: asking again is a no-op, not a second stamp or a refusal.
			f.setCommit(90)
			if err := l.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
				t.Fatalf("an admitted lease is not asked twice: %v", err)
			}
		})
	}
}

// A figure the grant admits is declared as always; a caller that sets both gets the grant's.
func TestAnAdmittedFigureWinsOverAWaitingOne(t *testing.T) {
	m, _ := ramScoped(t, 10)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "both", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: 20, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if rec := recordOf(t, m, l.Epoch()); rec.HostRAMGiB != 20 || rec.HostRAMWaitGiB != 0 {
		t.Fatalf("the grant admitted 20 and nothing is left to wait for: host_ram %.1f, wait %.1f", rec.HostRAMGiB, rec.HostRAMWaitGiB)
	}
	if err := l.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
		t.Fatalf("nothing to await: %v", err)
	}
}

// NOT LOOSER. Over a table of hosts, needs and leases already running, the await admits and refuses exactly where the
// grant does, and says the same words. The preexisting lease declared its need while the host was empty and has loaded
// none of it, so its need is still to load for both.
func TestTheAwaitAdmitsExactlyWhereTheGrantDoes(t *testing.T) {
	for _, commit := range []float64{20, 40, 55, 60, 70, 85} {
		for _, need := range []float64{10, 30, 45} {
			for _, running := range []float64{0, 15} {
				decide := func(deferred bool) (bool, string) {
					m, f := ramScoped(t, 5)
					if running > 0 {
						other, err := m.TryAcquire(ClassMedia, Options{Reason: "running", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: running})
						if err != nil {
							t.Fatalf("setup: %v", err)
						}
						t.Cleanup(func() { _ = other.Release() })
					}
					f.setCommit(commit)
					if !deferred {
						l, err := m.TryAcquire(ClassMedia, Options{Reason: "grant", TTL: time.Hour, Devices: []string{card0}, HostRAMGiB: need})
						if err != nil {
							return false, asHostRAM(t, err).Error()
						}
						_ = l.Release()
						return true, ""
					}
					l, err := m.TryAcquire(ClassMedia, Options{Reason: "await", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: need})
					if err != nil {
						t.Fatalf("the deferred grant takes the cards: %v", err)
					}
					defer func() { _ = l.Release() }()
					if err := l.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
						return false, asHostRAM(t, err).Error()
					}
					return true, ""
				}
				okG, whyG := decide(false)
				okA, whyA := decide(true)
				if okG != okA || whyG != whyA {
					t.Errorf("commit %.0f need %.0f running %.0f: the grant says (%v, %q) and the await says (%v, %q)", commit, need, running, okG, whyG, okA, whyA)
				}
			}
		}
	}
}

// THE CHECK AND THE STAMP ARE ONE CRITICAL SECTION. Four leases wait for 30 GiB each on a host with room for one: however
// the lock serves them, exactly one is admitted. A check made and a stamp written separately would let two read the same
// headroom.
func TestConcurrentAwaitsCannotBothTakeTheLastHeadroom(t *testing.T) {
	cards := []string{card0, card1, card2, card3}
	for round := 0; round < 25; round++ {
		m, _ := ramScoped(t, 40) // 40 + 30 = 70 <= 92, 40 + 60 = 100 > 92
		leases := make([]*Lease, len(cards))
		for i, c := range cards {
			l, err := m.TryAcquire(ClassMedia, Options{Reason: "racer", TTL: time.Hour, Devices: []string{c}, HostRAMWaitGiB: 30})
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			leases[i] = l
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		var admitted, refused atomic.Int32
		for _, l := range leases {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				err := l.AwaitHostRAM(AwaitHostRAMOptions{})
				var short *ErrHostRAM
				switch {
				case err == nil:
					admitted.Add(1)
				case errors.As(err, &short):
					refused.Add(1)
				default:
					t.Errorf("round %d: unexpected error %v", round, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		for _, l := range leases {
			_ = l.Release()
		}
		if admitted.Load() != 1 || refused.Load() != 3 {
			t.Fatalf("round %d: %d admitted and %d refused; exactly one of four may take the last headroom", round, admitted.Load(), refused.Load())
		}
	}
}

// TWO LEASES THAT WAIT DO NOT WAIT FOR EACH OTHER. The host fits one of them. Each one's check counts the leases that
// have been ADMITTED, and a waiting lease is not one, so the first is admitted and the second is refused until the first
// ends; then it is admitted too. (Kept declared from the grant on, each would count the other's 30 GiB as still to load
// and neither would ever be admitted.)
func TestTwoWaitingLeasesAreAdmittedOneAfterTheOther(t *testing.T) {
	m, _ := ramScoped(t, 40)
	a, err := m.TryAcquire(ClassMedia, Options{Reason: "a", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Release() }()
	b, err := m.TryAcquire(ClassMedia, Options{Reason: "b", TTL: time.Hour, Devices: []string{card1}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Release() }()
	if err := a.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
		t.Fatalf("the first to ask is admitted: %v", err)
	}
	asHostRAM(t, b.AwaitHostRAM(AwaitHostRAMOptions{})) // 40 + 30 + the 30 a has yet to load > 92
	if rec := recordOf(t, m, b.Epoch()); rec.HostRAMGiB != 0 || rec.HostRAMWaitGiB != 30 {
		t.Fatalf("the refused lease still declares nothing: %+v", rec)
	}
	_ = a.Release()
	if err := b.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
		t.Fatalf("the second is admitted once the first has ended: %v", err)
	}
}

// The cost of declaring nothing while waiting, stated and pinned: a grant that fits is admitted ahead of a waiting lease,
// and the waiting lease then waits for what that grant has yet to load. Nothing is admitted past the line either way.
func TestAGrantThatFitsIsAdmittedAheadOfAWaitingLeaseAndTheLeaseThenCountsIt(t *testing.T) {
	m, _ := ramScoped(t, 40)
	w, err := m.TryAcquire(ClassMedia, Options{Reason: "waiting", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Release() }()
	g, err := m.TryAcquire(ClassMedia, Options{Reason: "granted", TTL: time.Hour, Devices: []string{card1}, HostRAMGiB: 30})
	if err != nil {
		t.Fatalf("a waiting lease holds no claim on the host, so a grant that fits is admitted: %v", err)
	}
	defer func() { _ = g.Release() }()
	short := asHostRAM(t, w.AwaitHostRAM(AwaitHostRAMOptions{}))
	if short.PendingGiB != 30 {
		t.Fatalf("the waiting lease counts the 30 GiB the grant has yet to load, pending = %.1f", short.PendingGiB)
	}
}

// What no state of the box could ever admit ends at once, however long the wait is allowed to be.
func TestAnImpossibleAwaitedFigureEndsAtOnce(t *testing.T) {
	m, _ := ramScoped(t, 10)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "too big", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 99})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	start := time.Now()
	err = l.AwaitHostRAM(AwaitHostRAMOptions{Deadline: time.Now().Add(time.Hour)})
	if short := asHostRAM(t, err); !short.Impossible {
		t.Fatalf("99 GiB on a 100 GiB box with 8 headroom can never be admitted: %+v", short)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("an impossible figure waited %s", time.Since(start))
	}
}

// A wait that will go on says so once, admits the moment the host has the room, and is not told twice.
func TestAnAwaitSaysOnceThatItWaitsAndAdmitsWhenTheHostRecovers(t *testing.T) {
	m, f := ramScoped(t, 80) // 80 + 30 > 92
	m.pollEvery = 5 * time.Millisecond
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	var told atomic.Int32
	told1 := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- l.AwaitHostRAM(AwaitHostRAMOptions{
			Deadline: time.Now().Add(30 * time.Second),
			OnWait: func(e *ErrHostRAM) {
				if told.Add(1) == 1 {
					told1 <- struct{}{}
				}
			},
		})
	}()
	select {
	case <-told1:
	case <-time.After(10 * time.Second):
		t.Fatal("the wait was never told")
	}
	time.Sleep(60 * time.Millisecond) // many polls
	f.setCommit(30)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("admitted once the host has the room: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never admitted after the host recovered")
	}
	if n := told.Load(); n != 1 {
		t.Fatalf("the wait is told once, not per poll: %d times", n)
	}
}

// A deadline that passes ends the wait with the refusal it last had; a single attempt is never told it will wait.
func TestAnAwaitEndsAtItsDeadlineWithTheRefusal(t *testing.T) {
	m, _ := ramScoped(t, 80)
	m.pollEvery = 5 * time.Millisecond
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	var told atomic.Int32
	start := time.Now()
	err = l.AwaitHostRAM(AwaitHostRAMOptions{Deadline: time.Now().Add(150 * time.Millisecond), OnWait: func(*ErrHostRAM) { told.Add(1) }})
	asHostRAM(t, err)
	if el := time.Since(start); el < 150*time.Millisecond {
		t.Fatalf("gave up after %s, before its deadline", el)
	}
	if told.Load() != 1 {
		t.Fatalf("a wait that goes on is told once, told %d", told.Load())
	}
	told.Store(0)
	asHostRAM(t, l.AwaitHostRAM(AwaitHostRAMOptions{OnWait: func(*ErrHostRAM) { told.Add(1) }}))
	if told.Load() != 0 {
		t.Fatalf("a single attempt has no wait to tell, told %d", told.Load())
	}
}

// A wait told to stop leaves at once, and a lease that is gone says so.
func TestAnAwaitIsCancelledAndNoticesALostLease(t *testing.T) {
	m, _ := ramScoped(t, 80)
	m.pollEvery = 5 * time.Millisecond
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatal(err)
	}
	cancel := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- l.AwaitHostRAM(AwaitHostRAMOptions{Deadline: time.Now().Add(time.Minute), Cancel: cancel})
	}()
	time.Sleep(40 * time.Millisecond)
	close(cancel)
	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("a cancelled wait answers ErrCancelled, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled wait never returned")
	}
	epoch := l.Epoch()
	_ = l.Release()
	if err := m.AwaitHostRAM(epoch, AwaitHostRAMOptions{}); err == nil || errors.As(err, new(*ErrHostRAM)) {
		t.Fatalf("a lease that is gone is not a host-RAM refusal: %v", err)
	}
}

// A lease that waits for nothing (declared as always, or none) answers at once, whatever the host reads.
func TestAnAwaitOfALeaseWithNothingToWaitForIsAdmittedWithoutReadingTheHost(t *testing.T) {
	m, f := ramScoped(t, 99)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "plain", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	before := f.reads.Load()
	if err := l.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
		t.Fatalf("nothing to await: %v", err)
	}
	if got := f.reads.Load(); got != before {
		t.Fatalf("a lease with nothing to await read the host %d time(s)", got-before)
	}
}

// A DEFERRED LEASE IS A REQUEST THAT DECLARES NOTHING IN THE GRANT, so it passes a front waiter that waits only on the
// host's memory (G6), and that is the point: the waiter is short of memory the agent seat holds, the deferred lease is the
// one that unloads the seat, and queued behind the waiter it could never clear the memory the waiter waits for. The cost
// is the one G6 already states: when the memory recovers the waiter waits for the cards the lease took. The lease still
// asks the host for its own figure before its command starts, so nothing is admitted past the line.
func TestADeferredLeasePassesAWaiterThatWaitsOnlyOnMemoryAndStillAsksTheHost(t *testing.T) {
	m, f := ramScoped(t, 85) // 85 + 30 > 92
	startRAMWaiter(t, m, f, 30, card0)
	time.Sleep(5 * time.Millisecond) // the lease arrives AFTER the waiter

	l, err := m.Acquire(ClassMedia, Options{Reason: "unloads the seat", TTL: time.Hour, Devices: []string{card0}, HostRAMWaitGiB: 30})
	if err != nil {
		t.Fatalf("a lease that declares nothing in the grant passes a waiter that waits only on memory: %v", err)
	}
	defer func() { _ = l.Release() }()
	asHostRAM(t, l.AwaitHostRAM(AwaitHostRAMOptions{})) // 85 + 30 > 92: still not admitted
	if rec := recordOf(t, m, l.Epoch()); rec.HostRAMGiB != 0 || rec.HostRAMWaitGiB != 30 {
		t.Fatalf("a refused await changes nothing: %+v", rec)
	}
	f.setCommit(40) // the unload freed the seat's memory
	if err := l.AwaitHostRAM(AwaitHostRAMOptions{}); err != nil {
		t.Fatalf("admitted once the host has the room: %v", err)
	}
}

// A lease that never asks (it declares nothing and waits for nothing) is untouched by all of this: its record has neither
// field, so a reader that predates both sees the lease it always saw.
func TestARecordWithoutAWaitCarriesNeitherHostRAMField(t *testing.T) {
	m, _ := ramScoped(t, 10)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "plain", TTL: time.Hour, Devices: []string{card0}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	raw, err := os.ReadFile(epochRecordPath(m.leaseDir(), l.Epoch()))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"host_ram_gib", "host_ram_wait_gib"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("a lease with nothing to declare or await writes no %s: %s", key, raw)
		}
	}
	l2, err := m.TryAcquire(ClassMedia, Options{Reason: "deferred", TTL: time.Hour, Devices: []string{card1}, HostRAMWaitGiB: 12.5})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l2.Release() }()
	raw, err = os.ReadFile(epochRecordPath(m.leaseDir(), l2.Epoch()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"host_ram_wait_gib":12.5`) || strings.Contains(string(raw), `"host_ram_gib"`) {
		t.Errorf("a deferred lease carries the wait and declares nothing: %s", raw)
	}
}
