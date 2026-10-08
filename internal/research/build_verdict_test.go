package research

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// thinPage has too little prose for a document anchor (fewer than six distinctive
// tokens): the shape of a page that loaded as a "please enable JavaScript" shell.
const thinPage = "Loading... Please enable JavaScript."

// The harness's own digest owes one thing on EVERY page, whether or not the page
// is thick enough to anchor: that it said something. Empty lists are a complete
// answer for a page with nothing to say, but a digest with empty lists AND no
// verdict said nothing at all, and it must not read as a success. A thin page
// carries no anchor, so without this check its acceptance was empty (register
// C-74, review of the required-fields change: "verified nothing read as
// verified"). The check is the any-of over the digest's four fields, NOT the
// verdict alone: the verdict alone failed 36 pages that had populated lists.
func TestDefaultDigestCarriesTheAnyOfGuardOnEveryPage(t *testing.T) {
	for name, page := range map[string]string{"an anchored page": prosePage, "a page too thin for an anchor": thinPage} {
		specs, _ := Build(Request{Goal: "List the transfer modes named."}, pageWith(page))
		if len(specs) != 1 {
			t.Fatalf("%s: %d contracts, want 1", name, len(specs))
		}
		found := 0
		for _, a := range specs[0].Acceptance {
			if a == "nonempty:key_facts|numbers|quotes|verdict" {
				found++
			}
			if a == "nonempty:verdict" {
				t.Fatalf("%s: acceptance = %v carries the verdict-only check that failed pages with populated lists", name, specs[0].Acceptance)
			}
		}
		if found != 1 {
			t.Fatalf("%s: acceptance = %v, want nonempty:key_facts|numbers|quotes|verdict once on every page of the default digest", name, specs[0].Acceptance)
		}
		if got := requiredOf(t, specs[0].OutputSchema); !containsString(got, "verdict") {
			t.Fatalf("%s: required = %v, want verdict declared", name, got)
		}
	}
}

// A digest that says nothing — every list empty and no verdict — fails on either
// kind of page, and a faithful "nothing here" digest (empty lists, a verdict that
// says so) still passes: a value somewhere is what is asked for, never items.
func TestAllEmptyDefaultDigestFailsButAFaithfulEmptyOneStillPasses(t *testing.T) {
	const allEmpty = `{"key_facts":[],"numbers":[],"quotes":[],"verdict":""}`
	const faithful = `{"key_facts":[],"numbers":[],"quotes":[],"verdict":"the page holds only a loading notice"}`
	for name, page := range map[string]string{"an anchored page": prosePage, "a page too thin for an anchor": thinPage} {
		specs, _ := Build(Request{Goal: "List the transfer modes named."}, pageWith(page))
		c := specs[0].AgentContract
		// The prose the loop wrote is what the anchor reads; the object is what the
		// re-pack produced from it.
		prose := "The page describes pinned staging memory and shared handle transfers, but names none of the requested topics."
		failures := delegate.EvalAcceptance(c, core.AgentWireResult{Output: prose, Structured: json.RawMessage(allEmpty)})
		if len(failures) != 1 || !strings.Contains(failures[0], "nonempty:key_facts|numbers|quotes|verdict") || !strings.Contains(failures[0], `field "verdict" is an empty string`) {
			t.Fatalf("%s: an all-empty digest must fail with ONE reason that names the check and each empty field (failures %v)", name, failures)
		}
		if failures := delegate.EvalAcceptance(c, core.AgentWireResult{Output: prose, Structured: json.RawMessage(faithful)}); len(failures) != 0 {
			t.Fatalf("%s: a faithful empty digest failed acceptance: %v", name, failures)
		}
	}
}

