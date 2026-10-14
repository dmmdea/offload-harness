package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/sandbox"
)

// ---------------------------------------------------------------------------
// Fixtures. The listings below are rendered by the PRODUCTION formatters
// (renderGrep, formatShellResult), so a change to what a tool prints breaks
// these tests instead of silently orphaning the rung's parser. (The frozen
// golden fixtures of ladder_fixtures_test.go deliberately do the opposite.)
// ---------------------------------------------------------------------------

const rungTask = "Why does the fetch timeout get exceeded on a large request?"

func rungQuery() []string { return searchQueryTerms(rungTask) }

func prodGrouped(hits []fixtureHit, capped bool) string {
	ms := make([]grepMatch, len(hits))
	for i, h := range hits {
		ms[i] = grepMatch{path: h.path, line: h.line, text: h.text}
	}
	return renderGrep(ms, capped, "timeout", "content")
}

func prodShell(exit int, stdout, stderr string) string {
	return formatShellResult(sandbox.Result{ExitCode: exit, Stdout: stdout, Stderr: stderr})
}

// benignLines carry no query term of rungTask and none of the ladder's signal
// vocabulary, so a listing made of them ranks flat and trips no signalLine.
var benignLines = []string{
	"let alpha = beta + gamma",
	"return delta",
	"for i := range items {",
	"value := lookup(key)",
	"x = y * 2",
	"count++",
	"names = append(names, n)",
	"// advance the cursor",
	"switch kind {",
	"defer unlock()",
}

// benignHits returns n hits over the first nfiles of the sorted fixture files,
// ascending by line within a file, spread round-robin so every file has hits.
func benignHits(seed uint64, nfiles, n int) []fixtureHit {
	r := &fixtureRand{s: seed}
	files := append([]string(nil), fixtureFiles...)
	sort.Strings(files)
	files = files[:nfiles]
	next := map[string]int{}
	var out []fixtureHit
	for i := 0; i < n; i++ {
		f := files[i%nfiles]
		next[f] += 1 + r.intn(40)
		out = append(out, fixtureHit{path: f, line: next[f], text: benignLines[r.intn(len(benignLines))]})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].path != out[b].path {
			return out[a].path < out[b].path
		}
		return out[a].line < out[b].line
	})
	return out
}

func mustThin(t *testing.T, body string, query []string) string {
	t.Helper()
	out, ok := searchRungBody(body, query)
	if !ok {
		t.Fatalf("expected the rung to thin this body (%d bytes), it returned it untouched", len(body))
	}
	return out
}

// footerOf finds the rung's footer line and returns the four numbers in it.
func footerOf(t *testing.T, out string) (elidedLines, totalLines, elidedFiles, totalFiles int) {
	t.Helper()
	for _, ln := range strings.Split(out, "\n") {
		if n, err := fmt.Sscanf(ln, "[... %d of %d lines elided, %d of %d files elided ...]", &elidedLines, &totalLines, &elidedFiles, &totalFiles); err == nil && n == 4 {
			return
		}
	}
	t.Fatalf("no footer in:\n%s", out)
	return
}

func isRungFooter(ln string) bool {
	return strings.HasPrefix(ln, "[... ") && strings.Contains(ln, " lines elided, ") && strings.HasSuffix(ln, " files elided ...]")
}

// ---------------------------------------------------------------------------
// Tokens, query, task
// ---------------------------------------------------------------------------

func TestSearchTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"fetchTimeout", []string{"fetchtimeout", "fetch", "timeout"}},
		{"HTTPServer", []string{"httpserver", "http", "server"}},
		{"max_retry_count", []string{"max_retry_count", "max", "retry", "count"}},
		{"parse2Words", []string{"parse2words", "parse2", "words"}},
		{"a b cc", []string{"cc"}},
		{"Ünïcode_ident", []string{"ünïcode_ident", "ünïcode", "ident"}},
	}
	for _, c := range cases {
		got := searchTokens(c.in)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("searchTokens(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSearchQueryTermsAreUniqueSortedAndBounded(t *testing.T) {
	got := searchQueryTerms("Find the fetch timeout, find the FETCH timeout and the Timeout")
	if strings.Join(got, ",") != "fetch,timeout" {
		t.Errorf("terms = %v, want [fetch timeout]", got)
	}
	var words []string
	for i := 0; i < 200; i++ {
		words = append(words, fmt.Sprintf("term%03d", i))
	}
	many := searchQueryTerms(strings.Join(words, " "))
	if len(many) != searchRungMaxQueryTerms || !sort.StringsAreSorted(many) {
		t.Errorf("want %d sorted terms, got %d (sorted=%v)", searchRungMaxQueryTerms, len(many), sort.StringsAreSorted(many))
	}
	if searchQueryTerms("the and for") != nil {
		t.Error("a task of stopwords has no terms")
	}
	// A huge task is cut on a rune boundary, never into invalid UTF-8.
	big := strings.Repeat("é", 5000) + " fetch"
	_ = searchQueryTerms(big)
}

// The cap on the query keeps the terms the objective names FIRST, in order of
// appearance; the alphabet has no say in which terms survive. The kept terms are
// then sorted, which only fixes the order every sum over them runs in.
func TestSearchQueryTermsCapKeepsTheFirstOccurringTerms(t *testing.T) {
	// 48 terms in a scrambled order (7 is coprime to 48, so i*7 mod 48 visits each
	// number once), then 20 more that all sort BEFORE every one of them.
	var words, first []string
	for i := 0; i < searchRungMaxQueryTerms; i++ {
		w := fmt.Sprintf("q%02d", (i*7)%searchRungMaxQueryTerms)
		words = append(words, w)
		first = append(first, w)
	}
	for i := 0; i < 20; i++ {
		words = append(words, fmt.Sprintf("a%02d", i))
	}
	words = append(words, "q00", "q13") // a repeat of a kept term costs no slot and changes nothing
	got := searchQueryTerms(strings.Join(words, " "))
	want := append([]string(nil), first...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("cap must keep the first %d terms of the objective, sorted:\n got  %v\n want %v", searchRungMaxQueryTerms, got, want)
	}

	// The decisive terms come first and a long, alphabetically earlier tail follows
	// them: a cap by alphabet would push all three out.
	var task []string
	task = append(task, "retry", "backoff", "timeout")
	for i := 0; i < 60; i++ {
		task = append(task, fmt.Sprintf("aa%02d", i))
	}
	kept := map[string]bool{}
	for _, term := range searchQueryTerms(strings.Join(task, " ")) {
		kept[term] = true
	}
	for _, term := range []string{"retry", "backoff", "timeout"} {
		if !kept[term] {
			t.Errorf("%q opens the objective and must survive the cap", term)
		}
	}
	if len(kept) != searchRungMaxQueryTerms || kept["aa47"] || kept["aa59"] {
		t.Errorf("want exactly the first %d terms (3 named, then aa00..aa44), got %d terms", searchRungMaxQueryTerms, len(kept))
	}

	// Under the cap nothing is dropped and the output is sorted whatever the order of appearance.
	if got := strings.Join(searchQueryTerms("timeout fetch alpha"), ","); got != "alpha,fetch,timeout" {
		t.Errorf("terms under the cap must come back sorted, got %s", got)
	}
}

func TestSearchRungTaskIsTheLastUserMessageOfThePreamble(t *testing.T) {
	msgs := []Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "recalled notes"},
		{Role: "user", Content: "the objective"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "search_files"}}},
		{Role: "tool", ToolCallID: "c1", Content: "x"},
		{Role: "user", Content: "a later nudge"},
	}
	if got := searchRungTask(msgs, 3); got != "the objective" {
		t.Errorf("preamble of 3: got %q", got)
	}
	if got := searchRungTask(msgs, 2); got != "recalled notes" {
		t.Errorf("preamble of 2: got %q", got)
	}
	if got := searchRungTask(msgs, 1); got != "" {
		t.Errorf("a preamble with no user message has no task, got %q", got)
	}
	if got := searchRungTask(msgs, 0); got != "" {
		t.Errorf("no preamble, no task, got %q", got)
	}
	if got := searchRungTask(msgs, 99); got != "a later nudge" {
		t.Errorf("an over-long preamble clamps to the transcript, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

func TestParseSearchListingAcceptsTheHarnessShapes(t *testing.T) {
	hits := benignHits(1, 6, 40)
	l, ok := parseSearchListing(prodGrouped(hits, true))
	if !ok || !l.grouped || len(l.hits) != 40 || len(l.files) != 6 || len(l.trailer) != 1 || l.trailer[0] != grepMoreMarker {
		t.Fatalf("grouped listing: ok=%v %+v", ok, l)
	}

	flat, ok := parseSearchListing(fixtureFlat(hits))
	if !ok || flat.grouped || len(flat.hits) != 40 || len(flat.files) != 6 {
		t.Fatalf("flat listing: ok=%v", ok)
	}

	col, ok := parseSearchListing("pkg/a.go:3:7:colon: in text\npkg/a.go:9:2:more")
	if !ok || len(col.hits) != 2 || col.hits[0].payload != "colon: in text" {
		t.Fatalf("flat listing with columns: ok=%v %+v", ok, col)
	}

	trunc, ok := parseSearchListing("pkg/a.go:3:x\n" + searchTruncatedTrailer)
	if !ok || len(trunc.trailer) != 1 {
		t.Fatalf("a shell truncation note is a trailer: ok=%v", ok)
	}

	// CRLF flat output is accepted and kept byte for byte.
	crlf, ok := parseSearchListing("pkg/a.go:1:x\r\npkg/a.go:2:y\r")
	if !ok || crlf.hits[1].raw != "pkg/a.go:2:y\r" {
		t.Fatalf("crlf flat: ok=%v", ok)
	}
}

func TestParseSearchListingRejectsEverythingElse(t *testing.T) {
	good := prodGrouped(benignHits(2, 4, 20), false)
	cases := map[string]string{
		"empty":                    "",
		"only a trailer":           grepMoreMarker,
		"entry before any header":  "  L1: x\n  L2: y",
		"header without entries":   "pkg/a.go:\npkg/b.go:\n  L1: x",
		"header at the end":        "pkg/a.go:\n  L1: x\npkg/b.go:",
		"stray line":               good + "\nrandom words here",
		"blank line in the middle": "pkg/a.go:1:x\n\npkg/a.go:2:y",
		"timestamps":               "12:30:45 up 3 days\n12:30:46 up 3 days",
		"paths only (files mode)":  "pkg/a.go\npkg/b.go\npkg/c.go",
		"crlf grouped":             strings.ReplaceAll(good, "\n", "\r\n"),
		"numeric path":             "12:30:45\n13:30:45",
		"flat then grouped":        "pkg/a.go:1:x\npkg/b.go:\n  L1: y",
		"entry with bad number":    "pkg/a.go:\n  L0: x",
		"indented header":          " pkg/a.go:\n  L1: x",
	}
	for name, body := range cases {
		if l, ok := parseSearchListing(body); ok {
			t.Errorf("%s: parsed as a listing (%d hits), want rejection", name, len(l.hits))
		}
	}
}

// ---------------------------------------------------------------------------
// The rung on one body
// ---------------------------------------------------------------------------

func TestSearchRungThinsAGroupedListing(t *testing.T) {
	body := prodGrouped(benignHits(3, 20, 90), true)
	out := mustThin(t, body, rungQuery())

	if !strings.HasPrefix(out, skeletonPrefix+fmt.Sprintf("%d chars]\n", len(body))) {
		t.Errorf("must open with the skeleton disclosure naming the original size, got %q", out[:min(80, len(out))])
	}
	if len(out) >= len(body) {
		t.Errorf("not shorter: %d >= %d", len(out), len(body))
	}
	if !IsCompactionArtifact(out) || !isSkeletonized(out) {
		t.Error("the result must be recognised as a compaction artifact by every later rung")
	}
	if n, ok := skeletonOriginalSize(out); !ok || n != len(body) {
		t.Errorf("skeletonOriginalSize = %d,%v want %d", n, ok, len(body))
	}

	orig, _ := parseSearchListing(body)
	elL, totL, elF, totF := footerOf(t, out)
	if totL != len(orig.hits) || totF != len(orig.files) {
		t.Errorf("footer totals %d lines / %d files, listing has %d / %d", totL, totF, len(orig.hits), len(orig.files))
	}

	// Count what survived and check the caps.
	var kept, headers, perFile, maxPerFile int
	for _, ln := range strings.Split(out, "\n")[1:] {
		switch {
		case searchGroupedEntry.MatchString(ln):
			kept++
			perFile++
			if perFile > maxPerFile {
				maxPerFile = perFile
			}
		case isRungFooter(ln) || ln == grepMoreMarker:
		default:
			headers++
			perFile = 0
		}
	}
	if kept > 30 || maxPerFile > 5 || headers > 15 {
		t.Errorf("caps broken: kept=%d (max 30) perFile=%d (max 5) files=%d (max 15)", kept, maxPerFile, headers)
	}
	if elL != totL-kept || elF != totF-headers {
		t.Errorf("footer says %d lines / %d files elided, but %d lines and %d files were dropped", elL, elF, totL-kept, totF-headers)
	}
	if !strings.HasSuffix(out, "\n"+grepMoreMarker) {
		t.Error("the tool's own more-matches note must stay, last")
	}

	// Everything kept is an original line, in the original order.
	assertSubsequence(t, out, body)
}

// assertSubsequence checks that every line of out, except the disclosure line and
// the footer, is a line of body and that they appear in the same order.
func assertSubsequence(t *testing.T, out, body string) {
	t.Helper()
	src := strings.Split(body, "\n")
	j := 0
	for _, ln := range strings.Split(out, "\n")[1:] {
		if isRungFooter(ln) {
			continue
		}
		for j < len(src) && src[j] != ln {
			j++
		}
		if j == len(src) {
			t.Fatalf("line %q is not an original line, or is out of order", ln)
		}
		j++
	}
}

func TestSearchRungThinsFlatAndShellFramedListings(t *testing.T) {
	hits := benignHits(4, 12, 70)
	flat := fixtureFlat(hits)

	out := mustThin(t, flat, rungQuery())
	assertSubsequence(t, out, flat)
	for _, ln := range strings.Split(out, "\n")[1:] {
		if !isRungFooter(ln) && !searchFlatLine.MatchString(ln) {
			t.Errorf("a flat listing keeps flat lines only, got %q", ln)
		}
	}

	nl := mustThin(t, flat+"\n", rungQuery())
	if !strings.HasSuffix(nl, "]\n") {
		t.Error("a trailing newline in the listing is kept")
	}

	body := prodShell(0, flat+"\n", "grep: vendor: Permission denied")
	sh := mustThin(t, body, rungQuery())
	wantHead := "exit=0\n--- stdout ---\n"
	wantTail := "\n--- stderr ---\ngrep: vendor: Permission denied"
	disclosure := skeletonPrefix + fmt.Sprintf("%d chars]\n", len(body))
	if !strings.HasPrefix(sh, disclosure+wantHead) || !strings.HasSuffix(sh, wantTail) {
		t.Errorf("shell framing must survive around the thinned stdout:\n%s", sh)
	}
	assertSubsequence(t, sh, body)

	// A killed command's framing carries a signal note on the first line.
	killed := "exit=-1 (killed by signal 9)\n--- stdout ---\n" + flat + "\n\n--- stderr ---\n"
	if _, ok := searchRungBody(killed, rungQuery()); !ok {
		t.Error("the signal note does not change the framing")
	}
}

func TestSearchRungKeepsTheTaskRelevantLines(t *testing.T) {
	hits := benignHits(5, 24, 96)
	relevant := map[int]string{
		90: "const fetchTimeout = 30 // the request is exceeded after this",
		70: "return errRequestLarge // a large request",
		50: "client.Timeout = fetchTimeout",
	}
	for i, p := range relevant {
		hits[i].text = p
	}
	body := prodGrouped(hits, false)
	out := mustThin(t, body, rungQuery())
	for _, p := range relevant {
		if !strings.Contains(out, p) {
			t.Errorf("the line %q matches the task and must survive:\n%s", p, out)
		}
	}

	// And the score says why: every relevant line outranks every filler line.
	l, _ := parseSearchListing(body)
	scoreSearchListing(l, rungQuery())
	var minRel, maxFill int64 = 1 << 62, 0
	for _, h := range l.hits {
		isRel := false
		for _, p := range relevant {
			if h.payload == p {
				isRel = true
			}
		}
		if isRel && h.score < minRel {
			minRel = h.score
		}
		if !isRel && h.score > maxFill {
			maxFill = h.score
		}
	}
	if minRel <= maxFill {
		t.Errorf("relevant lines (min score %d) must outrank filler (max %d)", minRel, maxFill)
	}
}

func TestSearchRungBoostsErrorLinesAndOnlyThey(t *testing.T) {
	build := func(second, third string) string {
		hits := benignHits(6, 24, 96)
		// The last two files sort last, so with equal scores position would drop them.
		var late []int
		for i, h := range hits {
			if h.path == hits[len(hits)-1].path || h.path == hits[len(hits)-5].path {
				late = append(late, i)
			}
		}
		hits[late[1]].text = second
		hits[late[len(late)-1]].text = third
		return prodGrouped(hits, false)
	}
	q := searchQueryTerms("zzqq nothing here") // matches no line: only the boost can lift one

	boosted := mustThin(t, build("panic: unreachable state", "build failed with exit 2"), q)
	for _, want := range []string{"panic: unreachable state", "build failed with exit 2"} {
		if !strings.Contains(boosted, want) {
			t.Errorf("error line %q must be boosted into the kept set", want)
		}
	}
	control := mustThin(t, build("calm: unreachable state", "build passed with exit 0"), q)
	for _, absent := range []string{"calm: unreachable state", "build passed with exit 0"} {
		if strings.Contains(control, absent) {
			t.Errorf("control: %q is no error line and sits in a late file; it must not be kept (the boost is what kept its twin)", absent)
		}
	}

	l, _ := parseSearchListing(build("panic: x", "plain text"))
	scoreSearchListing(l, nil)
	var boostedN, other int
	for _, h := range l.hits {
		if h.score == 3_000_000 {
			boostedN++
		} else if h.score == 0 {
			other++
		} else {
			t.Fatalf("with no query a line scores 0 or exactly the boost, got %d for %q", h.score, h.payload)
		}
	}
	if boostedN != 1 || other != len(l.hits)-1 {
		t.Errorf("boost applied to %d lines, want exactly the one error line", boostedN)
	}
}

func TestSearchRungKeepsFilesDiverseAndCapsEachFile(t *testing.T) {
	var hits []fixtureHit
	for i := 0; i < 60; i++ {
		hits = append(hits, fixtureHit{path: "pkg/big.go", line: i + 1, text: "fetch timeout exceeded on a large request"})
	}
	files := append([]string(nil), fixtureFiles...)
	sort.Strings(files)
	n := 0
	for _, f := range files {
		if f == "pkg/big.go" || n == 9 {
			continue
		}
		hits = append(hits, fixtureHit{path: f, line: 1, text: "let alpha = beta"})
		n++
	}
	body := prodGrouped(hits, false)
	out := mustThin(t, body, rungQuery())

	perFile := map[string]int{}
	cur := ""
	for _, ln := range strings.Split(out, "\n")[1:] {
		switch {
		case searchGroupedEntry.MatchString(ln):
			perFile[cur]++
		case isRungFooter(ln):
		default:
			cur = strings.TrimSuffix(ln, ":")
			perFile[cur] += 0
		}
	}
	if len(perFile) != 10 {
		t.Errorf("the big file must not crowd out the others: %d files kept, want 10 (%v)", len(perFile), perFile)
	}
	if perFile["pkg/big.go"] != searchRungPerFileCap {
		t.Errorf("the big file keeps its cap of %d lines, kept %d", searchRungPerFileCap, perFile["pkg/big.go"])
	}
	// 60 + 9 hits, 5 + 9 kept, no file dropped entirely.
	if want := "[... 55 of 69 lines elided, 0 of 10 files elided ...]"; !strings.Contains(out, want) {
		t.Errorf("footer should read %q, got:\n%s", want, out)
	}
}

func TestSearchRungMarkersCarryNoSignalVocabulary(t *testing.T) {
	// The drop rung refuses to drop a unit whose body still matches signalLine, on
	// the reading that only a preserved error line can. The rung's own disclosure
	// and footer must never make a listing look like it holds one.
	out := mustThin(t, prodGrouped(benignHits(7, 12, 80), false), rungQuery())
	if signalLine.MatchString(out) {
		t.Errorf("a listing of benign lines must stay free of signal vocabulary after thinning:\n%s", out)
	}
}

func TestSearchRungLeavesEverythingElseAlone(t *testing.T) {
	longListing := prodGrouped(benignHits(8, 10, 80), false)
	lines := strings.Split(longListing, "\n")
	stray := append(append(append([]string(nil), lines[:30]...), "this line belongs to no listing"), lines[30:]...)

	var numbered strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&numbered, "%d. Result title number %d\n   https://example.invalid/%d\n   snippet text for result %d\n", i, i, i, i)
	}
	var paths strings.Builder
	for _, h := range benignHits(9, 20, 60) {
		paths.WriteString(h.path + "\n")
	}
	var testOut strings.Builder
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&testOut, "ok  \tpkg/mod%02d\t0.%03ds\n", i, i)
	}
	flat := fixtureFlat(benignHits(10, 10, 60))

	var huge strings.Builder
	for huge.Len() <= searchRungMaxBytes {
		huge.WriteString("pkg/some/long/path/name.go:12345:let alpha = beta + gamma + delta\n")
	}

	// 31 short lines over 7 files: the caps drop one line, and the disclosure and the
	// footer cost more than that one line saves, so the body must come back whole.
	var shortLines []string
	for f, per := range []int{5, 5, 5, 4, 4, 4, 4} {
		for i := 0; i < per; i++ {
			shortLines = append(shortLines, fmt.Sprintf("f%d/b.go:%d:xxxxxxxxxxxx", f, i+1))
		}
	}
	noGain := strings.Join(shortLines, "\n")
	if len(noGain) < skeletonMinChars {
		t.Fatalf("the no-gain fixture must clear the minimum size to reach the gain check (%d < %d)", len(noGain), skeletonMinChars)
	}

	artifact := mustThin(t, longListing, rungQuery())
	cases := map[string]string{
		"empty":                       "",
		"web_search results":          numbered.String(),
		"files-mode paths":            paths.String(),
		"go test output":              testOut.String(),
		"shell framing, no stderr":    "exit=0\n--- stdout ---\n" + flat,
		"shell framing, odd 2nd line": "exit=0\nstdout:\n" + flat + "\n--- stderr ---\n",
		"shell framing, bad stdout":   prodShell(0, testOut.String(), ""),
		"nul byte":                    strings.Replace(longListing, "let", "l\x00t", 1),
		"invalid utf-8":               strings.Replace(longListing, "let", "l\xffet", 1),
		"stray line":                  strings.Join(stray, "\n"),
		"31 short lines (no gain)":    noGain,
		"its own output":              artifact,
		"a skeleton":                  skeletonPrefix + "9999 chars]\n" + flat,
		"an elision marker":           elisionMarker(99999) + "\n" + flat,
		"over the size bound":         huge.String(),
		"30 hits (at the line cap)":   fixtureFlat(benignHits(11, 6, searchRungMaxLines)),
	}
	for name, body := range cases {
		out, ok := searchRungBody(body, rungQuery())
		if ok || out != body {
			t.Errorf("%s: must come back untouched (ok=%v, changed=%v)", name, ok, out != body)
		}
	}
}

