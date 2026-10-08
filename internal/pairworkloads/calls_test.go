package pairworkloads

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

func methodsOf(c *capture) map[string]map[string]any {
	out := map[string]map[string]any{}
	for i := 0; i < c.count(); i++ {
		if m, ok := c.method(i).(string); ok {
			out[m] = c.info(i)
		}
	}
	return out
}

// A long call opens a QUEUED card at Begin, turns it running when the lane
// marks the work started, and its ledger row closes that same card: same id
// and engine, the row's model and outcome, the start the mark recorded. The
// End that follows sends nothing more.
func TestBeginQueuedThenWorkingThenRowCloses(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	e.AttachLedger(l)

	_, working, end := e.Begin("animate_character", "offload_animate_character")
	if working == nil || end == nil {
		t.Fatal("animate_character must open a card")
	}
	e.Wait()
	if c.count() != 1 || c.method(0) != "workload:submitted" {
		t.Fatalf("want one queued frame at Begin, got %d frames", c.count())
	}
	open := c.info(0)
	if open["state"] != "queued" || open["engine"] != "comfyui" || open["startedAt"] != nil {
		t.Fatalf("queued frame wrong: %v", open)
	}

	working()
	working() // a second mark is a no-op
	e.Wait()
	if c.count() != 2 || c.method(1) != "workload:started" || c.info(1)["startedAt"] == nil {
		t.Fatalf("want exactly one running frame after the mark, frames = %d", c.count())
	}

	if err := l.Record(ledger.Entry{Task: "animate_character", ModelTier: "wan2.2-animate", LatencyMs: 5000}); err != nil {
		t.Fatal(err)
	}
	end(false, "")
	e.Wait()
	if c.count() != 3 {
		t.Fatalf("frames = %d, want 3 (the row closes the card; End adds nothing)", c.count())
	}
	closed := c.info(2)
	if c.method(2) != "workload:completed" || closed["id"] != open["id"] || closed["runId"] != open["runId"] ||
		closed["engine"] != "comfyui" || closed["model"] != "wan2.2-animate" || closed["createdAt"] != open["createdAt"] ||
		closed["startedAt"] != c.info(1)["startedAt"] {
		t.Fatalf("closing frame must name the card and its real start: open %v closed %v", open, closed)
	}
}

// A call that never got its engine (it waited, then failed) closes with no
// start, and End closes it when no row came.
func TestEndClosesUnstartedCardWithoutRow(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})

	_, working, end := e.Begin("transcribe", "cli:transcribe")
	end(true, "whisper unreachable")
	working() // after the close: nothing
	e.Wait()
	byM := methodsOf(c)
	if c.count() != 2 || byM["workload:submitted"] == nil || byM["workload:errored"] == nil {
		t.Fatalf("frames = %d, want queued + failed", c.count())
	}
	failed := byM["workload:errored"]
	if failed["id"] != byM["workload:submitted"]["id"] || failed["engine"] != "whispercpp" ||
		failed["error"] != "whisper unreachable" || failed["startedAt"] != nil {
		t.Fatalf("End must fail the same card with no start: %v", failed)
	}
	end(false, "")
	e.Wait()
	if c.count() != 2 {
		t.Fatalf("a second End must send nothing, frames = %d", c.count())
	}
}

