package compeval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// --- MsgsToTurns / ToMsgs round-trip -----------------------------------------

func TestMsgsToTurnsRoundTrip(t *testing.T) {
	msgs := []agent.Msg{
		{Role: "system", Content: "you are an agent"},
		{Role: "user", Content: "objective: map the repo"},
		{Role: "assistant", Content: "", ToolCalls: []agent.ToolCall{{ID: "c1", Name: "list_dir", Args: `{"path":"."}`}}},
		{Role: "tool", ToolCallID: "c1", Content: "a.go\nb.go"},
		{Role: "assistant", Content: "two files"},
	}
	back := ToMsgs(MsgsToTurns(msgs))
	// IsError is deliberately not carried; everything else must survive exactly.
	if !reflect.DeepEqual(msgs, back) {
		t.Fatalf("round trip diverged:\n%#v\nvs\n%#v", msgs, back)
	}
}

// --- redaction ---------------------------------------------------------------

func TestRedactorGitDerivedEmails(t *testing.T) {
	// The motivating case: git log output carries author emails; the corpus
	// vet refuses emails, so harvest must neutralize them deterministically.
	in := "commit abc123\nAuthor: Dan M <dan@example.com>\ncommit def456\nAuthor: Ana P <ana@example.org>\nAuthor: Dan M <dan@example.com>"
	r := newRedactor()
	out := r.redact(in)
	if strings.Contains(out, "@") {
		t.Fatalf("emails survived redaction: %q", out)
	}
	// Distinct emails → distinct placeholders; the repeated email reuses its
	// first placeholder (distinctness survives, determinism holds).
	if !strings.Contains(out, "[redacted-email-1]") || !strings.Contains(out, "[redacted-email-2]") {
		t.Fatalf("expected numbered placeholders, got: %q", out)
	}
	if strings.Count(out, "[redacted-email-1]") != 2 {
		t.Fatalf("repeated email should reuse placeholder 1 twice, got: %q", out)
	}
	if r.Hits["email"] != 3 {
		t.Fatalf("expected 3 email substitutions, got %d", r.Hits["email"])
	}
	// A second redactor over the same input produces identical bytes.
	if out2 := newRedactor().redact(in); out2 != out {
		t.Fatalf("redaction not deterministic:\n%q\nvs\n%q", out, out2)
	}
}

func TestRedactorCredentialClasses(t *testing.T) {
	// All values are FAKE, pattern-shaped fixtures assembled at runtime so the
	// repo's own secret scanners never see a literal credential shape.
	cases := map[string]string{
		"bearer-token":       "Authorization: Bearer " + strings.Repeat("ab12", 5),
		"api-key-assignment": `api_key = "` + "sk_" + strings.Repeat("x", 14) + `"`,
		"aws-access-key":     "creds " + "AKIA" + strings.Repeat("ABCD", 4) + " end",
		"github-token":       "remote token " + "ghp_" + strings.Repeat("z9", 12) + " end",
	}
	for class, in := range cases {
		r := newRedactor()
		out := r.redact(in)
		if r.Hits[class] == 0 {
			t.Errorf("%s: no substitution in %q → %q", class, in, out)
			continue
		}
		// The redacted text must be vet-clean for that class.
		e := Entry{ID: "x", Kind: "prose", Turns: []Turn{{Role: "user", Content: out}}}
		for _, f := range VetPII([]Entry{e}) {
			if f.Class == class {
				t.Errorf("%s: residual vet finding after redaction: %q", class, out)
			}
		}
	}
}

// pemMarker assembles a PEM boundary line at runtime (never a literal in
// source, so the repo's secret scanners don't flag the test file itself).
func pemMarker(kind, alg string) string {
	return "-----" + kind + " " + alg + "PRIVATE" + " KEY-----"
}

