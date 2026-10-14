package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Register D-101 (ADR 0087): `ledger --by-session` adds a sessions array, each session's card tokens split by where
// they ran (this box or a fleet node). It is additive: without the flag the output is what it always was.

// sessionLedger writes a ledger of two sessions and one bake and returns the config path that reads it. Every row
// names its session, so the go-test origin of this suite does not turn them all into test traffic.
func sessionLedger(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	rows := []ledger.Entry{
		{Task: "agent_delegate", JobID: "agd-1", FleetJobID: "agd-1", CardsTokens: 700, TokensOut: 50, OriginSession: "sess-abc"},
		{Task: "agent_delegate", JobID: "agd-2", CardsTokens: 300, TokensOut: 20, OriginSession: "sess-abc"},
		{Task: "summarize", CardsTokens: 90, TokensIn: 80, TokensOut: 10, OriginSession: "sess-xyz"},
		{Task: "agent", Door: "fleet", FleetJobID: "agd-7", CardsTokens: 40, TokensOut: 5, OriginSession: "sess-served", Requester: "node-a"},
		{Task: "agent_delegate", JobID: "agd-bake", FleetJobID: "agd-bake", CardsTokens: 5, RunClass: "bake", OriginSession: "sess-abc"},
	}
	for _, e := range rows {
		if err := led.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	led.Close()
	cfg, _ := json.Marshal(map[string]any{"home": dir, "ledger_path": ledgerPath})
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func runLedgerOut(t *testing.T, args ...string) string {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() { runErr = runLedger(args) })
	if runErr != nil {
		t.Fatalf("runLedger %v: %v", args, runErr)
	}
	return out
}

func sessionsOf(t *testing.T, out string) []ledger.SessionShare {
	t.Helper()
	var o struct {
		Sessions *[]ledger.SessionShare `json:"sessions"`
	}
	if err := json.Unmarshal(jsonObject(t, out), &o); err != nil {
		t.Fatalf("ledger output is not JSON: %v\n%s", err, out)
	}
	if o.Sessions == nil {
		t.Fatalf("no sessions array in:\n%s", out)
	}
	return *o.Sessions
}

// Without the flag the JSON is the object it always was: no sessions key. With it, the same object plus the array,
// so a script that reads the ledger today reads the same fields tomorrow.
func TestLedgerByThisSessionIsAdditive(t *testing.T) {
	cfgPath := sessionLedger(t)
	plain := runLedgerOut(t, "--config", cfgPath)
	if strings.Contains(plain, "sessions") {
		t.Fatalf("the default output grew a sessions key:\n%s", plain)
	}
	with := runLedgerOut(t, "--config", cfgPath, "--by-session")
	var withObj map[string]json.RawMessage
	if err := json.Unmarshal(jsonObject(t, with), &withObj); err != nil {
		t.Fatal(err)
	}
	if _, ok := withObj["sessions"]; !ok {
		t.Fatalf("--by-session printed no sessions key:\n%s", with)
	}
	delete(withObj, "sessions")
	var plainObj map[string]json.RawMessage
	if err := json.Unmarshal(jsonObject(t, plain), &plainObj); err != nil {
		t.Fatal(err)
	}
	if len(withObj) != len(plainObj) {
		t.Fatalf("--by-session changed the other keys: %d vs %d", len(withObj), len(plainObj))
	}
	for k, v := range plainObj {
		if !bytes.Equal(withObj[k], v) {
			t.Fatalf("--by-session changed %q: %s vs %s", k, withObj[k], v)
		}
	}
}

func TestLedgerBySessionSplitsEachSessionByWhereItRan(t *testing.T) {
	cfgPath := sessionLedger(t)
	out := runLedgerOut(t, "--config", cfgPath, "--by-session")
	shares := sessionsOf(t, out)
	by := map[string]ledger.SessionShare{}
	for _, s := range shares {
		by[s.Session] = s
	}
	if len(shares) != 3 {
		t.Fatalf("sessions = %+v, want sess-abc, sess-xyz and sess-served (the bake is left out)", shares)
	}
	if s := by["sess-abc"]; s.RemoteTokens != 700 || s.LocalTokens != 300 || s.RemoteShare != 0.7 {
		t.Fatalf("sess-abc = %+v, want 700 remote, 300 local, share 0.7 (the bake is not counted)", s)
	}
	if s := by["sess-xyz"]; s.LocalTokens != 90 || s.RemoteTokens != 0 {
		t.Fatalf("sess-xyz = %+v, want 90 local", s)
	}
	if s := by["sess-served"]; s.ServedRows != 1 || s.ServedTokens != 40 || s.CardsTokens != 0 {
		t.Fatalf("sess-served = %+v, want one served row of 40 and none of its own", s)
	}
	// The existing left-out line still says what was left out, and the new view says what its number is.
	if !strings.Contains(out, "left out: test, bake and measurement traffic (bake 1 job(s))") {
		t.Fatalf("the left-out line is missing:\n%s", out)
	}
	if !strings.Contains(out, "placement split") {
		t.Fatalf("the output does not say what remote_share is:\n%s", out)
	}
	// --include-test-traffic brings the bake back into the session it belongs to.
	out = runLedgerOut(t, "--config", cfgPath, "--by-session", "--include-test-traffic")
	for _, s := range sessionsOf(t, out) {
		if s.Session == "sess-abc" && s.RemoteTokens != 705 {
			t.Fatalf("with traffic sess-abc = %+v, want the bake's 5 added", s)
		}
	}
}

// --session narrows the array to one session and implies --by-session; a session the ledger does not hold is an
// empty array, not an error and not a missing key.
func TestLedgerSessionFlagNarrowsTheArray(t *testing.T) {
	cfgPath := sessionLedger(t)
	shares := sessionsOf(t, runLedgerOut(t, "--config", cfgPath, "--session", "sess-xyz"))
	if len(shares) != 1 || shares[0].Session != "sess-xyz" || shares[0].LocalTokens != 90 {
		t.Fatalf("--session sess-xyz = %+v", shares)
	}
	shares = sessionsOf(t, runLedgerOut(t, "--config", cfgPath, "--by-session", "--session", "sess-nobody"))
	if len(shares) != 0 {
		t.Fatalf("--session sess-nobody = %+v, want an empty array", shares)
	}
}