// Short calls, a disabled emitter, and work this box serves for another
// (the fleet door) open nothing.
func TestBeginSkipsShortTasksDisabledAndFleetDoor(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	for _, task := range []string{"summarize", "classify", "vqa", "media", "agent_delegate"} {
		if id, w, end := e.Begin(task, ""); id != "" || w != nil || end != nil {
			t.Fatalf("%s must not open a card", task)
		}
	}
	if id, w, end := e.Begin("generate_video", FleetDoor); id != "" || w != nil || end != nil {
		t.Fatal("a fleet-served call is the asking box's card, not this box's")
	}
	off := New(Config{Enabled: false, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	if id, w, end := off.Begin("generate_video", ""); id != "" || w != nil || end != nil {
		t.Fatal("a disabled emitter must open nothing")
	}
	e.Wait()
	if c.count() != 0 {
		t.Fatalf("frames = %d, want 0", c.count())
	}
}

// A ledger row for work served over the fleet door makes no card here.
func TestLedgerObserverSkipsFleetDoorRows(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	e.AttachLedger(l)
	if err := l.Record(ledger.Entry{Task: "generate_video", ModelTier: "wan", LatencyMs: 10, Door: FleetDoor}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(ledger.Entry{Task: "summarize", ModelTier: "gemma-4-e4b", LatencyMs: 10, Door: "cli:summarize"}); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if c.count() != 1 || c.info(0)["model"] != "gemma-4-e4b" {
		t.Fatalf("want only the local row's card, frames = %d", c.count())
	}
}

// A process started under a `gpu reserve` lease card reports nothing itself.
func TestFromConfigSilentUnderLease(t *testing.T) {
	cfg := config.Config{PairWorkloadsEnabled: true}
	if !FromConfig(cfg).Enabled {
		t.Fatal("enabled config must enable the emitter")
	}
	t.Setenv(UnderLeaseEnv, "1")
	if FromConfig(cfg).Enabled {
		t.Fatal("under a lease card the wrapped command must not report")
	}
}

// terminalsByID folds a capture into the terminal frame of each card id, and
// the set of ids that opened (queued).
func terminalsByID(c *capture) (opened map[string]bool, terminal map[string]map[string]any) {
	opened, terminal = map[string]bool{}, map[string]map[string]any{}
	for i := 0; i < c.count(); i++ {
		info := c.info(i)
		id, _ := info["id"].(string)
		switch c.method(i) {
		case "workload:submitted":
			opened[id] = true
		case "workload:completed", "workload:errored":
			terminal[id] = info
		}
	}
	return opened, terminal
}

// D15: two overlapping calls of one task. The SHORTER call's row lands first.
// Matched first-in first-out it closed the OLDER call's card (swapping model and
// timings), the shorter call's End then closed its own still-open card with no
// row data, and when the older call's row finally came the queue was empty, so
// it got a THIRD card (11 surplus cards on 2026-10-01).
// A row now names its call, and claims exactly that card.
func TestOverlappingCallsEachRowClosesItsOwnCard(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	e.AttachLedger(l)

	idA, _, endA := e.Begin("transcribe", "cli:transcribe")
	idB, _, endB := e.Begin("transcribe", "cli:transcribe")
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("Begin must return a distinct call id per call, got %q and %q", idA, idB)
	}
	// B, the short call, finishes first: its row, then its End.
	if err := l.Record(ledger.Entry{Task: "transcribe", ModelTier: "model-b", LatencyMs: 1000, CallID: idB}); err != nil {
		t.Fatal(err)
	}
	endB(false, "")
	if err := l.Record(ledger.Entry{Task: "transcribe", ModelTier: "model-a", LatencyMs: 9000, CallID: idA}); err != nil {
		t.Fatal(err)
	}
	endA(false, "")
	e.Wait()

	opened, terminal := terminalsByID(c)
	if c.count() != 4 || len(opened) != 2 || len(terminal) != 2 {
		t.Fatalf("frames = %d, opened %d, closed %d: want exactly two cards (queued + closed each)", c.count(), len(opened), len(terminal))
	}
	for id, model := range map[string]string{idA: "model-a", idB: "model-b"} {
		if !opened[id] || terminal[id] == nil {
			t.Fatalf("card %s must open and close under its own id (opened %v, closed %v)", id, opened, terminal)
		}
		if terminal[id]["model"] != model {
			t.Fatalf("card %s closed with model %v, want its own row's %q (the rows were swapped)", id, terminal[id]["model"], model)
		}
	}
	a, b := terminal[idA], terminal[idB]
	if a["completedAt"].(float64)-a["createdAt"].(float64) < b["completedAt"].(float64)-b["createdAt"].(float64) {
		t.Fatalf("the 9 s call must span longer than the 1 s call: a=%v b=%v", a, b)
	}
}

// A row that names a call whose card is not open (it was closed already, or
// belongs to another process) never falls back to first-in first-out: that
// would close SOMEONE ELSE's card. It gets its own card, and the open card
// stays for its own call's End.
func TestRowWithUnknownCallIDLeavesOpenCardsAlone(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	e.AttachLedger(l)

	idA, _, endA := e.Begin("transcribe", "cli:transcribe")
	if err := l.Record(ledger.Entry{Task: "transcribe", ModelTier: "model-x", LatencyMs: 5, CallID: "call-from-elsewhere"}); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	_, terminal := terminalsByID(c)
	if terminal[idA] != nil {
		t.Fatalf("a row for another call closed card %s", idA)
	}
	endA(false, "")
	e.Wait()
	opened, terminal := terminalsByID(c)
	if terminal[idA] == nil || !opened[idA] {
		t.Fatalf("End must still close its own card %s", idA)
	}
	if len(terminal) != 2 {
		t.Fatalf("want the stray row's own card plus A's, got %d closed cards", len(terminal))
	}
}

// D12: an inner row (ParentJobID set) is a step of a call whose parent row is
// carded; it is never a card of its own.
func TestLedgerObserverSkipsInnerRows(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	e.AttachLedger(l)
	for i := 0; i < 3; i++ {
		if err := l.Record(ledger.Entry{Task: "video_watch", ModelTier: "vlm-win", LatencyMs: 10, ParentJobID: "vw-1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Record(ledger.Entry{Task: "video_watch", ModelTier: "vlm-call", LatencyMs: 30, JobID: "vw-1"}); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if c.count() != 1 || c.info(0)["model"] != "vlm-call" {
		t.Fatalf("want one card for the call's own row, frames = %d", c.count())
	}
}
