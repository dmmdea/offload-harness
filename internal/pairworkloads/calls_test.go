package pairworkloads

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// A long call opens a running card at Begin and its ledger row closes THAT
// card: same id and engine, the row's model and outcome. The End that follows
// sends nothing more.
func TestBeginOpensRunningCardAndRowClosesIt(t *testing.T) {
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

	end := e.Begin("animate_character")
	if end == nil {
		t.Fatal("animate_character must open a running card")
	}
	e.Wait()
	if c.count() != 1 || c.method(0) != "workload:started" {
		t.Fatalf("want one started frame at Begin, got %d frames", c.count())
	}
	open := c.info(0)
	if open["state"] != "running" || open["engine"] != "comfyui" || open["startedAt"] == nil {
		t.Fatalf("running frame wrong: %v", open)
	}

	if err := l.Record(ledger.Entry{Task: "animate_character", ModelTier: "wan2.2-animate", LatencyMs: 5000}); err != nil {
		t.Fatal(err)
	}
	end(false, "")
	e.Wait()
	if c.count() != 2 {
		t.Fatalf("frames = %d, want 2 (the row closes the card; End adds nothing)", c.count())
	}
	closed := c.info(1)
	if c.method(1) != "workload:completed" || closed["id"] != open["id"] || closed["runId"] != open["runId"] ||
		closed["engine"] != "comfyui" || closed["model"] != "wan2.2-animate" || closed["createdAt"] != open["createdAt"] {
		t.Fatalf("closing frame must name the running card: open %v closed %v", open, closed)
	}
}

// A call whose row never came (a cache hit, a path that records nothing)
// is closed by End with the call's own outcome.
func TestEndClosesCardWithoutRow(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})

	end := e.Begin("transcribe")
	end(true, "whisper unreachable")
	e.Wait()
	if c.count() != 2 {
		t.Fatalf("frames = %d, want running + failed", c.count())
	}
	var started, failed map[string]any
	for i := 0; i < 2; i++ {
		switch c.method(i) {
		case "workload:started":
			started = c.info(i)
		case "workload:errored":
			failed = c.info(i)
		}
	}
	if started == nil || failed == nil || failed["id"] != started["id"] || failed["engine"] != "whispercpp" || failed["error"] != "whisper unreachable" {
		t.Fatalf("End must fail the same card: started %v failed %v", started, failed)
	}
	end(false, "")
	e.Wait()
	if c.count() != 2 {
		t.Fatalf("a second End must send nothing, frames = %d", c.count())
	}
}

// Short calls and a disabled emitter open nothing: their engine is not known
// up front, and a card keyed on a guessed engine would never close.
func TestBeginSkipsShortTasksAndDisabled(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	for _, task := range []string{"summarize", "classify", "vqa", "media", "agent_delegate"} {
		if e.Begin(task) != nil {
			t.Fatalf("%s must not open a running card", task)
		}
	}
	off := New(Config{Enabled: false, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	if off.Begin("generate_video") != nil {
		t.Fatal("a disabled emitter must open nothing")
	}
	e.Wait()
	if c.count() != 0 {
		t.Fatalf("frames = %d, want 0", c.count())
	}
}
