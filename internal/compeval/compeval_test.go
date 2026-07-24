package compeval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// mkEntry builds a transcript slice with one big tool body designed to force
// the ladder to work: an objective turn (protected), an assistant tool call,
// a long tool result carrying known entities, and a final user turn (recent).
func mkEntry(id, kind, toolBody string) Entry {
	return Entry{
		ID: id, Kind: kind,
		Turns: []Turn{
			{Role: "user", Content: "objective: verify the build and report"},
			{Role: "assistant", ToolCalls: []TurnToolCall{{ID: "c1"}}},
			{Role: "tool", ToolCallID: "c1", Content: toolBody},
			{Role: "user", Content: "and then?"},
		},
	}
}

// logsBody synthesizes a long log with buried signal entities.
func logsBody() string {
	var b strings.Builder
	for i := 0; i < 120; i++ {
		b.WriteString("info: routine line with nothing interesting at all here\n")
	}
	b.WriteString("ERROR: build FAILED at internal/pipeline/pipeline.go with exit_code=2\n")
	for i := 0; i < 120; i++ {
		b.WriteString("info: more filler output continues without any signal\n")
	}
	return b.String()
}

func writeCorpus(t *testing.T, entries []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "corpus.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(entries, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_HashAndRefusals(t *testing.T) {
	p := writeCorpus(t, []string{
		`{"id":"a","kind":"logs","turns":[{"role":"user","content":"x"}]}`,
	})
	entries, hash, err := Load(p)
	if err != nil || len(entries) != 1 || len(hash) != 64 {
		t.Fatalf("load: %v entries=%d hash=%q", err, len(entries), hash)
	}
	// Same bytes => same hash (the pin); different bytes => different hash.
	_, hash2, _ := Load(p)
	if hash != hash2 {
		t.Fatal("hash must be deterministic over identical bytes")
	}
	// Case-INSENSITIVE key matching is Go's decoder behavior, so "Role"/"Content"
	// parse CORRECTLY into the tagged fields — that shape is safe, not a refusal.
	if _, _, err := Load(writeCorpus(t, []string{`{"id":"ok","kind":"logs","turns":[{"Role":"user","Content":"x"}]}`})); err != nil {
		t.Fatalf("case-folded keys of the SAME names must parse: %v", err)
	}
	// Refusals: the real half-parse trap (Go-struct casing "ToolCalls" vs the
	// wire tag tool_calls — no case fold makes those equal), unknown kind,
	// missing id, malformed line, empty turns, missing role, tool turn without
	// tool_call_id, duplicate id.
	for _, bad := range []string{
		`{"id":"b","kind":"logs","turns":[{"role":"assistant","ToolCalls":[{"id":"c1"}]}]}`,
		`{"id":"b","kind":"martian","turns":[{"role":"user","content":"x"}]}`,
		`{"kind":"logs","turns":[{"role":"user","content":"x"}]}`,
		`{not json`,
		`{"id":"c","kind":"logs","turns":[]}`,
		`{"id":"d","kind":"logs","turns":[{"content":"no role"}]}`,
		`{"id":"e","kind":"logs","turns":[{"role":"tool","content":"orphan body"}]}`,
	} {
		if _, _, err := Load(writeCorpus(t, []string{bad})); err == nil {
			t.Fatalf("corpus line %q must refuse", bad)
		}
	}
	dup := `{"id":"same","kind":"logs","turns":[{"role":"user","content":"x"}]}`
	if _, _, err := Load(writeCorpus(t, []string{dup, dup})); err == nil {
		t.Fatal("duplicate ids must refuse (baseline maps key per-entry)")
	}
}

