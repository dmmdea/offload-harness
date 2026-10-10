package gpualloc

// F24: a read of the card table that runs out of time is made once more under a longer deadline.
//
// The reader here is a stand-in that behaves like nvidia-smi under load: it answers at once, fails at
// once, or hangs until the deadline it was given ends it. Nothing in this file starts a process.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// readScript is a card-table reader whose attempts are scripted: "ok" answers, "fail" errors at once,
// "hang" blocks until the attempt's context is done.
type readScript struct {
	mu    sync.Mutex
	steps []string
	cards []gpuprobe.Card
	left  []time.Duration // time left on each attempt's deadline when it began (0 = none)
}

func (r *readScript) read(ctx context.Context, _ config.Config) ([]gpuprobe.Card, string, error) {
	r.mu.Lock()
	mode := "ok"
	if len(r.steps) > 0 {
		mode, r.steps = r.steps[0], r.steps[1:]
	}
	left := time.Duration(0)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	r.left = append(r.left, left)
	r.mu.Unlock()
	switch mode {
	case "fail":
		return nil, "", errors.New("nvidia-smi: not on PATH")
	case "hang":
		select {
		case <-ctx.Done():
			return nil, "", fmt.Errorf("nvidia-smi: nvidia-smi: %w", ctx.Err())
		case <-time.After(30 * time.Second):
			return nil, "", errors.New("the attempt was never given a deadline")
		}
	}
	return r.cards, "", nil
}

func (r *readScript) attempts() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.left...)
}

func scripted(cards []gpuprobe.Card, steps ...string) (*readScript, Deps) {
	r := &readScript{steps: steps, cards: cards}
	return r, Deps{Cards: r.read, ReadDeadline: 40 * time.Millisecond, RetryDeadline: 2 * time.Second}
}

func TestACardTableReadThatRunsOutOfTimeIsReadOnceMoreUnderALongerDeadline(t *testing.T) {
	r, d := scripted(threeCards(), "hang", "ok")
	cards, _, err := d.CardTable(context.Background(), config.Config{})
	if err != nil || len(cards) != 3 {
		t.Fatalf("cards=%d err=%v, want the retry's table", len(cards), err)
	}
	a := r.attempts()
	if len(a) != 2 {
		t.Fatalf("%d attempts (%v), want 2", len(a), a)
	}
	if a[0] > 40*time.Millisecond {
		t.Errorf("the first attempt had %v, want the first deadline (40ms)", a[0])
	}
	if a[1] < time.Second || a[1] > 2*time.Second {
		t.Errorf("the retry had %v, want the longer deadline (2s)", a[1])
	}
}

func TestACardTableReadThatRunsOutOfTimeTwiceFailsAndSaysItWasReadTwice(t *testing.T) {
	r, d := scripted(threeCards(), "hang", "hang")
	d.RetryDeadline = 80 * time.Millisecond
	_, _, err := d.CardTable(context.Background(), config.Config{})
	if err == nil {
		t.Fatal("a table nothing answered for is an error")
	}
	if n := len(r.attempts()); n != 2 {
		t.Errorf("%d attempts, want 2: one retry and no more", n)
	}
	if !strings.Contains(err.Error(), "read twice: no answer within 40ms, then none within 80ms") {
		t.Errorf("the error must say how the table was tried: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the cause stays in the chain: %v", err)
	}
}

// A failure that comes back at once is not a slow read, and a longer deadline cannot fix it.
func TestACardTableReadThatFailsAtOnceIsNotRetried(t *testing.T) {
	r, d := scripted(nil, "fail", "ok")
	_, _, err := d.CardTable(context.Background(), config.Config{})
	if err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("err = %v, want the failure as it was", err)
	}
	if strings.Contains(err.Error(), "read twice") {
		t.Errorf("an instant failure was not read twice: %v", err)
	}
	if n := len(r.attempts()); n != 1 {
		t.Errorf("%d attempts, want 1", n)
	}
}

// The caller's own context ending is not a slow nvidia-smi: nobody is waiting for a second attempt.
func TestACardTableReadIsNotRetriedWhenTheCallersContextIsDone(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		r, d := scripted(threeCards(), "hang", "ok")
		d.ReadDeadline = 10 * time.Second
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(30 * time.Millisecond); cancel() }()
		_, _, err := d.CardTable(ctx, config.Config{})
		if err == nil || attemptCount(r) != 1 {
			t.Fatalf("err=%v attempts=%d, want one failed attempt", err, attemptCount(r))
		}
	})
	t.Run("its own deadline ran out first", func(t *testing.T) {
		r, d := scripted(threeCards(), "hang", "ok")
		d.ReadDeadline = 10 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, _, err := d.CardTable(ctx, config.Config{})
		if err == nil || attemptCount(r) != 1 {
			t.Fatalf("err=%v attempts=%d, want one failed attempt: the caller's deadline is spent, a second attempt has no time", err, attemptCount(r))
		}
	})
}

func attemptCount(r *readScript) int { return len(r.attempts()) }

