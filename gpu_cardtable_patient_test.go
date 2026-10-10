package main

// F24 on the verbs: a read of the card table that runs out of time is made once more under a longer
// deadline before it counts as "no table". The verbs that REFUSE without a table (`gpu reserve
// --cards`, `--devices`, `node-swap --cards`) and the ones whose fence widens without it (the scope
// of a drain or an unload) read through that retry; the views (`gpu cards`, `gpu status`) stay at one
// attempt, because "no table" is a fine thing for them to print. Nothing here starts nvidia-smi.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/nodeswap"
)

// slowTable replaces the card-table seam with a reader whose first `hangs` reads block until their
// deadline ends them (nvidia-smi under load) and which answers from then on. It records the deadline
// each read was given. The budgets of the patient read are shrunk for the test and put back after.
type slowTable struct {
	mu    sync.Mutex
	hangs int
	cards []gpuprobe.Card
	left  []time.Duration
}

func useSlowTable(t *testing.T, cards []gpuprobe.Card, hangs int) *slowTable {
	t.Helper()
	s := &slowTable{hangs: hangs, cards: cards}
	oldFn, oldFirst, oldRetry := cardTableFn, cardReadFirst, cardReadRetry
	cardReadFirst, cardReadRetry = 40*time.Millisecond, time.Second
	cardTableFn = func(ctx context.Context, _ config.Config) ([]gpuprobe.Card, string, error) {
		s.mu.Lock()
		left := time.Duration(0)
		if dl, ok := ctx.Deadline(); ok {
			left = time.Until(dl)
		}
		s.left = append(s.left, left)
		hang := s.hangs > 0
		if hang {
			s.hangs--
		}
		s.mu.Unlock()
		if hang {
			select {
			case <-ctx.Done():
				return nil, "", fmt.Errorf("nvidia-smi: nvidia-smi: %w", ctx.Err())
			case <-time.After(30 * time.Second):
				return nil, "", errors.New("the read was never given a deadline")
			}
		}
		return s.cards, "", nil
	}
	t.Cleanup(func() { cardTableFn, cardReadFirst, cardReadRetry = oldFn, oldFirst, oldRetry })
	return s
}

func (s *slowTable) reads() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.left...)
}

func TestTheDecidingVerbsReadTheCardTableWithTheRetryAndTheViewsDoNot(t *testing.T) {
	t.Run("cardTablePatient reads once more under the longer deadline", func(t *testing.T) {
		s := useSlowTable(t, statusCards(), 1)
		cards, _, err := cardTablePatient(context.Background(), config.Config{})
		if err != nil || len(cards) == 0 {
			t.Fatalf("cards=%d err=%v, want the retry's table", len(cards), err)
		}
		r := s.reads()
		if len(r) != 2 || r[0] > 40*time.Millisecond || r[1] < 400*time.Millisecond {
			t.Errorf("reads = %v, want a 40ms attempt and then a longer one", r)
		}
	})
	t.Run("cardTable, the view's read, is one attempt", func(t *testing.T) {
		s := useSlowTable(t, statusCards(), 1)
		_, _, err := cardTable(context.Background(), config.Config{})
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the timeout as it was", err)
		}
		if r := s.reads(); len(r) != 1 {
			t.Errorf("reads = %v, want exactly one", r)
		}
	})
}

// `node-swap --cards` resolves the cards the operator named against the table. One slow read must not
// refuse the deploy; two must, and the refusal says the table was read twice.
func TestNodeSwapCardsSurvivesOneSlowCardTableRead(t *testing.T) {
	useSlowTable(t, statusCards(), 1)
	plan, err := resolveNodeSwapCards(context.Background(), nodeswap.Plan{Cards: []string{"2"}}, config.Config{}, nil)
	if err != nil || len(plan.Cards) != 1 || plan.Cards[0] != "gpu-cccc0000-x" {
		t.Fatalf("plan=%+v err=%v, want the card resolved from the retry's table", plan.Cards, err)
	}

	useSlowTable(t, statusCards(), 2)
	_, err = resolveNodeSwapCards(context.Background(), nodeswap.Plan{Cards: []string{"2"}}, config.Config{}, nil)
	if err == nil || !strings.Contains(err.Error(), "--cards") || !strings.Contains(err.Error(), "read twice") {
		t.Fatalf("err = %v, want the refusal to name --cards and say the table was read twice", err)
	}
}

// `gpu reserve --cards N` needs the table twice (to plan, and to allocate); a slow first read used to
// refuse the reserve outright with "--cards needs the card table and nvidia-smi gave none".
func TestGPUReserveCardsSurvivesOneSlowCardTableRead(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useCardTable(t, "") // the other live reads (processes, seats, RAM); the table below replaces its reader
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16, UtilKnown: true},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T2", TotalGiB: 16, FreeGiB: 16, UtilKnown: true, DisplayActive: true},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16, UtilKnown: true},
	}, "")
	s := useSlowTable(t, cards, 1)
	t.Setenv("LO_HELPER_SLEEP_MS", "1500")
	done := make(chan error, 1)
	go func() {
		done <- runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--cards", "2", "--wait", "0"}, helperCmd()...))
	}()
	l := waitForLeases(t, m, 1)
	if got := strings.Join(l[0].Devices, ","); got != "gpu-aaaa0000-x,gpu-cccc0000-x" {
		t.Fatalf("two cards, never the display card: %q", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("one slow read refused the reserve: %v", err)
	}
	if r := s.reads(); len(r) < 2 || r[0] > 40*time.Millisecond || r[1] < 400*time.Millisecond {
		t.Errorf("reads = %v, want the first attempt to run out and the second to get the longer deadline", r)
	}
}

// The scope of a drain or an unload is read through the retry too: without a table every seat counts as
// on the leased cards, so one slow read would unload the seats on the cards the lease does not hold.
func TestALeaseScopeSurvivesOneSlowCardTableRead(t *testing.T) {
	s := useSlowTable(t, statusCards(), 1)
	scope := newLeaseScope(context.Background(), config.Config{Layers: []config.LayerSpec{{Name: "single", Seats: []config.LayerSeat{
		{Role: "agent", Model: "on-card-0", Device: "0"},
		{Role: "vision", Model: "on-card-2", Device: "2"},
	}}}}, []string{"gpu-cccc0000-x"})
	if scope.touches("on-card-0") || !scope.touches("on-card-2") {
		t.Errorf("a lease on card 2 must touch the seat on card 2 and spare the one on card 0 (touches 0=%v 2=%v)", scope.touches("on-card-0"), scope.touches("on-card-2"))
	}
	if r := s.reads(); len(r) != 2 {
		t.Errorf("reads = %v, want the attempt that ran out and its retry", r)
	}
}
