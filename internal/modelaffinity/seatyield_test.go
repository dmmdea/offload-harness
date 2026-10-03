package modelaffinity

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/seatload"
)

// The seat race rule (plan P4): the lease claims cards first, then unloads the seats that
// sit on them. A seat whose load passed the gate before the claim becomes resident AFTER it,
// so it re-checks the lease once its model is resident and yields: it unloads ITSELF. The
// seat always yields, never the long job, and never the memory stack.

// fakeSwap is the llama-swap the yield path talks to: which models are resident, how many
// requests each holds, and which unloads were asked for.
type fakeSwap struct {
	mu       sync.Mutex
	resident map[string]int // model -> requests in flight
	unloaded []string
	failOn   map[string]bool
	// readErr makes the engine's own gauge unreadable; reads counts the gauge reads made.
	readErr error
	reads   int
}

func (f *fakeSwap) gaugeReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeSwap) unloads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unloaded...)
}

func armYield(t *testing.T, resident map[string]int, protect ...string) *fakeSwap {
	t.Helper()
	f := &fakeSwap{resident: resident, failOn: map[string]bool{}}
	prevRead, prevUnload := yieldRead, yieldUnload
	yieldRead = func(_ context.Context, _, model string) (seatload.Reading, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reads++
		if f.readErr != nil {
			return seatload.Reading{}, f.readErr
		}
		n, ok := f.resident[model]
		return seatload.Reading{Loaded: ok, Inflight: n, Canonical: model}, nil
	}
	yieldUnload = func(_ context.Context, _, model string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failOn[model] {
			return errors.New("unload refused")
		}
		f.unloaded = append(f.unloaded, model)
		delete(f.resident, model)
		return nil
	}
	SetYieldProtect(protect)
	resetYieldWarnings()
	t.Cleanup(func() {
		yieldRead, yieldUnload = prevRead, prevUnload
		SetYieldProtect(nil)
		resetYieldWarnings()
	})
	return f
}

// resetYieldWarnings forgets which seats have already been reported, so one test's report does
// not silence another's.
func resetYieldWarnings() {
	yieldWarnMu.Lock()
	yieldWarned = map[string]time.Time{}
	yieldWarnMu.Unlock()
}

func TestSeatLoadRacingDeviceGrantSeatYields(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	f := armYield(t, map[string]int{"seat-card0": 0})

	// The load passed the gate: no lease was held.
	tk, err := Admit(context.Background(), "http://yield-endpoint", "seat-card0", 5*time.Second)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	// A device grant on card 0 lands while the model is loading (the lease unloaded the seats
	// resident at its claim, and this one was not yet).
	mediaLeaseOn(t, m, idCard0)
	tk.Release()
	if got := f.unloads(); len(got) != 1 || got[0] != "seat-card0" {
		t.Fatalf("a seat resident on a card a lease now holds must unload itself, got %v", got)
	}
}

func TestSeatDoesNotYieldToALeaseOnAnotherCard(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	f := armYield(t, map[string]int{"seat-card0": 0})
	tk, err := Admit(context.Background(), "http://yield-endpoint", "seat-card0", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	mediaLeaseOn(t, m, idCard2)
	tk.Release()
	if got := f.unloads(); len(got) != 0 {
		t.Fatalf("a render on card 2 is no reason for a card-0 seat to leave, got %v", got)
	}
}

func TestYieldWaitsForTheBatchToDrainAndNeverTakesABusySeat(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	f := armYield(t, map[string]int{"seat-card0": 0})
	base := "http://yield-batch"
	first, err := Admit(context.Background(), base, "seat-card0", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Admit(context.Background(), base, "seat-card0", 5*time.Second) // joins the batch
	if err != nil {
		t.Fatal(err)
	}
	mediaLeaseOn(t, m, idCard0)
	first.Release()
	if got := f.unloads(); len(got) != 0 {
		t.Fatalf("a request of the same batch is still in flight: the seat must not be pulled from under it, got %v", got)
	}
	second.Release()
	if got := f.unloads(); len(got) != 1 {
		t.Fatalf("the batch drained: the seat yields now, got %v", got)
	}

	// The engine's own gauge says a request is still running (another process's): not idle.
	f2 := armYield(t, map[string]int{"seat-card0": 3})
	if yielded, _ := YieldIfFenced(context.Background(), base, "seat-card0"); yielded {
		t.Fatal("a seat the engine says is busy is never pulled from under its requests")
	}
	if got := f2.unloads(); len(got) != 0 {
		t.Fatalf("nothing may be unloaded from under in-flight requests, got %v", got)
	}
}

func TestYieldIfFencedRefusesTheMemoryStackAndTheHoldersOwnChild(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, map[string][]string{"embeddinggemma": {"0"}, "seat-card0": {"0"}})
	f := armYield(t, map[string]int{"embeddinggemma": 0, "seat-card0": 0}, "embeddinggemma")
	l := holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: []string{idCard0}})

	if yielded, _ := YieldIfFenced(context.Background(), "http://yield-endpoint", "embeddinggemma"); yielded {
		t.Fatal("the memory stack never yields to a lease (register C-87)")
	}
	// The holder's own child is inside the lease and is never fenced by it.
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(l.Epoch(), 10))
	if yielded, _ := YieldIfFenced(context.Background(), "http://yield-endpoint", "seat-card0"); yielded {
		t.Fatal("a process inside the lease does not yield to its own lease")
	}
	if got := f.unloads(); len(got) != 0 {
		t.Fatalf("nothing should have been unloaded, got %v", got)
	}
	t.Setenv("GPU_LEASE_EPOCH", "")
	yielded, why := YieldIfFenced(context.Background(), "http://yield-endpoint", "seat-card0")
	if !yielded || why == "" {
		t.Fatalf("a foreign seat on the held card yields and says why: %v %q", yielded, why)
	}
}