// The defaults are the deadlines the harness has always given the read (5 s) and the longer one the
// retry gets (15 s).
func TestTheCardTableReadDeadlinesDefaultToFiveAndFifteenSeconds(t *testing.T) {
	if DefaultCardRead != 5*time.Second || DefaultCardReadRetry != 15*time.Second {
		t.Fatalf("defaults = %v, %v", DefaultCardRead, DefaultCardReadRetry)
	}
	r, d := scripted(threeCards(), "hang", "ok")
	d.RetryDeadline = 0 // unset: the default retry deadline
	if _, _, err := d.CardTable(context.Background(), config.Config{}); err != nil {
		t.Fatal(err)
	}
	if a := r.attempts(); len(a) != 2 || a[1] < 14*time.Second || a[1] > 15*time.Second {
		t.Errorf("attempts = %v, want the retry under the default 15s", a)
	}
	r2, d2 := scripted(threeCards())
	d2.ReadDeadline = 0
	if _, _, err := d2.CardTable(context.Background(), config.Config{}); err != nil {
		t.Fatal(err)
	}
	if a := r2.attempts(); len(a) != 1 || a[0] < 4*time.Second || a[0] > 5*time.Second {
		t.Errorf("attempts = %v, want the first under the default 5s", a)
	}
}

// BuildInput's card-table failure is typed (the media path places from the table it holds when it sees
// one) and says what it always said.
func TestBuildInputReturnsATypedCardTableError(t *testing.T) {
	_, d := scripted(threeCards(), "hang", "hang")
	d.RetryDeadline = 50 * time.Millisecond
	_, err := BuildInput(context.Background(), scratchManager(t), config.Config{}, Need{}, d)
	var ct *CardTableError
	if !errors.As(err, &ct) {
		t.Fatalf("err = %v (%T), want a *CardTableError", err, err)
	}
	if !strings.HasPrefix(err.Error(), "the card table: nvidia-smi: ") {
		t.Errorf("text = %q", err.Error())
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the cause stays in the chain: %v", err)
	}
}

// The allocator's input is built from a table that took two attempts exactly as from one that took one.
func TestBuildInputAfterARetryIsTheSameInput(t *testing.T) {
	_, slow := scripted(threeCards(), "hang", "ok")
	_, quick := scripted(threeCards())
	got, err := BuildInput(context.Background(), scratchManager(t), config.Config{}, Need{}, withHost(slow))
	if err != nil {
		t.Fatal(err)
	}
	want, err := BuildInput(context.Background(), scratchManager(t), config.Config{}, Need{}, withHost(quick))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cards) != len(want.Cards) || got.AllowDisplay != want.AllowDisplay || got.HostFreeOK != want.HostFreeOK {
		t.Errorf("slow = %+v\nquick = %+v", got, want)
	}
}

func withHost(d Deps) Deps {
	d.HostFreeRAM = func() (float64, bool) { return 64, true }
	d.Presence = func(config.Config) (bool, bool) { return true, false }
	return d
}

// Claims is what BuildInput reports of the lease directory and the caller, without the card table.
func TestClaimsAreTheLeasesAndTheCallersOwnWithoutTheCardTable(t *testing.T) {
	m := scratchManager(t)
	cards := threeCards()
	whole, claimed := Claims(m, Need{Claimed: map[string]bool{cards[2].LeaseID(): true, cards[1].LeaseID(): false}})
	if whole || len(claimed) != 1 || !claimed[cards[2].LeaseID()] {
		t.Fatalf("an empty lease directory: whole=%v claimed=%v, want only the caller's claim", whole, claimed)
	}
	held, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "held", Devices: []string{cards[0].LeaseID()}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	whole, claimed = Claims(m, Need{})
	if whole || !claimed[cards[0].LeaseID()] || len(claimed) != 1 {
		t.Errorf("a card lease: whole=%v claimed=%v", whole, claimed)
	}
	_ = held.Release()
	node, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "node", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = node.Release() }()
	if whole, _ = Claims(m, Need{}); !whole {
		t.Error("a lease on no card is the whole node")
	}
}

// A lease scope reads the table through the same retry: a read that ran out of time once does not make
// every seat count as sitting on the leased cards (which would unload the seats on the other cards).
func TestALeaseScopeReadsTheCardTableWithTheRetry(t *testing.T) {
	cards := threeCards()
	_, d := scripted(cards, "hang", "ok")
	var warn strings.Builder
	scope := NewLeaseScope(context.Background(), scopedCfg(), []string{cards[2].LeaseID()}, d, &warn)
	if warn.Len() != 0 {
		t.Fatalf("the retry answered, so there is nothing to warn about: %s", warn.String())
	}
	if scope.Touches("on-card-0") {
		t.Error("a seat on card 0 is not on a lease that holds card 2")
	}
	if !scope.Touches("on-card-2") {
		t.Error("the seat on card 2 is on it")
	}
}

// The allocation's own reader asks for the per-device fields only: it never lists processes (the
// foreign-busy reader is a separate call that the media path does not make).
func TestTheAllocationReadsNoProcessListing(t *testing.T) {
	if got := DefaultDeps().ForeignBusy; got == nil || len(got(context.Background(), config.Config{})) != 0 {
		t.Error("the default foreign-busy reader must see nothing: it is not wired to nvidia-smi")
	}
}
