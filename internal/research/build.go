package research

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

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

// defaultDigestCheck is the acceptance Build derives for the default digest on
// EVERY page, anchored or not: the digest said something, a value in any one of
// its four fields. It fails only the silent-empty digest (every list empty and
// no verdict), which is what it exists to catch; see Build for why it is not
// the verdict alone.
const defaultDigestCheck = "nonempty:key_facts|numbers|quotes|verdict"

// defaultVerdictAsk ends the default digest's goal. The research goal is the
// caller's and never asks for a verdict, and the re-pack writes an empty value
// for a field the loop's final text did not carry, so the digest is asked for
// the statement outright. A caller-supplied schema owns what it asks for and
// gets no sentence.
const defaultVerdictAsk = " Always end with a one-sentence verdict that answers the goal from this page, or says plainly that the page does not address it."

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
	// The harness's own digest owes one thing on every page: that it said
	// something. A page too thin to anchor carries no anchor check, so without a
	// guard its acceptance was empty and a digest that said nothing (every list
	// empty, no verdict) was delivered as a success. Empty lists stay a complete
	// answer; the verdict is where a digest of an empty page says so.
	//
	// The guard first asked for the verdict alone (nonempty:verdict, 0.147.0), and
	// that failed digests that had said plenty. The research goal never asked for a
	// verdict and the re-pack prompt tells the extractor to use empty values for a
	// field that is absent, so a loop whose final text carried none re-packed to
	// verdict "": since 2026-09-30, 36 default-digest pages failed it with their
	// lists populated (key_facts median 10.5 items), every one from the vLLM
	// json_schema re-pack lane, and an acceptance-only failure is not retried, so
	// each one discarded a whole run (4.07 h of seat wall time). The guard now
	// fails what it was written for, every list empty AND no verdict (0 of 687
	// pages), by passing when any of the four fields has a value, and the goal
	// asks for the verdict (defaultVerdictAsk) so it is normally there.
	defaultDigest := len(req.OutputSchema) == 0
	var derived []string
	if defaultDigest {
		derived = []string{defaultDigestCheck}
	} else {
		derived = nonEmptyChecks(req.OutputSchema)
	}
	schema = core.RequireAcceptanceFields(schema, append(append([]string{}, derived...), req.Acceptance...))
	// A schema with a `quotes` list gets the sentence that makes it the page's own
	// words (verbatimQuotesAsk); VerifyQuotes is the delegator's check of the result.
	hasQuotes := schemaHasQuotes(schema)
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
		// A page with no content is refused before a seat is spent on it (see emptyPage).
		if why := emptyPage(f); why != "" {
			src.Err, src.ErrKind, src.Skipped = why, ErrKindEmptyPage, why
			sources = append(sources, src)
			continue
		}
		name := DocName(i, f.FinalURL, f.URL)

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
		if hasQuotes {
			fullGoal += verbatimQuotesAsk
		}
		if defaultDigest {
			fullGoal += defaultVerdictAsk
		}

		// The anchors are chosen off the goal THE SEAT IS HANDED, not only the caller's: the fixed sentences above
		// ("Always end with a one-sentence verdict", the file's name and title, "most important first") are the goal's
		// words too, and a page word that is one of them would let the goal echoed back restate the page.
		anchor := AnchorCheck(f.Text, fullGoal)
		src.DocName, src.Anchor = name, anchor
		src.Fingerprinted = anchor != ""
		sources = append(sources, src)

		acc := []string{}
		if anchor != "" {
			acc = append(acc, anchor)
		}
		acc = append(acc, derived...)
		acc = append(acc, req.Acceptance...)

		specs = append(specs, delegate.SubtaskSpec{AgentContract: core.AgentContract{
			Goal:         fullGoal,
			Context:      []core.ContextDoc{{Name: name, Text: WrapLongLines(f.Text, MaxLineChars)}},
			OutputSchema: schema,
			Acceptance:   acc,
			Profile:      req.Profile,
			MaxSteps:     req.MaxSteps,
			TimeoutSec:   req.TimeoutSec,
		}})
	}
	return specs, sources
}

// ErrKindEmptyPage is the error_kind of a source Build refused because the page holds no content (emptyPage): the caller can
// read it as "there is nothing on this page to digest", where a fetch failure says the page could not be read.
const ErrKindEmptyPage = "empty_page"

// MinPageRunes is the floor of a markup page's text, in characters once the markup is stripped. A page under it is a title, a
// banner, a "please wait" or a login wall, and a full run on it (five to thirteen minutes of a seat) can only say so. The
// number is the 30 days to 2026-11-12 of research pages: 168 of the 4,622 pages that passed and 16 of the 356 that failed
// verification were under 150 characters; 59 of the 168 were JSON, and of the other 109 the ones read were titles, cookie gates,
// bot checks and login walls, bar a few (a batch file, an abstract, a README heading). A sample of 25 passes between 150 and 300
// characters held 8 short real pages (archive capture counts, a changelog entry) among the shells, so the floor stops where
// nearly all of what it refuses is a shell. Larger shells (a 349-character "unsupported browser" page was dispatched eight times)
// are not caught by a length, and a page of markup with no prose at any size has no floor here.
const MinPageRunes = 150

