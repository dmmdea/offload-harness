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

	working, end := e.Begin("animate_character", "offload_animate_character")
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

	working, end := e.Begin("transcribe", "cli:transcribe")
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
		if w, end := e.Begin(task, ""); w != nil || end != nil {
			t.Fatalf("%s must not open a card", task)
		}
	}
	if w, end := e.Begin("generate_video", FleetDoor); w != nil || end != nil {
		t.Fatal("a fleet-served call is the asking box's card, not this box's")
	}
	off := New(Config{Enabled: false, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	if w, end := off.Begin("generate_video", ""); w != nil || end != nil {
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
