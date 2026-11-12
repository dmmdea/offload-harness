package research

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// The research lane failed three kinds of answer that were right, in the 30 days to 2026-11-12 (356 failed_verification rows,
// 346 of them research pages): a digest that quotes its page but restates none of the twenty-four anchor words (the goal's words
// are excluded from them, so a narrow goal's answer uses the goal's words), a digest that says a page holds none of what a
// caller's schema marked required, and a page with no content that was dispatched all the same. This drives the public path
// the lane takes (Build, then the merge gate's EvalAcceptance) with one real-shaped page and digest per case, and the case that
// must keep failing beside each: a digest of another document, a digest that is silent, a shell page that is refused.
func TestResearchAcceptanceDoesNotFailWhatIsAboutThePage(t *testing.T) {
	const limitsPage = `API rate limits

Every request to the image endpoint is metered per workspace, and the limits below apply to each key created in it.

Free workspaces may send 60 requests per minute and hold 4 renders in flight at once; renders beyond that wait in a queue.

Team workspaces may send 600 requests per minute and hold 40 renders in flight at once, and a burst of 2,000 is allowed once an hour.

A request that goes over its limit is answered with status 429 and a Retry-After header naming the seconds to wait.

Enterprise workspaces negotiate limits in their contract; the dashboard shows the figure that applies to a key.
`
	const bakingPage = `Sourdough proofing times

A starter that doubles within six hours is ready to leaven a loaf, and a cooler kitchen slows every stage of the rise.

Bulk fermentation takes four to six hours at room temperature, after which the dough is shaped and left to proof overnight in the refrigerator.

Score the loaf just before baking in a covered Dutch oven so that steam keeps the crust supple while the crumb sets.
`
	goal := "What are the request limits for each plan?"
	// a caller's schema that marks plans required, as the research lane's callers write them
	callerSchema := json.RawMessage(`{"type":"object","properties":{"plans":{"type":"array","items":{"type":"string"}},` +
		`"quotes":{"type":"array","items":{"type":"string"}},"verdict":{"type":"string"}},"required":["plans","quotes","verdict"]}`)

	build := func(t *testing.T, text, contentType string, schema json.RawMessage) core.AgentContract {
		t.Helper()
		specs, sources := Build(Request{Goal: goal, URLs: []string{"https://docs.example.com/limits"}, OutputSchema: schema},
			[]Fetched{{URL: "https://docs.example.com/limits", ContentType: contentType, Text: text, TextBytes: len(text)}})
		if len(specs) != 1 {
			t.Fatalf("Build made %d contracts, want 1 (sources: %+v)", len(specs), sources)
		}
		c := specs[0].AgentContract
		c.Door = "offload_research"
		return c
	}
	grade := func(c core.AgentContract, output, structured string) []string {
		return delegate.EvalAcceptance(c, core.AgentWireResult{Output: output, Structured: json.RawMessage(structured)})
	}
	// the fingerprint on its own, before any excuse: what the lane failed before this change
	fingerprintFails := func(c core.AgentContract, output, structured string) bool {
		for i, o := range delegate.GradeAcceptance(c, core.AgentWireResult{Output: output, Structured: json.RawMessage(structured)}) {
			if strings.Contains(c.Acceptance[i], "(?P<docanchor>") {
				return o.Verdict != core.VerdictPass
			}
		}
		t.Fatal("the contract carries no document fingerprint")
		return false
	}

	t.Run("a digest that quotes its page passes although it restates none of the anchor words", func(t *testing.T) {
		c := build(t, limitsPage, "text/html", nil)
		structured := `{"key_facts":["Free is capped at one request a second","Team is ten times that"],"numbers":["60","600"],` +
			`"quotes":["Free workspaces may send 60 requests per minute and hold 4 renders in flight at once","Team workspaces may send 600 requests per minute and hold 40 renders in flight at once"],"verdict":"Free 60, Team 600."}`
		output := "Free: 60. Team: 600. Burst: 2,000 hourly."
		if !fingerprintFails(c, output, structured) {
			t.Fatal("setup: the anchor regex matched, so this case no longer exercises the quotes route")
		}
		if f := grade(c, output, structured); len(f) != 0 {
			t.Fatalf("a digest whose quotes are the page's own words failed: %v", f)
		}
		// the same quotes with their words changed are no longer the page's, and the digest fails as it always did
		altered := strings.NewReplacer("Free workspaces may send", "Free accounts can post", "Team workspaces may send", "Team accounts can post").Replace(structured)
		if f := grade(c, output, altered); len(f) == 0 {
			t.Fatal("a digest whose quotes are not on the page passed")
		}
	})

	t.Run("a digest of another document fails", func(t *testing.T) {
		c := build(t, limitsPage, "text/html", nil)
		structured := `{"key_facts":["Dough proofs overnight in the refrigerator"],"numbers":["6"],` +
			`"quotes":["Bulk fermentation takes four to six hours at room temperature","Score the loaf just before baking in a covered Dutch oven"],"verdict":"Proof the shaped dough overnight."}`
		f := grade(c, "A starter that doubles within six hours is ready; bulk fermentation takes four to six hours.", structured)
		if len(f) == 0 || !strings.Contains(strings.Join(f, " "), "docanchor") {
			t.Fatalf("a digest of a baking page passed as a digest of the limits page: %v", f)
		}
	})

	t.Run("a digest that says the page holds none of what the schema marked required passes", func(t *testing.T) {
		c := build(t, bakingPage, "text/html", callerSchema)
		structured := `{"plans":[],"quotes":[],"verdict":"The page does not address request limits."}`
		if f := grade(c, "The page does not address request limits.", structured); len(f) != 0 {
			t.Fatalf("an honest none failed: %v", f)
		}
	})

	t.Run("a silent empty digest still fails", func(t *testing.T) {
		for name, schema := range map[string]json.RawMessage{"default schema": nil, "caller schema": callerSchema} {
			c := build(t, limitsPage, "text/html", schema)
			structured := `{"key_facts":[],"numbers":[],"quotes":[],"verdict":""}`
			if name == "caller schema" {
				structured = `{"plans":[],"quotes":[],"verdict":""}`
			}
			if f := grade(c, "Done.", structured); len(f) == 0 {
				t.Fatalf("%s: a digest with nothing in it was accepted", name)
			}
		}
	})

	t.Run("a page with no content is refused, and a short JSON body is not", func(t *testing.T) {
		const shell = "Please wait...\n\nChecking your browser"
		specs, sources := Build(Request{Goal: goal, URLs: []string{"https://docs.example.com/limits"}},
			[]Fetched{{URL: "https://docs.example.com/limits", ContentType: "text/html; charset=utf-8", Text: shell, TextBytes: len(shell)}})
		if len(specs) != 0 || len(sources) != 1 || sources[0].Skipped == "" || sources[0].ErrKind != ErrKindEmptyPage {
			t.Fatalf("a shell page was dispatched or not typed: specs %d, sources %+v", len(specs), sources)
		}
		const body = `{"available":true,"timestamp":"20260101"}`
		if specs, _ := Build(Request{Goal: goal, URLs: []string{"https://docs.example.com/limits"}},
			[]Fetched{{URL: "https://docs.example.com/limits", ContentType: "application/json", Text: body, TextBytes: len(body)}}); len(specs) != 1 {
			t.Fatal("a short JSON body was refused as having no content")
		}
	})
}
