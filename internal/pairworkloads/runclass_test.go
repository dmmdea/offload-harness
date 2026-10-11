package pairworkloads

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Run class (register F48 / job-error class C23): a bake or measurement run opened 42 cards on the operator's
// PAIR dashboard for a seat no endpoint served. A process that declares a non-production class opens no card,
// and its asker headers must not ask a fleet node to card the job in its place.

func clearRunClassEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{ledger.EnvRunClass, "LOCAL_OFFLOAD_ORIGIN", TestCardsEnv, UnderLeaseEnv} {
		t.Setenv(k, "")
	}
}

func TestFromConfigOpensNoCardsForANonProductionRun(t *testing.T) {
	cases := []struct {
		name         string
		env          map[string]string
		wantEnabled  bool
		wantSuppress bool
	}{
		{"production", nil, true, false},
		{"explicit bake", map[string]string{ledger.EnvRunClass: "bake"}, false, true},
		{"explicit measurement", map[string]string{ledger.EnvRunClass: "measurement"}, false, true},
		{"a class-prefixed origin", map[string]string{"LOCAL_OFFLOAD_ORIGIN": "bake:run-7"}, false, true},
		{"production says production", map[string]string{ledger.EnvRunClass: "production"}, true, false},
		// The repo's own tests point their emitters at fixtures, so the go-test origin labels rows and nothing more.
		{"the go-test origin is a row label, not a process class", map[string]string{"LOCAL_OFFLOAD_ORIGIN": "go-test"}, true, false},
		{"the opt-in puts a labelled run's cards back (testing PAIR itself)", map[string]string{ledger.EnvRunClass: "bake", TestCardsEnv: "1"}, true, false},
	}
	for _, c := range cases {
		clearRunClassEnv(t)
		for k, v := range c.env {
			t.Setenv(k, v)
		}
		got := FromConfig(config.Config{PairWorkloadsEnabled: true})
		if got.Enabled != c.wantEnabled || got.Suppressed != c.wantSuppress {
			t.Errorf("%s: Enabled=%v Suppressed=%v, want %v/%v", c.name, got.Enabled, got.Suppressed, c.wantEnabled, c.wantSuppress)
		}
	}
}

// The wire contract: an asker with no PAIR emitter sends X-Offload-Pair-Card: node and the serving node then cards
// the job. A suppressed run sends neither header value, so the node (which reads silence as "the asker cards it")
// opens nothing either; a plain disabled emitter keeps asking, as it always did.
func TestASuppressedAskerDoesNotAskTheNodeToCard(t *testing.T) {
	clearRunClassEnv(t)
	t.Setenv(ledger.EnvRunClass, "bake")
	h := http.Header{}
	New(FromConfig(config.Config{PairWorkloadsEnabled: true})).SetWireHeaders(h)
	if got := h.Get(core.PairCardHeader); got != "" {
		t.Fatalf("a suppressed run asked the node to card its job: %q", got)
	}
	h = http.Header{}
	WireHeadersFor(config.Config{PairWorkloadsEnabled: false}, h)
	if got := h.Get(core.PairCardHeader); got != "" {
		t.Fatalf("a suppressed run asked the node to card its job through WireHeadersFor: %q", got)
	}

	clearRunClassEnv(t)
	h = http.Header{}
	WireHeadersFor(config.Config{PairWorkloadsEnabled: false}, h)
	if got := h.Get(core.PairCardHeader); got != core.PairCardNode {
		t.Fatalf("a production asker whose emitter is off must still ask the node to card (unchanged): %q", got)
	}
}

// A row of a non-production class is no card even in a production process: the call's caller pinned it for
// measurement, or it came through the measure door. The control row proves the observer still cards real work.
func TestTheLedgerObserverCardsNoRowOfANonProductionClass(t *testing.T) {
	clearRunClassEnv(t)
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
	rows := []ledger.Entry{
		{TS: 100, Task: "summarize", ModelTier: "gemma-4-e4b", LatencyMs: 2500},                           // real work: one card
		{TS: 101, Task: "summarize", ModelTier: "gemma-4-e4b", LatencyMs: 2500, RunClass: "bake"},         // labelled by its writer
		{TS: 102, Task: "generate_image", ModelTier: "img", LatencyMs: 900, Door: "cli:fleet-measure"},    // the measure door
		{TS: 103, Task: "transcribe", ModelTier: "whisper-stt", LatencyMs: 900, PinReason: "measurement"}, // pinned for measurement
		{TS: 104, Task: "summarize", ModelTier: "gemma-4-e4b", LatencyMs: 2500, OriginSession: "go-test"}, // a go-test escape
	}
	for _, r := range rows {
		if err := l.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	e.Wait()
	if c.count() != 1 {
		t.Fatalf("frames = %d, want 1 (only the real row is carded)", c.count())
	}
}

// A long call opens its card at Begin, before any ledger row exists, so the measure door must be refused there
// too, or the card of a fleet-measure render sits on the dashboard until the call ends. The control proves
// Begin still opens a card for the same task through an ordinary door.
func TestBeginOpensNoCardForTheMeasureDoor(t *testing.T) {
	clearRunClassEnv(t)
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})

	id, working, end := e.Begin("generate_image", "cli:generate-image")
	if id == "" || working == nil || end == nil {
		t.Fatal("control: an ordinary door must open a card")
	}
	end(core.Result{OK: true})
	e.Wait()
	before := c.count()

	id, working, end = e.Begin("generate_image", "cli:fleet-measure")
	if id != "" || working != nil || end != nil {
		t.Fatalf("the measure door opened a card: id=%q", id)
	}
	e.Wait()
	if c.count() != before {
		t.Fatalf("the measure door sent %d frame(s)", c.count()-before)
	}
}

// A render of the measure door that overflows to a fleet node (ADR 0082) is a call whose card is the asker's
// RemoteCall: it opens none, while the asker's ledger row is still written and still says what it was. The
// control is the same call through an ordinary door.
func TestARemoteCallThroughTheMeasureDoorOpensNoCardButStillWritesItsRow(t *testing.T) {
	clearRunClassEnv(t)
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	lp := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := ledger.Open(lp)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	call := func(door string) {
		rc := NewRemoteCall(e, l, core.Request{Task: core.TaskGenerateImage, Door: door, Input: "a plain gray sphere"}, "remote")
		rc.Dispatched("http://node-b.example:18811", "node-b", "job-"+door)
		rc.Running()
		rc.Finish(core.Result{OK: true, Meta: core.Meta{Node: "node-b", Model: "img"}})
		e.Wait()
	}
	call("cli:generate-image")
	ordinary := c.count()
	if ordinary == 0 {
		t.Fatal("control: an ordinary door must open a card")
	}
	call("cli:fleet-measure")
	if c.count() != ordinary {
		t.Fatalf("the measure door's remote call sent %d frame(s) to PAIR", c.count()-ordinary)
	}
	rows, _ := ledger.ReadAll(lp)
	if len(rows) != 2 || rows[1].Door != "cli:fleet-measure" || rows[1].RunClass != "measurement" {
		t.Fatalf("the asker's row must still be written, and say what it was: %+v", rows)
	}
}
