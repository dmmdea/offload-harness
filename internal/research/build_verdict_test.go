package research

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// thinPage has too little prose for a document anchor (fewer than six distinctive
// tokens): the shape of a page that loaded as a "please enable JavaScript" shell.
const thinPage = "Loading... Please enable JavaScript."

// The harness's own digest owes one thing on EVERY page, whether or not the page
// is thick enough to anchor: a verdict. Empty lists are a complete answer for a
// page with nothing to say, but a digest with empty lists AND no verdict said
// nothing at all, and it must not read as a success. A thin page carries no
// anchor, so without this check its acceptance was empty (register C-74, review
// of the required-fields change: "verified nothing read as verified").
func TestDefaultDigestCarriesAVerdictCheckOnEveryPage(t *testing.T) {
	for name, page := range map[string]string{"an anchored page": prosePage, "a page too thin for an anchor": thinPage} {
		specs, _ := Build(Request{Goal: "List the transfer modes named."}, pageWith(page))
		if len(specs) != 1 {
			t.Fatalf("%s: %d contracts, want 1", name, len(specs))
		}
		found := false
		for _, a := range specs[0].Acceptance {
			if a == "nonempty:verdict" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: acceptance = %v, want nonempty:verdict on every page of the default digest", name, specs[0].Acceptance)
		}
		if got := requiredOf(t, specs[0].OutputSchema); !containsString(got, "verdict") {
			t.Fatalf("%s: required = %v, want verdict declared", name, got)
		}
	}
}

// A digest that says nothing — every list empty and no verdict — fails on either
// kind of page, and a faithful "nothing here" digest (empty lists, a verdict that
// says so) still passes: presence of the statement is what is asked for, never
// items.
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
		if len(failures) == 0 || !strings.Contains(strings.Join(failures, " "), "verdict") {
			t.Fatalf("%s: an all-empty digest passed acceptance (failures %v)", name, failures)
		}
		if failures := delegate.EvalAcceptance(c, core.AgentWireResult{Output: prose, Structured: json.RawMessage(faithful)}); len(failures) != 0 {
			t.Fatalf("%s: a faithful empty digest failed acceptance: %v", name, failures)
		}
	}
}

// The verdict check belongs to the harness's own schema only. A caller who
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
