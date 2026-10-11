package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/research"
)

// researchQuotesServer serves one page and answers every contract with the
// given digest, the way a seat that finished its loop and structured its answer
// does. The prose names the page's own words, so a document anchor, if the page
// earns one, is met.
func researchQuotesServer(t *testing.T, page, digest string) *Server {
	t.Helper()
	s := delegateTestServer(t, func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat",
			Output:     "The page covers the changes button, the dialog that closes, and the export menu and its folders.",
			Structured: json.RawMessage(digest), StopReason: "done"}, nil
	})
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		return []research.Fetched{{URL: urls[0], FinalURL: urls[0], Status: 200, Title: "Help", Text: page, Bytes: len(page), TextBytes: len(page)}}
	}
	return s
}

const researchQuotesArgs = `{"goal":"Give the exact label of every button the page names.","urls":["https://docs.example.com/help"],"route":"local"}`

func firstResult(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	results, _ := m["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", m["results"])
	}
	r0, _ := results[0].(map[string]any)
	return r0
}

// Register F4, end to end through the handler: a goal that asks for exact
// labels, a seat that paraphrases one and copies another with straight quote
// marks, and a page with typographic ones. The paraphrase is flagged and kept;
// the copy is published as the page's own bytes; neither fails the digest.
func TestResearchPublishesThePagesOwnQuotesAndFlagsTheRest(t *testing.T) {
	lq, rq := string(rune(0x201C)), string(rune(0x201D))
	page := "Press the " + lq + "Save changes" + rq + " button and the dialog closes. Then open the export menu."
	s := researchQuotesServer(t, page, `{"key_facts":["Saving closes the dialog."],"numbers":[],`+
		`"quotes":["press the \"save changes\" button","The save button stores your work"],"verdict":"The page names one button."}`)
	res, err := s.handleResearch(context.Background(), callReq(researchQuotesArgs))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	if summary, _ := m["summary"].(map[string]any); summary["succeeded"] != float64(1) || summary["failed_verification"] != nil && summary["failed_verification"] != float64(0) {
		t.Fatalf("summary = %v, want the digest a success: a quote that is not on the page is flagged, never failed", m["summary"])
	}
	r0 := firstResult(t, m)
	structured, _ := r0["structured"].(map[string]any)
	quotes, _ := structured["quotes"].([]any)
	if len(quotes) != 2 || quotes[0] != "Press the "+lq+"Save changes"+rq+" button" {
		t.Fatalf("quotes = %v, want the first replaced by the page's own span and the paraphrase kept in place", quotes)
	}
	if quotes[1] != "The save button stores your work" {
		t.Fatalf("quotes[1] = %v, want the seat's paraphrase kept as written (flagged, not dropped)", quotes[1])
	}
	qc, _ := r0["quote_check"].(map[string]any)
	if qc == nil || qc["checked"] != float64(2) || qc["corrected"] != float64(1) {
		t.Fatalf("quote_check = %v, want 2 checked and 1 corrected", r0["quote_check"])
	}
	unverified, _ := qc["unverified"].([]any)
	if len(unverified) != 1 || unverified[0] != "The save button stores your work" {
		t.Fatalf("quote_check.unverified = %v, want the paraphrase named", qc["unverified"])
	}
	if note, _ := qc["note"].(string); !strings.Contains(note, "not on the fetched page") {
		t.Fatalf("quote_check.note = %q", qc["note"])
	}
}

// A digest whose quotes are all the page's bytes still says it was checked, and
// a digest with no quotes says nothing about them.
func TestResearchQuoteCheckIsPresentWhenCheckedAndAbsentWhenThereIsNothingToCheck(t *testing.T) {
	const page = "Press Save to keep your work. The export menu lists three formats."
	s := researchQuotesServer(t, page, `{"key_facts":["a"],"numbers":[],"quotes":["Press Save to keep your work."],"verdict":"v"}`)
	res, err := s.handleResearch(context.Background(), callReq(researchQuotesArgs))
	if err != nil {
		t.Fatal(err)
	}
	qc, _ := firstResult(t, decodeResult(t, res))["quote_check"].(map[string]any)
	if qc == nil || qc["checked"] != float64(1) || qc["verbatim"] != float64(1) || qc["unverified"] != nil || qc["note"] != nil {
		t.Fatalf("quote_check = %v, want one verbatim quote and no flag", qc)
	}

	s = researchQuotesServer(t, page, `{"key_facts":["a"],"numbers":[],"quotes":[],"verdict":"v"}`)
	res, err = s.handleResearch(context.Background(), callReq(researchQuotesArgs))
	if err != nil {
		t.Fatal(err)
	}
	if qc, present := firstResult(t, decodeResult(t, res))["quote_check"]; present {
		t.Fatalf("quote_check = %v on a digest with no quotes, want it absent", qc)
	}
}

