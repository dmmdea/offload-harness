package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Search-results rung — an optional rung of the compaction ladder
// (compactOpts.SearchResults, default OFF) for older tool bodies that are
// search listings: the harness's grouped search_files output ("path:" then
// "  L<n>: text") and bare "path:line:text" lines such as grep -rn or ripgrep
// print, alone or inside a shell tool's exit/stdout/stderr framing.
//
// A long listing is thinned to the lines that matter for the TASK: every line
// is scored with BM25 against the objective's terms (the line's own path is
// part of its document), lines that look like errors get a fixed boost, and the
// survivors are chosen round-robin across files so one noisy file cannot take
// the whole allowance. What is dropped is counted in a footer, and the body opens
// with the skeleton disclosure line, so every later rung, the idempotence checks
// and the re-execute-on-repeat path (resultDestroyed, ADR 0016) treat the result
// as the compaction artifact it is, with no change to any of them.
//
// Concept: the search-result compressor documented by the Headroom project
// (Apache-2.0): relevance ranking, a per-file cap, file diversity and an error
// boost, with the caps its documented configuration lists. This file is an independent
// implementation written from that documented behaviour; no upstream source was
// read or copied (see NOTICE).
//
// Gate: the rung reads a body's shape AND the call that produced it (searchRung): only
// search_files (grouped form) and a run_shell / run call whose command is grep, rg, ag,
// ack or git grep (flat form) qualify. A linter prints the same "path:line:col:text"
// lines and none of its diagnostics may be dropped.
//
// Properties, each pinned by rung_search_test.go and rung_search_gate_test.go:
//   - deterministic: no map iteration reaches the output, ties break on position,
//     and scores are quantized to integers before they are compared;
//   - idempotent: the output opens with skeletonPrefix, which the rung (and the
//     skeleton rung) refuse to reprocess;
//   - monotonic: the result is returned only if it is strictly shorter than the
//     body, otherwise the body is returned untouched;
//   - fails closed: a body that is not, line for line, one of the accepted shapes
//     (or is binary, not UTF-8, or too small to gain) comes back unchanged;
//   - KV-prefix stable: the ladder calls it only on [protectedEnd, recentStart),
//     the span every lossy rung already rewrites (ADR 0017), and it changes only
//     a tool message's Content, never a Role, a ToolCallID or a message count.

const (
	// searchRungMaxLines / PerFileCap / MaxFiles are chosen to match the values the
	// upstream configuration example shows (see NOTICE): 30 lines in total, at most
	// 5 per file, from at most 15 files. At most 15 files survive, so a listing of F files always
	// elides at least F-15 of them.
	searchRungMaxLines   = 30
	searchRungPerFileCap = 5
	searchRungMaxFiles   = 15

	// searchRungMaxBytes bounds the scoring cost; tool results are already capped
	// far below it at the loop boundary.
	searchRungMaxBytes = 1 << 20

	// searchRungMaxQueryTerms bounds the query taken from the objective.
	searchRungMaxQueryTerms = 48

	searchBM25K1 = 1.2
	searchBM25B  = 0.75

	// searchErrorBoostMicros is the score added to an error-looking line, in
	// millionths: about one strong term match, enough to lift an error above an
	// ordinary hit without outranking a line that matches the task closely.
	searchErrorBoostMicros = 3_000_000

	// searchTruncatedTrailer is the line a shell tool appends when it clips its
	// output; it is kept verbatim at the end.
	searchTruncatedTrailer = "…(truncated)"
)

// searchErrorLine marks lines that look like errors. Deliberately narrower than
// signalLine (which the elision rung preserves): in a code search "invalid",
// "missing" or "timeout" are ordinary identifiers, and boosting them would
// flatten the ranking.
var searchErrorLine = regexp.MustCompile(`(?i)\b(errors?|fail(s|ed|ure|ures|ing)?|panic(s|ked)?|exceptions?|fatal)\b`)

// searchFlatLine is a bare "path:line:text" match, with an optional column. The
// path has no whitespace; whether it looks like a file is checked in code.
var searchFlatLine = regexp.MustCompile(`^([^\s:][^\s]*?):([1-9][0-9]*):(?:[1-9][0-9]*:)?(.*)$`)

