package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Register F48 / job-error class C23: the commands that report the ledger leave test, bake and measurement
// traffic out by default, say how many jobs they left out, and count them again under --include-test-traffic.

// trafficLedger writes a ledger with one real job and two jobs that were not real work (a bake its writer
// labelled, a call its caller pinned for measurement), every row attributed to a real session so the go-test
// origin of this suite does not make them all test traffic, and returns the config path that reads it.
func trafficLedger(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	const sess = "sess-real"
	rows := []ledger.Entry{
		{Task: "summarize", TokensIn: 100, TokensOut: 20, OriginSession: sess},
		{Task: "agent_delegate", JobID: "agd-bake", TokensOut: 5, RunClass: "bake", OriginSession: sess},
		{Task: "agent_delegate", JobID: "agd-meas", TokensOut: 7, PinReason: "measurement", RouteAsked: "local", Route: "local", OriginSession: sess},
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

// jsonObject cuts the leading JSON object off a command's stdout (the ledger verb follows it with a text line).
func jsonObject(t *testing.T, out string) []byte {
	t.Helper()
	i := strings.Index(out, "\n}\n")
	if i < 0 {
		t.Fatalf("no JSON object in:\n%s", out)
	}
	return []byte(out[:i+2])
}

func TestLedgerCommandLeavesOutTestTrafficAndSaysSo(t *testing.T) {
	cfgPath := trafficLedger(t)
	var runErr error
	out := captureStdout(t, func() { runErr = runLedger([]string{"--config", cfgPath}) })
	if runErr != nil {
		t.Fatalf("runLedger: %v", runErr)
	}
	var s ledger.Summary
	if err := json.Unmarshal(jsonObject(t, out), &s); err != nil {
		t.Fatalf("ledger output is not JSON: %v\n%s", err, out)
	}
	if s.Calls != 1 || s.TokensOut != 20 {
		t.Fatalf("default view must count the real job only: calls=%d tokens_out=%d", s.Calls, s.TokensOut)
	}
	if s.ExcludedNonProduction["bake"] != 1 || s.ExcludedNonProduction["measurement"] != 1 || len(s.ExcludedNonProduction) != 2 {
		t.Fatalf("the view must count what it left out: %v", s.ExcludedNonProduction)
	}
	if !strings.Contains(out, "left out: test, bake and measurement traffic (bake 1, measurement 1 job(s))") {
		t.Fatalf("the text line must say what was left out:\n%s", out)
	}

	out = captureStdout(t, func() { runErr = runLedger([]string{"--config", cfgPath, "--include-test-traffic"}) })
	if runErr != nil {
		t.Fatalf("runLedger --include-test-traffic: %v", runErr)
	}
	s = ledger.Summary{}
	if err := json.Unmarshal(jsonObject(t, out), &s); err != nil {
		t.Fatal(err)
	}
	if s.Calls != 3 || s.TokensOut != 32 || len(s.ExcludedNonProduction) != 0 || strings.Contains(out, "left out:") {
		t.Fatalf("with the flag every job counts and nothing is reported left out: %+v\n%s", s, out)
	}
}

func TestLoupeLeavesOutTestTrafficAndSaysSo(t *testing.T) {
	cfgPath := trafficLedger(t)
	var rep struct {
		Rows     int            `json:"rows"`
		Excluded map[string]int `json:"excluded_non_production"`
	}
	var runErr error
	out := captureStdout(t, func() { runErr = runLoupe([]string{"--config", cfgPath, "--json"}) })
	if runErr != nil {
		t.Fatalf("runLoupe: %v", runErr)
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("loupe --json is not JSON: %v\n%s", err, out)
	}
	if rep.Rows != 1 || rep.Excluded["bake"] != 1 || rep.Excluded["measurement"] != 1 {
		t.Fatalf("loupe default view: rows=%d excluded=%v, want 1 row and the two left-out jobs counted", rep.Rows, rep.Excluded)
	}
	out = captureStdout(t, func() { runErr = runLoupe([]string{"--config", cfgPath, "--json", "--include-test-traffic"}) })
	if runErr != nil {
		t.Fatalf("runLoupe --include-test-traffic: %v", runErr)
	}
	rep.Rows, rep.Excluded = 0, nil
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Rows != 3 || len(rep.Excluded) != 0 {
		t.Fatalf("loupe with the flag: rows=%d excluded=%v, want all 3 rows and nothing left out", rep.Rows, rep.Excluded)
	}
}

func TestStatsAndAuditSampleLeaveOutTestTraffic(t *testing.T) {
	cfgPath := trafficLedger(t)
	var runErr error
	out := captureStdout(t, func() { runErr = runStats([]string{"--config", cfgPath}) })
	if runErr != nil {
		t.Fatalf("runStats: %v", runErr)
	}
	var byTask map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &byTask); err != nil {
		t.Fatalf("stats is not JSON: %v\n%s", err, out)
	}
	if _, ok := byTask["agent_delegate"]; ok || len(byTask) != 1 {
		t.Fatalf("stats must report the real task only, got %v", keysOf(byTask))
	}
	out = captureStdout(t, func() { runErr = runStats([]string{"--config", cfgPath, "--include-test-traffic"}) })
	if runErr != nil {
		t.Fatal(runErr)
	}
	byTask = nil
	if err := json.Unmarshal([]byte(out), &byTask); err != nil {
		t.Fatal(err)
	}
	if _, ok := byTask["agent_delegate"]; !ok {
		t.Fatalf("stats --include-test-traffic must report the delegations too, got %v", keysOf(byTask))
	}

	var picked []ledger.Entry
	out = captureStdout(t, func() { runErr = runAuditSample([]string{"--config", cfgPath, "--json", "--n", "10"}) })
	if runErr != nil {
		t.Fatalf("runAuditSample: %v", runErr)
	}
	if err := json.Unmarshal([]byte(out), &picked); err != nil {
		t.Fatalf("audit-sample is not JSON: %v\n%s", err, out)
	}
	if len(picked) != 1 || picked[0].Task != "summarize" {
		t.Fatalf("audit-sample must sample real work only: %+v", picked)
	}
	out = captureStdout(t, func() {
		runErr = runAuditSample([]string{"--config", cfgPath, "--json", "--n", "10", "--include-test-traffic"})
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	picked = nil
	if err := json.Unmarshal([]byte(out), &picked); err != nil || len(picked) != 3 {
		t.Fatalf("audit-sample --include-test-traffic: %d rows err=%v, want 3", len(picked), err)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
