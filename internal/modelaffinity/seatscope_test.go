package modelaffinity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The consumers' half of card-scoped leases (plan P4): a lease on one card fences the
// seats that sit on that card and nothing else. The synthetic box is three cards, PCI
// order 0 / 1 / 2, card 1 the display card, which is the shape of the 3-card tier.

const (
	idCard0 = "gpu-aaaa0000"
	idCard1 = "gpu-bbbb0000"
	idCard2 = "gpu-cccc0000"
)

func scopeCards() []gpuprobe.Card {
	return []gpuprobe.Card{
		// ComfyOrder is the box's declared FASTEST_FIRST order: the faster display card is 0.
		{UUID: "GPU-aaaa0000", NvidiaIndex: 0, Name: "T", VRAMTotalGiB: 16, VRAMFreeGiB: 15, ComfyOrder: 1},
		{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Name: "T", VRAMTotalGiB: 16, VRAMFreeGiB: 12, Display: true, ComfyOrder: 0},
		{UUID: "GPU-cccc0000", NvidiaIndex: 2, Name: "T", VRAMTotalGiB: 16, VRAMFreeGiB: 15, ComfyOrder: 2},
	}
}

// armSeatScope declares the seat pins (index lists, PCI order, the way a layer seat's
// device is written) and the card table the gate resolves them against, and counts how
// many times the card table is read.
func armSeatScope(t *testing.T, pins map[string][]string) *atomic.Int64 {
	t.Helper()
	var reads atomic.Int64
	SetSeatPins(func(model string) ([]string, bool) {
		p, ok := pins[model]
		return p, ok
	})
	prev := readCardTable
	readCardTable = func(context.Context, string) ([]gpuprobe.Card, string, error) {
		reads.Add(1)
		return scopeCards(), "", nil
	}
	resetCardMemo()
	t.Cleanup(func() {
		SetSeatPins(nil)
		readCardTable = prev
		resetCardMemo()
	})
	return &reads
}

var tripleBoxPins = map[string][]string{
	"seat-card0":  {"0"},
	"seat-card2":  {"2"},
	"seat-pair":   {"0", "2"},
	"seat-triple": {"2", "1", "0"},
}

func cardScoped(t *testing.T) *gpulease.Manager {
	t.Helper()
	m := armLease(t)
	m.SetCardScoped(true)
	return m
}

func mediaLeaseOn(t *testing.T, m *gpulease.Manager, devs ...string) {
	t.Helper()
	holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "render", Origin: "pipeline", Devices: devs})
}

func TestSeatOnFreeCardLoadsUnderMediaLeaseOnOtherCard(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	mediaLeaseOn(t, m, idCard2)

	// A seat on card 0 is not gated: the render owns card 2 only.
	tk := admitFast(t, "http://scope-free", "seat-card0")
	tk.Release()
	// A new run on that seat is not cordoned either.
	if err := AwaitRunSlot(context.Background(), "http://scope-free", "seat-card0", time.Now()); err != nil {
		t.Fatalf("AwaitRunSlot on a seat whose card is free = %v", err)
	}
	// The seat ON the held card is still fenced, which is what the lease exists for.
	_, err := Admit(context.Background(), "http://scope-held", "seat-card2", 300*time.Millisecond)
	var le *LeaseError
	if !errors.As(err, &le) {
		t.Fatalf("a seat on the held card must wait for the render, got %v", err)
	}
}

func TestSeatWithUnknownPinStillBlocked(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	mediaLeaseOn(t, m, idCard2)

	// No pin declared for this model: it could be on any card, so today's answer stands.
	_, err := Admit(context.Background(), "http://scope-unknown", "mystery-seat", 300*time.Millisecond)
	var le *LeaseError
	if !errors.As(err, &le) {
		t.Fatalf("a seat with no declared pin must stay fenced by any live lease, got %v", err)
	}
}

func TestTripleSeatFencedByCard2Lease(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	mediaLeaseOn(t, m, idCard2)

	for _, seat := range []string{"seat-triple", "seat-pair"} {
		_, err := Admit(context.Background(), "http://scope-"+seat, seat, 300*time.Millisecond)
		var le *LeaseError
		if !errors.As(err, &le) {
			t.Errorf("%s spans card 2 and must be fenced by a card-2 lease, got %v", seat, err)
		}
	}
}

// The mirror image: a card-0 lease leaves the card-2 seat alone.
func TestCard0LeaseLeavesTheCard2SeatFree(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	mediaLeaseOn(t, m, idCard0)
	tk := admitFast(t, "http://scope-other", "seat-card2")
	tk.Release()
	_, err := Admit(context.Background(), "http://scope-other-pair", "seat-pair", 300*time.Millisecond)
	if !IsLeaseRefusal(err) {
		t.Fatalf("the pair spans card 0 and must wait, got %v", err)
	}
}

func TestUnresolvablePinFailsClosed(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, map[string][]string{"seat-ghost": {"7"}}) // no card has index 7
	mediaLeaseOn(t, m, idCard2)
	_, err := Admit(context.Background(), "http://scope-ghost", "seat-ghost", 300*time.Millisecond)
	var le *LeaseError
	if !errors.As(err, &le) {
		t.Fatalf("a pin the card table cannot place is every card, got %v", err)
	}
}

func TestNoCardTableFailsClosed(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	readCardTable = func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return nil, "", errors.New("nvidia-smi not found")
	}
	resetCardMemo()
	mediaLeaseOn(t, m, idCard2)
	_, err := Admit(context.Background(), "http://scope-nosmi", "seat-card0", 300*time.Millisecond)
	var le *LeaseError
	if !errors.As(err, &le) {
		t.Fatalf("with no card table the pin cannot be resolved: fence, as before card-scoped leases; got %v", err)
	}
}