func TestSearchRungIsIdempotent(t *testing.T) {
	for name, body := range map[string]string{
		"grouped": prodGrouped(benignHits(12, 18, 90), true),
		"flat":    fixtureFlat(benignHits(13, 14, 75)),
		"shell":   prodShell(0, fixtureFlat(benignHits(14, 9, 66))+"\n", "warn: x"),
	} {
		once := mustThin(t, body, rungQuery())
		twice, ok := searchRungBody(once, rungQuery())
		if ok || twice != once {
			t.Errorf("%s: applying the rung to its own output must be a no-op", name)
		}
	}
}

func TestSearchRungIsDeterministic(t *testing.T) {
	body := prodGrouped(benignHits(15, 22, 100), true)
	want := mustThin(t, body, rungQuery())
	for i := 0; i < 40; i++ {
		fresh := strings.Clone(body)
		if got, _ := searchRungBody(fresh, searchQueryTerms(rungTask)); got != want {
			t.Fatalf("run %d produced different bytes", i)
		}
	}
}

func TestSearchRungScoresAreIntegerBM25(t *testing.T) {
	l, ok := parseSearchListing(prodGrouped(benignHits(16, 8, 40), false))
	if !ok {
		t.Fatal("fixture must parse")
	}
	l.hits[3].payload = "fetch timeout fetch timeout"
	l.hits[9].payload = "fetch only"
	scoreSearchListing(l, rungQuery())
	if !(l.hits[3].score > l.hits[9].score && l.hits[9].score > 0) {
		t.Errorf("two matching terms must outscore one, one must outscore none: %d, %d", l.hits[3].score, l.hits[9].score)
	}
	for i, h := range l.hits {
		if i != 3 && i != 9 && h.score != 0 {
			t.Errorf("a line with no query term scores 0, hit %d scored %d", i, h.score)
		}
	}
	// Same input, same integers.
	a := append([]searchHit(nil), l.hits...)
	scoreSearchListing(l, rungQuery())
	for i := range a {
		if a[i].score != l.hits[i].score {
			t.Fatalf("score %d moved between runs", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Property sweep: arbitrary mutations of valid and invalid listings.
// ---------------------------------------------------------------------------

func checkSearchRungProperties(t *testing.T, body string, query []string) {
	t.Helper()
	out, ok := searchRungBody(body, query)
	if !ok {
		if out != body {
			t.Fatalf("not applied, but the body changed")
		}
		return
	}
	if len(out) >= len(body) {
		t.Fatalf("monotonic: %d >= %d", len(out), len(body))
	}
	if !IsCompactionArtifact(out) || !utf8.ValidString(out) {
		t.Fatalf("result is not a valid compaction artifact")
	}
	if n, okn := skeletonOriginalSize(out); !okn || n != len(body) {
		t.Fatalf("original size not recoverable: %d,%v", n, okn)
	}
	if again, ok2 := searchRungBody(out, query); ok2 || again != out {
		t.Fatalf("idempotence broken")
	}
	assertSubsequence(t, out, body)
}

func rungRandomBody(r *fixtureRand) string {
	n := r.intn(130)
	hits := benignHits(r.next(), 1+r.intn(len(fixtureFiles)-1), n+1)
	for i := range hits {
		if r.intn(9) == 0 {
			hits[i].text = []string{"panic: x", "fetch timeout", "error: y", "", "a:b:c", "L12: nested"}[r.intn(6)]
		}
	}
	var body string
	switch r.intn(4) {
	case 0:
		body = prodGrouped(hits, r.intn(2) == 0)
	case 1:
		body = fixtureFlat(hits)
	case 2:
		body = prodShell(r.intn(3), fixtureFlat(hits)+"\n", []string{"", "warn: z"}[r.intn(2)])
	default:
		body = fixtureFlat(hits) + "\n"
	}
	switch r.intn(8) {
	case 0: // byte flip
		if len(body) > 0 {
			b := []byte(body)
			b[r.intn(len(b))] = byte(r.intn(256))
			body = string(b)
		}
	case 1: // delete a line
		ls := strings.Split(body, "\n")
		if len(ls) > 1 {
			i := r.intn(len(ls))
			body = strings.Join(append(ls[:i:i], ls[i+1:]...), "\n")
		}
	case 2: // insert garbage
		ls := strings.Split(body, "\n")
		i := r.intn(len(ls))
		body = strings.Join(append(ls[:i:i], append([]string{"garbage line"}, ls[i:]...)...), "\n")
	case 3: // truncate
		if len(body) > 0 {
			body = body[:r.intn(len(body))]
		}
	case 4: // duplicate a line
		ls := strings.Split(body, "\n")
		i := r.intn(len(ls))
		body = strings.Join(append(ls[:i+1:i+1], ls[i:]...), "\n")
	}
	return body
}

func TestSearchRungPropertiesHoldOverMutatedInputs(t *testing.T) {
	r := &fixtureRand{s: 20261014}
	applied := 0
	for i := 0; i < 1500; i++ {
		body := rungRandomBody(r)
		if _, ok := searchRungBody(body, rungQuery()); ok {
			applied++
		}
		checkSearchRungProperties(t, body, rungQuery())
	}
	if applied < 100 {
		t.Errorf("the sweep should exercise the rung itself, not only its refusals: applied %d of 1500", applied)
	}
}

func FuzzSearchRungBody(f *testing.F) {
	r := &fixtureRand{s: 7}
	for i := 0; i < 24; i++ {
		f.Add(rungRandomBody(r))
	}
	f.Add("")
	f.Add("exit=0\n--- stdout ---\n\n--- stderr ---\n")
	f.Add("pkg/a.go:1:x")
	f.Fuzz(func(t *testing.T, body string) {
		checkSearchRungProperties(t, body, rungQuery())
	})
}

// ---------------------------------------------------------------------------
// The rung inside the ladder
// ---------------------------------------------------------------------------

// rungTranscript is [system, objective, 7 tool turns]: three listings of the harness
// shape, one shell-framed flat listing, and three ordinary bodies.
func rungTranscript() []Msg {
	hits := func(seed uint64, files, n int) []fixtureHit {
		h := benignHits(seed, files, n)
		h[len(h)/2].text = "const fetchTimeout = 30 // exceeded on a large request"
		return h
	}
	return fixtureTranscript([]fixtureTurn{
		{"search_files", `{"pattern":"timeout"}`, prodGrouped(hits(21, 20, 95), true)},
		{"read_file", `{"path":"internal/agent/fetchtool.go"}`, fixtureNumbered(31, 40)},
		{"run_shell", `{"command":"grep -rn timeout ."}`, prodShell(0, fixtureFlat(hits(22, 16, 75))+"\n", "")},
		{"search_files", `{"pattern":"deadline"}`, prodGrouped(hits(23, 18, 85), false)},
		{"read_file", `{"path":"internal/netx/dial.go"}`, fixtureNumbered(32, 25)},
		{"search_files", `{"pattern":"Timeout"}`, prodGrouped(hits(24, 14, 60), false)},
		{"read_file", `{"path":"README.md"}`, "short file"},
	})
}

const rungKeep, rungProtected = 2, 2

func rungCompact(msgs []Msg, budget int, opts compactOpts) []Msg {
	in := make([]Msg, len(msgs))
	copy(in, msgs)
	return compact(context.Background(), in, budget, rungKeep, rungProtected, opts)
}

func TestLadderSearchRungThinsTheOldestListingFirst(t *testing.T) {
	msgs := rungTranscript()
	thin := mustThin(t, msgs[3].Content, searchQueryTerms(fixtureObjective))

	// A budget that exactly one thinned body satisfies: the ladder must stop there.
	want := make([]Msg, len(msgs))
	copy(want, msgs)
	want[3].Content = thin
	budget := estimateTokens(want)

	got := rungCompact(msgs, budget, compactOpts{SearchResults: true})
	if len(got) != len(want) {
		t.Fatalf("len %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Content != want[i].Content {
			t.Fatalf("message %d differs: only the oldest listing should have been thinned", i)
		}
	}
}

func TestLadderSearchRungIsOffByDefault(t *testing.T) {
	msgs := rungTranscript()
	for _, pct := range []int{90, 60, 30, 5} {
		budget := estimateTokens(msgs) * pct / 100
		for _, opts := range []compactOpts{{}, {GCF: true, Skeleton: true}} {
			for i, m := range rungCompact(msgs, budget, opts) {
				for _, ln := range strings.Split(m.Content, "\n") {
					if isRungFooter(ln) {
						t.Fatalf("budget %d%%, message %d: the rung ran with its flag off", pct, i)
					}
				}
			}
		}
	}
}

func TestLadderSearchRungRunsBeforeTheGenericSkeleton(t *testing.T) {
	msgs := rungTranscript()
	budget := estimateTokens(msgs) * 70 / 100
	got := rungCompact(msgs, budget, compactOpts{GCF: true, Skeleton: true, SearchResults: true})
	sawRung := false
	for _, m := range got {
		if m.Role == "tool" {
			for _, ln := range strings.Split(m.Content, "\n") {
				if isRungFooter(ln) {
					sawRung = true
				}
			}
		}
	}
	if !sawRung {
		t.Fatal("with both rungs on, a listing must be thinned by the search rung, not skeletonized by the generic one")
	}
}

func TestLadderSearchRungChangesOnlyOlderToolBodies(t *testing.T) {
	msgs := rungTranscript()
	est := estimateTokens(msgs)
	optionSets := []compactOpts{
		{SearchResults: true},
		{Skeleton: true, SearchResults: true},
		{GCF: true, Skeleton: true, SearchResults: true},
	}
	for pct := 95; pct >= 5; pct -= 10 {
		budget := est * pct / 100
		for oi, opts := range optionSets {
			got := rungCompact(msgs, budget, opts)

			// KV-prefix: the preamble and the kept-recent tail are byte for byte what they were.
			for i := 0; i < rungProtected; i++ {
				if got[i].Role != msgs[i].Role || got[i].Content != msgs[i].Content {
					t.Fatalf("pct %d opts %d: preamble message %d changed", pct, oi, i)
				}
			}
			tail := msgs[len(msgs)-rungKeep:]
			gtail := got[len(got)-rungKeep:]
			for i := range tail {
				if gtail[i].Role != tail[i].Role || gtail[i].Content != tail[i].Content || gtail[i].ToolCallID != tail[i].ToolCallID {
					t.Fatalf("pct %d opts %d: recent message %d changed", pct, oi, i)
				}
			}

			// Pairing: every tool result answers a call made earlier, every call has its result.
			called := map[string]bool{}
			answered := map[string]bool{}
			for _, m := range got {
				for _, c := range m.ToolCalls {
					called[c.ID] = true
				}
				if m.Role == "tool" {
					if !called[m.ToolCallID] {
						t.Fatalf("pct %d opts %d: orphan tool result %s", pct, oi, m.ToolCallID)
					}
					answered[m.ToolCallID] = true
				}
			}
			for id := range called {
				if !answered[id] {
					t.Fatalf("pct %d opts %d: call %s lost its result", pct, oi, id)
				}
			}

			// Monotonic: never more tokens, and where no message was dropped, no longer body.
			if estimateTokens(got) > est {
				t.Fatalf("pct %d opts %d: the ladder grew the transcript", pct, oi)
			}
			if len(got) == len(msgs) {
				for i := range got {
					if len(got[i].Content) > len(msgs[i].Content) {
						t.Fatalf("pct %d opts %d: message %d grew", pct, oi, i)
					}
					if got[i].Role != msgs[i].Role || got[i].ToolCallID != msgs[i].ToolCallID {
						t.Fatalf("pct %d opts %d: message %d changed role or pairing id", pct, oi, i)
					}
				}
			}

			// Idempotent at a fixed budget.
			again := rungCompact(got, budget, opts)
			if len(again) != len(got) {
				t.Fatalf("pct %d opts %d: a second pass changed the length", pct, oi)
			}
			for i := range got {
				if again[i].Content != got[i].Content {
					t.Fatalf("pct %d opts %d: a second pass changed message %d", pct, oi, i)
				}
			}
		}
	}
}

func TestLadderSearchRungSparesAPinnedBody(t *testing.T) {
	msgs := rungTranscript()
	budget := estimateTokens(msgs) * 10 / 100
	got := rungCompact(msgs, budget, compactOpts{GCF: true, Skeleton: true, SearchResults: true, Pinned: map[string]bool{"c1": true}})
	found := false
	for _, m := range got {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			found = true
			if m.Content != msgs[3].Content {
				t.Error("a body the model re-requested is pinned: the rung must leave it whole")
			}
		}
	}
	if !found {
		t.Error("the pinned unit must survive the drop rung")
	}
}

func TestLadderSearchRungNeedsATaskAndSkipsErrorBodies(t *testing.T) {
	msgs := rungTranscript()
	budget := estimateTokens(msgs) * 60 / 100

	// No protected preamble, so no objective to rank against: the flag changes nothing.
	off := compact(context.Background(), append([]Msg(nil), msgs...), budget, rungKeep, 0, compactOpts{})
	on := compact(context.Background(), append([]Msg(nil), msgs...), budget, rungKeep, 0, compactOpts{SearchResults: true})
	if len(off) != len(on) {
		t.Fatalf("lengths differ: %d vs %d", len(off), len(on))
	}
	for i := range off {
		if off[i].Content != on[i].Content {
			t.Fatalf("message %d differs with no task text", i)
		}
	}

	// A tool body flagged as an error is never thinned, whatever it looks like.
	withErr := append([]Msg(nil), msgs...)
	withErr[3].IsError = true
	before := withErr[3].Content
	searchRung(withErr, rungProtected, len(withErr)-rungKeep, 1, nil)
	if withErr[3].Content != before {
		t.Error("an IsError body must be left alone")
	}
}

func TestLadderSearchRungResultIsAnArtifactForTheRecoveryPaths(t *testing.T) {
	msgs := rungTranscript()
	thin := mustThin(t, msgs[3].Content, searchQueryTerms(fixtureObjective))
	m2 := append([]Msg(nil), msgs...)
	m2[3].Content = thin
	out := rungCompact(msgs, estimateTokens(m2), compactOpts{SearchResults: true})

	if !resultDestroyed(out, "c1") {
		t.Error("a thinned listing is not the raw result: a repeat of the call must be re-executed (ADR 0016), not refused")
	}
	if resultDestroyed(out, "c2") {
		t.Error("an untouched body is still the raw result")
	}
	// A later, harder pass reports the ORIGINAL size in the bare marker.
	shrunk := emergencyShrink(out, 1, rungProtected)
	want := elisionMarker(len(msgs[3].Content))
	if !strings.HasPrefix(shrunk[3].Content, want) {
		t.Errorf("elision after the rung must name the original size: got %q, want prefix %q", shrunk[3].Content[:min(80, len(shrunk[3].Content))], want)
	}
}

func TestLadderSearchRungIsInertOnTheMiniCorpus(t *testing.T) {
	for _, c := range miniCorpusCases(t) {
		est := estimateTokens(c.msgs)
		for _, pct := range []int{90, 60, 25} {
			budget := est*pct/100 + 1
			a := compact(context.Background(), append([]Msg(nil), c.msgs...), budget, c.keep, c.protected, compactOpts{GCF: true, Skeleton: true})
			b := compact(context.Background(), append([]Msg(nil), c.msgs...), budget, c.keep, c.protected, compactOpts{GCF: true, Skeleton: true, SearchResults: true})
			if ladderDigest(a, fitUnknown) != ladderDigest(b, fitUnknown) {
				t.Errorf("%s at %d%%: the rung changed a transcript that holds no search listing", c.name, pct)
			}
		}
	}
}

// TestSearchRungSavingsReport prints, for the frozen search-shaped fixtures and the
// ladder transcript, what the rung does at three budgets next to what the default
// ladder does at the same budget, in the ladder's own token estimate. It reports;
// it decides nothing, and no default depends on it.
func TestSearchRungSavingsReport(t *testing.T) {
	cases := append(ladderGoldenSynthetic(), ladderGoldenCase{name: "ladder-transcript", msgs: rungTranscript(), keep: rungKeep, protected: rungProtected})
	var listings int
	t.Logf("%-18s %7s %8s %9s %10s %9s", "transcript", "budget", "before", "default", "with-rung", "thinned")
	for _, c := range cases {
		before := estimateTokens(c.msgs)
		for _, pct := range []int{90, 60, 25} {
			budget := before * pct / 100
			def := compact(context.Background(), append([]Msg(nil), c.msgs...), budget, c.keep, c.protected, compactOpts{GCF: true, Skeleton: true})
			with := compact(context.Background(), append([]Msg(nil), c.msgs...), budget, c.keep, c.protected, compactOpts{GCF: true, Skeleton: true, SearchResults: true})
			thinned := 0
			for _, m := range with {
				for _, ln := range strings.Split(m.Content, "\n") {
					if isRungFooter(ln) {
						thinned++
					}
				}
			}
			listings += thinned
			t.Logf("%-18s %6d%% %8d %9d %10d %9d", c.name, pct, before, estimateTokens(def), estimateTokens(with), thinned)
		}
	}
	if listings == 0 {
		t.Error("the report exercised no listing: the fixtures no longer reach the rung")
	}
}

func TestSearchRungScoresTheFilePathAsPartOfTheLine(t *testing.T) {
	// The objective names a package, not a symbol: only the path says which lines are about it.
	hits := benignHits(17, 16, 64)
	var target string
	for _, h := range hits {
		if strings.Contains(h.path, "netx") {
			target = h.path
			break
		}
	}
	if target == "" {
		t.Fatal("fixture has no netx file")
	}
	l, ok := parseSearchListing(prodGrouped(hits, false))
	if !ok {
		t.Fatal("fixture must parse")
	}
	scoreSearchListing(l, searchQueryTerms("what does the netx package do"))
	for _, h := range l.hits {
		inTarget := strings.Contains(l.files[h.file].path, "netx")
		if inTarget && h.score <= 0 {
			t.Errorf("a line of %s must score on the path alone, got %d", l.files[h.file].path, h.score)
		}
		if !inTarget && h.score != 0 {
			t.Errorf("a line outside the package scored %d", h.score)
		}
	}
}

func TestSearchRungWeighsRareTermsAndLineLengthLikeBM25(t *testing.T) {
	// "fetch" is on almost every line, "deadline" on two: a line with the rare term
	// outranks a line with only the common one, and between two lines with the same
	// term the shorter one scores higher.
	hits := benignHits(18, 8, 40)
	for i := range hits {
		hits[i].text = "fetch zzz"
	}
	hits[5].text = "deadline zzz"
	hits[6].text = "fetch one"
	hits[7].text = "fetch one plus a long tail of further words that dilute the match"
	hits[20].text = "deadline yyy"
	l, ok := parseSearchListing(prodGrouped(hits, false))
	if !ok {
		t.Fatal("fixture must parse")
	}
	scoreSearchListing(l, searchQueryTerms("fetch deadline"))
	rare, common := l.hits[5].score, l.hits[0].score
	short, long := l.hits[6].score, l.hits[7].score
	if rare <= common {
		t.Errorf("a rare term must outweigh a common one: %d vs %d", rare, common)
	}
	if short <= long {
		t.Errorf("a shorter line must outscore a longer one with the same term: %d vs %d", short, long)
	}
}

func TestSearchRungCapsAreTheConfigurationExampleValues(t *testing.T) {
	if searchRungMaxLines != 30 || searchRungPerFileCap != 5 || searchRungMaxFiles != 15 {
		t.Errorf("the caps are the values shown in the upstream configuration example (30 lines, 5 per file, 15 files); got %d, %d, %d",
			searchRungMaxLines, searchRungPerFileCap, searchRungMaxFiles)
	}
}

func TestLadderSearchRungNeverTouchesTheRecentTail(t *testing.T) {
	listing := func(seed uint64) string { return prodGrouped(benignHits(seed, 18, 90), false) }
	msgs := fixtureTranscript([]fixtureTurn{
		{"search_files", `{"pattern":"a"}`, listing(41)},
		{"search_files", `{"pattern":"b"}`, listing(42)},
		{"search_files", `{"pattern":"c"}`, listing(43)},
		{"search_files", `{"pattern":"d"}`, listing(44)},
	})
	for _, keep := range []int{2, 4} {
		for _, pct := range []int{80, 40, 5} {
			budget := estimateTokens(msgs) * pct / 100
			in := make([]Msg, len(msgs))
			copy(in, msgs)
			got := compact(context.Background(), in, budget, keep, 2, compactOpts{SearchResults: true})
			tail := msgs[len(msgs)-keep:]
			gtail := got[len(got)-keep:]
			for i := range tail {
				if gtail[i].Content != tail[i].Content || gtail[i].ToolCallID != tail[i].ToolCallID || gtail[i].Role != tail[i].Role {
					t.Fatalf("keep %d at %d%%: kept-recent message %d was rewritten", keep, pct, i)
				}
			}
		}
	}
}

func TestSearchRungKeepsTheShellToolsTruncationNote(t *testing.T) {
	// A shell result clipped at its output cap ends in a note the real formatter
	// writes; the rung must keep it, last in the stdout section, and still thin.
	line := "pkg/x.go:1234:" + strings.Repeat("a", 46) + "\n"
	stdout := strings.Repeat(line, 1200)
	body := prodShell(0, stdout, "")
	if !strings.Contains(body, "\n"+searchTruncatedTrailer+"\n--- stderr ---\n") {
		t.Fatalf("the fixture must go through the real clip (cap %d, stdout %d)", shellOutputCap, len(stdout))
	}
	out := mustThin(t, body, rungQuery())
	if !strings.Contains(out, "files elided ...]\n"+searchTruncatedTrailer+"\n--- stderr ---\n") {
		t.Errorf("the truncation note must stay after the footer and before the stderr framing:\n%s", out[max(0, len(out)-200):])
	}
	checkSearchRungProperties(t, body, rungQuery())
}

// TestSearchRungBodySavingsReport compares, body by body, what the rung and the
// generic skeleton leave of every search listing in the frozen fixtures, in the
// ladder's own token estimate. Report only: the adoption verdict belongs to the
// measurement gate, on a harvested corpus.
func TestSearchRungBodySavingsReport(t *testing.T) {
	cases := append(ladderGoldenSynthetic(), ladderGoldenCase{name: "ladder-transcript", msgs: rungTranscript(), keep: rungKeep, protected: rungProtected})
	tok := func(s string) int { return estimateTokens([]Msg{{Role: "tool", Content: s}}) }
	query := searchQueryTerms(fixtureObjective)
	var bodies, before, rung, skel int
	for _, c := range cases {
		cb, cr, cs := 0, 0, 0
		for _, m := range c.msgs {
			if m.Role != "tool" {
				continue
			}
			thin, ok := searchRungBody(m.Content, query)
			if !ok {
				continue
			}
			sk, skOK := skeletonize(m.Content)
			if !skOK {
				sk = m.Content
			}
			cb += tok(m.Content)
			cr += tok(thin)
			cs += tok(sk)
			bodies++
		}
		t.Logf("%-18s listing tokens before %6d  rung %6d (%3d%% left)  generic skeleton %6d (%3d%% left)", c.name, cb, cr, pct(cr, cb), cs, pct(cs, cb))
		before, rung, skel = before+cb, rung+cr, skel+cs
	}
	t.Logf("%-18s %d listings: before %d tokens, rung %d (%d%% left), generic skeleton %d (%d%% left)", "total", bodies, before, rung, pct(rung, before), skel, pct(skel, before))
	if bodies == 0 || rung >= before {
		t.Errorf("the report found %d listings and the rung kept %d of %d tokens", bodies, rung, before)
	}
}

func pct(a, b int) int {
	if b == 0 {
		return 0
	}
	return a * 100 / b
}

// TestSearchRungKeepsPlantedLinesReport plants three task-relevant lines in each of
// 40 listings and counts how many survive the rung and, for comparison, the generic
// skeleton. A synthetic probe, not a corpus verdict: it shows what the rung is for,
// which is keeping the lines the task is about, not a smaller body.
func TestSearchRungKeepsPlantedLinesReport(t *testing.T) {
	planted := []string{
		"const fetchTimeout = 30 // exceeded after this",
		"return errRequestLarge // a large request",
		"client.Timeout = fetchTimeout",
	}
	total, byRung, bySkeleton := 0, 0, 0
	for seed := uint64(100); seed < 140; seed++ {
		hits := benignHits(seed, 24, 96)
		r := &fixtureRand{s: seed}
		var marked []string
		for k, p := range planted {
			i := r.intn(len(hits))
			hits[i].text = fmt.Sprintf("%s #%d", p, k) // the suffix keeps two plantings on one line distinct
			marked = append(marked, hits[i].text)
		}
		body := prodGrouped(hits, false)
		thin := mustThin(t, body, rungQuery())
		sk, _ := skeletonize(body)
		for _, line := range marked {
			total++
			if strings.Contains(thin, line) {
				byRung++
			}
			if strings.Contains(sk, line) {
				bySkeleton++
			}
		}
	}
	t.Logf("planted task-relevant lines kept: rung %d of %d, generic skeleton %d of %d", byRung, total, bySkeleton, total)
	if byRung != total {
		t.Errorf("the rung must keep every planted task-relevant line: %d of %d", byRung, total)
	}
}

func TestParseSearchListingKeepsEveryTrailerLineInOrder(t *testing.T) {
	l, ok := parseSearchListing("pkg/a.go:1:x" + "\n" + grepMoreMarker + "\n" + searchTruncatedTrailer + "\n" + grepMoreMarker)
	if !ok || len(l.hits) != 1 || strings.Join(l.trailer, "|") != grepMoreMarker+"|"+searchTruncatedTrailer+"|"+grepMoreMarker {
		t.Fatalf("trailer lines must all be kept, in order: ok=%v %+v", ok, l)
	}
	// A body that is nothing but trailer lines is not a listing, however many there are.
	if _, ok := parseSearchListing(strings.Repeat(grepMoreMarker+"\n", 50000) + grepMoreMarker); ok {
		t.Error("trailer lines alone are not a listing")
	}
}

// ---------------------------------------------------------------------------
// Contracts the review round pinned: preamble, transitive pins, roles, the
// documented error words, and the production (Tokenizer) path.
// ---------------------------------------------------------------------------

// exemplarPreamble is shaped like a production preamble that carries profile
// exemplars: [system, user, assistant(call), tool(listing), user(objective)]. The
// tool body inside it is a 90-hit listing, far over the rung's size floor, so a rung
// that wandered into the preamble would thin it.
func exemplarPreamble(listing string) []Msg {
	return []Msg{
		{Role: "system", Content: "You are a coding agent."},
		{Role: "user", Content: "Show how you would search for a symbol."},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "ex_search_1", Name: "search_files", Args: `{"pattern":"timeout"}`}}},
		{Role: "tool", ToolCallID: "ex_search_1", Content: listing},
		{Role: "user", Content: rungTask},
	}
}