func TestVetPII_RefusalClasses(t *testing.T) {
	// Every value below is a SYNTHETIC pattern probe for the refusal classes —
	// documentation-grade placeholders, not credentials.
	cases := map[string]string{
		"email":              "contact me at someone@example.com please",
		"private-key-block":  "-----BEGIN RSA PRIVATE KEY-----", // secret-ok (regex fixture, no key material)
		"bearer-token":       "Authorization: Bearer abcdefghijklmnop1234",
		"api-key-assignment": `api_key = "sk_abcdefghijklmnop"`, // secret-ok (placeholder)
		"aws-access-key":     "AKIAIOSFODNN7EXAMPLE",            // secret-ok (AWS's own documentation example key)
		"github-token":       "ghp_abcdefghijklmnopqrstuv0123456789", // secret-ok (placeholder)
	}
	for class, content := range cases {
		e := []Entry{{ID: "x", Kind: "logs", Turns: []Turn{{Role: "tool", ToolCallID: "c", Content: content}}}}
		f := VetPII(e)
		if len(f) == 0 || f[0].Class != class {
			t.Errorf("class %s: findings %v", class, f)
		}
	}
	// Tool-call ARGS are scanned too — raw arguments are where harvested
	// transcripts actually carry credentials.
	inArgs := []Entry{{ID: "x", Kind: "logs", Turns: []Turn{{
		Role: "assistant", ToolCalls: []TurnToolCall{{ID: "c1", Name: "fetch", Args: `{"token":"Bearer abcdefghijklmnop1234"}`}},
	}}}}
	if f := VetPII(inArgs); len(f) == 0 || f[0].Class != "bearer-token" {
		t.Errorf("tool-call args must be vetted: %v", f)
	}
	clean := []Entry{{ID: "x", Kind: "logs", Turns: []Turn{{Role: "tool", ToolCallID: "c", Content: "plain build output exit 0"}}}}
	if f := VetPII(clean); len(f) != 0 {
		t.Errorf("clean corpus flagged: %v", f)
	}
}

func TestEntities_ClassesAndRetention(t *testing.T) {
	s := `render failed at D:\repos\x\main.go line 42 with exit_code=2, see https://example.org/build and hash deadbeef01 version 1.2.3 MARKER_DONE`
	ents := Entities(s)
	for _, want := range []string{
		`D:\repos\x\main.go`, "main.go", "exit_code=2", "https://example.org/build", "deadbeef01", "1.2.3", "42", "MARKER_DONE",
	} {
		if _, ok := ents[want]; !ok {
			t.Errorf("entity %q not extracted; got %v", want, ents)
		}
	}
	// Retention: dropping the tool body loses its entities, listed by value.
	before := []agent.Msg{{Role: "tool", Content: s}}
	after := []agent.Msg{{Role: "tool", Content: "[elided]"}}
	ratio, lost := Retention(before, after)
	if ratio != 0 || len(lost) == 0 {
		t.Fatalf("full loss: ratio=%v lost=%v", ratio, lost)
	}
	// Identity keeps everything.
	ratio, lost = Retention(before, before)
	if ratio != 1 || len(lost) != 0 {
		t.Fatalf("identity: ratio=%v lost=%v", ratio, lost)
	}
}