func TestYieldReportsAnUnloadThatFailed(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	f := armYield(t, map[string]int{"seat-card0": 0})
	f.failOn["seat-card0"] = true
	mediaLeaseOn(t, m, idCard0)
	yielded, why := YieldIfFenced(context.Background(), "http://yield-endpoint", "seat-card0")
	if yielded {
		t.Fatal("an unload that failed is not a yield")
	}
	if why == "" {
		t.Fatal("and it says so: the seat is still resident on a leased card")
	}
}

// ---- the widening hook: a legacy lease's inferred scope grows --------------------------

type scopeBox struct {
	t     *testing.T
	m     *gpulease.Manager
	clock time.Time
	tree  []gpulease.TreeProc
}

// legacyScopeBox holds a whole-node (legacy-format) media lease, gives the evidence rule a
// process tree and a card table with a declared ComfyUI order, and a clock the test moves.
func legacyScopeBox(t *testing.T) *scopeBox {
	t.Helper()
	m := armLease(t)
	b := &scopeBox{t: t, m: m, clock: time.Now()}
	prevTree, prevNow := scopeTree, scopeNow
	scopeTree = func(root int) ([]gpulease.TreeProc, error) { return b.tree, nil }
	scopeNow = func() time.Time { return b.clock }
	// The host has turned the inference on; the lease is the record an older binary wrote.
	SetLegacyInference(true)
	t.Cleanup(func() {
		scopeTree, scopeNow = prevTree, prevNow
		SetLegacyInference(false)
		resetScopers()
	})
	resetScopers()
	m.EmulateLegacyWriter()
	holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "legacy film render"})
	return b
}

func TestInferredScopeWideningEvictsSeatNotLongJob(t *testing.T) {
	b := legacyScopeBox(t)
	armSeatScope(t, tripleBoxPins)
	f := armYield(t, map[string]int{
		"seat-card0":     0,
		"seat-card2":     0,
		"embeddinggemma": 0, // the memory stack, pinned nowhere the tiers declare
		"mystery":        0, // no declared pin
	}, "embeddinggemma")
	base := "http://yield-widen"

	// The job is a ComfyUI on --cuda-device 2 (card 2): the scope is established at the first
	// read, and NOTHING is unloaded for it. A read of the lease is not the action of a process
	// that wants the card (plan I5), and the seats resident on card 2 predate the lease.
	b.tree = []gpulease.TreeProc{{PID: 1, Cmdline: "python main.py --cuda-device 2"}}
	if got := SeatLease("seat-card0"); got.Held {
		t.Fatalf("an inferred card-2 lease leaves a card-0 seat free, got %+v", got)
	}
	if got := f.unloads(); len(got) != 0 {
		t.Fatalf("establishing a scope unloads nothing, got %v", got)
	}
	// A card-0 seat is admitted while the card looks free.
	tk := admitFast(t, base, "seat-card0")

	// The job spreads onto card 0 (ComfyUI order 1): the next reading WIDENS the scope. That
	// reading unloads nothing either.
	b.clock = b.clock.Add(2 * time.Minute)
	b.tree = []gpulease.TreeProc{
		{PID: 1, Cmdline: "python main.py --cuda-device 2"},
		{PID: 2, PPID: 1, Cmdline: "python main.py --cuda-device 1 --port 8189"},
	}
	if got := SeatLease("seat-card0"); !got.Held {
		t.Fatalf("after widening the card-0 seat is on a held card, got %+v", got)
	}
	if got := f.unloads(); len(got) != 0 {
		t.Fatalf("a read that widens a scope unloads nothing, got %v", got)
	}

	// The seat's own request completes: it re-reads the lease, finds it on its card, and the
	// SEAT yields. The long job's lease is the one thing that is never disturbed.
	tk.Release()
	got := f.unloads()
	if len(got) != 1 || got[0] != "seat-card0" {
		t.Fatalf("the seat that was admitted on a card the lease spread onto unloads itself, got %v", got)
	}
	if info := b.m.Inspect(); !info.Held {
		t.Fatal("the long job's lease is the one thing that is never disturbed")
	}
}

func TestWideningNeverPullsABusySeat(t *testing.T) {
	b := legacyScopeBox(t)
	armSeatScope(t, tripleBoxPins)
	f := armYield(t, map[string]int{"seat-card0": 2})
	base := "http://yield-widen-busy"
	b.tree = []gpulease.TreeProc{{PID: 1, Cmdline: "python main.py --cuda-device 2"}}
	SeatLease("seat-card0")
	tk := admitFast(t, base, "seat-card0")
	b.clock = b.clock.Add(2 * time.Minute)
	b.tree = []gpulease.TreeProc{
		{PID: 1, Cmdline: "python main.py --cuda-device 2"},
		{PID: 2, PPID: 1, Cmdline: "python main.py --cuda-device 1 --port 8189"},
	}
	tk.Release()
	if got := f.unloads(); len(got) != 0 {
		t.Fatalf("a seat with requests in flight is not pulled from under them, got %v", got)
	}
}