func TestLadderSearchRungNeverTouchesAToolBodyInsideThePreamble(t *testing.T) {
	exemplar := prodGrouped(benignHits(41, 20, 90), true)
	older := prodGrouped(benignHits(42, 18, 85), false)
	newer := prodGrouped(benignHits(43, 16, 70), false)
	msgs := append(exemplarPreamble(exemplar),
		Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "search_files", Args: `{"pattern":"deadline"}`}}},
		Msg{Role: "tool", ToolCallID: "c1", Content: older},
		Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "search_files", Args: `{"pattern":"Timeout"}`}}},
		Msg{Role: "tool", ToolCallID: "c2", Content: newer},
		Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: "c3", Name: "read_file", Args: `{"path":"README.md"}`}}},
		Msg{Role: "tool", ToolCallID: "c3", Content: "short file"},
	)
	const protected = 5
	if _, ok := searchRungBody(exemplar, searchQueryTerms(rungTask)); !ok {
		t.Fatal("fixture: the exemplar listing must be one the rung would thin, or this test proves nothing")
	}

	checkPreamble := func(label string, got []Msg) {
		t.Helper()
		for i := 0; i < protected; i++ {
			if got[i].Role != msgs[i].Role || got[i].Content != msgs[i].Content || got[i].ToolCallID != msgs[i].ToolCallID || got[i].IsError != msgs[i].IsError {
				t.Fatalf("%s: preamble message %d changed (role %q, tool call id %q)", label, i, got[i].Role, got[i].ToolCallID)
			}
		}
	}
	footers := func(ms []Msg) (n int) {
		for _, m := range ms {
			for _, ln := range strings.Split(m.Content, "\n") {
				if isRungFooter(ln) {
					n++
				}
			}
		}
		return n
	}

	// The rung on its own, with a budget nothing can meet: it works through the whole span.
	direct := append([]Msg(nil), msgs...)
	searchRung(direct, protected, len(direct)-rungKeep, 1, nil)
	checkPreamble("searchRung", direct)
	if got := footers(direct[protected:]); got != 2 {
		t.Errorf("both older listings after the preamble must be thinned, got %d thinned", got)
	}
	if footers(direct[:protected]) != 0 {
		t.Error("a thinned listing appeared inside the protected preamble")
	}

	// And through the ladder, at several budgets, with every rung switched on.
	est := estimateTokens(msgs)
	for _, pct := range []int{90, 60, 30, 5} {
		got := compact(context.Background(), append([]Msg(nil), msgs...), est*pct/100, rungKeep, protected, compactOpts{GCF: true, Skeleton: true, SearchResults: true})
		checkPreamble(fmt.Sprintf("compact at %d%%", pct), got)
	}
}

