package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpucards"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// Verb-level tests of the per-card surfaces (plan P3 acceptance: `gpu status --json` has
// per-card rows). The helper-level tests in gpu_cards_test.go cannot notice the verb
// dropping the merge, the card_scoped_leases key, the text table or the no-nvidia-smi path.

// useQuietStatus swaps the live reads behind `gpu status` for a synthetic host: a card
// table (or none), no foreign processes, an idle activity view.
func useQuietStatus(t *testing.T, cards []gpuprobe.Card, cardsErr error) {
	t.Helper()
	oldCards, oldAct, oldForeign := cardTableFn, statusActivityFn, statusForeignFn
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		if cardsErr != nil {
			return nil, "", cardsErr
		}
		return cards, "", nil
	}
	statusActivityFn = func(context.Context, gpuactivity.Options) gpuactivity.View {
		return gpuactivity.View{At: time.Now(), Verdict: gpuactivity.VerdictFree, Note: "synthetic: nothing running"}
	}
	statusForeignFn = func(context.Context, config.Config) []ForeignGPUHolder { return nil }
	t.Cleanup(func() { cardTableFn, statusActivityFn, statusForeignFn = oldCards, oldAct, oldForeign })
}

// holdCardTwoWithAWaiter takes card 2 and queues a card-scoped waiter behind it, returning
// the holder and a function that releases it and waits for the waiter to finish.
func holdCardTwoWithAWaiter(t *testing.T, m *gpulease.Manager) (*gpulease.Lease, func()) {
	t.Helper()
	holder, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{
		Reason: "batch", Devices: []string{"gpu-cccc0000-x"}, TTL: time.Hour, Group: "batch-7", Command: "python run.py --cuda-device 2",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		l, aerr := m.Acquire(gpulease.ClassMedia, gpulease.Options{Reason: "behind", Devices: []string{"gpu-cccc0000-x"}, TTL: time.Hour, Wait: time.Minute, WaitOut: true})
		if aerr == nil {
			_ = l.Release()
		}
		got <- aerr
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(m.Waiters()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the waiter never registered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return holder, func() {
		_ = holder.Release()
		if err := <-got; err != nil {
			t.Errorf("the queued waiter must be granted once the holder lets go: %v", err)
		}
	}
}

func TestGPUStatusJSONVerbCarriesPerCardRowsLeasesAndTheQueue(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	holder, finish := holdCardTwoWithAWaiter(t, m)
	defer finish()

	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json must be one JSON document: %v\n%s", err, out)
	}
	// Keys that existed before the per-card work are still there with their types.
	for _, k := range []string{"held", "class", "epoch", "pid", "age_s", "reason", "command", "state_root", "queued", "seat_warm_owed", "verdict", "activity", "queue_with"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("the key %q existed before the per-card work and must still be printed", k)
		}
	}
	// The additions.
	for _, k := range []string{"cards", "leases", "card_scoped_leases", "devices", "epochs"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("the per-card key %q is missing from `gpu status --json`:\n%s", k, out)
		}
	}
	var rows []gpucards.Row
	if err := json.Unmarshal(doc["cards"], &rows); err != nil || len(rows) != 3 {
		t.Fatalf("one row per card: %v %s", err, doc["cards"])
	}
	held := rowByIndex(t, rows, 2)
	if held.State != "held" || held.Holder == nil || held.Holder.Epoch != holder.Epoch() || held.Holder.Group != "batch-7" || !strings.Contains(held.Holder.Command, "run.py") {
		t.Fatalf("card 2 shows its holder: %+v", held)
	}
	for _, idx := range []int{0, 1} {
		if r := rowByIndex(t, rows, idx); r.State != "free" || r.Holder != nil {
			t.Errorf("card %d is not held by a lease on card 2: %+v", idx, r)
		}
	}
	var on bool
	if err := json.Unmarshal(doc["card_scoped_leases"], &on); err != nil || !on {
		t.Errorf("card_scoped_leases must say the writer is on for this host: %s", doc["card_scoped_leases"])
	}
	// The queue keeps the four keys consumers read, and gains the device set and scope.
	var queued []map[string]any
	if err := json.Unmarshal(doc["queued"], &queued); err != nil || len(queued) != 1 {
		t.Fatalf("one waiter is queued: %v %s", err, doc["queued"])
	}
	for _, k := range []string{"pid", "class", "reason", "since", "devices", "scope"} {
		if _, ok := queued[0][k]; !ok {
			t.Errorf("queued row lost or lacks %q: %v", k, queued[0])
		}
	}
	if queued[0]["scope"] != "card" {
		t.Errorf("a card-scoped waiter says so: %v", queued[0])
	}
}