// TestEvaluate_SkeletonBeatsMarkersOnRetention: the whole point of the gentler
// rung — on a logs entry with buried error signal, the skeleton ladder must
// retain MORE entities than the marker-only base ladder, at a fitting budget.
func TestEvaluate_SkeletonBeatsMarkersOnRetention(t *testing.T) {
	entries := []Entry{mkEntry("logs-1", "logs", logsBody())}
	base := Evaluate(entries, "h", LadderOpts{})
	skel := Evaluate(entries, "h", LadderOpts{Skeleton: true})
	if base.Ladder != "base" || skel.Ladder != "skeleton" {
		t.Fatalf("labels: %q %q", base.Ladder, skel.Ladder)
	}
	b, s := base.Entries[0], skel.Entries[0]
	if !b.FitBudget || !s.FitBudget {
		t.Fatalf("both ladders must fit the derived budget: base=%v skel=%v", b.FitBudget, s.FitBudget)
	}
	if s.EntityRetention <= b.EntityRetention {
		t.Fatalf("skeleton retention %v must beat base %v (the error line must survive skeletonization)", s.EntityRetention, b.EntityRetention)
	}
	// The buried ERROR line's signal survives the skeleton ladder.
	found := false
	for _, l := range b.LostEntities {
		if strings.Contains(l, "exit_code=2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("base ladder should have LOST the exit_code entity; lost=%v", b.LostEntities)
	}
	for _, l := range s.LostEntities {
		if strings.Contains(l, "exit_code=2") {
			t.Fatalf("skeleton ladder must KEEP the exit_code entity; lost=%v", s.LostEntities)
		}
	}
}

func TestRatchet_FreezeCheckRefusals(t *testing.T) {
	entries := []Entry{mkEntry("logs-1", "logs", logsBody())}
	rep := Evaluate(entries, "hash-a", LadderOpts{Skeleton: true})
	b := Freeze(rep)
	p := filepath.Join(t.TempDir(), "baseline.json")
	if err := SaveBaseline(p, b); err != nil {
		t.Fatal(err)
	}
	b2, err := LoadBaseline(p)
	if err != nil {
		t.Fatal(err)
	}
	// Identical run: ratchet holds, no breaches.
	breaches, err := Check(b2, rep, 0)
	if err != nil || len(breaches) != 0 {
		t.Fatalf("identical run: %v %v", err, breaches)
	}
	// Corpus-hash mismatch refuses.
	repOther := rep
	repOther.CorpusHash = "hash-b"
	if _, err := Check(b2, repOther, 0); err == nil {
		t.Fatal("cross-corpus ratchet must refuse")
	}
	// Ladder mismatch refuses.
	repLadder := Evaluate(entries, "hash-a", LadderOpts{})
	if _, err := Check(b2, repLadder, 0); err == nil {
		t.Fatal("cross-ladder ratchet must refuse")
	}
	// A drifted entry beyond tolerance breaches.
	repDrift := rep
	repDrift.Entries = append([]EntryResult{}, rep.Entries...)
	repDrift.Entries[0].TokensAfter = int(float64(rep.Entries[0].TokensAfter) * 1.10)
	breaches, err = Check(b2, repDrift, 0.02)
	if err != nil || len(breaches) != 1 {
		t.Fatalf("drift: err=%v breaches=%v", err, breaches)
	}
	// A missing entry refuses.
	repMissing := rep
	repMissing.Entries = nil
	if _, err := Check(b2, repMissing, 0); err == nil {
		t.Fatal("missing entry must refuse")
	}
}

func TestAB_ControlPairGateAborts(t *testing.T) {
	entries := []Entry{mkEntry("logs-1", "logs", logsBody())}
	pairs := []ControlPair{{Name: "p", Good: "good content", Degraded: "degraded"}}
	// A blind scorer (constant) mis-ranks (tie) => the A/B must ABORT.
	blind := func(ctx context.Context, s string) (float64, error) { return 0.5, nil }
	if _, err := RunAB(context.Background(), blind, entries, "h", LadderOpts{}, pairs, nil); err == nil {
		t.Fatal("blind scorer must abort at the control-pair gate")
	}
	// No pairs configured => refuse outright.
	lengthScorer := func(ctx context.Context, s string) (float64, error) { return float64(len(s)), nil }
	if _, err := RunAB(context.Background(), lengthScorer, entries, "h", LadderOpts{}, nil, nil); err == nil {
		t.Fatal("A/B without control pairs must refuse")
	}
	// A working scorer passes the gate and produces paired results + delta.
	rep, err := RunAB(context.Background(), lengthScorer, entries, "h", LadderOpts{}, pairs, []string{"model-x"})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.GatePassed || len(rep.Results) != 1 {
		t.Fatalf("gate=%v results=%d", rep.GatePassed, len(rep.Results))
	}
	// Compaction shrinks the rendered text, so with a length scorer the
	// compacted side must score lower — direction sanity for the pairing.
	if rep.Results[0].ScoreCompact >= rep.Results[0].ScoreFull {
		t.Fatalf("compacted render must be shorter: %+v", rep.Results[0])
	}
	// A scoring error mid-run aborts (no partial A/B).
	failing := func(ctx context.Context, s string) (float64, error) {
		if strings.Contains(s, "objective") {
			return 0, errors.New("boom")
		}
		return float64(len(s)), nil
	}
	if _, err := RunAB(context.Background(), failing, entries, "h", LadderOpts{}, pairs, nil); err == nil {
		t.Fatal("mid-run scoring error must abort")
	}
}
