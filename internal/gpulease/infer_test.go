package gpulease

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The legacy lease (plan P4): a whole-node record written by a binary that predates
// card-scoped leases. It names no cards, so the only way a card it does not use stops being
// fenced is evidence about what its process tree actually runs. These tests drive the
// evidence rule with synthetic facts (a fake clock, a fake process tree, a fake launch
// marker, a fake card sampler); the live capture of what a real tree shows is deferred to the
// milestone that enables card-scoped leases on a host (plan P6).

// inferBox is the synthetic 3-card box: nvidia-smi order 0 / 1 / 2, card 1 the display card,
// and ComfyUI's FASTEST_FIRST order card 1 (the faster display card) = 0, card 0 = 1, card
// 2 = 2.
func inferCards() []gpuprobe.Card {
	return []gpuprobe.Card{
		{UUID: "GPU-aaaa0000", NvidiaIndex: 0, ComfyOrder: 1},
		{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Display: true, ComfyOrder: 0},
		{UUID: "GPU-cccc0000", NvidiaIndex: 2, ComfyOrder: 2},
	}
}

type inferHarness struct {
	t     *testing.T
	dir   string
	clock *time.Time
	sc    *Scoper

	treeCalls  atomic.Int64
	mu         sync.Mutex
	tree       []TreeProc
	treeErr    error
	marker     *Marker
	treeCards  []string
	treeCardOK bool
	present    bool
	events     []ScopeEvent
	warns      []string
}

func newInferHarness(t *testing.T) *inferHarness {
	t.Helper()
	h := &inferHarness{t: t, dir: filepath.Join(t.TempDir(), "gpu", "lease")}
	if err := os.MkdirAll(h.dir, 0o777); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	h.clock = &now
	h.sc = h.newScoper()
	return h
}

// newScoper builds a Scoper over this harness's seams; a second call is a second "process"
// that shares nothing with the first but the directory.
func (h *inferHarness) newScoper() *Scoper {
	return &Scoper{
		Dir:   h.dir,
		Now:   func() time.Time { return *h.clock },
		Cards: func() ([]gpuprobe.Card, error) { return inferCards(), nil },
		Tree: func(root int) ([]TreeProc, error) {
			h.treeCalls.Add(1)
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.tree, h.treeErr
		},
		Marker: func() (Marker, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.marker == nil {
				return Marker{}, false
			}
			return *h.marker, true
		},
		TreeCards: func([]int) ([]string, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.treeCards, h.treeCardOK
		},
		Present:     func() bool { return h.present },
		Live:        func(uint64) bool { return true },
		StartUnixMs: func(ms int64) (int64, bool) { return ms, true },
		Warn: func(msg string) {
			h.mu.Lock()
			h.warns = append(h.warns, msg)
			h.mu.Unlock()
		},
		Audit: func(e ScopeEvent) {
			h.mu.Lock()
			h.events = append(h.events, e)
			h.mu.Unlock()
		},
	}
}

func (h *inferHarness) advance(d time.Duration) { *h.clock = h.clock.Add(d) }

// legacy is a whole-node lease record the way an old binary wrote it: epoch 1205, held by the
// wrapper pid 4242, three hours old.
func (h *inferHarness) legacy(age time.Duration, command string) Info {
	return Info{Held: true, Class: ClassMedia, Epoch: 1205, Epochs: []uint64{1205}, PID: 4242,
		Age: age, Command: command, Reason: "film", Legacy: true}
}

func (h *inferHarness) warnsSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.warns...)
}

func (h *inferHarness) seenPath() string { return filepath.Join(h.dir, "seen.1205") }

func (h *inferHarness) eventsSnapshot() []ScopeEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ScopeEvent(nil), h.events...)
}

func tree(procs ...TreeProc) []TreeProc { return procs }