// searchGroupedEntry is one "  L<n>: text" line of the grouped listing.
var searchGroupedEntry = regexp.MustCompile(`^  L([1-9][0-9]*): (.*)$`)

var searchStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "was": true, "with": true, "this": true,
	"that": true, "from": true, "into": true, "what": true, "how": true, "why": true, "when": true,
	"where": true, "which": true, "does": true, "did": true, "not": true, "you": true, "your": true,
	"can": true, "has": true, "have": true, "use": true, "using": true, "find": true, "show": true,
	"please": true, "all": true, "any": true, "its": true, "than": true, "then": true, "there": true,
	"is": true, "it": true, "of": true, "to": true, "in": true, "on": true, "at": true, "be": true,
	"as": true, "by": true, "or": true, "an": true, "if": true, "do": true, "we": true, "so": true,
}

// searchTokens splits text into lower-case identifier words and, for a word
// that is snake_case or camelCase, its parts as well, so "fetchTimeout" matches
// both "fetchtimeout" and "timeout". Words under two characters are dropped.
func searchTokens(s string) []string {
	var out []string
	var word []rune
	emit := func(rs []rune) {
		if len(rs) >= 2 {
			out = append(out, strings.ToLower(string(rs)))
		}
	}
	flush := func() {
		if len(word) == 0 {
			return
		}
		emit(word)
		// Parts: split on '_' and on lower/digit-to-upper and acronym-to-word boundaries.
		var parts [][]rune
		start := 0
		for i := 0; i < len(word); i++ {
			if word[i] == '_' {
				if i > start {
					parts = append(parts, word[start:i])
				}
				start = i + 1
				continue
			}
			if i > start && unicode.IsUpper(word[i]) {
				prev := word[i-1]
				nextLower := i+1 < len(word) && unicode.IsLower(word[i+1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
					parts = append(parts, word[start:i])
					start = i
				}
			}
		}
		if start < len(word) {
			parts = append(parts, word[start:])
		}
		if len(parts) > 1 {
			for _, p := range parts {
				emit(p)
			}
		}
		word = word[:0]
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			word = append(word, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// searchQueryTerms is the task's term set: unique, stopwords removed, bounded to
// the first searchRungMaxQueryTerms terms in order of appearance (what the
// objective names first is what it is about; the alphabet has no say), and then
// sorted so every sum over it runs in one fixed order.
func searchQueryTerms(task string) []string {
	if len(task) > 4096 {
		task = task[:4096]
		for !utf8.ValidString(task) {
			task = task[:len(task)-1]
		}
	}
	seen := map[string]bool{}
	var terms []string
	for _, t := range searchTokens(task) {
		if searchStopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		terms = append(terms, t)
		if len(terms) == searchRungMaxQueryTerms {
			break
		}
	}
	sort.Strings(terms)
	return terms
}

// searchRungTask is the text the listing is ranked against: the last user message
// of the protected preamble, which the loop fills with the objective. It is the
// one task text that is the same on every step, so a body thinned on one step is
// the body every later step would have produced. "" when there is none.
func searchRungTask(msgs []Msg, protectedEnd int) string {
	if protectedEnd > len(msgs) {
		protectedEnd = len(msgs)
	}
	for i := protectedEnd - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// searchHit is one match line of a listing.
type searchHit struct {
	file    int    // index into searchListing.files
	raw     string // the line exactly as it appeared, without its newline
	payload string // the matched text, without the path or line-number prefix
	score   int64  // BM25 in millionths plus the error boost
}

// searchFile is one file of a listing, in order of first appearance.
type searchFile struct {
	header string // the "path:" line of the grouped form; "" for the flat form
	path   string
	hits   []int // indices into searchListing.hits, in listing order
}

// searchListing is a parsed listing: its files, its hits in order, and the
// trailer lines (a tool's own "more matches" or "truncated" note) kept verbatim.
type searchListing struct {
	grouped bool
	files   []searchFile
	hits    []searchHit
	trailer []string
}

func isSearchTrailer(line string) bool {
	return line == grepMoreMarker || line == searchTruncatedTrailer
}

// looksLikePath is the bar a flat line's path must clear so that "12:30:45 up"
// or a log timestamp is never mistaken for a match.
func looksLikePath(p string) bool {
	if !strings.ContainsAny(p, "/\\.") {
		return false
	}
	for _, r := range p {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// parseSearchListing parses section into a listing, or reports false. Every
// non-trailer line must be a header, an entry or a flat match: one stray line
// is an unexpected shape and the whole body is left alone.
func parseSearchListing(section string) (*searchListing, bool) {
	lines := strings.Split(section, "\n")
	end := len(lines)
	for end > 0 && isSearchTrailer(lines[end-1]) {
		end--
	}
	if end == 0 {
		return nil, false
	}
	trailer := append([]string(nil), lines[end:]...)
	lines = lines[:end]
	l := &searchListing{trailer: trailer}
	if m := searchFlatLine.FindStringSubmatch(lines[0]); m != nil && looksLikePath(m[1]) {
		fileIdx := map[string]int{} // lookup only; order comes from l.files
		for _, ln := range lines {
			m := searchFlatLine.FindStringSubmatch(ln)
			if m == nil || !looksLikePath(m[1]) {
				return nil, false
			}
			fi, ok := fileIdx[m[1]]
			if !ok {
				fi = len(l.files)
				fileIdx[m[1]] = fi
				l.files = append(l.files, searchFile{path: m[1]})
			}
			l.files[fi].hits = append(l.files[fi].hits, len(l.hits))
			l.hits = append(l.hits, searchHit{file: fi, raw: ln, payload: m[3]})
		}
		return l, true
	}
	l.grouped = true
	cur := -1
	for _, ln := range lines {
		if m := searchGroupedEntry.FindStringSubmatch(ln); m != nil {
			if cur < 0 {
				return nil, false
			}
			l.files[cur].hits = append(l.files[cur].hits, len(l.hits))
			l.hits = append(l.hits, searchHit{file: cur, raw: ln, payload: m[2]})
			continue
		}
		if len(ln) > 1 && !strings.HasPrefix(ln, " ") && strings.HasSuffix(ln, ":") && !strings.ContainsAny(ln, "\r\t") {
			if cur >= 0 && len(l.files[cur].hits) == 0 {
				return nil, false // a header with no entries is not what the tool prints
			}
			cur = len(l.files)
			l.files = append(l.files, searchFile{header: ln, path: strings.TrimSuffix(ln, ":")})
			continue
		}
		return nil, false
	}
	if cur < 0 || len(l.files[cur].hits) == 0 {
		return nil, false
	}
	return l, true
}

// scoreSearchListing sets every hit's score: BM25 against the query terms, in
// millionths, plus the error boost. The float arithmetic runs in one fixed order
// (sorted terms, document order) with explicit conversions that stop a compiler
// fusing operations, and is quantized before any comparison, so the ranking does
// not depend on the platform.
func scoreSearchListing(l *searchListing, query []string) {
	n := len(l.hits)
	inQuery := make(map[string]bool, len(query))
	for _, q := range query {
		inQuery[q] = true
	}
	tf := make([]map[string]int, n)
	dl := make([]int, n)
	df := make(map[string]int, len(query))
	total := 0
	for i := range l.hits {
		toks := searchTokens(l.files[l.hits[i].file].path + " " + l.hits[i].payload)
		dl[i] = len(toks)
		total += len(toks)
		m := map[string]int{}
		for _, t := range toks {
			if inQuery[t] {
				m[t]++
			}
		}
		tf[i] = m
		for t := range m { // counting only: the order of this loop cannot reach any output
			df[t]++
		}
	}
	avg := float64(total) / float64(n)
	if avg <= 0 {
		avg = 1
	}
	for i := range l.hits {
		var sum float64
		for _, q := range query {
			f := tf[i][q]
			if f == 0 {
				continue
			}
			d := float64(df[q])
			idf := math.Log(1 + (float64(n)-d+0.5)/(d+0.5))
			lenNorm := float64(searchBM25B * float64(dl[i]) / avg)
			norm := float64(searchBM25K1 * float64((1-searchBM25B)+lenNorm))
			num := float64(float64(f) * (searchBM25K1 + 1))
			sum += float64(idf * float64(num/(float64(f)+norm)))
		}
		sc := int64(math.Round(sum * 1e6))
		if searchErrorLine.MatchString(l.hits[i].payload) {
			sc += searchErrorBoostMicros
		}
		l.hits[i].score = sc
	}
}

// selectSearchHits picks the lines to keep: files ranked by their best line, the
// top searchRungMaxFiles of them, then round-robin over those files taking each
// file's next-best line, best score first within a round, until
// searchRungMaxLines are kept or every file reached searchRungPerFileCap. Ties
// break on position in the listing.
func selectSearchHits(l *searchListing) []bool {
	// Rank each file's hits: score descending, position ascending.
	ranked := make([][]int, len(l.files))
	for fi, f := range l.files {
		r := append([]int(nil), f.hits...)
		sort.SliceStable(r, func(a, b int) bool {
			if l.hits[r[a]].score != l.hits[r[b]].score {
				return l.hits[r[a]].score > l.hits[r[b]].score
			}
			return r[a] < r[b]
		})
		ranked[fi] = r
	}
	order := make([]int, len(l.files))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ba, bb := l.hits[ranked[order[a]][0]], l.hits[ranked[order[b]][0]]
		if ba.score != bb.score {
			return ba.score > bb.score
		}
		return order[a] < order[b]
	})
	if len(order) > searchRungMaxFiles {
		order = order[:searchRungMaxFiles]
	}
	keep := make([]bool, len(l.hits))
	kept := 0
	for round := 0; round < searchRungPerFileCap && kept < searchRungMaxLines; round++ {
		var cand []int
		for _, fi := range order {
			if round < len(ranked[fi]) {
				cand = append(cand, ranked[fi][round])
			}
		}
		sort.SliceStable(cand, func(a, b int) bool {
			if l.hits[cand[a]].score != l.hits[cand[b]].score {
				return l.hits[cand[a]].score > l.hits[cand[b]].score
			}
			return cand[a] < cand[b]
		})
		for _, h := range cand {
			if kept >= searchRungMaxLines {
				break
			}
			keep[h] = true
			kept++
		}
	}
	return keep
}

// renderSearchListing writes the kept lines in listing order (a grouped listing
// regains the "path:" header of each file that still has a line), then the
// footer, then the tool's own trailer lines.
func renderSearchListing(l *searchListing, keep []bool) string {
	var b strings.Builder
	keptLines, keptFiles := 0, 0
	shown := make([]bool, len(l.files))
	last := -1
	for h, hit := range l.hits {
		if !keep[h] {
			continue
		}
		if !shown[hit.file] {
			shown[hit.file] = true
			keptFiles++
		}
		if l.grouped && hit.file != last {
			b.WriteString(l.files[hit.file].header)
			b.WriteByte('\n')
		}
		last = hit.file
		keptLines++
		b.WriteString(hit.raw)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "[... %d of %d lines elided, %d of %d files elided ...]",
		len(l.hits)-keptLines, len(l.hits), len(l.files)-keptFiles, len(l.files))
	for _, t := range l.trailer {
		b.WriteByte('\n')
		b.WriteString(t)
	}
	return b.String()
}

const (
	shellResultStdoutLine = "--- stdout ---"
	shellResultStderrSep  = "\n--- stderr ---\n"
)

// splitShellFraming separates a shell tool's "exit=N / --- stdout --- / ...
// --- stderr --- / ..." result into the framing before the stdout, the stdout
// and the framing after it. A body that does not open with that framing is one
// stdout section with no framing (ok=true, head and tail empty); a body that opens
// with it but is not intact reports ok=false.
func splitShellFraming(content string) (head, stdout, tail string, ok bool) {
	if !strings.HasPrefix(content, "exit=") {
		return "", content, "", true
	}
	nl := strings.IndexByte(content, '\n')
	if nl < 0 || !strings.HasPrefix(content[nl+1:], shellResultStdoutLine+"\n") {
		return "", "", "", false
	}
	headEnd := nl + 1 + len(shellResultStdoutLine) + 1
	rest := content[headEnd:]
	i := strings.Index(rest, shellResultStderrSep)
	if i < 0 {
		return "", "", "", false
	}
	return content[:headEnd], rest[:i], rest[i:], true
}

// searchForm is the listing form a body must have to be thinned.
type searchForm int

const (
	searchFormAny     searchForm = iota // either form: the shape check on its own
	searchFormGrouped                   // the harness's "path:" then "  L<n>: text" form
	searchFormFlat                      // bare "path:line:text" lines
)

// searchRungBody thins one tool body on its shape alone. It returns (thinned, true)
// only for a body that parses as a search listing of more than searchRungMaxLines
// lines and whose thinned form is strictly shorter; every other input is returned
// as it came. searchRung does not call it directly: it first asks which call
// produced the body and passes the one form that call can print.
func searchRungBody(content string, query []string) (string, bool) {
	return searchRungBodyForm(content, query, searchFormAny)
}

// searchRungBodyForm is searchRungBody restricted to one listing form.
func searchRungBodyForm(content string, query []string, want searchForm) (string, bool) {
	if len(content) < skeletonMinChars || len(content) > searchRungMaxBytes ||
		IsCompactionArtifact(content) || !utf8.ValidString(content) || strings.IndexByte(content, 0) >= 0 {
		return content, false
	}
	head, stdout, tail, ok := splitShellFraming(content)
	if !ok {
		return content, false
	}
	hadNL := strings.HasSuffix(stdout, "\n")
	section := strings.TrimSuffix(stdout, "\n")
	l, ok := parseSearchListing(section)
	if !ok || len(l.hits) <= searchRungMaxLines ||
		(want == searchFormGrouped && !l.grouped) || (want == searchFormFlat && l.grouped) {
		return content, false
	}
	scoreSearchListing(l, query)
	body := renderSearchListing(l, selectSearchHits(l))
	var b strings.Builder
	fmt.Fprintf(&b, "%s%d chars]\n", skeletonPrefix, len(content))
	b.WriteString(head)
	b.WriteString(body)
	if hadNL {
		b.WriteByte('\n')
	}
	b.WriteString(tail)
	out := b.String()
	if len(out) >= len(content) {
		return content, false // no gain: leave it for the next rung.
	}
	return out, true
}

// searchGrepPrograms are the programs whose output is a flat match listing. A
// linter (flake8, ruff, shellcheck -f gcc, rubocop, golangci-lint) prints the same
// "path:line:col:text" lines, but every one of its diagnostics is something the
// agent must fix, so the command is the gate and the shape alone is not.
var searchGrepPrograms = map[string]bool{"grep": true, "rg": true, "ag": true, "ack": true}

// searchCommandIsGrep reports whether a shell command line starts with a search
// program: grep, rg, ag, ack or git grep, optionally by path. Only the first word is
// read, so "cd x && grep ..." or "echo grep" do not qualify; a pipeline after the
// search ("grep -rn x . | head") does, since its output is still the search's. A
// command that chains another one after the search (";", "&&", "||", "&" or a
// newline, outside quotes) does not, because the body then holds that command's
// output as well, and it may be a linter's.
func searchCommandIsGrep(command string) bool {
	w := strings.Fields(command)
	if len(w) == 0 || searchCommandChains(command) {
		return false
	}
	return searchProgramIsGrep(w[0], w[1:])
}

// searchCommandChains reports whether a shell command line runs a second command
// after the first: an unquoted ";", "&&", "||", a lone "&" or a newline. A plain pipe
// ("|", "|&") does not chain, since the pipeline's output is its last stage's, which
// the first-word test already accepts, and the redirections "2>&1", ">&2" and "&>f"
// do not either. It is a quote-aware scan, not a shell parser: single quotes,
// double quotes and a backslash hide a separator, and nothing else does.
func searchCommandChains(command string) bool {
	var quote byte // 0, '\'' or '"'
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case quote == '"':
			if c == '\\' {
				i++ // the escaped byte cannot close the quote
			} else if c == '"' {
				quote = 0
			}
		case c == '\\':
			i++
		case c == '\'' || c == '"':
			quote = c
		case c == ';' || c == '\n' || c == '\r':
			return true
		case c == '|':
			if i+1 < len(command) && command[i+1] == '|' {
				return true
			}
			if i+1 < len(command) && command[i+1] == '&' {
				i++ // "|&" is a pipe
			}
		case c == '&':
			redirect := (i > 0 && (command[i-1] == '>' || command[i-1] == '<')) ||
				(i+1 < len(command) && command[i+1] == '>')
			if !redirect {
				return true
			}
		}
	}
	return false
}

// searchProgramIsGrep is the same test on an already split program and arguments.
func searchProgramIsGrep(prog string, args []string) bool {
	if i := strings.LastIndexAny(prog, `/\`); i >= 0 {
		prog = prog[i+1:]
	}
	if searchGrepPrograms[prog] {
		return true
	}
	return prog == "git" && len(args) > 0 && args[0] == "grep"
}

// searchFormFor says which listing form the call that produced a body can print, or
// searchFormAny with ok=false when the call is not a search at all.
func searchFormFor(c ToolCall) (searchForm, bool) {
	switch c.Name {
	case "search_files":
		return searchFormGrouped, true
	case "run_shell":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(c.Args), &in) == nil && searchCommandIsGrep(in.Command) {
			return searchFormFlat, true
		}
	case "run":
		var in struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}
		if json.Unmarshal([]byte(c.Args), &in) == nil && searchProgramIsGrep(strings.TrimSpace(in.Command), in.Args) {
			return searchFormFlat, true
		}
	}
	return searchFormAny, false
}

// searchCallsByID maps every tool-call id in msgs to its call. An id claimed by two
// different calls is ambiguous and is left out, so a result under it is not thinned;
// the same call repeated under one id is one call. The pairing ignores order (a call
// anywhere in msgs claims its id) because real transcripts put the call first.
func searchCallsByID(msgs []Msg) map[string]ToolCall {
	calls := map[string]ToolCall{}
	ambiguous := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if c.ID == "" {
				continue
			}
			if prev, dup := calls[c.ID]; dup && (prev.Name != c.Name || prev.Args != c.Args) {
				ambiguous[c.ID] = true
			}
			calls[c.ID] = c
		}
	}
	for id := range ambiguous {
		delete(calls, id)
	}
	return calls
}

// searchRung runs the rung over the older tool bodies in [from, to), oldest
// first, until the transcript fits budget: the same span, order and pin rule as
// the skeleton rung, so a pinned body (one the model re-requested) is never
// touched. It rewrites only a tool message's Content, in place in out (the
// ladder's own copy). With no task text there is nothing to rank against and
// the rung does nothing.
//
// A body is thinned only if the call that produced it is a search: the harness's
// search_files (grouped form) or a run_shell / run call whose command is a
// grep-family program (flat form). The tool message's ToolCallID is mapped to its
// call; a body with no matching call, or from any other tool, is left untouched,
// because the same line shape is what a linter prints.
func searchRung(out []Msg, from, to, budget int, pinned map[string]bool) {
	task := searchRungTask(out, from)
	if strings.TrimSpace(task) == "" {
		return
	}
	query := searchQueryTerms(task)
	calls := searchCallsByID(out)
	for i := from; i < to && estimateTokens(out) > budget; i++ {
		if out[i].Role != "tool" || out[i].IsError || pinned[out[i].ToolCallID] {
			continue
		}
		call, ok := calls[out[i].ToolCallID]
		if !ok {
			continue
		}
		form, ok := searchFormFor(call)
		if !ok {
			continue
		}
		if r, ok := searchRungBodyForm(out[i].Content, query, form); ok {
			out[i].Content = r
		}
	}
}