// The silent-empty guard belongs to the harness's own schema only. A caller who
// supplies a schema owns what it asks for (nonEmptyChecks, their own
// acceptance): a schema with no `verdict` field must not gain a check on one.
func TestCallerSchemaGetsNoVerdictCheck(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"rows":{"type":"array","items":{"type":"string"}}}}`)
	specs, _ := Build(Request{Goal: "List the rows.", OutputSchema: schema}, pageWith(thinPage))
	for _, a := range specs[0].Acceptance {
		if strings.HasPrefix(a, "nonempty:") {
			t.Fatalf("a caller's schema got %q: the verdict check is the default digest's own", a)
		}
	}
	if got := requiredOf(t, specs[0].OutputSchema); len(got) != 0 {
		t.Fatalf("required = %v, want none invented for a caller's schema", got)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// The failure measured since 2026-09-30: 36 default-digest pages failed
// nonempty:verdict with populated lists (key_facts median 10.5 items) and
// verdict "", because the research goal never asked for a verdict and the
// re-pack writes an empty value for a field the loop's text did not carry. None
// was an empty page. A digest that said something in ANY field passes, on an
// anchored page and on a page too thin to anchor.
func TestADigestWithPopulatedListsAndNoVerdictPassesAcceptance(t *testing.T) {
	prose := "The page describes pinned staging memory and shared handle transfers, and names the requested modes."
	structured := map[string]string{
		"the live failure shape: facts, numbers and quotes, no verdict": `{"key_facts":["Every buffer is copied through pinned staging memory.","The shared handle path is faster."],"numbers":["4 transfer modes"],"quotes":["The driven path uses shared memory transfers between processes."],"verdict":""}`,
		"key_facts alone": `{"key_facts":["Every buffer is copied through pinned staging memory."],"numbers":[],"quotes":[],"verdict":""}`,
		"numbers alone":   `{"key_facts":[],"numbers":["4 transfer modes"],"quotes":[],"verdict":""}`,
		"quotes alone":    `{"key_facts":[],"numbers":[],"quotes":["The driven path uses shared memory transfers between processes."],"verdict":""}`,
	}
	for pageName, page := range map[string]string{"an anchored page": prosePage, "a page too thin for an anchor": thinPage} {
		specs, _ := Build(Request{Goal: "List the transfer modes named."}, pageWith(page))
		for shape, s := range structured {
			if failures := delegate.EvalAcceptance(specs[0].AgentContract, core.AgentWireResult{Output: prose, Structured: json.RawMessage(s)}); len(failures) != 0 {
				t.Fatalf("%s, %s: a digest that said something failed acceptance: %v", pageName, shape, failures)
			}
		}
	}
}

// A page too thin to anchor, digested to empty lists and a verdict that says so,
// is a complete answer. The thin page carries no anchor check, so the any-of
// guard is its whole acceptance: pin that, so the pass below is the guard's
// and not an anchor's.
func TestAThinPageWithEmptyListsAndAVerdictPasses(t *testing.T) {
	specs, sources := Build(Request{Goal: "List the transfer modes named."}, pageWith(thinPage))
	if sources[0].Fingerprinted {
		t.Fatalf("the thin page was anchored (%q): the test needs a page with no anchor check", sources[0].Anchor)
	}
	c := specs[0].AgentContract
	if want := []string{"nonempty:key_facts|numbers|quotes|verdict"}; !reflect.DeepEqual(c.Acceptance, want) {
		t.Fatalf("a thin page's acceptance = %v, want only %v", c.Acceptance, want)
	}
	thinDigest := core.AgentWireResult{Structured: json.RawMessage(`{"key_facts":[],"numbers":[],"quotes":[],"verdict":"the page is a loading notice and names no transfer mode"}`)}
	if failures := delegate.EvalAcceptance(c, thinDigest); len(failures) != 0 {
		t.Fatalf("a thin page with empty lists and a verdict failed acceptance: %v", failures)
	}
}

// The default digest's goal asks for the verdict on every page, once, as its
// last sentence, and the anchor words still come from the caller's goal alone. A
// caller-supplied schema owns what it asks for and gets no such sentence.
func TestTheDefaultDigestGoalAsksForAVerdictAndACallersSchemaGoalDoesNot(t *testing.T) {
	const ask = "Always end with a one-sentence verdict that answers the goal from this page, or says plainly that the page does not address it."
	const goal = "List the transfer modes named."
	pages := append(pageWith(prosePage), pageWith(thinPage)...)
	specs, _ := Build(Request{Goal: goal, Questions: []string{"Which mode is fastest?"}}, pages)
	if len(specs) != 2 {
		t.Fatalf("%d contracts, want 2", len(specs))
	}
	for i, s := range specs {
		if !strings.HasSuffix(s.Goal, ask) || strings.Count(s.Goal, ask) != 1 {
			t.Fatalf("page %d: the default digest's goal must end with the verdict ask exactly once: %q", i, s.Goal)
		}
	}

	schema := json.RawMessage(`{"type":"object","properties":{"rows":{"type":"array","items":{"type":"string"}}}}`)
	custom, _ := Build(Request{Goal: goal, OutputSchema: schema}, pages)
	for i, s := range custom {
		if strings.Contains(s.Goal, "verdict") {
			t.Fatalf("page %d: a caller's schema goal gained a verdict ask: %q", i, s.Goal)
		}
	}
}

// What Build writes for the default digest crosses the wire to a node, which
// parses its acceptance at the ACK and never evaluates it: the contract must
// decode and validate there. The schema comes through byte for byte (the
// default digest already requires every field the any-of check names, so
// declaring the check's fields changes nothing), and the check names exactly
// the fields of DefaultSchema, so a field added to one is caught on the other.
func TestTheDefaultDigestContractPassesTheNodesACKAndKeepsItsSchemaByteForByte(t *testing.T) {
	specs, _ := Build(Request{Goal: "List the transfer modes named."}, pageWith(prosePage))
	c := specs[0].AgentContract
	if !bytes.Equal(c.OutputSchema, DefaultSchema) {
		t.Fatalf("the default digest's schema was reshaped:\n got %s\nwant %s", c.OutputSchema, DefaultSchema)
	}
	if got := core.RequireAcceptanceFields(DefaultSchema, []string{defaultDigestCheck}); !bytes.Equal(got, DefaultSchema) {
		t.Fatalf("declaring the default digest's check reshaped the default schema:\n got %s\nwant %s", got, DefaultSchema)
	}
	chk, err := core.ParseAcceptanceCheck(defaultDigestCheck)
	if err != nil {
		t.Fatalf("the default digest's check does not parse: %v", err)
	}
	if got, want := chk.Fields(), requiredOf(t, DefaultSchema); !reflect.DeepEqual(got, want) {
		t.Fatalf("the check reads %v but the default schema requires %v: they name one set of fields", got, want)
	}

	c.SchemaVersion = core.AgentWireSchemaVersion
	wire, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := core.DecodeAgentContract(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("a node refused the default digest's contract at the ACK: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate refused the default digest's contract: %v", err)
	}
}
