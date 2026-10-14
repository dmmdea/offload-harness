package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// RSI-OH-1: `ledger --failures` is a read-only failure-class register over the ledger. Rows are grouped by
// failure class (the closed reason code), with the door, seat and model of each row as breakdowns, and each
// class is joined by name to the human-maintained table in docs/systems/failure-register.md for its meaning,
// root cause and structural fix. These tests run it on a synthetic ledger (testdata/failure-register) and a
// synthetic table, so editing the real docs page never moves them.

const (
	failureFixtureLedger = "testdata/failure-register/ledger.jsonl"
	failureFixtureTable  = "testdata/failure-register/table.md"
	failureFixtureGolden = "testdata/failure-register/expected.json"
)

// failureFixtureConfig writes a config that reads the fixture ledger.
func failureFixtureConfig(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.FromSlash(failureFixtureLedger))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{"home": dir, "ledger_path": abs})
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// runLedgerModeOut runs the ledger verb the way main does (runLedgerMode) and returns its stdout.
func runLedgerModeOut(t *testing.T, args ...string) string {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() { runErr = runLedgerMode(args) })
	if runErr != nil {
		t.Fatalf("ledger %v: %v", args, runErr)
	}
	return out
}

// runFailures runs `ledger --failures` against the fixture ledger and the synthetic table.
func runFailures(t *testing.T, extra ...string) string {
	t.Helper()
	args := append([]string{"--failures", "--config", failureFixtureConfig(t), "--table", filepath.FromSlash(failureFixtureTable)}, extra...)
	return runLedgerModeOut(t, args...)
}

func decodeFailureReport(t *testing.T, out string) failureReport {
	t.Helper()
	var r failureReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("the register output is not one JSON object: %v\n%s", err, out)
	}
	return r
}

func classNames(r failureReport) []string {
	names := []string{}
	for _, c := range r.Classes {
		names = append(names, c.Class)
	}
	return names
}

func classByName(t *testing.T, r failureReport, name string) failureClassOut {
	t.Helper()
	for _, c := range r.Classes {
		if c.Class == name {
			return c
		}
	}
	t.Fatalf("class %q is not in the output (classes: %v)", name, classNames(r))
	return failureClassOut{}
}

// The same ledger and table give the same bytes whatever order the flags come in, the classes come out
// count-descending then by name, and the output is the committed golden file. The output is ONE JSON value:
// the verb's usual trailing text lines would break a script (and the evidence file) that reads it.
func TestFailureRegisterIsDeterministicOnTheFixtureLedger(t *testing.T) {
	a := runFailures(t)
	b := runLedgerModeOut(t, "--table", filepath.FromSlash(failureFixtureTable), "--config", failureFixtureConfig(t), "--failures")
	if a != b {
		t.Fatalf("flag order changed the output:\n--- a\n%s\n--- b\n%s", a, b)
	}
	if !json.Valid([]byte(a)) {
		t.Fatalf("stdout is not a single JSON value (a text line follows it?):\n%s", a)
	}
	r := decodeFailureReport(t, a)
	want := []string{"node_busy", "abstention", "failed_verification", "capacity_wait", "budget", "config", "other", "stall_prefill"}
	if got := classNames(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("class order = %v, want %v (count descending, then class name)", got, want)
	}
	for i := 1; i < len(r.Classes); i++ {
		p, c := r.Classes[i-1], r.Classes[i]
		if p.Count < c.Count || (p.Count == c.Count && p.Class > c.Class) {
			t.Errorf("classes %q (%d) and %q (%d) are out of order", p.Class, p.Count, c.Class, c.Count)
		}
	}
	golden, err := os.ReadFile(filepath.FromSlash(failureFixtureGolden))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	// A Windows checkout may have rewritten the golden's line endings.
	wantBytes := bytes.TrimSpace(bytes.ReplaceAll(golden, []byte("\r\n"), []byte("\n")))
	if got := bytes.TrimSpace([]byte(a)); !bytes.Equal(got, wantBytes) {
		t.Fatalf("output differs from %s:\n--- got\n%s\n--- want\n%s", failureFixtureGolden, got, wantBytes)
	}
}