func TestRedactorPrivateKeyBlockWholeBlock(t *testing.T) {
	fakeBody := "MIIEow" + strings.Repeat("A", 10)
	full := "before\n" + pemMarker("BEGIN", "RSA ") + "\n" + fakeBody + "\nzzzz\n" + pemMarker("END", "RSA ") + "\nafter"
	out := newRedactor().redact(full)
	if strings.Contains(out, fakeBody) || strings.Contains(out, "BEGIN RSA") {
		t.Fatalf("key material survived: %q", out)
	}
	if !strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Fatalf("redaction ate surrounding text: %q", out)
	}
	// Truncated block (no END marker): redact through end-of-string — leaving
	// the body behind would pass the vet (it only knows the BEGIN header).
	trunc := "before\n" + pemMarker("BEGIN", "") + "\n" + fakeBody + "\nzzzz"
	out2 := newRedactor().redact(trunc)
	if strings.Contains(out2, fakeBody) {
		t.Fatalf("truncated key body survived: %q", out2)
	}
}

// Parity by construction: every vet class must have a same-named redactor (a
// vet class without one turns the residual gate into a hard failure on real
// data), and the redactor's regex must match everything the vet regex matches.
func TestRedactorCoversAllVetClasses(t *testing.T) {
	if len(redactPatterns) != len(piiPatterns) {
		t.Fatalf("redactPatterns has %d classes, piiPatterns has %d", len(redactPatterns), len(piiPatterns))
	}
	for i, p := range piiPatterns {
		if redactPatterns[i].name != p.name {
			t.Fatalf("class order/name drift at %d: redact=%q vet=%q", i, redactPatterns[i].name, p.name)
		}
	}
	for name := range redactOverrides {
		found := false
		for _, p := range piiPatterns {
			if p.name == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("redact override %q names a class the vet does not have", name)
		}
	}
}

// The placeholders themselves must never re-trip the vet (a placeholder that
// matched a pattern would make every harvest refuse itself).
func TestPlaceholdersAreVetClean(t *testing.T) {
	var b strings.Builder
	for _, p := range redactPatterns {
		for i := 1; i <= 3; i++ {
			b.WriteString(" [redacted-" + p.name + "-" + string(rune('0'+i)) + "]")
		}
	}
	e := Entry{ID: "x", Kind: "prose", Turns: []Turn{{Role: "user", Content: b.String()}}}
	if f := VetPII([]Entry{e}); len(f) > 0 {
		t.Fatalf("placeholders trip the vet: %+v", f)
	}
}

// --- kind classification -----------------------------------------------------

func toolTurns(contents ...string) []Turn {
	turns := []Turn{{Role: "user", Content: "objective: x"}}
	for i, c := range contents {
		id := "c" + string(rune('1'+i))
		turns = append(turns,
			Turn{Role: "assistant", ToolCalls: []TurnToolCall{{ID: id}}},
			Turn{Role: "tool", ToolCallID: id, Content: c},
		)
	}
	return turns
}

