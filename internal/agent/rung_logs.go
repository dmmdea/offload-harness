package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/gcf"
)

// Log-collapse rung — an OPTIONAL rung of the compaction ladder, default off
// (compactOpts.LogCollapse). It sits after the lossless GCF rung and before
// skeleton pruning: a tool result that is a build, test or server log is
// mostly runs of lines that differ only in a timestamp, a request id or a
// counter, and the line a later step needs (an error, a failing test, the
// summary) is rarely inside such a run. The rung folds each run into its first
// line plus a count, keeps every line that looks like a problem or a test
// summary, keeps the first and last few lines, and appends a footer that says
// what was folded. Strictly less destructive than a skeleton, which drops
// everything that is not a head, tail or signal line.
//
// Provenance. The idea of compressing build and test logs while keeping the
// failures and errors and the boundary lines is the documented behavior of the
// Headroom project (Apache-2.0, https://github.com/headroomlabs-ai/headroom).
// Everything here — the masking of digit-bearing tokens, the consecutive-run
// rule, the head/tail windows, the markers and the footer — is this harness's
// own design, written without reading or copying any of that project's source
// (see NOTICE).
//
// Properties, each pinned by a test in rung_logs_test.go:
//
//   - Deterministic: no maps, no clocks, no randomness; same body, same bytes.
//   - Idempotent: a collapsed body opens with logsPrefix and is never touched
//     again, so compacting twice equals compacting once.
//   - Monotonic: a body is replaced only when the result is strictly shorter;
//     otherwise the ORIGINAL string comes back untouched. A run is folded only
//     when its replacement is shorter than the run.
//   - Fails closed: a body that is too small or too large, is not valid UTF-8,
//     holds a NUL byte, parses as a JSON document (GCF's territory, and folding
//     pretty-printed JSON would corrupt it), already carries another rung's
//     marker or a GCF block, or makes the rewrite panic, comes back unchanged.
//   - Scoped like every other rung: logsRung only touches tool-role bodies in
//     the window the ladder passes it ([protectedEnd, recentStart)), skips
//     pinned call ids, folds only the output of the command tools (run,
//     run_shell; an unknown, ambiguous or any other producer fails closed),
//     rewrites Content only (never Role, ToolCallID or the assistant tool
//     calls, so pairing is intact), works oldest-first and stops at the first
//     fit. The protected preamble and the recent turns are therefore never
//     edited, and the edit point only moves forward, which is what keeps the
//     server's KV prefix as stable as the rest of the ladder (ADR 0017: any
//     edit costs a re-prefill, so a rung must not scatter edits).
//   - Honest about size: when the rung is on, the skeleton prefix, the elide
//     marker and the dedupe reference of a body this rung folded name the
//     size the body had BEFORE the fold (logsMarkerSize), so a marker never
//     tells the model that less was lost than was.
//
// Honest limits, stated here rather than papered over:
//
//   - Every line that matches signalLine (or the extra shapes in logsExtraKeep)
//     is kept verbatim and never counted into a run, even when fifty of them
//     are identical: the cost is that a log made only of one repeated warning
//     does not shrink. A count-preserving fold of signal runs is a later tune
//     for the measurement gate, not a decision made here.
//   - Matching ignores every digit-bearing token, so three consecutive lines
//     such as "port 8080", "port 8081", "port 9090" fold to the first plus a
//     count and the other two values are not recoverable from the transcript.
//     The footer says what was folded, and the original size is in the header.
//   - A body that begins with logsPrefix is taken for an already collapsed
//     body, because the prefix is the only idempotence marker there is: with
//     the rung on, a raw body that merely quotes the header is skipped by this
//     rung. A later rung's marker believes the size in that first line only
//     when this rung could have written it (logsOriginalSize: the exact
//     spelling, a size strictly above the body's own length and inside the
//     sizes the rung folds, a line count a fold needs); a malformed or
//     impossible header is ignored and the marker names the body's own
//     length. Nothing in the text separates a well-formed header with a
//     plausible size from the real one, so a quoted header that passes those
//     tests is still believed; carrying the size out of band would remove
//     that, and no caller can yet. With the rung off nothing here is
//     consulted (logsMarkerSize, IsLogCollapseArtifact), so the default
//     ladder treats that body as the ordinary content it is.
//   - A hex id made only of the letters a-f and shorter than eight characters
//     is not masked, so two lines that differ only in such an id do not match.

