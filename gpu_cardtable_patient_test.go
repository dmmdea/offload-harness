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
	// script, when set, gives the next reads one by one ("ok" answers, "hang" blocks until the read's
	// deadline ends it) before hangs applies: a verb whose reads are not the first ones it makes.
	script []string
	cards  []gpuprobe.Card
	left   []time.Duration
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
		if len(s.script) > 0 {
			hang, s.script = s.script[0] == "hang", s.script[1:]
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

// useScriptedTable is useSlowTable with the reads scripted one by one.
func useScriptedTable(t *testing.T, cards []gpuprobe.Card, modes ...string) *slowTable {
	t.Helper()
	s := useSlowTable(t, cards, 0)
	s.mu.Lock()
	s.script = append([]string(nil), modes...)
	s.mu.Unlock()
	return s
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

// The wrapper pins its command to the cards the lease holds, and what the command pins ITSELF to is
// read from the card table: a bare index it inherited (CUDA_VISIBLE_DEVICES=2 in PCI order) means a
// card only with the table. A pin inside the held cards is tighter than the lease and is left alone;
// one the wrapper cannot resolve is REPLACED with the held cards and said so. That read is one of the
// deciding ones: one slow nvidia-smi must not make the wrapper overwrite a pin it could have confirmed.
func TestGPUReserveConfinementReadsTheCardTableWithTheRetry(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	useCardTable(t, "") // the other live reads (processes, seats, RAM); the table below replaces its reader
	clearCardPins(t)
	t.Setenv("CUDA_VISIBLE_DEVICES", "2")
	t.Setenv("CUDA_DEVICE_ORDER", "PCI_BUS_ID")
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16, UtilKnown: true},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T2", TotalGiB: 16, FreeGiB: 16, UtilKnown: true, DisplayActive: true},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16, UtilKnown: true},
	}, "")
	// Three verbs' reads, in order: the plan's, the unload list's scope (wrapperUnloadEnv) and the wrapper's
	// own, taken to read the command's pin once the lease is held. The third one's first attempt runs out of time.
	s := useScriptedTable(t, cards, "ok", "ok", "hang")
	out := t.TempDir() + "/env.txt"
	t.Setenv("LO_HELPER_ENV_OUT", out)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	if err := runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--devices", "2", "--wait", "0"}, envHelperCmd(out)...)); err != nil {
		t.Fatal(err)
	}
	if got := envValue(out, "cuda_visible"); got != "2" {
		t.Errorf("the command's own pin is inside the card the lease holds, so it is left alone; the wrapper saw no table and replaced it: CUDA_VISIBLE_DEVICES=%q", got)
	}
	if r := s.reads(); len(r) != 4 || r[2] > 40*time.Millisecond || r[3] < 400*time.Millisecond {
		t.Errorf("reads = %v, want the plan's and the scope's reads, then the wrapper's attempt that ran out and its retry under the longer deadline", r)
	}
}