func TestClassifyKind(t *testing.T) {
	jsonPayload := `[{"file":"a.go","lines":10},{"file":"b.go","lines":20}]`
	logPayload := "2026-07-24T10:00:01 INFO starting\nERROR: connection refused\n--- FAIL: TestX (0.02s)\nexit status 1"
	codePayload := "package main\n\nfunc main() {\n\tx := 1\n\treturn\n}\n"
	prosePayload := "The report covers quarterly revenue trends and their seasonal drivers in plain narrative form without structure."
	cases := []struct {
		name string
		in   []Turn
		want string
	}{
		{"tool-json", toolTurns(jsonPayload), "tool-json"},
		{"logs", toolTurns(logPayload), "logs"},
		{"code", toolTurns(codePayload), "code"},
		{"tool-text", toolTurns(prosePayload), "tool-text"},
		{"prose-no-tools", []Turn{{Role: "user", Content: "objective"}, {Role: "assistant", Content: "long analysis"}}, "prose"},
		// Three near-equal payloads of different classes: no class reaches the
		// ≥60% byte majority → honestly "mixed".
		{"mixed", toolTurns(jsonPayload, logPayload, prosePayload), "mixed"},
	}
	for _, c := range cases {
		if got := ClassifyKind(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	// Determinism: same input, same answer, every time.
	for i := 0; i < 5; i++ {
		if got := ClassifyKind(cases[5].in); got != "mixed" {
			t.Fatalf("classifier nondeterministic on run %d: %q", i, got)
		}
	}
}

// --- preamble ----------------------------------------------------------------

func TestPreambleLen(t *testing.T) {
	turns := []Turn{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "objective"},
		{Role: "assistant", Content: "plan"},
		{Role: "user", Content: "go"},
	}
	if got := preambleLen(turns); got != 2 {
		t.Fatalf("preamble: got %d want 2", got)
	}
	if got := preambleLen([]Turn{{Role: "assistant", Content: "x"}}); got != 1 {
		t.Fatalf("assistant-first preamble floor: got %d want 1", got)
	}
	if got := preambleLen([]Turn{{Role: "user", Content: "x"}}); got != 1 {
		t.Fatalf("no-assistant preamble floor: got %d want 1", got)
	}
}

// --- harvest end-to-end ------------------------------------------------------

func writeTraceFile(t *testing.T, dir, name string, rec TraceRecord) {
	t.Helper()
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sampleTrace(id string, toolPayload string) TraceRecord {
	return TraceRecord{
		ID: id, Goal: "map the repo", StopReason: "done", Steps: 2, Output: "done",
		Transcript: []agent.Msg{
			{Role: "system", Content: "you are the local agent"},
			{Role: "user", Content: "objective: map the repo"},
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"log.txt"}`}}},
			{Role: "tool", ToolCallID: "c1", Content: toolPayload},
			{Role: "assistant", Content: "summary of findings"},
		},
	}
}

func TestHarvestTracesEndToEnd(t *testing.T) {
	dir := t.TempDir()
	// One trace with git-derived emails in the tool payload (the motivating case).
	writeTraceFile(t, dir, "g1.json", sampleTrace("g1",
		"commit abc\nAuthor: Dan <dan@example.com>\ncommit def\nAuthor: Ana <ana@example.org>"))
	// One clean log-ish trace.
	writeTraceFile(t, dir, "g2.json", sampleTrace("g2",
		"2026-07-24T10:00:01 INFO starting\nERROR: refused\nexit status 1"))
	// One corrupt file and one too-short trace: skipped WITH notes, never fatal.
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTraceFile(t, dir, "short.json", TraceRecord{ID: "s", Transcript: []agent.Msg{{Role: "user", Content: "hi"}}})

	entries, stats, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 4 || stats.Harvested != 2 || len(entries) != 2 {
		t.Fatalf("expected 2/4 harvested, got %d/%d (%d entries): %+v", stats.Harvested, stats.Files, len(entries), stats.Skipped)
	}
	if len(stats.Skipped) != 2 {
		t.Fatalf("expected 2 skip notes, got %+v", stats.Skipped)
	}
	if stats.Redactions["email"] != 2 {
		t.Fatalf("expected 2 email redactions, got %+v", stats.Redactions)
	}
	// Entry ids come from FILE names, prefixed; preamble = system+objective = 2.
	if entries[0].ID != "hv-g1" || entries[1].ID != "hv-g2" {
		t.Fatalf("ids: %q %q", entries[0].ID, entries[1].ID)
	}
	for _, e := range entries {
		if e.ProtectedPrefix != 2 {
			t.Fatalf("%s: protected_prefix %d want 2", e.ID, e.ProtectedPrefix)
		}
	}
	// The produced corpus is vet-clean and survives the strict writer/loader.
	if f := VetPII(entries); len(f) > 0 {
		t.Fatalf("harvested entries not vet-clean: %+v", f)
	}
	out := filepath.Join(dir, "corpus.jsonl")
	hash, err := WriteCorpus(out, entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) != 64 {
		t.Fatalf("bad corpus hash %q", hash)
	}
	// Determinism: harvesting again writes a byte-identical corpus (same hash).
	entries2, _, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil {
		t.Fatal(err)
	}
	out2 := filepath.Join(dir, "corpus2.jsonl")
	hash2, err := WriteCorpus(out2, entries2)
	if err != nil {
		t.Fatal(err)
	}
	if hash2 != hash {
		t.Fatalf("harvest not deterministic: %s vs %s", hash, hash2)
	}
}

// Fidelity: harvested entries must replay at PRODUCTION keep-recent, and a
// transcript the ladder already compacted mid-run must be refused with a note.
func TestHarvestMirrorsProductionKeepRecent(t *testing.T) {
	dir := t.TempDir()
	writeTraceFile(t, dir, "a.json", sampleTrace("a", "plain payload"))
	entries, _, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].KeepRecent != agent.DefaultKeepRecent {
		t.Fatalf("keep_recent %d, want production default %d", entries[0].KeepRecent, agent.DefaultKeepRecent)
	}
}

func TestHarvestRefusesPreCompactedTranscripts(t *testing.T) {
	dir := t.TempDir()
	elided := sampleTrace("e", "[earlier result elided to fit context — 4321 chars]")
	writeTraceFile(t, dir, "elided.json", elided)
	skel := sampleTrace("s", "[skeleton — pruned from 9876 chars]\nkept line\nERROR kept")
	writeTraceFile(t, dir, "skel.json", skel)
	writeTraceFile(t, dir, "clean.json", sampleTrace("c", "raw payload"))
	entries, stats, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "hv-clean" {
		t.Fatalf("expected only the clean trace, got %+v", entries)
	}
	if stats.PreCompacted != 2 {
		t.Fatalf("pre_compacted count %d, want 2", stats.PreCompacted)
	}
	for _, n := range stats.Skipped {
		if !strings.Contains(n.Reason, "pre-compacted") {
			t.Fatalf("skip note must name the refusal: %+v", n)
		}
	}
}

// A malformed trace (tool turn without a tool_call_id) becomes a per-file skip
// note — never a whole-harvest abort at write time.
func TestHarvestSkipsStructurallyInvalidTraces(t *testing.T) {
	dir := t.TempDir()
	bad := sampleTrace("b", "payload")
	bad.Transcript[3].ToolCallID = ""
	writeTraceFile(t, dir, "bad.json", bad)
	writeTraceFile(t, dir, "good.json", sampleTrace("g", "payload"))
	entries, stats, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "hv-good" {
		t.Fatalf("expected only the good trace, got %+v", entries)
	}
	if len(stats.Skipped) != 1 || !strings.Contains(stats.Skipped[0].Reason, "tool_call_id") {
		t.Fatalf("expected a tool_call_id skip note, got %+v", stats.Skipped)
	}
}

// PII inside tool-call ARGS must be redacted through the harvest path (not
// merely refused by the residual gate).
func TestHarvestRedactsToolCallArgs(t *testing.T) {
	dir := t.TempDir()
	tr := sampleTrace("a", "clean payload")
	tr.Transcript[2].ToolCalls[0].Args = `{"path":"inbox","query":"from:dan@example.com"}`
	writeTraceFile(t, dir, "a.json", tr)
	entries, stats, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil {
		t.Fatalf("harvest refused instead of redacting Args: %v", err)
	}
	args := entries[0].Turns[2].ToolCalls[0].Args
	if strings.Contains(args, "@") || !strings.Contains(args, "[redacted-email-1]") {
		t.Fatalf("Args not redacted: %q", args)
	}
	if stats.Redactions["email"] != 1 {
		t.Fatalf("redaction stats missed Args: %+v", stats.Redactions)
	}
}

// A refused corpus must never exist at the destination (atomic write): entries
// that fail the strict re-load leave no file and no temp behind.
func TestWriteCorpusRefusalLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "corpus.jsonl")
	bad := []Entry{{ID: "x", Kind: "prose", Turns: []Turn{{Role: "tool", ToolCallID: "", Content: "orphan"}}}}
	if _, err := WriteCorpus(out, bad); err == nil {
		t.Fatal("expected the strict re-load to refuse")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("refused corpus exists at destination: %v", err)
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestHarvestMaxEntriesCap(t *testing.T) {
	dir := t.TempDir()
	writeTraceFile(t, dir, "a.json", sampleTrace("a", "payload one"))
	writeTraceFile(t, dir, "b.json", sampleTrace("b", "payload two"))
	entries, stats, err := HarvestTraces(dir, HarvestOpts{MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || stats.Harvested != 1 {
		t.Fatalf("cap ignored: %d entries", len(entries))
	}
	if len(stats.Skipped) != 1 || !strings.Contains(stats.Skipped[0].Reason, "cap") {
		t.Fatalf("capped file must be NOTED, not silently dropped: %+v", stats.Skipped)
	}
}

// The residual gate is defense-in-depth against redactor/vet drift: feed it an
// entry that still carries PII (as if a future vet class had no redactor) and
// it must refuse.
func TestRefuseResidualPII(t *testing.T) {
	dirty := []Entry{{ID: "d1", Kind: "prose", Turns: []Turn{{Role: "user", Content: "mail me at leak@example.com"}}}}
	if err := refuseResidualPII(dirty); err == nil {
		t.Fatal("residual PII did not refuse")
	} else if !strings.Contains(err.Error(), "d1:email") {
		t.Fatalf("refusal must name entry+class: %v", err)
	}
	if err := refuseResidualPII([]Entry{{ID: "ok", Kind: "prose", Turns: []Turn{{Role: "user", Content: "clean"}}}}); err != nil {
		t.Fatalf("clean entries refused: %v", err)
	}
}

// A harvested corpus must actually REPLAY — the whole point. Run the
// deterministic evaluator over a harvested corpus end-to-end.
func TestHarvestedCorpusReplays(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("2026-07-24T10:00:01 INFO step ok\nERROR: retrying id=42\n", 40)
	// Enough exchanges that OLDER tool turns fall outside the production
	// keep-recent window (4) the harvest now mirrors — the long payloads sit
	// early so the ladder has something it is allowed to compact.
	rec := TraceRecord{
		ID: "g1", Goal: "analyze logs", StopReason: "done", Steps: 5, Output: "done",
		Transcript: []agent.Msg{
			{Role: "system", Content: "you are the local agent"},
			{Role: "user", Content: "objective: analyze the logs"},
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"a.log"}`}}},
			{Role: "tool", ToolCallID: "c1", Content: long},
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c2", Name: "read_file", Args: `{"path":"b.log"}`}}},
			{Role: "tool", ToolCallID: "c2", Content: long},
			{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c3", Name: "read_file", Args: `{"path":"c.log"}`}}},
			{Role: "tool", ToolCallID: "c3", Content: "short tail"},
			{Role: "assistant", Content: "summary of findings"},
		},
	}
	writeTraceFile(t, dir, "g1.json", rec)
	entries, _, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "corpus.jsonl")
	hash, err := WriteCorpus(out, entries)
	if err != nil {
		t.Fatal(err)
	}
	loaded, lhash, err := Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if lhash != hash {
		t.Fatalf("hash mismatch: %s vs %s", hash, lhash)
	}
	rep := Evaluate(loaded, lhash, LadderOpts{Skeleton: true})
	if len(rep.Entries) != 1 || rep.Entries[0].TokensBefore == 0 {
		t.Fatalf("replay produced no measurement: %+v", rep)
	}
	if rep.Entries[0].Ratio >= 1.0 {
		t.Fatalf("60%%-budget replay should compress a long log payload, ratio=%v", rep.Entries[0].Ratio)
	}
}