const (
	// logsHeadLines and logsTailLines are the windows always kept verbatim; the
	// same sizes as the skeleton rung so the two read alike.
	logsHeadLines = skeletonHeadLines
	logsTailLines = skeletonTailLines
	// logsMinRun is the shortest run of matching lines that may fold. Runs of
	// two (a repeated closing brace, a doubled blank-ish line) are left alone.
	logsMinRun = 3
	// logsMinChars: below this a body is not worth the header and footer.
	logsMinChars = 400
	// logsMaxChars bounds the work: larger bodies are left to the later rungs.
	logsMaxChars = 4 << 20
	// logsPrefix opens every collapsed body: disclosure to the model and the
	// idempotence marker for isLogCollapsed (pure ASCII, like the other rungs'
	// prefixes). Its wording, the markers and the footer deliberately avoid the
	// signalLine vocabulary: the drop rung refuses a unit whose body still
	// matches it, and a rung that matched would make every folded body
	// undroppable.
	logsPrefix = "[log-collapse: original "
)

// logsVolatile matches the tokens that vary between otherwise identical log
// lines: a UUID, a hex run of eight or more characters, and any alphanumeric
// token containing a digit (numbers, timestamp fields, counters, short hex ids,
// "worker7"). Alternatives are tried leftmost-first, so a UUID is masked whole.
var logsVolatile = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|\b[0-9a-fA-F]{8,}\b|[A-Za-z0-9_]*[0-9][A-Za-z0-9_]*`)

// logsANSI matches a terminal control sequence (colours, cursor moves).
var logsANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// logsExtraKeep adds, to signalLine, the shapes of a stack frame, a traceback,
// a goroutine dump, the summary lines of common test runners, the exception
// names of the common languages (TypeError, KeyError, AssertionError,
// NullPointerException: signalLine's word boundary cannot see "error" inside a
// CamelCase name) and the failure shapes a shell or a package manager prints
// (a killed process, a segfault, a non-zero exit status, npm's "ERR!" prefix)
// that signalLine's vocabulary does not catch. signalLine itself is left
// alone: the skeleton and elide rungs use it on the default path.
var logsExtraKeep = regexp.MustCompile(`^\s+at\s+\S|^\s+File ".+", line \d+|^Traceback \(most recent call last\)|^goroutine \d+ \[|^(PASS|FAIL|SKIP)\b|^Ran \d+ tests?\b|^(Tests?|Test Suites?|Test Files?):|^test result:|\bpanicked at\b|\b\w*(Error|Exception)\b|(?i:\bsegmentation fault\b|\bkilled\b)|^exit status [1-9]|\bERR!`)

// logsKey is the equality key of a line for run detection: terminal control
// sequences removed, every volatile token replaced by NUL (which cannot occur
// in a body logsEligible accepts), trailing whitespace and carriage return
// trimmed. Indentation is kept: it carries structure.
func logsKey(line string) string {
	line = logsANSI.ReplaceAllString(line, "")
	line = logsVolatile.ReplaceAllString(line, "\x00")
	return strings.TrimRight(line, " \t\r")
}

// logsKeep reports whether a line must survive verbatim and never join a run:
// a blank line, a line that looks like an error, warning, failure or test
// summary (signalLine), or one of the extra shapes. Each test is made on the
// line as written AND with its terminal control sequences removed, as logsKey
// does: an ESC [ 31 m colour code leaves no word boundary before the word it
// colours, and a line of nothing but colour codes is blank to a reader.
func logsKeep(line string) bool {
	plain := line
	if strings.IndexByte(line, '\x1b') >= 0 {
		plain = logsANSI.ReplaceAllString(line, "")
	}
	if strings.TrimSpace(plain) == "" {
		return true
	}
	if signalLine.MatchString(line) || logsExtraKeep.MatchString(line) {
		return true
	}
	return plain != line && (signalLine.MatchString(plain) || logsExtraKeep.MatchString(plain))
}

// isLogCollapsed reports whether a body is already a collapsed log, so a
// re-compaction never touches it again.
func isLogCollapsed(content string) bool {
	return len(content) >= len(logsPrefix) && content[:len(logsPrefix)] == logsPrefix
}

// logsHeader is the disclosure line that opens a collapsed body, newline
// included. collapseLogs writes it and logsOriginalSize compares against it, so
// the writer and the parser cannot drift apart.
func logsHeader(origChars, origLines int) string {
	return fmt.Sprintf("%s%d chars, %d lines]\n", logsPrefix, origChars, origLines)
}

// logsOriginalSize parses the original body size out of a collapsed body's
// header, so a later rung's marker can name what was lost in total and not the
// size the fold left behind (the same job skeletonOriginalSize does).
//
// The header is the only record of that size, and a body that merely quotes one
// (a saved collapsed log read back, a search hit, a pasted transcript) would
// otherwise hand any integer to the elide, dedupe and skeleton markers. So the
// header is believed only when this rung could have written it:
//
//   - it is spelled exactly as logsHeader spells it (no sign, no leading zeros,
//     no extra spaces, one newline);
//   - the size is above len(content): a fold is kept only when strictly shorter
//     than the body it replaced, and trimming or pruning only shortens it more;
//   - the size is within [logsMinChars, logsMaxChars], the bodies logsEligible
//     lets through;
//   - the line count is at least the head, the tail and one run
//     (logsHeadLines+logsTailLines+logsMinRun, what collapseLogs needs) and at
//     most the size, since a line takes at least one byte.
//
// Anything else returns (0, false) and the caller falls back to the body's own
// length. A well-formed header whose numbers are merely plausible is still
// believed: nothing in the text can tell it from the real one.
func logsOriginalSize(content string) (int, bool) {
	if !isLogCollapsed(content) {
		return 0, false
	}
	var n, lines int
	if _, err := fmt.Sscanf(content[len(logsPrefix):], "%d chars, %d lines]", &n, &lines); err != nil {
		return 0, false
	}
	if n <= len(content) || n < logsMinChars || n > logsMaxChars {
		return 0, false
	}
	if lines < logsHeadLines+logsTailLines+logsMinRun || lines > n {
		return 0, false
	}
	if !strings.HasPrefix(content, logsHeader(n, lines)) {
		return 0, false
	}
	return n, true
}

// logsSkeletonSize is skeletonOriginalSize for the log rung's markers: the size
// a skeleton's disclosure line records, believed only when the skeleton rung
// could have written it. A body that merely quotes a skeleton header (a saved
// skeleton read back, a search hit) would otherwise hand any integer to the
// elide and last-resort markers. The line is believed when
//
//   - it is spelled exactly as skeletonHeader spells it (no sign, no leading
//     zeros, no extra spaces, one newline);
//   - the size is above len(content): a skeleton is kept only when strictly
//     shorter than the body it pruned, and the size it reports is that body's
//     length or the length before an earlier rung shortened it;
//   - the size is within [skeletonMinChars, logsMaxChars]: the skeleton rung
//     leaves a body under skeletonMinChars alone, and logsMaxChars is the
//     largest size this rung vouches for. A skeleton of a larger body is
//     named by its own length, which understates what was lost.
//
// Anything else returns (0, false) and the caller falls back to the body's own
// length. A well-formed header whose number is merely plausible is still
// believed: nothing in the text can tell it from the real one.
func logsSkeletonSize(content string) (int, bool) {
	n, ok := skeletonOriginalSize(content)
	if !ok {
		return 0, false
	}
	if n <= len(content) || n < skeletonMinChars || n > logsMaxChars {
		return 0, false
	}
	if !strings.HasPrefix(content, skeletonHeader(n)) {
		return 0, false
	}
	return n, true
}

// logsMarkerSize is the size an elide, dedupe or skeleton marker names for a
// body. It is len(content) when the rung is off, which keeps every marker the
// default ladder writes byte-identical to what it always wrote: a body that
// merely quotes the rung's header is ordinary content there. With the rung on
// it is what the body measured before any rung shrank it: the size in a
// collapse header that logsOriginalSize believes, or the size a skeleton
// recorded that logsSkeletonSize believes. A header it does not believe leaves
// the body's own length.
func logsMarkerSize(content string, on bool) int {
	if on {
		if n, ok := logsOriginalSize(content); ok {
			return n
		}
		if n, ok := logsSkeletonSize(content); ok {
			return n
		}
	}
	return len(content)
}

// IsLogCollapseArtifact reports whether a tool body is a PRODUCT of the
// optional log-collapse rung. It is deliberately separate from
// IsCompactionArtifact: that predicate also drives the default ladder (the
// always-on dedupe rung) and the live loop's re-execute-versus-refuse decision,
// and a rung that is off must not change either, even for a body that merely
// quotes the header. The trace harvest, which refuses already-compacted
// transcripts whichever ladder produced them, asks both questions. Wiring the
// rung into the live loop later must also teach resultDestroyed this predicate.
func IsLogCollapseArtifact(content string) bool { return isLogCollapsed(content) }

// logsEligible is the fail-closed gate: only a plain-text body of sensible size
// that no other rung has already rewritten may be folded.
func logsEligible(content string) bool {
	if len(content) < logsMinChars || len(content) > logsMaxChars {
		return false
	}
	if isLogCollapsed(content) || isElided(content) || isSkeletonized(content) || isDeduped(content) || gcf.IsCompacted(content) {
		return false
	}
	if !utf8.ValidString(content) || strings.IndexByte(content, 0) >= 0 {
		return false
	}
	return !json.Valid([]byte(content))
}

// logsCollapseImpl is the rewrite itself, held in a variable only so a test can
// make it panic and prove that logCollapse fails closed.
var logsCollapseImpl = collapseLogs

// logCollapse folds runs of near-identical lines in one tool body. It returns
// (folded, true) only when the result is strictly shorter and carries
// logsPrefix; in every other case, including a panic, it returns the original
// body and false.
func logCollapse(content string) (out string, ok bool) {
	defer func() {
		if recover() != nil {
			out, ok = content, false
		}
	}()
	folded, changed := logsCollapseImpl(content)
	if !changed || len(folded) >= len(content) || !isLogCollapsed(folded) {
		return content, false
	}
	return folded, true
}

// collapseLogs does the work for logCollapse. Lines are split on "\n" only, so
// a carriage return stays on its line and every kept line is byte-for-byte as
// it was; one trailing newline is dropped (the footer is the new last line).
func collapseLogs(content string) (string, bool) {
	if !logsEligible(content) {
		return content, false
	}
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	n := len(lines)
	lo, hi := logsHeadLines, n-logsTailLines // [lo, hi) may fold
	if hi-lo < logsMinRun {
		return content, false
	}

	keys := make([]string, n)
	haveKey := make([]bool, n)
	key := func(i int) string {
		if !haveKey[i] {
			keys[i], haveKey[i] = logsKey(lines[i]), true
		}
		return keys[i]
	}

	var body strings.Builder
	line := func(i int) {
		body.WriteString(lines[i])
		body.WriteByte('\n')
	}
	for i := 0; i < lo; i++ {
		line(i)
	}
	runs, hidden := 0, 0
	for i := lo; i < hi; {
		if logsKeep(lines[i]) {
			line(i)
			i++
			continue
		}
		j := i + 1
		for j < hi && !logsKeep(lines[j]) && key(j) == key(i) {
			j++
		}
		if run := j - i; run >= logsMinRun {
			marker := fmt.Sprintf("[... %d similar lines folded ...]", run-1)
			runBytes := 0
			for k := i; k < j; k++ {
				runBytes += len(lines[k]) + 1
			}
			if len(lines[i])+1+len(marker)+1 < runBytes {
				line(i)
				body.WriteString(marker)
				body.WriteByte('\n')
				runs++
				hidden += run - 1
				i = j
				continue
			}
		}
		for k := i; k < j; k++ {
			line(k)
		}
		i = j
	}
	for i := hi; i < n; i++ {
		line(i)
	}
	if runs == 0 {
		return content, false
	}

	footer := fmt.Sprintf("[log-collapse: folded %d run(s) of near-identical lines (%d lines hidden; digits are ignored when comparing lines); kept verbatim: the first %d and last %d lines and every line flagged as a problem or a test summary]",
		runs, hidden, logsHeadLines, logsTailLines)
	out := logsHeader(len(content), n) + body.String() + footer
	if len(out) >= len(content) {
		return content, false
	}
	return out, true
}

// logsCommandTools names the tools whose results are command output (build,
// test and server logs): the only bodies this rung folds. A file read, a search
// result, a fetched page or an API reply is content the model asked for
// verbatim, and its near-identical lines (a table, a list of matches with
// different line numbers) each carry information a fold would lose. A result
// whose producing call cannot be found, is declared by more than one call, or
// has any other name, is left alone: an unknown or ambiguous shape fails closed.
var logsCommandTools = map[string]bool{"run": true, "run_shell": true}

// logsRung folds OLDER command-output bodies in out[from:to], oldest-first,
// until the transcript fits budget. The caller passes the ladder's own window
// ([protectedEnd, recentStart)) and pin set, so the preamble, the recent turns
// and every pinned result stay as they are; only Content is rewritten, never
// Role, ToolCallID or the assistant tool calls. It mutates out in place, which
// is already the ladder's private copy. An out-of-range window is clamped.
func logsRung(out []Msg, from, to, budget int, pinned map[string]bool) {
	if from < 0 {
		from = 0
	}
	if to > len(out) {
		to = len(out)
	}
	if from >= to {
		return
	}
	// An id that more than one call declares (a backend that numbers its calls
	// from 0 in every turn, a model that invents ids) does not say which call
	// produced a result, so such a result is unknown and fails closed. The
	// drop and cut rungs also assume unique ids; this is the same stance.
	producer := map[string]string{} // tool_call_id -> tool name
	declared := map[string]int{}    // tool_call_id -> how many calls declare it
	for _, m := range out {
		for _, c := range m.ToolCalls {
			producer[c.ID] = c.Name
			declared[c.ID]++
		}
	}
	for i := from; i < to && estimateTokens(out) > budget; i++ {
		id := out[i].ToolCallID
		if out[i].Role != "tool" || pinned[id] || declared[id] != 1 || !logsCommandTools[producer[id]] {
			continue
		}
		if c, ok := logCollapse(out[i].Content); ok {
			out[i].Content = c
		}
	}
}

// logsCallIsBuild reports whether a call is a build or test invocation.
// Stub: the gate is still the tool name alone.
func logsCallIsBuild(c ToolCall) bool { return logsCommandTools[c.Name] }