func TestGPUStatusTextVerbPrintsTheCardTableAndTheHolder(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)

	free := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"GPU: free", "card table:", "GPU-aaaa0000-x", "GPU-bbbb0000-x", "GPU-cccc0000-x", "display"} {
		if !strings.Contains(free, want) {
			t.Errorf("a free host's status must contain %q:\n%s", want, free)
		}
	}

	holder, finish := holdCardTwoWithAWaiter(t, m)
	defer finish()
	held := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"GPU: held", "card table:", "GPU-cccc0000-x", "batch-7", "run.py", "queued: 1", "cards gpu-cccc0000-x"} {
		if !strings.Contains(held, want) {
			t.Errorf("a held host's status must contain %q:\n%s", want, held)
		}
	}
	if !strings.Contains(held, "epoch") || !strings.Contains(held, "cards: gpu-cccc0000-x") {
		t.Errorf("the lease line names its epoch and its cards (%d):\n%s", holder.Epoch(), held)
	}
}

// No nvidia-smi: status still prints the leases and says there is no table; it never fails.
func TestGPUStatusVerbWithoutACardTableSaysSoAndStillPrintsTheLease(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, nil, errors.New("nvidia-smi not found"))
	holder, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "batch", Devices: []string{"gpu-cccc0000-x"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	text := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatalf("a missing card table must not fail status: %v", err)
		}
	})
	if !strings.Contains(text, "card table: none") || !strings.Contains(text, "nvidia-smi not found") || !strings.Contains(text, "GPU: held") {
		t.Errorf("the text must say there is no table and still print the lease:\n%s", text)
	}
	if strings.Contains(text, "idx ") {
		t.Errorf("no table header without a table:\n%s", text)
	}

	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var doc struct {
		Cards     []gpucards.Row   `json:"cards"`
		Leases    []map[string]any `json:"leases"`
		CardsNote string           `json:"cards_note"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cards) != 0 || !strings.Contains(doc.CardsNote, "no card table") || len(doc.Leases) != 1 {
		t.Errorf("no cards, a note saying why, and the lease list still present: %+v\n%s", doc, out)
	}
}

func TestGPUCardsVerbJSONAndText(t *testing.T) {
	cfg, m := scopedLeaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	holder, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "batch", Devices: []string{"gpu-aaaa0000-x"}, TTL: time.Hour, Group: "g1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	out := captureStdout(t, func() {
		if err := runGPUCards([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var doc struct {
		Cards  []gpucards.Row   `json:"cards"`
		Leases []map[string]any `json:"leases"`
		On     bool             `json:"card_scoped_leases"`
		Root   string           `json:"state_root"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(doc.Cards) != 3 || !doc.On || doc.Root == "" || len(doc.Leases) != 1 {
		t.Fatalf("three card rows, the switch, the state root and the lease: %+v", doc)
	}
	if r := rowByIndex(t, doc.Cards, 0); r.State != "held" || r.Holder == nil || r.Holder.Group != "g1" {
		t.Errorf("card 0 shows its holder: %+v", r)
	}
	if r := rowByIndex(t, doc.Cards, 2); r.State != "free" {
		t.Errorf("card 2 is free: %+v", r)
	}

	text := captureStdout(t, func() {
		if err := runGPUCards([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"idx", "GPU-aaaa0000-x", "g1", "display", "free"} {
		if !strings.Contains(text, want) {
			t.Errorf("the text table must contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "card-scoped leases: off") {
		t.Errorf("this host has the writer on:\n%s", text)
	}
}

func TestGPUCardsVerbOnAFlagOffHostSaysCardScopedLeasesAreOff(t *testing.T) {
	cfg, _ := leaseFixture(t)
	useQuietStatus(t, statusCards(), nil)
	text := captureStdout(t, func() {
		if err := runGPUCards([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(text, "card-scoped leases: off") {
		t.Errorf("a host that cannot write card-scoped leases says so:\n%s", text)
	}
	out := captureStdout(t, func() {
		if err := runGPUCards([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var doc struct {
		On bool `json:"card_scoped_leases"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.On {
		t.Errorf("card_scoped_leases is false on a flag-off host: %v %s", err, out)
	}
}

// `gpu cards` IS the card table: with none there is nothing to print, so it fails and says why.
func TestGPUCardsVerbFailsWithoutACardTable(t *testing.T) {
	cfg, _ := leaseFixture(t)
	useQuietStatus(t, nil, errors.New("nvidia-smi not found"))
	var err error
	captureStdout(t, func() { err = runGPUCards([]string{"--config", cfg}) })
	if err == nil || !strings.Contains(err.Error(), "no card table") || !strings.Contains(err.Error(), "nvidia-smi not found") {
		t.Fatalf("no table, no verb: %v", err)
	}
}

func TestPrintCardTableNotesAnUndeclaredComfyOrderAndTheDeclaredOne(t *testing.T) {
	undeclared, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "A", TotalGiB: 16, FreeGiB: 16},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "A", TotalGiB: 16, FreeGiB: 16},
	}, "")
	text := captureStdout(t, func() { printCardTable(undeclared, "a note", nil, nil) })
	for _, want := range []string{"card table:", "GPU-aaaa0000-x", "note: a note", "gpu_comfy_order"} {
		if !strings.Contains(text, want) {
			t.Errorf("an undeclared order must be explained (%q):\n%s", want, text)
		}
	}
	if text := captureStdout(t, func() { printCardTable(statusCards(), "", nil, nil) }); strings.Contains(text, "gpu_comfy_order") {
		t.Errorf("a declared order needs no warning:\n%s", text)
	}
	if text := captureStdout(t, func() { printCardTable(nil, "", errors.New("boom"), nil) }); !strings.Contains(text, "card table: none") || !strings.Contains(text, "boom") {
		t.Errorf("an error is said, not hidden:\n%s", text)
	}
}

// The headline says what holds the card: a LEASE of a class. "GPU: held by text  pid 792210"
// read as a text SEAT holding the card, and two sessions argued over who held it (F9,
// 2026-10-07); the holder was a bench's reservation. Both classes say "lease", and an
// exclusive one keeps saying so.
func TestGPUStatusHeadlineNamesALeaseOfAClassNotABareClass(t *testing.T) {
	for _, tc := range []struct {
		class     gpulease.Class
		exclusive bool
		want      string
	}{
		{gpulease.ClassText, true, "a text-class lease"},
		{gpulease.ClassText, false, "a text-class lease"},
		{gpulease.ClassMedia, false, "a media-class lease"},
	} {
		name := string(tc.class)
		if tc.exclusive {
			name += "-exclusive"
		}
		t.Run(name, func(t *testing.T) {
			cfg, m := leaseFixture(t)
			useQuietStatus(t, nil, errors.New("nvidia-smi not found"))
			l, err := m.TryAcquire(tc.class, gpulease.Options{Reason: "bench", TTL: time.Hour, Exclusive: tc.exclusive})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l.Release() }()

			text := captureStdout(t, func() {
				if err := runGPUStatus([]string{"--config", cfg}); err != nil {
					t.Fatal(err)
				}
			})
			head := "GPU: held by " + tc.want + "  pid " + strconv.Itoa(os.Getpid()) + "  epoch "
			if !strings.Contains(text, head) {
				t.Errorf("the headline must lead with %q:\n%s", head, text)
			}
			if bare := "held by " + string(tc.class) + " "; strings.Contains(text, bare) {
				t.Errorf("the headline names the bare class (%q), which reads as a seat holding the card:\n%s", bare, text)
			}
			if excl := strings.Contains(text, "(exclusive: text loads wait or route elsewhere)"); excl != (tc.exclusive && tc.class == gpulease.ClassText) {
				t.Errorf("an exclusive text lease keeps saying so (and a plain one does not): exclusive line present = %v\n%s", excl, text)
			}
		})
	}
}

// The error a fail-fast detached reserve (--wait 0) reports when another reservation won the card
// names that holder as a LEASE of a class too: "another holder took the GPU first: text (pid ..."
// read as a text seat, the same defect as the headline above.
func TestForeignHolderErrorNamesALeaseOfAClassNotABareClass(t *testing.T) {
	for _, tc := range []struct {
		class gpulease.Class
		want  string
	}{
		{gpulease.ClassText, `another holder took the GPU first: a text-class lease (pid 4242, reason "kv bench")`},
		{gpulease.ClassMedia, `another holder took the GPU first: a media-class lease (pid 4242, reason "kv bench")`},
		{gpulease.Class(""), `another holder took the GPU first: a lease (pid 4242, reason "kv bench")`},
	} {
		got := foreignHolderError(gpulease.Info{Held: true, Class: tc.class, PID: 4242, Reason: "kv bench"}).Error()
		if got != tc.want {
			t.Errorf("class %q: got %q, want %q", string(tc.class), got, tc.want)
		}
	}
}