func TestLegacyLeaseScopedByCommandLineEvidence(t *testing.T) {
	h := newInferHarness(t)
	// The wrapper runs a script; ComfyUI, its grandchild, was started with --cuda-device 2,
	// which in ComfyUI's order is the card at nvidia-smi index 2.
	h.tree = tree(
		TreeProc{PID: 4242, PPID: 1, Cmdline: `local-offload gpu reserve -- python run_film.py`},
		TreeProc{PID: 5001, PPID: 4242, Cmdline: `python run_film.py --spec film.json`},
		TreeProc{PID: 5002, PPID: 5001, Cmdline: `python main.py --listen 127.0.0.1 --cuda-device 2 --port 8188`},
	)
	got := h.sc.Scope(h.legacy(3*time.Hour, "python run_film.py"))
	if got.ScopeKind() != ScopeInferred || !reflect.DeepEqual(got.Inferred, []string{idCard2}) {
		t.Fatalf("a ComfyUI tree on --cuda-device 2 scopes the lease to card 2, got %v (%s: %s)", got.Inferred, got.ScopeKind(), got.ScopeWhy)
	}
	if got.Touches([]string{idCard0}) || !got.Touches([]string{idCard2}) {
		t.Fatal("card 0 is free of the inferred lease and card 2 is not")
	}
	if len(got.Devices) != 0 {
		t.Fatalf("the declared devices stay empty (the record is a whole-node claim), got %v", got.Devices)
	}
	if !strings.Contains(got.ScopeWhy, "command line") {
		t.Errorf("the reason names its evidence: %q", got.ScopeWhy)
	}
}

func TestLegacyLeaseScopedByTheRecordedCommand(t *testing.T) {
	h := newInferHarness(t)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: `local-offload gpu reserve -- python main.py`})
	got := h.sc.Scope(h.legacy(time.Hour, "python main.py --cuda-device 1 --port 8188"))
	// ComfyUI order 1 is nvidia-smi card 0.
	if !reflect.DeepEqual(got.Inferred, []string{idCard0}) {
		t.Fatalf("the recorded wrapped command is evidence too: want card 0, got %v (%s)", got.Inferred, got.ScopeWhy)
	}
}

func TestMarkerNotTiedToLeaseIsNotEvidence(t *testing.T) {
	leaseStart := func(h *inferHarness, age time.Duration) time.Time { return h.clock.Add(-age) }

	t.Run("a leftover marker from before the lease proves nothing", func(t *testing.T) {
		h := newInferHarness(t)
		h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python run.py"})
		h.marker = &Marker{PID: 9001, OwnerPID: 7777, StartedAtMs: leaseStart(h, 5*time.Hour).UnixMilli(), Args: []string{"main.py", "--cuda-device", "2"}}
		got := h.sc.Scope(h.legacy(3*time.Hour, "python run.py"))
		if got.ScopeKind() != ScopeWholeNode {
			t.Fatalf("a marker older than the lease, with no tree tie, must not scope it: got %v", got.Inferred)
		}
	})
	t.Run("a marker whose owner is in the wrapper's tree is tied by ancestry", func(t *testing.T) {
		h := newInferHarness(t)
		h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python run.py"}, TreeProc{PID: 5001, PPID: 4242, Cmdline: "node render.mjs"})
		// Even a marker stamped long before is tied when the node process that spawned
		// ComfyUI is this lease's own descendant.
		h.marker = &Marker{PID: 9001, OwnerPID: 5001, StartedAtMs: leaseStart(h, 9*time.Hour).UnixMilli(), Args: []string{"main.py", "--cuda-device", "2"}}
		got := h.sc.Scope(h.legacy(3*time.Hour, "python run.py"))
		if !reflect.DeepEqual(got.Inferred, []string{idCard2}) || !strings.Contains(got.ScopeWhy, "launch marker") {
			t.Fatalf("a marker owned by a descendant is evidence: got %v (%s)", got.Inferred, got.ScopeWhy)
		}
	})
	t.Run("a marker started after the lease and claimed by no other lease is tied by time", func(t *testing.T) {
		h := newInferHarness(t)
		h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python run.py"})
		h.marker = &Marker{PID: 9001, OwnerPID: 7777, StartedAtMs: leaseStart(h, time.Hour).UnixMilli(), Args: []string{"main.py", "--cuda-device", "2"}}
		got := h.sc.Scope(h.legacy(3*time.Hour, "python run.py"))
		if !reflect.DeepEqual(got.Inferred, []string{idCard2}) {
			t.Fatalf("a marker born inside the lease window is evidence: got %v (%s)", got.Inferred, got.ScopeWhy)
		}
	})
	t.Run("a marker another live lease claims is not this lease's", func(t *testing.T) {
		h := newInferHarness(t)
		h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python run.py"})
		h.marker = &Marker{PID: 9001, OwnerPID: 7777, StartedAtMs: leaseStart(h, time.Hour).UnixMilli(), Args: []string{"main.py", "--cuda-device", "2"}}
		other := Info{Held: true, Class: ClassMedia, Epoch: 1300, Epochs: []uint64{1205, 1300}, Devices: []string{idCard2}}
		in := h.legacy(3*time.Hour, "python run.py")
		in.Epochs = []uint64{1205, 1300}
		in.Leases = []Info{func() Info { l := in; l.Leases = nil; l.Epochs = []uint64{1205}; return l }(), other}
		got := h.sc.Scope(in)
		legacy := got.Each()[0]
		if legacy.ScopeKind() != ScopeWholeNode {
			t.Fatalf("card 2 is another lease's: the marker cannot scope the legacy lease to it, got %v", legacy.Inferred)
		}
	})
}