func TestWholeNodeLeaseStillFencesEverySeat(t *testing.T) {
	m := armLease(t)
	reads := armSeatScope(t, tripleBoxPins)
	holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "legacy render"})
	_, err := Admit(context.Background(), "http://scope-whole", "seat-card0", 300*time.Millisecond)
	var le *LeaseError
	if !errors.As(err, &le) {
		t.Fatalf("a whole-node lease fences every seat, got %v", err)
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("a whole-node lease needs no card table, but it was read %d time(s)", n)
	}
}

// The card table is an nvidia-smi exec. It must never sit on the unfenced path.
func TestCardTableNotReadWhenNoLeaseIsHeld(t *testing.T) {
	armLease(t)
	reads := armSeatScope(t, tripleBoxPins)
	tk := admitFast(t, "http://scope-idle", "seat-card0")
	tk.Release()
	if n := reads.Load(); n != 0 {
		t.Fatalf("an idle box read the card table %d time(s)", n)
	}
}

func TestCardTableIsMemoisedAcrossPolls(t *testing.T) {
	m := cardScoped(t)
	reads := armSeatScope(t, tripleBoxPins)
	mediaLeaseOn(t, m, idCard2)
	for i := 0; i < 5; i++ {
		if got := SeatLease("seat-card0"); got.Held {
			t.Fatalf("seat-card0 must read free under a card-2 lease, got %+v", got)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("five reads inside the memo window read the card table %d times, want 1", n)
	}
}

func TestSeatLeaseNamesTheLeasesOnTheSeatsCards(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	l0 := gpulease.Options{Reason: "card0 job", Devices: []string{idCard0}}
	holdLease(t, m, gpulease.ClassMedia, l0)
	mediaLeaseOn(t, m, idCard2)

	if got := SeatLease("seat-card2"); !got.Held || got.Devices[0] != idCard2 {
		t.Fatalf("card-2 seat = %+v, want the card-2 lease", got)
	}
	both := SeatLease("seat-pair")
	if len(both.Each()) != 2 {
		t.Fatalf("the pair spans both held cards and must see both leases, got %+v", both)
	}
	if got := SeatLease("seat-unpinned"); len(got.Each()) != 2 {
		t.Fatalf("an unknown pin sees every live lease, got %+v", got)
	}
}

func TestAwaitUpstreamFenceIsPerSeat(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	mediaLeaseOn(t, m, idCard2)
	var runningReads atomic.Int64
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/running" {
			runningReads.Add(1)
		}
		_, _ = w.Write([]byte(`{"running":[]}`))
	}))
	t.Cleanup(counting.Close)
	stub := newStubSwap(t, nil)

	if _, err := AwaitUpstream(context.Background(), counting.URL, "seat-card0", "/props", time.Now()); err != nil {
		t.Fatalf("a card-0 seat's /upstream probe under a card-2 lease = %v", err)
	}
	if n := runningReads.Load(); n != 0 {
		t.Fatalf("an unfenced seat pays no residency read, but /running was read %d time(s)", n)
	}
	_, err := AwaitUpstream(context.Background(), stub.srv.URL, "seat-card2", "/props", time.Now())
	if !IsLeaseRefusal(err) {
		t.Fatalf("a card-2 seat's /upstream probe must be refused under a card-2 lease, got %v", err)
	}
}

func TestCardsHeldNamesTheForeignLeaseOnTheSeatsCards(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	l := holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: []string{idCard2}})

	if held, why := CardsHeld([]string{"0"}); held {
		t.Fatalf("card 0 is free under a card-2 lease, got held (%s)", why)
	}
	held, why := CardsHeld([]string{"2"})
	if !held || !strings.Contains(why, "media") || !strings.Contains(why, "epoch") {
		t.Fatalf("card 2 = %v %q, want held and the lease named", held, why)
	}
	if held, _ := CardsHeld([]string{"0", "2"}); !held {
		t.Fatal("a seat spanning card 2 is held")
	}
	if held, _ := CardsHeld(nil); !held {
		t.Fatal("an unknown pin is every card: any live lease holds it")
	}
	// The holder's own child is not blocked by the holder's lease.
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(l.Epoch(), 10))
	if held, why := CardsHeld([]string{"2"}); held {
		t.Fatalf("a process inside the lease is not held by it, got %q", why)
	}
}

func TestCardsHeldIsFalseWhenTheGateIsNotArmed(t *testing.T) {
	leaseMu.Lock()
	leaseDir = ""
	leaseMu.Unlock()
	if held, _ := CardsHeld([]string{"0"}); held {
		t.Fatal("an unarmed gate (no config.Load) holds nothing")
	}
}

// A blocked admission re-reads the lease every poll, and each read is narrowed to the seat's
// cards: when the card that blocked it is released and another card is taken, the card-0 seat
// is admitted at once instead of waiting out a render it does not share a card with.
func TestBlockedAdmissionIsAdmittedWhenTheHeldCardChanges(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	first, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render a", Devices: []string{idCard0}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = first.Release()
		if l, aerr := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render b", Devices: []string{idCard2}}); aerr == nil {
			t.Cleanup(func() { _ = l.Release() })
		}
	}()
	start := time.Now()
	tk, aerr := Admit(context.Background(), "http://scope-flip", "seat-card0", 6*time.Second)
	if aerr != nil {
		t.Fatalf("the seat's card was released and the new render is on another card: Admit = %v", aerr)
	}
	tk.Release()
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("admitted after %s: the poll must narrow each read to the seat's cards", el)
	}
}
