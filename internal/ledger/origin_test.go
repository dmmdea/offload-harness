package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// resetOriginForTest re-arms the once so a test's environment is what
// ProcessOrigin reads, not whatever an earlier test (or the developer's own
// shell, which exports a real CLAUDE_CODE_SESSION_ID) resolved first.
func resetOriginForTest() { originOnce = sync.Once{} }

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestOriginFromPrefersExplicitLabelThenClaudeSession(t *testing.T) {
	sid := "7c561f0b-2e24-4047-baf9-3a81dadd3694"
	o := originFrom(envMap(map[string]string{"CLAUDE_CODE_SESSION_ID": sid}), 41, 7)
	if o.Session != sid || o.PID != 41 || o.PPID != 7 {
		t.Fatalf("claude session: %+v", o)
	}
	o = originFrom(envMap(map[string]string{"LOCAL_OFFLOAD_ORIGIN": " opencode-9 ", "CLAUDE_CODE_SESSION_ID": sid}), 41, 7)
	if o.Session != "opencode-9" {
		t.Fatalf("the explicit label must win and be trimmed: %+v", o)
	}
	o = originFrom(envMap(map[string]string{"LOCAL_OFFLOAD_ORIGIN": "bad\nlabel", "CLAUDE_CODE_SESSION_ID": sid}), 41, 7)
	if o.Session != sid {
		t.Fatalf("a refused explicit label must fall through to the session id: %+v", o)
	}
	o = originFrom(envMap(nil), 41, 7)
	if o.Session != "" || o.PID != 41 || o.PPID != 7 {
		t.Fatalf("no environment: pids only, %+v", o)
	}
}

func TestOriginRefusesUnprintableOrOversizedLabels(t *testing.T) {
	for _, bad := range []string{"", "   ", "a\nb", "tab\there", strings.Repeat("x", maxOriginLen+1), "ünïcode"} {
		if got := cleanOrigin(bad); got != "" {
			t.Fatalf("cleanOrigin(%q) = %q, want empty", bad, got)
		}
	}
	if got := cleanOrigin(strings.Repeat("x", maxOriginLen)); got == "" {
		t.Fatal("a label exactly at the cap must be kept")
	}
}

func TestRecordStampsOriginAndCardsTokensOnEveryRow(t *testing.T) {
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-test-1")
	resetOriginForTest()
	t.Cleanup(resetOriginForTest)

	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	rows := []Entry{
		{Task: "summarize", TokensIn: 100, TokensOut: 20},                 // cascade: prompt + generated
		{Task: "agent_delegate", SeatTokensIn: 5000, TokensOut: 300},      // delegate: the seat's prompt work + generated
		{Task: "classify", TokensIn: 50, CacheHit: true},                  // cache hit: no card work
		{Task: "generate_image", InputChars: 400},                         // render, no token counts: chars/4
		{Task: "agent_delegate", Deferred: true, Reason: "dispatch: 503"}, // deferred before any model ran
		{Task: "agent", SeatTokensIn: 900, TokensOut: 10, Deferred: true}, // deferred after the seat worked
		{Task: "triage", TokensIn: 70, Deferred: true},                    // a low-confidence defer still ran the seat
	}
	for _, e := range rows {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	got, err := ReadAll(p)
	if err != nil || len(got) != len(rows) {
		t.Fatalf("ReadAll: err=%v rows=%d want %d", err, len(got), len(rows))
	}
	want := []int{120, 5300, 0, 100, 0, 910, 70}
	for i, e := range got {
		if e.CardsTokens != want[i] {
			t.Errorf("row %d (%s): cards_tokens=%d want %d", i, e.Task, e.CardsTokens, want[i])
		}
		if e.OriginSession != "sess-test-1" || e.OriginPID != os.Getpid() || e.OriginPPID != os.Getppid() {
			t.Errorf("row %d (%s): origin not stamped: session=%q pid=%d ppid=%d", i, e.Task, e.OriginSession, e.OriginPID, e.OriginPPID)
		}
	}
	// The cards_tokens KEY is present even when the figure is 0: its presence is
	// what tells a reader the row carries the 0.124.0 schema.
	raw, _ := os.ReadFile(p)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["cards_tokens"]; !ok {
			t.Fatalf("cards_tokens key missing on %s", line)
		}
	}
}

func TestRecordKeepsACallerProvidedOriginAndCapsPlacement(t *testing.T) {
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-test-2")
	resetOriginForTest()
	t.Cleanup(resetOriginForTest)

	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("p", maxReasonLen+40)
	if err := l.Record(Entry{Task: "agent_delegate", OriginSession: "other-session", OriginPID: 99, Placement: long, JobID: "agd-1", Route: "spread", Steps: 3, StopReason: "final", AcceptanceResult: "pass"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	got, _ := ReadAll(p)
	if len(got) != 1 {
		t.Fatalf("rows=%d", len(got))
	}
	e := got[0]
	if e.OriginSession != "other-session" || e.OriginPID != 99 {
		t.Fatalf("a caller-named origin must be kept: %+v", e)
	}
	if len(e.Placement) != maxReasonLen {
		t.Fatalf("placement must be capped like reason: len=%d", len(e.Placement))
	}
	if e.JobID != "agd-1" || e.Route != "spread" || e.Steps != 3 || e.StopReason != "final" || e.AcceptanceResult != "pass" {
		t.Fatalf("job fields must round-trip: %+v", e)
	}
}

func TestOldRowsWithoutProvenanceParseAsUnattributed(t *testing.T) {
	var e Entry
	if err := json.Unmarshal([]byte(`{"ts":1,"task":"summarize","tokens_in":10,"tokens_out":2,"latency_ms":5,"tok_per_s":1,"cache_hit":false,"deferred":false}`), &e); err != nil {
		t.Fatal(err)
	}
	if e.OriginSession != "" || e.OriginPID != 0 || e.OriginPPID != 0 || e.CardsTokens != 0 || e.JobID != "" {
		t.Fatalf("a pre-0.124.0 row must read as unattributed, not invented: %+v", e)
	}
}