// A token total nobody measured is null, never 0: the column of a class whose rows recorded no card work is
// a gap, and a 0 there would read as "this failure cost nothing". A measured zero (the row carries the
// cards_tokens key, with 0) is a real zero and stays 0; a mixed class sums what was measured and says how many
// rows that was.
func TestFailureRegisterUnresolvedTokensAreNullNeverZero(t *testing.T) {
	out := runFailures(t)
	var raw struct {
		Classes []map[string]any `json:"classes"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, c := range raw.Classes {
		byName[c["class"].(string)] = c
	}
	for _, name := range []string{"capacity_wait", "config", "other"} {
		c := byName[name]
		v, present := c["cards_tokens"]
		if !present {
			t.Errorf("%s: the cards_tokens key is missing; an unresolved total must be present and null", name)
		}
		if v != nil {
			t.Errorf("%s: cards_tokens = %v, want null (no row of the class recorded card work)", name, v)
		}
		if c["rows_with_tokens"].(float64) != 0 {
			t.Errorf("%s: rows_with_tokens = %v, want 0", name, c["rows_with_tokens"])
		}
		for _, g := range c["groups"].([]any) {
			if gv, ok := g.(map[string]any)["cards_tokens"]; !ok || gv != nil {
				t.Errorf("%s: a group's cards_tokens = %v (present=%v), want null", name, gv, ok)
			}
		}
	}
	// Text-level guard: no unresolved class may carry a literal 0 in its token column.
	for _, name := range []string{"capacity_wait", "config", "other"} {
		i := strings.Index(out, `"class": "`+name+`"`)
		if i < 0 {
			t.Fatalf("class %s not found in output", name)
		}
		block := out[i:]
		if j := strings.Index(block, `"doors"`); j > 0 {
			block = block[:j]
		}
		if strings.Contains(block, `"cards_tokens": 0`) {
			t.Errorf("%s: serialized an unresolved total as 0:\n%s", name, block)
		}
	}
	if got := byName["node_busy"]["cards_tokens"]; got != float64(0) {
		t.Errorf("node_busy rows all carry the key with 0: cards_tokens = %v, want a measured 0", got)
	}
	if got := byName["abstention"]["cards_tokens"]; got != float64(360) {
		t.Errorf("abstention: cards_tokens = %v, want 360 (300 stored + 60 from the older columns; the third row has none)", got)
	}
	if got := byName["abstention"]["rows_with_tokens"]; got != float64(2) {
		t.Errorf("abstention: rows_with_tokens = %v, want 2 of 3", got)
	}
	if got := byName["stall_prefill"]["cards_tokens"]; got != float64(50) {
		t.Errorf("stall_prefill (an orphan inner row stored with 0): cards_tokens = %v, want 50 read from its older columns", got)
	}
}

// A class with no rows in the window never appears: not a zero line, not a placeholder. Test traffic, dispatch
// markers, inner rows with a parent, cache hits and successful rows make no class either, and the left-out
// traffic is counted. With --include-test-traffic the bake and test rows come back as their classes.
func TestFailureRegisterListsOnlyClassesWithRows(t *testing.T) {
	r := decodeFailureReport(t, runFailures(t))
	for _, c := range r.Classes {
		if c.Count <= 0 {
			t.Errorf("class %q listed with count %d", c.Class, c.Count)
		}
	}
	for _, absent := range []string{"write", "contract", "ok", "started", "stall_decode", "queue_full"} {
		for _, c := range r.Classes {
			if c.Class == absent {
				t.Errorf("class %q has no production failure row in the fixture and must not be listed", absent)
			}
		}
	}
	if r.FailureRows != 16 {
		t.Errorf("failure_rows = %d, want 16 (the inner row of j-bud, the marker, the cache hit, the ok row and the bake and test rows are not failures)", r.FailureRows)
	}
	if r.ExcludedNonProduction["bake"] != 1 || r.ExcludedNonProduction["test"] != 1 || len(r.ExcludedNonProduction) != 2 {
		t.Errorf("excluded_non_production = %v, want bake 1 and test 1", r.ExcludedNonProduction)
	}
	if b := classByName(t, r, "budget"); b.Count != 1 || b.ExampleReason != "step budget exhausted" {
		t.Errorf("budget = %d x %q: the inner row of the job must not count or supply the example", b.Count, b.ExampleReason)
	}

	all := decodeFailureReport(t, runFailures(t, "--include-test-traffic"))
	if all.FailureRows != 18 || len(all.ExcludedNonProduction) != 0 {
		t.Errorf("with test traffic: failure_rows = %d (want 18), excluded = %v (want none)", all.FailureRows, all.ExcludedNonProduction)
	}
	classByName(t, all, "write")
	classByName(t, all, "contract")

	none := decodeFailureReport(t, runFailures(t, "--since", "1"))
	if none.FailureRows != 0 || none.Classes == nil || len(none.Classes) != 0 {
		t.Errorf("a window that holds no row: failure_rows = %d, classes = %#v, want an empty (non-null) list", none.FailureRows, none.Classes)
	}
}

// The docs table is joined to the output by class name. A class the table has no row for says "undocumented"
// in every human column; a table row for a class the ledger does not hold adds nothing; an empty fix cell reads
// "open" and an empty root-cause cell "not recorded".
func TestFailureRegisterJoinsTheDocsTableByClassName(t *testing.T) {
	r := decodeFailureReport(t, runFailures(t))
	nb := classByName(t, r, "node_busy")
	if !nb.Documented || nb.Meaning != "A seat answered a capacity defer after admission." || nb.RootCause != "Another job held what the call needed." || nb.StructuralFix != "open" {
		t.Errorf("node_busy join = %+v", nb)
	}
	ab := classByName(t, r, "abstention")
	if !ab.Documented || ab.RootCause != "The work, not the stack." || ab.StructuralFix != "open" {
		t.Errorf("abstention (empty fix cell) = documented %v, root cause %q, fix %q; want the table's root cause and open", ab.Documented, ab.RootCause, ab.StructuralFix)
	}
	fv := classByName(t, r, "failed_verification")
	if !fv.Documented || fv.RootCause != "not recorded" || fv.StructuralFix != "Tighten the contract (synthetic row)." {
		t.Errorf("failed_verification (empty root-cause cell) = documented %v, root cause %q, fix %q", fv.Documented, fv.RootCause, fv.StructuralFix)
	}
	for _, name := range []string{"capacity_wait", "config", "other"} {
		c := classByName(t, r, name)
		if c.Documented || c.Meaning != "undocumented" || c.RootCause != "undocumented" || c.StructuralFix != "undocumented" {
			t.Errorf("%s has no row in the table and must read undocumented in every human column: %+v", name, c)
		}
	}
	if want := []string{"capacity_wait", "config", "other"}; !reflect.DeepEqual(r.UndocumentedClasses, want) {
		t.Errorf("undocumented_classes = %v, want %v", r.UndocumentedClasses, want)
	}
	// The table's own `write` row has no ledger rows behind it, so it is not in the output (above), and a
	// table with the node_busy row removed turns that class undocumented.
	rows, err := os.ReadFile(filepath.FromSlash(failureFixtureTable))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(string(rows), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, "| `node_busy` |") {
			kept = append(kept, line)
		}
	}
	docs, err := parseFailureTable(strings.Join(kept, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := ledger.FailureRegisterFile(filepath.FromSlash(failureFixtureLedger), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := joinFailureDocs(reg, docs)
	for _, c := range joined.Classes {
		if c.Class == "node_busy" && (c.Documented || c.RootCause != "undocumented") {
			t.Errorf("node_busy after its docs row was removed = %+v, want undocumented", c)
		}
	}
}

func TestFailureTableParser(t *testing.T) {
	good := "intro\n\n| class | meaning | root cause | structural fix |\n|---|---|---|---|\n" +
		"| `a_one` | m1 | c1 | f1 |\n| `b_two` | has a \\| pipe | | |\n\nafter\n\n| wire defer class | filed under |\n|---|---|\n| `capacity` | `node_busy` |\n"
	for name, text := range map[string]string{"lf": good, "crlf": strings.ReplaceAll(good, "\n", "\r\n")} {
		docs, err := parseFailureTable(text)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(docs) != 2 {
			t.Fatalf("%s: %d rows, want 2 (the second table is not part of the first): %v", name, len(docs), docs)
		}
		if d := docs["b_two"]; d.Meaning != "has a | pipe" || d.RootCause != "not recorded" || d.StructuralFix != "open" {
			t.Errorf("%s: b_two = %+v", name, d)
		}
	}
	bad := map[string]string{
		"no table":       "just prose\n",
		"missing column": "| class | meaning | root cause |\n|---|---|---|\n| `x` | a | b |\n",
		"duplicate row":  "| class | meaning | root cause | structural fix |\n|---|---|---|---|\n| `x` | a | b | c |\n| `x` | a | b | c |\n",
		"empty class":    "| class | meaning | root cause | structural fix |\n|---|---|---|---|\n| | a | b | c |\n",
	}
	for name, text := range bad {
		if _, err := parseFailureTable(text); err == nil {
			t.Errorf("%s: parseFailureTable accepted a malformed table", name)
		}
	}
}

// The page the binary embeds covers the whole closed set: every reason code that can be a failure has a row
// (the join would otherwise print "undocumented" for a class the code itself defines), every row names a code
// that exists, and the wire defer classes listed on the page are exactly core's, filed under the code the
// ledger files them under.
func TestEmbeddedFailureTableCoversEveryReasonCode(t *testing.T) {
	docs, err := parseFailureTable(failureRegisterDoc)
	if err != nil {
		t.Fatalf("docs/systems/failure-register.md: %v", err)
	}
	isFailure := map[string]bool{}
	for _, c := range ledger.ReasonCodes() {
		if c == ledger.ReasonOK || c == ledger.ReasonStarted {
			continue
		}
		isFailure[c] = true
		d, ok := docs[c]
		if !ok {
			t.Errorf("reason code %q has no row in the table of docs/systems/failure-register.md", c)
			continue
		}
		if strings.TrimSpace(d.Meaning) == "" {
			t.Errorf("reason code %q: the meaning cell is empty", c)
		}
	}
	for c := range docs {
		if !isFailure[c] {
			t.Errorf("the table has a row for %q, which is not a failure member of the ledger's closed reason-code set", c)
		}
	}

	header, rows, err := readMarkdownTable(failureRegisterDoc, "wire defer class")
	if err != nil {
		t.Fatalf("wire defer class table: %v", err)
	}
	if len(header) != 2 {
		t.Fatalf("wire defer class table header = %v", header)
	}
	filed := map[string]string{}
	for _, r := range rows {
		filed[strings.Trim(r[0], "` ")] = strings.Trim(r[1], "` ")
	}
	wire := []string{core.DeferClassAbstention, core.DeferClassBudget, core.DeferClassInfrastructure, core.DeferClassConfig,
		core.DeferClassContract, core.DeferClassCapacity, core.DeferClassWrite}
	for _, c := range wire {
		want := ledger.CodeForDeferClass(c)
		if got, ok := filed[c]; !ok || got != want {
			t.Errorf("wire defer class %q: the page files it under %q (listed=%v), the ledger files it under %q", c, got, ok, want)
		}
	}
	if len(filed) != len(wire) {
		t.Errorf("the page lists %d wire defer classes, core defines %d", len(filed), len(wire))
	}
}

// Without --failures the ledger verb is what it was: no register in its output. With it, none of the savings
// summary's fields leak in either: it is a different report, not the same one plus a flag.
func TestLedgerFailuresModeIsAdditive(t *testing.T) {
	cfgPath := failureFixtureConfig(t)
	plain := runLedgerModeOut(t, "--config", cfgPath)
	for _, key := range []string{`"classes"`, `"failure_rows"`} {
		if strings.Contains(plain, key) {
			t.Errorf("the default ledger output grew %s:\n%s", key, plain)
		}
	}
	reg := runFailures(t)
	for _, key := range []string{`"calls"`, `"by_task"`, `"tokens_saved"`} {
		if strings.Contains(reg, key) {
			t.Errorf("the register output carries the summary field %s", key)
		}
	}
	for _, args := range [][]string{{"--failures"}, {"-failures"}, {"--failures=true"}, {"--config", "x", "--failures"}} {
		if !ledgerFailuresRequested(args) {
			t.Errorf("ledgerFailuresRequested(%v) = false", args)
		}
	}
	for _, args := range [][]string{nil, {"--since", "3"}, {"--failures=false"}, {"--by-session"}, {"--session", "x"}, {"--", "--failures"}} {
		if ledgerFailuresRequested(args) {
			t.Errorf("ledgerFailuresRequested(%v) = true", args)
		}
	}
}
