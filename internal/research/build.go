package research

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// DefaultSchema is what a digest comes back as when the caller gives no
// output_schema: the fields a research reader actually needs to merge across
// sources. Every field is a string or string list so the small seats' re-pack
// path (grammar-guided, scalar-coerced) handles it.
//
// All four fields are required, in declaration order (the gbnf builder reads
// `required` first for field order). Without it a seat's direct JSON answer
// that left one out validated, skipped the structured re-pack and reached the
// delegator with the field missing, and vLLM's structured_outputs was free to
// leave an optional field out of its own answer (register C-74). Required is
// presence only: an empty list is a complete answer for a page with nothing to
// say, so no minItems is declared here (see nonEmptyChecks for who asks for items).
var DefaultSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"key_facts":{"type":"array","items":{"type":"string"}},` +
	`"numbers":{"type":"array","items":{"type":"string"}},` +
	`"quotes":{"type":"array","items":{"type":"string"}},` +
	`"verdict":{"type":"string"}},` +
	`"required":["key_facts","numbers","quotes","verdict"]}`)

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
	// One design for what a digest owes (register C-74; the security review's R-02
	// and the diagnosis's PR-3): PRESENCE is declared for every field an
	// acceptance check reads, so a seat's direct JSON answer that leaves one out
	// goes to the structured re-pack instead of being delivered and failing
	// acceptance after a whole run; NON-EMPTINESS is asked for only where the
	// caller marked it (nonEmptyChecks), so a faithful "nothing on this page"
	// digest is a success. The same checks are derived once here because they do
	// not depend on the page.
	//
	// The harness's own digest asks for one statement on every page: a verdict.
	// A page too thin to anchor carries no anchor check, so without this its
	// acceptance was empty and a digest that said nothing (every list empty, no
	// verdict) was delivered as a success. Empty lists stay a complete answer; the
	// verdict is where a digest of an empty page says so.
	var derived []string
	if len(req.OutputSchema) > 0 {
		derived = nonEmptyChecks(req.OutputSchema)
	} else {
		derived = []string{"nonempty:verdict"}
	}
	schema = core.RequireAcceptanceFields(schema, append(append([]string{}, derived...), req.Acceptance...))
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
		acc = append(acc, derived...)
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

// nonEmptyChecks asks for items only where the CALLER marked them: the first
// array property their own schema lists in `required`, in the order they wrote
// it. It replaces firstArrayCheck, which demanded one item from the
// alphabetically first array of any schema. That array was the caller's choice
// only by accident: with one caller's custom schema it picked an array that was
// legitimately empty for the page, and every correct "none of the requested
// topics" digest was scored failed_verification (register C-74, security review
// R-02).
//
// One check, never one per required array: the rule may not add a failure a
// schema did not have before, and callers who list every field as required
// would otherwise be asked for items in all of them. A caller who wants more
// says so in their own acceptance (min_items: / nonempty:), which Build appends
// and declares required as well. The harness's default schema is not a caller's
// mark and asks for no items: a digest of a page with nothing to say is complete
// with empty lists and a verdict that says so, and Build asks for that verdict
// (nonempty:verdict) on every page of the default digest instead.
func nonEmptyChecks(schema json.RawMessage) []string {
	var s struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil || len(s.Properties) == 0 {
		return nil
	}
	for _, k := range s.Required {
		raw, declared := s.Properties[k]
		if !declared {
			continue
		}
		var p struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Type == "array" {
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

