package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func intentCfg(t *testing.T) (config.Config, string) {
	t.Helper()
	root := t.TempDir()
	return config.Config{StateDir: root}, root
}

func TestIntentLedgerRoundTrip(t *testing.T) {
	cfg, root := intentCfg(t)
	l := openIntentLedger(cfg)
	if l == nil {
		t.Fatal("ledger must open against a writable state dir")
	}
	l.dispatched("agd-aaa", "http://node-a:18811", "summarize the corpus")
	l.dispatched("agd-bbb", "http://node-b:18811", strings.Repeat("g", 500))
	l.done("agd-aaa", "terminal observed")
	open, lines, err := readOpenIntents(filepath.Join(root, "delegate-intent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines != 3 {
		t.Fatalf("lines = %d, want 3", lines)
	}
	if _, ok := open["agd-aaa"]; ok {
		t.Fatal("closed job must not be open")
	}
	ev, ok := open["agd-bbb"]
	if !ok {
		t.Fatal("agd-bbb must be open")
	}
	if ev.Base != "http://node-b:18811" {
		t.Fatalf("base = %q", ev.Base)
	}
	if len(ev.Goal) != 120 {
		t.Fatalf("goal must be capped at 120, got %d", len(ev.Goal))
	}
	// A nil ledger is inert, never a panic — the fail-open contract.
	var nilLedger *intentLedger
	nilLedger.dispatched("x", "y", "z")
	nilLedger.done("x", "n")
}

// TestRecoverOrphans covers the three node answers recovery can meet: a
// finished job (result FILED + closed), a denied job (closed as lost), and a
// still-running job (left open for the next pass).
func TestRecoverOrphans(t *testing.T) {
	cfg, root := intentCfg(t)
	wire, _ := json.Marshal(map[string]any{"output": "the answer", "schema_version": 1})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agd-done1"):
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "done", "data": json.RawMessage(wire)})
		case strings.HasSuffix(r.URL.Path, "/agd-gone1"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/agd-run1"):
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "running"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	l := openIntentLedger(cfg)
	l.dispatched("agd-done1", srv.URL, "finished elsewhere")
	l.dispatched("agd-gone1", srv.URL, "node restarted")
	l.dispatched("agd-run1", srv.URL, "still going")

	n, err := RecoverOrphans(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recovered = %d, want 1", n)
	}
	// The finished job's result is on disk, envelope naming its provenance.
	b, rerr := os.ReadFile(filepath.Join(root, "delegate-recovered", "agd-done1.json"))
	if rerr != nil {
		t.Fatalf("recovered file missing: %v", rerr)
	}
	var env struct {
		State string          `json:"state"`
		Data  json.RawMessage `json:"data"`
		Base  string          `json:"base"`
	}
	if uerr := json.Unmarshal(b, &env); uerr != nil {
		t.Fatal(uerr)
	}
	if env.State != "done" || env.Base != srv.URL || !strings.Contains(string(env.Data), "the answer") {
		t.Fatalf("envelope wrong: %s", b)
	}
	// Ledger state after the pass: done1 closed, gone1 closed-as-lost,
	// run1 STILL OPEN (the node owns it; a later pass will look again).
	open, _, _ := readOpenIntents(filepath.Join(root, "delegate-intent.jsonl"))
	if _, ok := open["agd-done1"]; ok {
		t.Fatal("recovered job must be closed")
	}
	if _, ok := open["agd-gone1"]; ok {
		t.Fatal("denied job must be closed as lost")
	}
	if _, ok := open["agd-run1"]; !ok {
		t.Fatal("running job must stay open")
	}
}