func TestLadderSearchRungSparesAListingAPinnedDedupeReferenceStandsFor(t *testing.T) {
	// c1's body is a dedupe reference to c2: the model re-requested c1, so what it
	// wants are the bytes that live at c2, and the ladder pins c2 through the
	// reference (ADR 0016). The rung must be handed that resolved set, not the raw one.
	listing := prodGrouped(benignHits(44, 20, 90), false)
	other := prodGrouped(benignHits(45, 18, 80), false)
	msgs := fixtureTranscript([]fixtureTurn{
		{"search_files", `{"pattern":"timeout"}`, dedupeMarker("c2", len(listing))},
		{"search_files", `{"pattern":"timeout"}`, listing},
		{"search_files", `{"pattern":"deadline"}`, other},
		{"read_file", `{"path":"README.md"}`, "short file"},
	})
	// A budget that thinning the unpinned listing alone satisfies, so the ladder stops
	// right after the rung and nothing else can explain a change to the pinned one.
	thin := mustThin(t, other, searchQueryTerms(fixtureObjective))
	fits := append([]Msg(nil), msgs...)
	fits[7].Content = thin
	got := rungCompact(msgs, estimateTokens(fits), compactOpts{SearchResults: true, Pinned: map[string]bool{"c1": true}})

	byID := map[string]string{}
	for _, m := range got {
		if m.Role == "tool" {
			byID[m.ToolCallID] = m.Content
		}
	}
	if byID["c2"] != listing {
		t.Errorf("the listing a pinned dedupe reference points at was thinned or lost (%d bytes, was %d)", len(byID["c2"]), len(listing))
	}
	if byID["c1"] != msgs[3].Content {
		t.Error("the pinned reference itself must stay as it was")
	}
	// The rung did run: the unpinned listing is the one that gave way.
	if byID["c3"] != thin {
		t.Errorf("control: the unpinned listing should have been thinned to meet the budget, got %d bytes", len(byID["c3"]))
	}
}