func TestLegacyLeaseWithoutEvidenceStaysWholeNode(t *testing.T) {
	h := newInferHarness(t)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "local-offload gpu reserve -- python run_film.py"})
	got := h.sc.Scope(h.legacy(3*time.Hour, "python run_film.py"))
	if got.ScopeKind() != ScopeWholeNode || len(got.Inferred) != 0 {
		t.Fatalf("no evidence: the lease stays whole-node, got %v", got.Inferred)
	}
	for _, id := range []string{idCard0, idCard1, idCard2} {
		if !got.Touches([]string{id}) {
			t.Errorf("a whole-node lease touches %s", id)
		}
	}
	// And it says exactly why, so the operator knows what evidence would change it.
	for _, want := range []string{"command line", "launch marker", "sampled"} {
		if !strings.Contains(got.ScopeWhy, want) {
			t.Errorf("the reason must say what was missing (%q): %s", want, got.ScopeWhy)
		}
	}
	if _, err := os.Stat(h.seenPath()); err == nil {
		t.Error("nothing was inferred and no sample was due: nothing may be written")
	}
}

func TestUnresolvableComfyOrderIsNamedNotGuessed(t *testing.T) {
	h := newInferHarness(t)
	h.sc.Cards = func() ([]gpuprobe.Card, error) {
		cs := inferCards()
		for i := range cs {
			cs[i].ComfyOrder = -1 // the box never declared gpu_comfy_order
		}
		return cs, nil
	}
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"})
	got := h.sc.Scope(h.legacy(3*time.Hour, "python main.py --cuda-device 2"))
	if got.ScopeKind() != ScopeWholeNode {
		t.Fatalf("an index in an order the box never declared must not be guessed onto a card, got %v", got.Inferred)
	}
	if !strings.Contains(got.ScopeWhy, "gpu_comfy_order") {
		t.Errorf("the reason must name the fix: %s", got.ScopeWhy)
	}
}

func TestInferenceIsStickyAndWidensWithLedgerLine(t *testing.T) {
	h := newInferHarness(t)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"})
	first := h.sc.Scope(h.legacy(3*time.Hour, "x"))
	if !reflect.DeepEqual(first.Inferred, []string{idCard2}) || first.ScopeWidened {
		t.Fatalf("first establishment: want card 2, not widened, got %v widened=%v", first.Inferred, first.ScopeWidened)
	}
	ev := h.eventsSnapshot()
	if len(ev) != 1 || ev[0].Kind != "scoped" || !reflect.DeepEqual(ev[0].To, []string{idCard2}) || ev[0].Epoch != 1205 {
		t.Fatalf("establishing a scope writes one ledger line, got %+v", ev)
	}

	// The job grows a second ComfyUI on card 0 (ComfyUI order 1): the scope WIDENS, never
	// replaces, and the widening is logged.
	h.advance(2 * time.Minute)
	h.tree = tree(
		TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"},
		TreeProc{PID: 4300, PPID: 4242, Cmdline: "python main.py --cuda-device 1 --port 8189"},
	)
	h.sc = h.newScoper() // a different process: only the sidecar carries the memory
	second := h.sc.Scope(h.legacy(3*time.Hour, "x"))
	if !reflect.DeepEqual(second.Inferred, []string{idCard0, idCard2}) || !second.ScopeWidened {
		t.Fatalf("widening: want cards 0 and 2, widened, got %v widened=%v", second.Inferred, second.ScopeWidened)
	}
	ev = h.eventsSnapshot()
	if len(ev) != 2 || ev[1].Kind != "widened" || !reflect.DeepEqual(ev[1].From, []string{idCard2}) || !reflect.DeepEqual(ev[1].To, []string{idCard0, idCard2}) {
		t.Fatalf("a widening writes a ledger line naming from and to, got %+v", ev)
	}

	// The second ComfyUI exits: the evidence now names only card 2, and the scope does NOT
	// shrink (a lease that has been seen on a card is never reported free of it).
	h.advance(2 * time.Minute)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"})
	h.sc = h.newScoper()
	third := h.sc.Scope(h.legacy(3*time.Hour, "x"))
	if !reflect.DeepEqual(third.Inferred, []string{idCard0, idCard2}) {
		t.Fatalf("the scope never shrinks, got %v", third.Inferred)
	}
	if len(h.eventsSnapshot()) != 2 {
		t.Fatalf("no change, no new ledger line, got %+v", h.eventsSnapshot())
	}
}

