package modelaffinity

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// Review fixes to the legacy-scope consumers (plan P4): the rule is off until a host turns it
// on, it reads only what an older binary wrote, and a READ of the lease writes and unloads
// nothing.

var card2Tree = []gpulease.TreeProc{{PID: 1, Cmdline: "python main.py --cuda-device 2"}}

func seenFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "seen.") {
			out = append(out, e.Name())
		}
	}
	return out
}

func ledgerLines(t *testing.T, leaseDir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(leaseDir), "scope-ledger.jsonl"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestLegacyInferenceIsOffUntilTheHostTurnsItOn(t *testing.T) {
	b := legacyScopeBox(t)
	armSeatScope(t, tripleBoxPins)
	b.tree = card2Tree

	SetLegacyInference(false)
	resetScopers()
	if got := SeatLease("seat-card0"); !got.Held {
		t.Fatalf("inference off: a legacy lease fences every seat, as it always did, got %+v", got)
	}
	info := ScopeInfo(b.m.Dir(), b.m.Inspect())
	if info.ScopeKind() != gpulease.ScopeWholeNode || !strings.Contains(info.ScopeWhy, "gpu_legacy_scope_inference") {
		t.Fatalf("status must say the rule is off and which key turns it on, got %s %q", info.ScopeKind(), info.ScopeWhy)
	}
	if n := len(seenFiles(t, b.m.Dir())); n != 0 {
		t.Fatalf("inference off: nothing is written, found %d sidecar(s)", n)
	}

	SetLegacyInference(true)
	resetScopers()
	if got := SeatLease("seat-card0"); got.Held {
		t.Fatalf("inference on: the card-2 render leaves card 0 free, got %+v", got)
	}
}

// A whole-node lease from a binary that could have named cards is the whole node by its
// writer's word: no command line narrows it, whatever the host's switch says.
func TestWholeNodeLeaseFromThisBinaryIsNeverInferredAtTheSeatGate(t *testing.T) {
	m := armLease(t)
	armSeatScope(t, tripleBoxPins)
	prevTree := scopeTree
	scopeTree = func(int) ([]gpulease.TreeProc, error) { return card2Tree, nil }
	SetLegacyInference(true)
	t.Cleanup(func() { scopeTree = prevTree; SetLegacyInference(false); resetScopers() })
	resetScopers()
	// `gpu reserve --whole-node`, or the pipeline's own media lease: this binary wrote it.
	holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "everything", Command: "python main.py --cuda-device 2"})
	if got := SeatLease("seat-card0"); !got.Held {
		t.Fatalf("a whole-node record this binary wrote fences card 0, got %+v", got)
	}
	if n := len(seenFiles(t, m.Dir())); n != 0 {
		t.Fatalf("a record that is not legacy is never inferred and never leaves a sidecar, found %d", n)
	}
}

// The status surfaces and the delegator's reads are inspections: they report the scope the
// evidence implies and write nothing, and nothing a read does unloads a model.
func TestStatusReadsNeverWriteOrUnload(t *testing.T) {
	b := legacyScopeBox(t)
	armSeatScope(t, tripleBoxPins)
	f := armYield(t, map[string]int{"seat-card0": 0, "seat-card2": 0})
	b.tree = card2Tree
	dir := b.m.Dir()

	info := ScopeInfo(dir, b.m.Inspect())
	if info.ScopeKind() != gpulease.ScopeInferred {
		t.Fatalf("an inspection still reports the scope, got %s (%s)", info.ScopeKind(), info.ScopeWhy)
	}
	if got := ScopeLeases(dir, b.m.Leases()); len(got) != 1 || got[0].ScopeKind() != gpulease.ScopeInferred {
		t.Fatalf("ScopeLeases: %+v", got)
	}
	if fn := ScopeFunc("", b.m.Root()); fn == nil || fn(b.m.Inspect()).ScopeKind() != gpulease.ScopeInferred {
		t.Fatal("ScopeFunc must scope what it is handed")
	}
	if held, _ := CardsHeld([]string{"0"}); held {
		t.Fatal("a card-2 render leaves a card-0 seat's card free")
	}
	if held, _ := CardsHeld([]string{"2"}); !held {
		t.Fatal("the card-2 seat's card is taken")
	}

	if n := len(seenFiles(t, dir)); n != 0 {
		t.Fatalf("an inspection wrote %d sidecar(s)", n)
	}
	if lines := ledgerLines(t, dir); len(lines) != 0 {
		t.Fatalf("an inspection wrote the ledger: %v", lines)
	}
	if got := f.unloads(); len(got) != 0 {
		t.Fatalf("an inspection unloaded %v", got)
	}
	if n := f.gaugeReads(); n != 0 {
		t.Fatalf("an inspection made %d call(s) to the engine", n)
	}
}

// The load gate is the one reader that remembers what it saw: it writes the sidecar and one
// ledger line, once, and still unloads nothing.
func TestTheLoadGateRemembersTheScopeAndUnloadsNothing(t *testing.T) {
	b := legacyScopeBox(t)
	armSeatScope(t, tripleBoxPins)
	f := armYield(t, map[string]int{"seat-card0": 0, "seat-card2": 0})
	b.tree = card2Tree
	dir := b.m.Dir()

	if got := SeatLease("seat-card0"); got.Held {
		t.Fatalf("card 0 is free of the card-2 render, got %+v", got)
	}
	if n := len(seenFiles(t, dir)); n != 1 {
		t.Fatalf("the gate persists the sticky set, found %d sidecar(s)", n)
	}
	lines := ledgerLines(t, dir)
	if len(lines) != 1 || !strings.Contains(lines[0], `"scoped"`) {
		t.Fatalf("one ledger line for the establishment, got %v", lines)
	}
	b.clock = b.clock.Add(2 * time.Minute)
	SeatLease("seat-card0")
	if lines := ledgerLines(t, dir); len(lines) != 1 {
		t.Fatalf("no change, no new line, got %v", lines)
	}
	if got := f.unloads(); len(got) != 0 {
		t.Fatalf("the gate unloaded %v", got)
	}
}

// A seat that cannot be checked is said, never folded into "not loaded".
func TestYieldIfFencedSaysWhenTheSeatCannotBeChecked(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	f := armYield(t, map[string]int{"seat-card0": 0})
	f.readErr = errors.New("connection refused")
	mediaLeaseOn(t, m, idCard0)

	yielded, why := YieldIfFenced(context.Background(), "http://yield-unreadable", "seat-card0")
	if yielded {
		t.Fatal("nothing was unloaded")
	}
	if !strings.Contains(why, "seat-card0") || !strings.Contains(why, "connection refused") || !strings.Contains(why, "left resident") {
		t.Fatalf("the reason names the seat, the error and that it stayed resident: %q", why)
	}
}

func TestReleaseLogsASeatThatCouldNotBeCheckedOnAHeldCard(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	f := armYield(t, map[string]int{"seat-card0": 0})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	tk, err := Admit(context.Background(), "http://yield-release-log", "seat-card0", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	mediaLeaseOn(t, m, idCard0)
	f.readErr = errors.New("connection refused")
	tk.Release()
	if out := buf.String(); !strings.Contains(out, "connection refused") || !strings.Contains(out, "seat-card0") {
		t.Fatalf("a release that could not check its seat on a held card must say so, log: %q", out)
	}
}
