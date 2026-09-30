package research

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// requiredOf reads a contract schema's `required` list.
func requiredOf(t *testing.T, schema json.RawMessage) []string {
	t.Helper()
	var s struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatalf("schema is not JSON: %v: %s", err, schema)
	}
	return s.Required
}

// minItemsChecks lists the min_items acceptance strings of a contract.
func minItemsChecks(acc []string) []string {
	var out []string
	for _, a := range acc {
		if strings.HasPrefix(a, "min_items:") {
			out = append(out, a)
		}
	}
	return out
}

// pageWith is one fetched page with enough distinctive prose for an anchor.
func pageWith(text string) []Fetched {
	return []Fetched{{URL: "https://docs.example.com/mp/", FinalURL: "https://docs.example.com/mp/", Title: "MP", Text: text}}
}

const prosePage = "The driven path uses shared memory transfers between processes.\nThe engine copies every buffer through pinned staging memory, which is slower than the shared handle path."

// A schema that does not say a checked field is required lets a seat's direct
// JSON answer omit it: the answer validates, skips the structured re-pack, and
// fails acceptance at the delegator after a whole run (register C-74, 36 of the
// 59 min_items failures on 2026-09-29 took exactly this path). Build declares
// every field an acceptance check reads as required, in the default schema and
// in a caller's.
func TestBuildRequiresTheFieldItsAcceptanceChecks(t *testing.T) {
	// The default schema names all four digest fields as required, in
	// declaration order (the gbnf builder reads `required` first for field order).
	specs, _ := Build(Request{Goal: "List the transfer modes named."}, pageWith(prosePage))
	if got, want := requiredOf(t, specs[0].OutputSchema), []string{"key_facts", "numbers", "quotes", "verdict"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("default schema required = %v, want %v", got, want)
	}

	// A caller's schema keeps its own required entries first and gains every
	// field the caller's acceptance reads, whether or not it was declared.
	custom := json.RawMessage(`{"type":"object","properties":{` +
		`"rows":{"type":"array","items":{"type":"string"}},` +
		`"verdict":{"type":"string"},` +
		`"notes":{"type":"array","items":{"type":"string"}}},` +
		`"required":["verdict"]}`)
	specs, _ = Build(Request{
		Goal: "List the rows.", OutputSchema: custom,
		Acceptance: []string{"min_items:rows:2", "nonempty:notes", "min_items:ghost:1"},
	}, pageWith(prosePage))
	got := requiredOf(t, specs[0].OutputSchema)
	if want := []string{"verdict", "rows", "notes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("caller schema required = %v, want %v: the caller's own entry first, then every field its checks read; a field the schema never declares (ghost) is never required", got, want)
	}
	// The contract still validates and the schema is otherwise untouched.
	if err := specs[0].AgentContract.Validate(); err != nil {
		t.Fatalf("contract with the augmented schema does not validate: %v", err)
	}
	if !strings.Contains(string(specs[0].OutputSchema), `"notes"`) || !strings.Contains(string(specs[0].OutputSchema), `"items"`) {
		t.Fatalf("schema lost its properties: %s", specs[0].OutputSchema)
	}
}

// R-02 + PR-3 as ONE design: presence is declared for every checked field, and
// non-emptiness is asked for only where the caller marked it. The old rule
// demanded one item from the ALPHABETICALLY first array of the schema, whatever
// it was (in one caller's schema, an array that was legitimately empty for the
// page), so a faithful "none of the requested topics" digest was scored
// failed_verification.
func TestBuildAsksForItemsOnlyWhereTheCallerMarked(t *testing.T) {
	arrays := `{"type":"object","properties":{` +
		`"a_findings":{"type":"array","items":{"type":"string"}},` +
		`"b_risks":{"type":"array","items":{"type":"string"}},` +
		`"summary":{"type":"string"}}`

	// Nothing marked: no automatic non-empty check at all, and no required list
	// invented either.
	specs, _ := Build(Request{Goal: "Digest it.", OutputSchema: json.RawMessage(arrays + `}`)}, pageWith(prosePage))
	if got := minItemsChecks(specs[0].Acceptance); len(got) != 0 {
		t.Fatalf("an unmarked schema got %v: the alphabetically-first array is not the caller's ask", got)
	}
	if got := requiredOf(t, specs[0].OutputSchema); len(got) != 0 {
		t.Fatalf("required = %v, want none: nothing is acceptance-checked", got)
	}

	// The caller marked b_risks (an array) and summary (a string) required: the
	// first REQUIRED array is the one that must come back non-empty — never
	// a_findings, which sorts first but was not marked.
	specs, _ = Build(Request{Goal: "Digest it.", OutputSchema: json.RawMessage(arrays + `,"required":["summary","b_risks"]}`)}, pageWith(prosePage))
	if got, want := minItemsChecks(specs[0].Acceptance), []string{"min_items:b_risks:1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("min_items checks = %v, want %v", got, want)
	}
	if got, want := requiredOf(t, specs[0].OutputSchema), []string{"summary", "b_risks"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("required = %v, want the caller's own list untouched: %v", got, want)
	}

	// The default schema is the harness's, not a caller's mark: it asks for no
	// items (its one statement, the verdict, is build_verdict_test.go's).
	specs, _ = Build(Request{Goal: "Digest it."}, pageWith(prosePage))
	if got := minItemsChecks(specs[0].Acceptance); len(got) != 0 {
		t.Fatalf("the default schema got %v: only a caller's own mark asks for items", got)
	}
}

// The end of the chain: a digest that faithfully says the page has nothing on
// the topic passes the contract Build wrote, and an answer that leaves a
// checked field out is still refused.
func TestFaithfulEmptyDigestPassesAcceptance(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{` +
		`"a_findings":{"type":"array","items":{"type":"string"}},` +
		`"summary":{"type":"string"}}}`)
	specs, _ := Build(Request{Goal: "Digest it.", OutputSchema: schema}, pageWith(prosePage))
	c := specs[0].AgentContract

	prose := "The page describes pinned staging memory and shared handle transfers, but names none of the requested topics."
	empty := core.AgentWireResult{Output: prose, Structured: json.RawMessage(`{"a_findings":[],"summary":"none of the requested topics appear"}`)}
	if failures := delegate.EvalAcceptance(c, empty); len(failures) != 0 {
		t.Fatalf("a faithful empty digest failed acceptance: %v", failures)
	}

	// The default-schema digest of the same page passes the same way.
	dflt, _ := Build(Request{Goal: "Digest it."}, pageWith(prosePage))
	emptyDefault := core.AgentWireResult{Output: prose, Structured: json.RawMessage(`{"key_facts":[],"numbers":[],"quotes":[],"verdict":"nothing on this page answers the goal"}`)}
	if failures := delegate.EvalAcceptance(dflt[0].AgentContract, emptyDefault); len(failures) != 0 {
		t.Fatalf("a faithful empty default digest failed acceptance: %v", failures)
	}

	// A caller who DID mark the array gets what they asked for.
	marked := json.RawMessage(`{"type":"object","properties":{"a_findings":{"type":"array","items":{"type":"string"}}},"required":["a_findings"]}`)
	m, _ := Build(Request{Goal: "Digest it.", OutputSchema: marked}, pageWith(prosePage))
	if failures := delegate.EvalAcceptance(m[0].AgentContract, core.AgentWireResult{Output: prose, Structured: json.RawMessage(`{"a_findings":[]}`)}); len(failures) == 0 {
		t.Fatal("an empty array the caller marked required passed acceptance")
	}
}