func TestDisplayCardNeverInferredFreeWhilePresent(t *testing.T) {
	h := newInferHarness(t)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"})

	h.present = true
	atDesk := h.sc.Scope(h.legacy(3*time.Hour, "x"))
	if !atDesk.Touches([]string{idCard1}) {
		t.Fatalf("the operator is at the desk: the display card is never reported free by an inference, got %v", atDesk.EffectiveDevices())
	}
	if atDesk.Touches([]string{idCard0}) {
		t.Fatal("card 0 is not the display card and the evidence does not name it: free")
	}
	if !strings.Contains(atDesk.ScopeWhy, "display card") {
		t.Errorf("the status must say why card 1 is in the set: %q", atDesk.ScopeWhy)
	}

	away := newInferHarness(t)
	away.tree = h.tree
	away.present = false
	got := away.sc.Scope(away.legacy(3*time.Hour, "x"))
	if got.Touches([]string{idCard1}) {
		t.Fatalf("the operator is away and the evidence names card 2 only: the display card is free, got %v", got.EffectiveDevices())
	}

	// Unknown presence reads as present: a reader that cannot tell never frees the display card.
	unknown := newInferHarness(t)
	unknown.tree = h.tree
	unknown.sc.Present = nil
	if got := unknown.sc.Scope(unknown.legacy(3*time.Hour, "x")); !got.Touches([]string{idCard1}) {
		t.Fatalf("an unreadable presence is present, got %v", got.EffectiveDevices())
	}
}

func TestSampledCardsNeedTwoSamplesFiveMinutesApartOnATenMinuteLease(t *testing.T) {
	h := newInferHarness(t)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python run.py"}, TreeProc{PID: 5001, PPID: 4242, Cmdline: "python render.py"})
	h.treeCards, h.treeCardOK = []string{idCard0}, true

	// A lease younger than ten minutes is never judged by sampling: its first minutes are
	// model loads that show on cards the job will not keep.
	young := h.sc.Scope(h.legacy(4*time.Minute, "x"))
	if young.ScopeKind() != ScopeWholeNode {
		t.Fatalf("a 4-minute-old lease must not be scoped by sampling, got %v", young.Inferred)
	}
	if _, err := os.Stat(h.seenPath()); err == nil {
		t.Fatal("no sample may be recorded for a lease younger than ten minutes")
	}

	// Old enough: the first sample alone proves nothing.
	h.advance(10 * time.Minute)
	first := h.sc.Scope(h.legacy(14*time.Minute, "x"))
	if first.ScopeKind() != ScopeWholeNode {
		t.Fatalf("one sample is not evidence, got %v", first.Inferred)
	}
	// The same process asking again a moment later takes no second sample (the throttle).
	h.advance(30 * time.Second)
	if again := h.sc.Scope(h.legacy(15*time.Minute, "x")); again.ScopeKind() != ScopeWholeNode {
		t.Fatalf("30 s later is not 5 minutes later, got %v", again.Inferred)
	}
	// Six minutes after the first sample, from a different process: the second sample
	// agrees, and the scope is established.
	h.advance(6 * time.Minute)
	h.sc = h.newScoper()
	got := h.sc.Scope(h.legacy(21*time.Minute, "x"))
	if got.ScopeKind() != ScopeInferred || !reflect.DeepEqual(got.Inferred, []string{idCard0}) {
		t.Fatalf("two agreeing samples five minutes apart scope the lease to card 0, got %v (%s)", got.Inferred, got.ScopeWhy)
	}
	if !strings.Contains(got.ScopeWhy, "sampled") {
		t.Errorf("the reason names its evidence: %q", got.ScopeWhy)
	}
}

func TestSamplingNeedsTheDriverToNameProcesses(t *testing.T) {
	// Under WDDM nvidia-smi often lists no compute apps: an empty answer is no evidence,
	// never "the tree uses no card".
	h := newInferHarness(t)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python run.py"})
	h.treeCards, h.treeCardOK = nil, false
	h.advance(10 * time.Minute)
	h.sc.Scope(h.legacy(14*time.Minute, "x"))
	h.advance(6 * time.Minute)
	got := h.sc.Scope(h.legacy(21*time.Minute, "x"))
	if got.ScopeKind() != ScopeWholeNode {
		t.Fatalf("an unreadable sample is no evidence, got %v", got.Inferred)
	}
	if _, err := os.Stat(h.seenPath()); err == nil {
		t.Fatal("a sample the driver could not name is recorded as nothing at all: the sidecar must not exist")
	}
}

