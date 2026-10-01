// C-75: a partial offload_research result is a SUCCESSFUL tool call, and its
// digests come before its long sources in the marshalled body.
//
// The measured defect: any failed or lost page flagged the whole MCP result as
// an error, the results marshalled as summary, sources, results, and the client
// keeps only the head and tail of an error-flagged body — so every partial
// research reply the workers saw had its digests (the middle) cut out. These
// tests pin both halves of the fix through the real handler.

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/research"
)

// researchServer builds a delegation server whose fetch seam serves n thin
// pages (thin on purpose: a page with too few distinctive words gets no
// document-fingerprint check, so the fake seat's answer needs no anchor) and
// whose local seat answers per page: pages named in failing return an error,
// the rest a digest.
func researchServer(t *testing.T, failing map[string]bool) *Server {
	t.Helper()
	s := delegateTestServer(t, func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		name := ""
		if len(c.Context) > 0 {
			name = c.Context[0].Name
		}
		for prefix := range failing {
			if strings.HasPrefix(name, prefix) {
				return core.AgentWireResult{}, errors.New("planner endpoint refused")
			}
		}
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat",
			Output: "digest of " + name, Structured: json.RawMessage(`{"summary":"digest of ` + name + `"}`), StopReason: "done"}, nil
	})
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		out := make([]research.Fetched, len(urls))
		for i, u := range urls {
			out[i] = research.Fetched{URL: u, Status: 200, Title: "Page", Text: "A short page about widgets.", Bytes: 28, TextBytes: 28}
		}
		return out
	}
	return s
}

const researchArgs = `{"goal":"digest the page","urls":["https://docs.example/a","https://docs.example/b","https://docs.example/c"],` +
	`"output_schema":{"properties":{"summary":{"type":"string"}}},"route":"local"}`

// TestResearchPartialResultIsNotAToolError: two pages digest, one fails. The
// call DELIVERED, so it is not flagged; the loss is in the body, and every
// existing field survives.
func TestResearchPartialResultIsNotAToolError(t *testing.T) {
	s := researchServer(t, map[string]bool{"02-": true})
	res, err := s.handleResearch(context.Background(), callReq(researchArgs))
	if err != nil {
		t.Fatalf("handleResearch: %v", err)
	}
	if res.IsError {
		t.Fatal("IsError = true on a partial research result: two of three digests are usable, and the client cuts the middle out of an error-flagged body")
	}
	m := decodeResult(t, res)
	summary, _ := m["summary"].(map[string]any)
	if summary["succeeded"] != float64(2) || summary["failed"] != float64(1) {
		t.Fatalf("summary = %v, want the two digests AND the lost page both reported", summary)
	}
	if results, _ := m["results"].([]any); len(results) != 3 {
		t.Fatalf("results = %v, want one entry per page", m["results"])
	}
	if rs, _ := m["result_sources"].([]any); len(rs) != 3 {
		t.Fatalf("result_sources = %v, want the page indices kept (every existing field survives)", m["result_sources"])
	}
	if srcs, _ := m["sources"].([]any); len(srcs) != 3 {
		t.Fatalf("sources = %v, want the three fetched pages", m["sources"])
	}
}

// TestResearchDigestsPrecedeSources: the digests (results) come before the long
// sources in the marshalled order — a client that keeps only the head and the
// tail of a long body then keeps the digests, not the page metadata. The body
// still LEADS with the summary (roast delta 14), and the notes of a batched run
// that returned an error (`partial`, `error`) sit beside it. With the error flag
// no longer marking a partial result, what says that pages are missing is the
// summary and each result's own fields; the two notes are the rarer case.
func TestResearchDigestsPrecedeSources(t *testing.T) {
	s := researchServer(t, nil)
	res, err := s.handleResearch(context.Background(), callReq(researchArgs))
	if err != nil {
		t.Fatalf("handleResearch: %v", err)
	}
	raw := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(raw, `{"summary"`) {
		t.Fatalf("the body must still lead with the summary, got %q…", raw[:min(len(raw), 40)])
	}
	iResults, iSources := strings.Index(raw, `"results"`), strings.Index(raw, `"sources"`)
	if iResults < 0 || iSources < 0 {
		t.Fatalf("body carries results at %d and sources at %d, want both present:\n%s", iResults, iSources, raw)
	}
	if iResults > iSources {
		t.Fatalf("results (at %d) come AFTER sources (at %d): the digests sit behind the long sources and are the part a truncating client cuts", iResults, iSources)
	}
	if iRS := strings.Index(raw, `"result_sources"`); iRS < 0 || iRS > iSources {
		t.Fatalf("result_sources at %d, sources at %d: the page index map belongs beside the results it indexes", iRS, iSources)
	}
}

// TestResearchWireFieldOrder pins the WHOLE marshalled order, including the two
// notes that only a batched run that returned an error carries: summary, partial,
// error, results, result_sources, sources. A partial result is no longer
// error-flagged, so if a run ever does set them they must stay in the head of
// the body.
func TestResearchWireFieldOrder(t *testing.T) {
	b, err := json.Marshal(researchWire{
		Summary: map[string]int{"succeeded": 1}, Partial: true, Error: "chunk 2 failed",
		Results: []int{1}, ResultSources: []int{0}, Sources: []research.Source{{Index: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b)
	prev, prevKey := -1, ""
	for _, key := range []string{`"summary"`, `"partial"`, `"error"`, `"results"`, `"result_sources"`, `"sources"`} {
		i := strings.Index(raw, key)
		if i < 0 {
			t.Fatalf("key %s missing from %s", key, raw)
		}
		if i < prev {
			t.Fatalf("key %s (at %d) marshals before %s (at %d); want summary, partial, error, results, result_sources, sources:\n%s", key, i, prevKey, prev, raw)
		}
		prev, prevKey = i, key
	}
}

// TestResearchNothingSucceededStaysAnError is the control: with every page lost
// the call did fail, and the flag stays.
func TestResearchNothingSucceededStaysAnError(t *testing.T) {
	s := researchServer(t, map[string]bool{"01-": true, "02-": true, "03-": true})
	res, err := s.handleResearch(context.Background(), callReq(researchArgs))
	if err != nil {
		t.Fatalf("handleResearch: %v", err)
	}
	if !res.IsError {
		t.Fatal("IsError = false although every page failed")
	}
	if m := decodeResult(t, res); m["summary"] == nil || m["results"] == nil {
		t.Fatalf("the body must survive the flag: %v", m)
	}
}
