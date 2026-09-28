package research

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// DefaultSchema is what a digest comes back as when the caller gives no
// output_schema: the fields a research reader actually needs to merge across
// sources. Every field is a string or string list so the small seats' re-pack
// path (grammar-guided, scalar-coerced) handles it.
var DefaultSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"key_facts":{"type":"array","items":{"type":"string"}},` +
	`"numbers":{"type":"array","items":{"type":"string"}},` +
	`"quotes":{"type":"array","items":{"type":"string"}},` +
	`"verdict":{"type":"string"}}}`)

// Request is one research call: a goal applied to every fetched source.
type Request struct {
	Goal         string
	URLs         []string
	Questions    []string        // optional extra asks, appended to the goal for every source
	OutputSchema json.RawMessage // optional; DefaultSchema when empty
	Acceptance   []string        // optional caller checks, appended to the grounded default
	TimeoutSec   int
	MaxSteps     int
	Profile      string
}

// Source is what the caller sees about each URL — with the text withheld: the
// point of the lane is that the calling context never pays for the page.
type Source struct {
	Index int `json:"index"`
	Fetched
	DocName string `json:"doc_name,omitempty"`
	Anchor  string `json:"anchor,omitempty"`
	// Fingerprinted says whether DocFingerprint produced the wrong-document
	// tripwire for this page; false = the page was too thin (< 6 distinctive
	// tokens) and an off-document answer cannot be caught by it.
	Fingerprinted bool   `json:"fingerprinted"`
	Skipped       string `json:"skipped,omitempty"`
}

// FetchAll fetches every URL with bounded concurrency, preserving order.
func FetchAll(ctx context.Context, urls []string, opt Options, workers int) []Fetched {
	if workers <= 0 {
		workers = 4
	}
	out := make([]Fetched, len(urls))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = Fetch(ctx, u, opt)
		}(i, u)
	}
	wg.Wait()
	return out
}

// Build turns fetched pages into one delegation contract per usable page. The
// contract is written the way the seats were measured to pass (2026-08-28/30):
// context docs are MATERIALIZED as files in the seat's read root (see
// pipeline.RunAgentContract), so the goal names the file and tells the seat to
// read it with its file tool — a goal that says "do not open anything" made
// every seat report the document missing, and one that says "read the document"
// without naming the file sent a small seat hunting. Acceptance is an
// alternation of prose content words that appear in the page and never in the
// goal (see AnchorCheck) — one any-of check, so an echoed question cannot pass
// as verified and a faithful digest needs to restate only one of them.
func Build(req Request, fetched []Fetched) (specs []delegate.SubtaskSpec, sources []Source) {
	schema := req.OutputSchema
	if len(schema) == 0 {
		schema = DefaultSchema
	}
	goal := strings.TrimSpace(req.Goal)
	if len(req.Questions) > 0 {
		goal += " Also answer, from the document only: " + strings.Join(req.Questions, " ")
	}
	for i, f := range fetched {
		src := Source{Index: i, Fetched: f}
		if f.Err != "" {
			src.Skipped = f.Err
			sources = append(sources, src)
			continue
		}
		name := DocName(i, f.FinalURL, f.URL)
		anchor := AnchorCheck(f.Text, goal)
		src.DocName, src.Anchor = name, anchor
		sources = append(sources, src)

		head := fmt.Sprintf("The page at %s", f.URL)
		if f.Title != "" {
			head += fmt.Sprintf(" (titled %q)", f.Title)
		}
		head += fmt.Sprintf(" has already been fetched for you and is provided as the context file %s in your read root — read that file with your file tool first; it is the ONLY source. Never fetch from the network. ", name)
		// The length rule is load-bearing: a long page digested into an unbounded
		// list overflowed the structured re-pack on the 27B AND the 4B seats
		// (2026-08-30, "invalid json: unexpected end of JSON input") — the seat
		// abstained, not the caller. Bounded lists fit every seat's re-pack budget.
		fullGoal := head + goal + " Answer only from the file; omit anything it does not contain rather than inventing it. Keep every list to at most 6 items of at most 18 words each — the most important first — and every string field under 40 words."

		acc := []string{}
		if anchor != "" {
			acc = append(acc, anchor)
		}
		src.Fingerprinted = anchor != ""
		sources[len(sources)-1] = src
		acc = append(acc, firstArrayCheck(schema)...)
		acc = append(acc, req.Acceptance...)

		specs = append(specs, delegate.SubtaskSpec{AgentContract: core.AgentContract{
			Goal:         fullGoal,
			Context:      []core.ContextDoc{{Name: name, Text: f.Text}},
			OutputSchema: schema,
			Acceptance:   acc,
			Profile:      req.Profile,
			MaxSteps:     req.MaxSteps,
			TimeoutSec:   req.TimeoutSec,
		}})
	}
	return specs, sources
}

// firstArrayCheck adds a shape check on the schema's first array property so a
// digest that returns nothing at all is failed_verification, not a success.
func firstArrayCheck(schema json.RawMessage) []string {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil || len(s.Properties) == 0 {
		return nil
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var p struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(s.Properties[k], &p) == nil && p.Type == "array" {
			return []string{"min_items:" + k + ":1"}
		}
	}
	return nil
}

var reDocName = regexp.MustCompile(`[^a-z0-9.-]+`)

// DocName is the flat context-doc filename for source i: "<i>-<host>.txt",
// host sanitized to the flat-filename shape core.ContextDoc.Validate demands
// (no separators, no traversal).
func DocName(i int, finalURL, rawURL string) string {
	host := ""
	for _, u := range []string{finalURL, rawURL} {
		if u == "" {
			continue
		}
		if p, err := url.Parse(u); err == nil && p.Hostname() != "" {
			host = strings.ToLower(p.Hostname())
			break
		}
	}
	host = strings.TrimPrefix(host, "www.")
	host = reDocName.ReplaceAllString(host, "-")
	host = strings.Trim(host, "-.")
	if host == "" {
		host = "source"
	}
	if len(host) > 40 {
		host = host[:40]
	}
	return fmt.Sprintf("%02d-%s.txt", i+1, host)
}

// boilerplate are the ≥6-letter words that appear on almost every software
// page and in almost every plausible phantom answer; a fingerprint built from
// them would match a digest written about a different page entirely (the
// council's example: "latest stable Go version… download" hit "version",
// "download", "release" on a page about ffmpeg).
var boilerplate = map[string]struct{}{}


func init() {
	for _, w := range strings.Fields("version versions release releases download downloads install installation installed " +
		"license licensed documentation documented latest stable update updates updated support supported github " +
		"package packages website contact privacy cookies cookie copyright rights reserved search navigation " +
		"content contents example examples section sections default defaults option options available " +
		"information general important following however without between through should before after " +
		"because during another others others others please thanks") {
		boilerplate[w] = struct{}{}
	}
}