func TestSeenSidecarIsWhatMakesItStickyAcrossProcesses(t *testing.T) {
	h := newInferHarness(t)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"})
	h.sc.Scope(h.legacy(3*time.Hour, "x"))

	raw, err := os.ReadFile(h.seenPath())
	if err != nil {
		t.Fatalf("an established scope is persisted as seen.<epoch>: %v", err)
	}
	var rec map[string]any
	if json.Unmarshal(raw, &rec) != nil {
		t.Fatalf("the sidecar is JSON: %s", raw)
	}
	// A reader that has no evidence at all (the tree is gone) still reports the scope.
	h.tree = nil
	h.treeErr = errors.New("tree unreadable")
	h.sc = h.newScoper()
	h.advance(5 * time.Minute)
	got := h.sc.Scope(h.legacy(3*time.Hour, "x"))
	if !reflect.DeepEqual(got.Inferred, []string{idCard2}) {
		t.Fatalf("the sidecar carries the scope when the evidence is gone, got %v (%s)", got.Inferred, got.ScopeWhy)
	}
}

func TestInferenceTouchesOnlyLegacyLeases(t *testing.T) {
	h := newInferHarness(t)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"})
	declared := Info{Held: true, Class: ClassMedia, Epoch: 8, Epochs: []uint64{8}, Devices: []string{idCard0}, PID: 4242}
	got := h.sc.Scope(declared)
	if !reflect.DeepEqual(got.EffectiveDevices(), []string{idCard0}) || got.ScopeKind() != ScopeDeclared {
		t.Fatalf("a declared lease is not re-scoped: %v %s", got.EffectiveDevices(), got.ScopeKind())
	}
	if h.treeCalls.Load() != 0 {
		t.Fatal("a declared lease needs no process-tree read")
	}
	if got := h.sc.Scope(Info{}); got.Held {
		t.Fatal("nothing held, nothing scoped")
	}
}

func TestScopeIsThrottledPerLease(t *testing.T) {
	h := newInferHarness(t)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python run.py"})
	in := h.legacy(3*time.Hour, "x")
	h.sc.Scope(in)
	h.sc.Scope(in)
	h.sc.Scope(in)
	if n := h.treeCalls.Load(); n != 1 {
		t.Fatalf("three reads inside the recheck window enumerated the process tree %d times, want 1", n)
	}
	h.advance(scopeRecheck + time.Second)
	h.sc.Scope(in)
	if n := h.treeCalls.Load(); n != 2 {
		t.Fatalf("past the window the tree is read again, got %d reads", n)
	}
}