func TestLadderSearchRungChangesOnlyToolMessages(t *testing.T) {
	// Listing-shaped content in an older assistant message and in an older user
	// message is not a tool result and is never rewritten; the tool body between
	// them is the control that the rung does run on this transcript.
	assistantBody := prodGrouped(benignHits(46, 20, 90), true)
	toolBody := prodGrouped(benignHits(47, 18, 85), false)
	userBody := prodGrouped(benignHits(48, 16, 80), false)
	for name, body := range map[string]string{"assistant": assistantBody, "user": userBody} {
		if _, ok := searchRungBody(body, searchQueryTerms(fixtureObjective)); !ok {
			t.Fatalf("fixture: the %s message's content must be one the rung would thin", name)
		}
	}
	msgs := []Msg{
		{Role: "system", Content: "You are a coding agent."},
		{Role: "user", Content: fixtureObjective},
		{Role: "assistant", Content: assistantBody, ToolCalls: []ToolCall{{ID: "c1", Name: "search_files", Args: `{"pattern":"timeout"}`}}},
		{Role: "tool", ToolCallID: "c1", Content: toolBody},
		{Role: "user", Content: userBody},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "read_file", Args: `{"path":"a.go"}`}}},
		{Role: "tool", ToolCallID: "c2", Content: "short file"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c3", Name: "read_file", Args: `{"path":"b.go"}`}}},
		{Role: "tool", ToolCallID: "c3", Content: "short file too"},
	}
	out := append([]Msg(nil), msgs...)
	searchRung(out, 2, len(out)-rungKeep, 1, nil)

	if out[2].Content != assistantBody {
		t.Error("an older assistant message must never be rewritten, whatever its content looks like")
	}
	if out[4].Content != userBody {
		t.Error("an older user message must never be rewritten, whatever its content looks like")
	}
	if out[3].Content == toolBody || !strings.Contains(out[3].Content, "lines elided") {
		t.Error("control: the older tool body is a listing and must have been thinned")
	}
	for i := range msgs {
		if out[i].Role != msgs[i].Role || out[i].ToolCallID != msgs[i].ToolCallID || len(out[i].ToolCalls) != len(msgs[i].ToolCalls) {
			t.Errorf("message %d: only a tool message's Content may change", i)
		}
	}
}