// Register F45 / C15, end to end through the handler: the shape that discarded
// nine finished digests (populated lists, an empty verdict) is a success, and the
// digest that said nothing at all (every list empty, no verdict) still fails.
func TestResearchDoesNotDiscardAFinishedDigestForAnEmptyVerdict(t *testing.T) {
	const page = "Press Save to keep your work. The export menu lists three formats."
	populated := `{"key_facts":["Save keeps the work.","Export lists three formats."],"numbers":["3 formats"],"quotes":["Press Save to keep your work."],"verdict":""}`
	empty := `{"key_facts":[],"numbers":[],"quotes":[],"verdict":""}`

	s := researchQuotesServer(t, page, populated)
	res, err := s.handleResearch(context.Background(), callReq(researchQuotesArgs))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	summary, _ := m["summary"].(map[string]any)
	if summary["succeeded"] != float64(1) || (summary["failed_verification"] != nil && summary["failed_verification"] != float64(0)) {
		t.Fatalf("summary = %v, want a digest with populated lists and an empty verdict to succeed (nonempty:verdict failed 9 of these)", m["summary"])
	}
	if fails, _ := firstResult(t, m)["acceptance_failures"].([]any); len(fails) != 0 {
		t.Fatalf("acceptance_failures = %v, want none", fails)
	}

	s = researchQuotesServer(t, page, empty)
	res, err = s.handleResearch(context.Background(), callReq(researchQuotesArgs))
	if err != nil {
		t.Fatal(err)
	}
	m = decodeResult(t, res)
	summary, _ = m["summary"].(map[string]any)
	if summary["failed_verification"] != float64(1) || summary["succeeded"] != nil && summary["succeeded"] != float64(0) {
		t.Fatalf("summary = %v, want the silent-empty digest to fail verification", m["summary"])
	}
	fails, _ := firstResult(t, m)["acceptance_failures"].([]any)
	if len(fails) != 1 || !strings.Contains(fails[0].(string), "nonempty:key_facts|numbers|quotes|verdict") {
		t.Fatalf("acceptance_failures = %v, want the default digest's any-of check named", fails)
	}
}

// The goal the seat is handed carries the copy-exactly sentence, and the harness
// asks the seat for nothing the page does not hold.
func TestResearchHandsTheSeatTheCopyExactlySentence(t *testing.T) {
	var goal string
	s := delegateTestServer(t, func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		goal = c.Goal
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat",
			Output: "ok", Structured: json.RawMessage(`{"key_facts":["a"],"numbers":[],"quotes":[],"verdict":"v"}`), StopReason: "done"}, nil
	})
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		return []research.Fetched{{URL: urls[0], FinalURL: urls[0], Status: 200, Text: "Press Save.", Bytes: 11, TextBytes: 11}}
	}
	if _, err := s.handleResearch(context.Background(), callReq(researchQuotesArgs)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(goal, "copied character for character from the file") {
		t.Fatalf("goal = %q, want the copy-exactly sentence", goal)
	}
}

// Results map to pages through the sources that produced one: when the first URL
// fails to fetch, the one digest belongs to the SECOND page, and its quotes are
// looked up there.
func TestResearchChecksEachDigestAgainstThePageItCameFrom(t *testing.T) {
	const page = "Press Save to keep your work. The export menu lists three formats."
	s := delegateTestServer(t, func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat",
			Output:     "The page covers saving work and the export menu with its three formats.",
			Structured: json.RawMessage(`{"key_facts":["a"],"numbers":[],"quotes":["Press Save to keep your work."],"verdict":"v"}`), StopReason: "done"}, nil
	})
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		return []research.Fetched{
			{URL: urls[0], Err: "http 404"},
			{URL: urls[1], FinalURL: urls[1], Status: 200, Title: "Help", Text: page, Bytes: len(page), TextBytes: len(page)},
		}
	}
	res, err := s.handleResearch(context.Background(), callReq(`{"goal":"Give the exact label of every button the page names.","urls":["https://docs.example.com/gone","https://docs.example.com/help"],"route":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	qc, _ := firstResult(t, decodeResult(t, res))["quote_check"].(map[string]any)
	if qc == nil || qc["verbatim"] != float64(1) || qc["unverified"] != nil {
		t.Fatalf("quote_check = %v, want the quote found on the second page, the one this digest came from", qc)
	}
}