// TestRecoverOrphansExpiry: an open entry older than intentMaxAge is closed as
// expired without polling anything (the node cannot plausibly still hold it).
func TestRecoverOrphansExpiry(t *testing.T) {
	cfg, root := intentCfg(t)
	l := openIntentLedger(cfg)
	l.append(intentEvent{E: "d", Job: "agd-old1", Base: "http://unreachable.invalid:1", Goal: "g",
		TS: time.Now().Add(-3 * 24 * time.Hour).Unix()})
	n, err := RecoverOrphans(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recovered = %d, want 0", n)
	}
	open, _, _ := readOpenIntents(filepath.Join(root, "delegate-intent.jsonl"))
	if _, ok := open["agd-old1"]; ok {
		t.Fatal("expired entry must be closed")
	}
}

// TestIntentLedgerNeverCompacts pins the round-1 review decision: many
// processes append to this file concurrently, so no pass may rewrite it —
// an append racing a rename-replace is lost work. Closed entries just stay.
func TestIntentLedgerNeverCompacts(t *testing.T) {
	cfg, root := intentCfg(t)
	l := openIntentLedger(cfg)
	for i := 0; i < 40; i++ {
		id := "agd-n" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		l.append(intentEvent{E: "d", Job: id, Base: "http://x", TS: time.Now().Unix()})
		l.append(intentEvent{E: "ok", Job: id})
	}
	if _, err := RecoverOrphans(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	_, lines, err := readOpenIntents(filepath.Join(root, "delegate-intent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines != 80 {
		t.Fatalf("ledger must never be rewritten by recovery, got %d lines (want 80)", lines)
	}
}

// TestIntentGoalRuneTruncation: a multi-byte goal is cut on a rune boundary.
func TestIntentGoalRuneTruncation(t *testing.T) {
	cfg, root := intentCfg(t)
	l := openIntentLedger(cfg)
	l.dispatched("agd-utf1", "http://x", strings.Repeat("é", 100)) // 200 bytes
	open, _, err := readOpenIntents(filepath.Join(root, "delegate-intent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	g := open["agd-utf1"].Goal
	if len(g) == 0 || len(g) > 120 || !utf8.ValidString(g) {
		t.Fatalf("goal must be capped and valid UTF-8, got %d bytes valid=%v", len(g), utf8.ValidString(g))
	}
}

// TestRunRemoteRecordsIntentAndOrphansOnDeadline is the end-to-end seam: a node
// that ACKS and then answers `running` forever leaves an OPEN intent when the
// poll deadline lands (the recovery case), while a node that finishes closes it.
func TestRunRemoteRecordsIntentAndOrphansOnDeadline(t *testing.T) {
	cfg, root := intentCfg(t)
	finish := make(chan struct{})
	wire, _ := json.Marshal(map[string]any{"output": "ok", "schema_version": 1})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		select {
		case <-finish:
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "done", "data": json.RawMessage(wire)})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "running"})
		}
	}))
	defer srv.Close()

	r := &runner{cfg: cfg, intent: openIntentLedger(cfg)}
	// 1-second budget: the node stays `running` past the deadline → owned-job
	// poll-deadline defer → the intent must survive OPEN.
	pr := r.runRemote(context.Background(), srv.URL, "agd-orphan1",
		coreContract("map the corpus", 1))
	if !pr.Result.Deferred {
		t.Fatalf("expected the owned-deadline defer, got %+v", pr)
	}
	if !pr.orphanable || !pr.intentRecorded {
		t.Fatalf("deadline exit must be orphanable+recorded, got orphanable=%v recorded=%v", pr.orphanable, pr.intentRecorded)
	}
	open, _, _ := readOpenIntents(filepath.Join(root, "delegate-intent.jsonl"))
	if _, ok := open["agd-orphan1"]; !ok {
		t.Fatal("owned-deadline job must stay open in the intent ledger")
	}
	// The same job later finishes server-side; recovery files it.
	close(finish)
	n, err := RecoverOrphans(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recovered = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(root, "delegate-recovered", "agd-orphan1.json")); err != nil {
		t.Fatalf("recovered result file missing: %v", err)
	}
}

func coreContract(goal string, timeoutSec int) core.AgentContract {
	return core.AgentContract{Goal: goal, TimeoutSec: timeoutSec}
}