// emptyPage returns why a fetched page is refused, or "" when it is not. Only a page read as markup (an HTML content type) is
// judged by its length: a JSON body, a plain-text file, a feed and a subtitle track are data however short (a 40-character
// archive-availability reply is the whole answer to a question the lane is asked), and 59 of the 168 short passes were JSON.
// A page this short usually has too few words to anchor, so it carried no fingerprint and any digest of it passed, a digest of
// an unrelated page included.
func emptyPage(f Fetched) string {
	if !strings.Contains(strings.ToLower(f.ContentType), "html") {
		return ""
	}
	n := utf8.RuneCountInString(strings.TrimSpace(f.Text))
	if n >= MinPageRunes {
		return ""
	}
	return fmt.Sprintf("page has no content: %d characters of text once the markup was stripped (under %d), a title, a banner or a login wall; no digest was run", n, MinPageRunes)
}

// MaxLineChars is the longest line a context document is handed to a seat with. The agent's read_file cuts every line at
// 2,000 characters and pages by line, so the rest of a longer line is out of the seat's reach; a page that arrives as
// one 98,304-character line gave it 2,020 of them. 1,800 leaves the line number and the tool's own framing room under
// that cap (a cross-package test pins the two).
const MaxLineChars = 1800

// WrapLongLines breaks every line of text that is longer than max runes into lines of at most max, at the last space in
// the final quarter of the window (in the middle of the run when that quarter holds no space at all), never inside a
// rune. The space it breaks at becomes the newline and nothing else is added or removed, so the words are the same words
// in the same order; text whose lines all fit is returned as it came.
func WrapLongLines(text string, max int) string {
	if max <= 0 || len(text) <= max {
		return text
	}
	lines := strings.Split(text, "\n")
	changed := false
	for i, line := range lines {
		if utf8.RuneCountInString(line) <= max {
			continue
		}
		changed = true
		rs := []rune(line)
		var b strings.Builder
		for len(rs) > max {
			cut := max
			for j := max; j > max-max/4 && j > 0; j-- {
				if rs[j-1] == ' ' || rs[j] == ' ' {
					cut = j
					break
				}
			}
			head, tail := rs[:cut], rs[cut:]
			// the space the line is broken at is the newline
			for len(head) > 0 && head[len(head)-1] == ' ' {
				head = head[:len(head)-1]
			}
			for len(tail) > 0 && tail[0] == ' ' {
				tail = tail[1:]
			}
			b.WriteString(string(head))
			b.WriteByte('\n')
			rs = tail
		}
		b.WriteString(string(rs))
		lines[i] = b.String()
	}
	if !changed {
		return text
	}
	return strings.Join(lines, "\n")
}

// nonEmptyChecks guards a caller's schema only where the CALLER marked it: a schema that lists an array property in
// `required` gets one check, that the digest said something. It replaces firstArrayCheck, which demanded one item from the
// alphabetically first array of any schema. That array was the caller's choice only by accident: with one caller's custom
// schema it picked an array that was legitimately empty for the page, and every correct "none of the requested topics"
// digest was scored failed_verification (register C-74, security review R-02).
//
// The first rewrite still demanded one item from the first required array (min_items:<field>:1), and that failed the same
// answers one level up. The goal tells the seat to omit what the page does not contain and to say so plainly, so a page with
// no prices comes back with an empty list and a sentence that says there are none. In the 30 days to 2026-11-12, 70 research
// digests failed the item count (six of them the fingerprint as well); of the 64 that failed nothing else, 58 had a value in a
// required field beside the empty list (the seat said there was none, or answered the page in other fields of the schema), and
// the other six were silent or spoke only in optional fields. Each failure discarded a whole run and, being an
// acceptance-only failure, was not retried. The check now is the default digest's, over the caller's own required fields:
// nonempty:<f1>|<f2>|..., a value in any one of them. It fails the digest that is silent (every required field empty or
// absent), which is what an item count was a proxy for, and it can only fail what the item count already failed: a digest
// with an item in the first required array has a value in a required field.
//
// One check, never one per required array, and none for a schema that marks no array: the rule may not add a failure a
// schema did not have before. A caller who wants items says so in their own acceptance (min_items: / nonempty:), which Build
// appends and declares required as well, and which an honest "none" does not excuse. The harness's default schema is not a
// caller's mark and has its own guard (defaultDigestCheck, built into Build) and a goal that asks for the verdict.
func nonEmptyChecks(schema json.RawMessage) []string {
	var s struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil || len(s.Properties) == 0 {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	hasArray := false
	for _, k := range s.Required {
		raw, declared := s.Properties[k]
		// a name the any-of form cannot spell (it splits on the bar) or one already listed is left out, never an error
		if !declared || seen[k] || k == "" || strings.Contains(k, "|") {
			continue
		}
		seen[k] = true
		names = append(names, k)
		var p struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Type == "array" {
			hasArray = true
		}
	}
	if !hasArray {
		return nil
	}
	return []string{"nonempty:" + strings.Join(names, "|")}
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