// TestSearchRungBoostsEveryDocumentedErrorWord pins the regexp word by word: the
// docs and the changelog fragment list error, fail, panic, exception and fatal, and
// each of them (with the inflections the pattern accepts) must lift a line by exactly
// the boost, while the same line without it, or with a word that only contains it,
// stays at zero.
func TestSearchRungBoostsEveryDocumentedErrorWord(t *testing.T) {
	l, ok := parseSearchListing(prodGrouped(benignHits(49, 6, 40), false))
	if !ok {
		t.Fatal("fixture must parse")
	}
	score := func(text string) int64 {
		l.hits[0].payload = text
		scoreSearchListing(l, nil) // no query: a line can only score through the boost
		return l.hits[0].score
	}
	words := []string{
		"error", "errors",
		"fail", "fails", "failed", "failure", "failures", "failing",
		"panic", "panics", "panicked",
		"exception", "exceptions",
		"fatal", "FATAL", "Panic",
	}
	for _, w := range words {
		if got := score("the step reported " + w + " again"); got != searchErrorBoostMicros {
			t.Errorf("%q must be boosted by exactly %d, scored %d", w, searchErrorBoostMicros, got)
		}
		if got := score("the step reported ok again"); got != 0 {
			t.Errorf("control for %q: the same line without it must score 0, scored %d", w, got)
		}
	}
	for _, near := range []string{"errorless mode", "terror alert", "errorcode table", "exceptional case", "fatalism", "failover plan", "panicky"} {
		if got := score("the step reported " + near + " again"); got != 0 {
			t.Errorf("%q only contains an error word and must not be boosted, scored %d", near, got)
		}
	}
}