func TestReleaseClearsTheSeenSidecar(t *testing.T) {
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(m.leaseDir(), "seen."+itoa(l.Epoch()))
	if err := os.WriteFile(seen, []byte(`{"epoch":1}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(seen); err == nil {
		t.Fatal("releasing a legacy lease must remove its seen sidecar")
	}
}

// ---- review fixes: what the sidecar, the process tree and the command lines may be trusted for ----

// A stub of the wrapper's own record, the way the tree seam reports a process: the root of the
// tree is the wrapper itself.
func rootAt(pid int, startMs int64, cmdline string) TreeProc {
	return TreeProc{PID: pid, PPID: 1, StartMs: startMs, Cmdline: cmdline}
}

// The sticky set is shared by every process that reads the lease, so it is merged under the
// epoch lock and never overwritten from a stale snapshot. Scoper A reads the empty sidecar, then
// stalls inside its process-tree read; scoper B runs to the end and infers cards 0 and 2; A
// resumes with narrower evidence. The set on disk must still be B's, and no second ledger line
// may be written for a set that was already there.
func TestInterleavedScopersNeverShrinkTheStickySet(t *testing.T) {
	h := newInferHarness(t)
	atTree := make(chan struct{})
	release := make(chan struct{})
	a := h.newScoper()
	a.Tree = func(int) ([]TreeProc, error) {
		close(atTree)
		<-release
		return tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"}), nil
	}
	done := make(chan Info, 1)
	go func() { done <- a.Scope(h.legacy(3*time.Hour, "x")) }()
	<-atTree

	h.tree = tree(
		TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"},
		TreeProc{PID: 4300, PPID: 4242, Cmdline: "python main.py --cuda-device 1 --port 8189"},
	)
	b := h.newScoper()
	if got := b.Scope(h.legacy(3*time.Hour, "x")); !reflect.DeepEqual(got.Inferred, []string{idCard0, idCard2}) {
		t.Fatalf("scoper B sees both cards, got %v (%s)", got.Inferred, got.ScopeWhy)
	}

	close(release)
	got := <-done
	if !reflect.DeepEqual(got.Inferred, []string{idCard0, idCard2}) {
		t.Errorf("A reports the merged set, never its own narrower evidence: %v", got.Inferred)
	}
	raw, err := os.ReadFile(h.seenPath())
	if err != nil {
		t.Fatal(err)
	}
	var rec seenRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec.Devices, []string{idCard0, idCard2}) {
		t.Fatalf("the sidecar shrank to %v: a slow reader overwrote a faster one's wider set", rec.Devices)
	}
	if ev := h.eventsSnapshot(); len(ev) != 1 || ev[0].Kind != "scoped" {
		t.Fatalf("one process established the scope, so one ledger line, got %+v", ev)
	}
	// A new process starting after both starts from the wide set, not from card 2 alone.
	h.tree = nil
	h.treeErr = errors.New("tree unreadable")
	if c := h.newScoper().Scope(h.legacy(3*time.Hour, "x")); !reflect.DeepEqual(c.Inferred, []string{idCard0, idCard2}) {
		t.Fatalf("a fresh process reads the wide set from the sidecar, got %v", c.Inferred)
	}
}

// Many readers arriving together on a lease nobody has scoped yet read the evidence once.
func TestConcurrentFirstReadsGatherTheEvidenceOnce(t *testing.T) {
	h := newInferHarness(t)
	release := make(chan struct{})
	h.sc.Tree = func(int) ([]TreeProc, error) {
		h.treeCalls.Add(1)
		<-release
		return tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"}), nil
	}
	const readers = 12
	results := make(chan Info, readers)
	for i := 0; i < readers; i++ {
		go func() { results <- h.sc.Scope(h.legacy(3*time.Hour, "x")) }()
	}
	// Give every reader time to reach the evidence read; with no single-flight each of them
	// would be inside the tree walk by now.
	deadline := time.Now().Add(2 * time.Second)
	for h.treeCalls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if n := h.treeCalls.Load(); n != 1 {
		t.Fatalf("%d concurrent first readers walked the process tree %d times, want 1", readers, n)
	}
	close(release)
	for i := 0; i < readers; i++ {
		if got := <-results; !reflect.DeepEqual(got.Inferred, []string{idCard2}) {
			t.Fatalf("every reader gets the one result, got %v", got.Inferred)
		}
	}
	if ev := h.eventsSnapshot(); len(ev) != 1 {
		t.Fatalf("one establishment, one ledger line, got %+v", ev)
	}
}

// A lease whose tree carries a ComfyUI on a card the box can place and another on a card it
// cannot place is not scoped to the first: the second one's card is unknown, and every doubt
// fences.
func TestPartialCommandLineEvidenceStaysWholeNode(t *testing.T) {
	h := newInferHarness(t)
	h.tree = tree(
		TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2 --port 8188"},
		TreeProc{PID: 4300, PPID: 4242, Cmdline: "python main.py --cuda-device 9 --port 8189"},
	)
	got := h.sc.Scope(h.legacy(3*time.Hour, "x"))
	if got.ScopeKind() != ScopeWholeNode || len(got.Inferred) != 0 {
		t.Fatalf("one process names a card the table cannot place: the lease stays whole-node, got %v (%s)", got.Inferred, got.ScopeWhy)
	}
	if !strings.Contains(got.ScopeWhy, "cannot place") {
		t.Errorf("the reason names the card that could not be placed: %q", got.ScopeWhy)
	}
	if _, err := os.Stat(h.seenPath()); err == nil {
		t.Error("nothing was inferred: nothing may be written")
	}
}

// A record that names no holder pid has no wrapper to read a tree from. Asking the process
// table for pid 0 would adopt the system process and every command line on the box.
func TestRecordWithoutAHolderPidIsNeverWalked(t *testing.T) {
	for _, pid := range []int{0, -1} {
		h := newInferHarness(t)
		h.tree = tree(TreeProc{PID: 77, PPID: 0, Cmdline: "python main.py --cuda-device 2"})
		in := h.legacy(3*time.Hour, "x")
		in.PID = pid
		got := h.sc.Scope(in)
		if n := h.treeCalls.Load(); n != 0 {
			t.Fatalf("pid %d: the process table was read %d time(s) for a record with no holder", pid, n)
		}
		if got.ScopeKind() != ScopeWholeNode || !strings.Contains(got.ScopeWhy, "no holder pid") {
			t.Fatalf("pid %d: want whole-node naming the missing pid, got %s %q", pid, got.ScopeKind(), got.ScopeWhy)
		}
	}
}

// A wrapper pid that started after the lease was taken is a recycled pid: its tree is a
// stranger's, whatever command lines it carries.
func TestRecycledWrapperPidIsNotBelieved(t *testing.T) {
	h := newInferHarness(t)
	leaseStart := h.clock.Add(-3 * time.Hour)
	stranger := rootAt(4242, leaseStart.Add(40*time.Minute).UnixMilli(), "python other.py")
	h.tree = tree(stranger, TreeProc{PID: 5002, PPID: 4242, Cmdline: "python main.py --cuda-device 2"})
	got := h.sc.Scope(h.legacy(3*time.Hour, "x"))
	if got.ScopeKind() != ScopeWholeNode {
		t.Fatalf("a root that started after the lease is not its wrapper: want whole-node, got %v", got.Inferred)
	}
	if !strings.Contains(got.ScopeWhy, "started after the lease") {
		t.Errorf("the reason says why the tree was not read: %q", got.ScopeWhy)
	}

	// The real wrapper started before the lease: its tree is read as ever.
	ok := newInferHarness(t)
	okStart := ok.clock.Add(-3 * time.Hour)
	ok.tree = tree(rootAt(4242, okStart.Add(-2*time.Second).UnixMilli(), "local-offload gpu reserve"),
		TreeProc{PID: 5002, PPID: 4242, Cmdline: "python main.py --cuda-device 2"})
	if got := ok.sc.Scope(ok.legacy(3*time.Hour, "x")); !reflect.DeepEqual(got.Inferred, []string{idCard2}) {
		t.Fatalf("a wrapper older than the lease is believed, got %v (%s)", got.Inferred, got.ScopeWhy)
	}
	// An unreadable start time is not a verdict either way.
	unk := newInferHarness(t)
	unk.tree = tree(rootAt(4242, 0, "local-offload gpu reserve"), TreeProc{PID: 5002, PPID: 4242, Cmdline: "python main.py --cuda-device 2"})
	if got := unk.sc.Scope(unk.legacy(3*time.Hour, "x")); !reflect.DeepEqual(got.Inferred, []string{idCard2}) {
		t.Fatalf("an unreadable start time does not discard the tree, got %v (%s)", got.Inferred, got.ScopeWhy)
	}
}

// An unreadable process table is said, not folded into "no command line names a card".
func TestUnreadableProcessTableIsNamedInTheReason(t *testing.T) {
	h := newInferHarness(t)
	h.treeErr = errors.New("access denied")
	got := h.sc.Scope(h.legacy(3*time.Hour, "python run_film.py"))
	if got.ScopeKind() != ScopeWholeNode {
		t.Fatalf("no evidence: whole-node, got %v", got.Inferred)
	}
	if !strings.Contains(got.ScopeWhy, "process table") || !strings.Contains(got.ScopeWhy, "access denied") {
		t.Errorf("the reason must name the unreadable process table and why: %q", got.ScopeWhy)
	}
}

// A scoper that is an inspector reports what the evidence implies and writes nothing at all.
func TestReadOnlyScoperWritesNothing(t *testing.T) {
	h := newInferHarness(t)
	h.sc.ReadOnly = true
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"})
	got := h.sc.Scope(h.legacy(3*time.Hour, "x"))
	if !reflect.DeepEqual(got.Inferred, []string{idCard2}) {
		t.Fatalf("an inspector still reports the scope the evidence implies, got %v (%s)", got.Inferred, got.ScopeWhy)
	}
	if _, err := os.Stat(h.seenPath()); err == nil {
		t.Fatal("an inspector wrote the sidecar")
	}
	if ev := h.eventsSnapshot(); len(ev) != 0 {
		t.Fatalf("an inspector wrote a ledger line: %+v", ev)
	}

	// It reads the sticky set an observer left, adds what it sees, and leaves the file as it was.
	obs := h.newScoper()
	obs.Scope(h.legacy(3*time.Hour, "x"))
	before, err := os.ReadFile(h.seenPath())
	if err != nil {
		t.Fatal(err)
	}
	h.advance(5 * time.Minute)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 1"})
	ins := h.newScoper()
	ins.ReadOnly = true
	got = ins.Scope(h.legacy(3*time.Hour, "x"))
	if !reflect.DeepEqual(got.Inferred, []string{idCard0, idCard2}) {
		t.Fatalf("the inspector's view is the sidecar plus fresh evidence, got %v", got.Inferred)
	}
	after, err := os.ReadFile(h.seenPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("an inspector changed the sidecar:\n%s\n%s", before, after)
	}
	if len(h.eventsSnapshot()) != 1 {
		t.Fatalf("an inspector wrote a ledger line: %+v", h.eventsSnapshot())
	}
}

// A reader that arrives after the lease ended must not recreate the sidecar the release removed.
func TestSidecarIsNotRecreatedForALeaseThatEnded(t *testing.T) {
	h := newInferHarness(t)
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"})
	h.sc.Live = func(uint64) bool { return false }
	h.sc.Scope(h.legacy(3*time.Hour, "x"))
	if _, err := os.Stat(h.seenPath()); err == nil {
		t.Fatal("the lease is gone and the sidecar was written anyway: it would stay as debris")
	}
	if ev := h.eventsSnapshot(); len(ev) != 0 {
		t.Fatalf("no lease, no ledger line, got %+v", ev)
	}
}

// A sidecar that cannot be written is said: the scope is not sticky across processes, and the
// operator reading `gpu status` must not be told "inferred" as if it were.
func TestUnwritableSidecarIsReportedNotSilent(t *testing.T) {
	h := newInferHarness(t)
	// A directory that cannot exist: its parent is a regular file.
	parent := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(parent, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	h.dir = filepath.Join(parent, "gpu", "lease")
	h.sc = h.newScoper()
	h.tree = tree(TreeProc{PID: 4242, PPID: 1, Cmdline: "python main.py --cuda-device 2"})
	got := h.sc.Scope(h.legacy(3*time.Hour, "x"))
	if !reflect.DeepEqual(got.Inferred, []string{idCard2}) {
		t.Fatalf("the evidence still scopes the lease for this process, got %v (%s)", got.Inferred, got.ScopeWhy)
	}
	if !strings.Contains(got.ScopeWhy, "not sticky") {
		t.Errorf("the reason must say the scope could not be persisted: %q", got.ScopeWhy)
	}
	if w := h.warnsSnapshot(); len(w) != 1 || !strings.Contains(w[0], "seen.1205") {
		t.Errorf("one warning names the sidecar that could not be written, got %q", w)
	}
	if ev := h.eventsSnapshot(); len(ev) != 0 {
		t.Errorf("an event for a scope that was not persisted would repeat on every read, got %+v", ev)
	}
}

// Two readers' samples merge by time, oldest first, and the list stays bounded with the first
// sample kept: the sidecar is shared, so a union that lost the first sample would lose the
// five-minute baseline the evidence needs.
func TestMergeSamplesUnionsByTimeAndStaysBounded(t *testing.T) {
	a := []seenSample{{AtMs: 1000, Cards: []string{idCard0}}, {AtMs: 3000, Cards: []string{idCard2}}}
	b := []seenSample{{AtMs: 2000, Cards: []string{idCard2}}, {AtMs: 3000, Cards: []string{idCard0}}}
	got := mergeSamples(a, b)
	if len(got) != 3 || got[0].AtMs != 1000 || got[1].AtMs != 2000 || got[2].AtMs != 3000 {
		t.Fatalf("merged by time, oldest first: %+v", got)
	}
	if !reflect.DeepEqual(got[2].Cards, []string{idCard0, idCard2}) {
		t.Fatalf("two readings of the same instant are united, got %v", got[2].Cards)
	}
	var many []seenSample
	for i := int64(1); i <= int64(maxSamples)+8; i++ {
		many = append(many, seenSample{AtMs: i * 1000, Cards: []string{idCard0}})
	}
	bounded := mergeSamples(many, nil)
	if len(bounded) != maxSamples || bounded[0].AtMs != 1000 || bounded[len(bounded)-1].AtMs != many[len(many)-1].AtMs {
		t.Fatalf("bounded to %d keeping the first and the latest, got %d samples %+v", maxSamples, len(bounded), bounded)
	}
	if mergeSamples(nil, nil) != nil {
		t.Fatal("nothing merged is nothing")
	}
}