// The ladder reaches the rung with a Tokenizer set in production. When the rung
// alone brings the transcript within the estimate budget the ladder returns
// right there: the verdict stays fitUnknown, the token-exact cut is never asked,
// and nothing but the thinned listing differs from the input.
func TestLadderSearchRungStopsTheLadderWhenItAloneFits(t *testing.T) {
	msgs := rungTranscript()
	thin := mustThin(t, msgs[3].Content, searchQueryTerms(fixtureObjective))
	want := append([]Msg(nil), msgs...)
	want[3].Content = thin
	budget := estimateTokens(want)

	tok := &runeTok{}
	got, verdict := compactWithVerdict(context.Background(), append([]Msg(nil), msgs...), budget, rungKeep, rungProtected,
		compactOpts{SearchResults: true, Tok: tok, RealBudget: 1 << 20})
	if verdict != fitUnknown {
		t.Errorf("verdict = %v, want fitUnknown: the estimate ladder fit the transcript and nothing measured it", verdict)
	}
	if tok.calls != 0 {
		t.Errorf("the tokenizer was asked %d times; the token-exact cut must not run once the rung fits", tok.calls)
	}
	if len(got) != len(want) {
		t.Fatalf("len %d, want %d: no message may be dropped", len(got), len(want))
	}
	for i := range want {
		if got[i].Content != want[i].Content || got[i].Role != want[i].Role || got[i].ToolCallID != want[i].ToolCallID {
			t.Fatalf("message %d differs: only the oldest listing should have been thinned", i)
		}
	}

	// Control, same options: a budget the rung cannot meet does reach the tokenizer.
	ctl := &runeTok{}
	_, v := compactWithVerdict(context.Background(), append([]Msg(nil), msgs...), 1, rungKeep, rungProtected,
		compactOpts{SearchResults: true, Tok: ctl, RealBudget: 1 << 20})
	if ctl.calls == 0 || v != fitReal {
		t.Errorf("control: with a budget the rung cannot meet the token-exact cut must run and measure (calls=%d, verdict=%v)", ctl.calls, v)
	}
}
