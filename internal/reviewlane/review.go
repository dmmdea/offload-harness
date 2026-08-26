// Package reviewlane runs a CLEAN-CONTEXT diff review on a local seat.
//
// The mechanism is the product. Cognition REPORTS that a dedicated reviewer in their
// Fusion setup catches ~2 bugs per PR, ~58% of them severe — that is the vendor's own
// published figure, unaudited, with no sample size and no A/B baseline, so it is cited
// here as a claim rather than as evidence. What IS independently supported is the
// underlying mechanism: context degradation over a long window (Chroma's 18-model
// context-rot study; Stanford's lost-in-the-middle), which is why a reviewer that never
// accumulated the author's context sees what the author's judgement has stopped seeing.
// Our seats have clean context by construction, so this lane offers something the lead
// cannot produce from inside its own context at all.
//
// So it deliberately ships NO history: the task statement and the diff, nothing else. Two
// consequences run through everything below.
//
// The diff rides in the GOAL, not in a context doc. A context doc becomes a file the seat
// must find with list_dir and open with read_file, and the measured failure mode of a
// small planner is calling no tool at all — which would produce confident findings about a
// diff never read. Putting the diff where the seat cannot fail to see it removes that whole
// class. The cost is that core.AgentContract.Validate's 256 KiB context cap never sees the
// diff, so this package owns that bound itself (MaxDiffBytes) and refuses early, naming the
// numbers, instead of shipping an unbounded prompt at a seat whose window cannot hold it.
//
// The contract carries NO acceptance check, and that is a decision rather than an omission.
// An empty findings list is a CORRECT outcome here — the honest reading of "this reviewer
// found nothing" — so any content check would either punish a clean diff or pass anything,
// which is exactly the decorative-acceptance pathology delegate.LintAcceptance exists to
// name. What replaces it is a check the harness can actually make: a finding naming a file
// the diff never touched is dropped and COUNTED (Ground), because it cannot be triaged from
// the diff and an invented path is the ordinary way a small seat fails here.
//
// Everything this lane returns is ADVISORY. It never gates a merge, never substitutes for
// the final does-it-actually-work verification, and a `severe` label from a small local
// model is a prompt for the lead to read those lines — not a verdict, and never something
// to apply unread.
package reviewlane

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// Typed refusals, in the same posture as internal/askjob: the caller gets a named reason
// and reviews the diff itself, which is strictly better than a contract that runs and
// reviews nothing.
var (
	// ErrNoTask means no task statement was supplied. Without intent there is nothing
	// to judge the diff AGAINST, and a reviewer with no intent grades style.
	ErrNoTask = errors.New("reviewlane: task is required — the reviewer needs to know what the change was supposed to do")
	// ErrNoDiff means no diff text was supplied (or the file held only whitespace).
	ErrNoDiff = errors.New("reviewlane: diff is empty — nothing to review")
	// ErrDiffTooLarge is the byte-cap refusal, raised HERE with the real numbers and
	// the fix rather than left for a seat to discover by overflowing its window.
	ErrDiffTooLarge = errors.New("reviewlane: diff too large for one review")
)

// MaxDiffBytes bounds a single review's diff. It is core.AgentContextMaxBytes because that
// is the harness's own context ceiling and the number every other lane's refusal names —
// but note it is enforced here and nowhere else: the diff rides in the goal, so Validate's
// context arithmetic never sees it. A diff anywhere near this size will still exhaust a
// small seat's window; splitting by path (`git diff -- <dir>`) is the answer, and the tool
// description says so.
const MaxDiffBytes = core.AgentContextMaxBytes

// DefaultMaxFindings is both the default cap on returned findings AND the ceiling, because
// it is the number the prompt itself asks the seat for. The structured re-pack runs on a
// 512-token budget (pipeline.agentRepackMaxTokens): asking for more findings than that can
// carry produces a truncated JSON array, which fails schema validation and defers a review
// that had already been done. So max_findings NARROWS the list, never widens it.
const DefaultMaxFindings = 10

// Finding is one reviewer observation about the diff.
type Finding struct {
	Severity string `json:"severity"` // severe | moderate | minor ("" when the seat named none)
	File     string `json:"file"`
	Line     int    `json:"line"`
	Claim    string `json:"claim"`
	Why      string `json:"why"`
}

// reviewOutputSchema is a flat object whose one field is an array of STRINGS, and both
// halves are forced by gbnf.FromJSONSchema's supported subset: nested object items compile
// to `stringarray` regardless of what the schema says, so an array of {severity,file,...}
// objects would silently become an array of strings anyway. Asking for the line format
// explicitly (see promptFormatTail) and parsing it delegator-side is the honest version of the
// same thing — and ParseFindings keeps whatever it cannot parse rather than dropping it.
var reviewOutputSchema = json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array","items":{"type":"string"}}},"required":["findings"]}`)

// promptHeader is the whole of the reviewer's instructions. It says "you have no prior
// knowledge" in as many words: the isolation is not merely a fact of how the seat is
// invoked, it is what the reviewer is asked to lean on.
//
// It does NOT claim that nothing else is reachable, which an earlier draft did. That claim
// was false: BuildContract leaves Profile empty so the executing box's own agent_profile
// decides the toolset, and an un-narrowed profile hands the loop list_dir/read_file over the
// job dir. So the instruction is phrased as something the reviewer must DO ("do not look
// anything up") rather than as a fact about its sandbox that the sandbox does not enforce.
const promptHeader = `You are reviewing a code diff. You have NO prior knowledge of this work — you were not
present for it. Do not look anything up: judge ONLY the diff below, against the stated task.

Report concrete defects: logic errors, off-by-one and boundary mistakes, unhandled errors,
missing edge cases, security problems, and changes that contradict the task. Do NOT comment
on style, naming, or formatting. Do NOT invent anything you cannot see in the diff — a
defect you cannot point at a changed line is not a finding.
`

// promptFormatHead/promptFormatTail are the line shape ParseFindings reads back, split around
// the finding count so the number the seat is asked for is DefaultMaxFindings itself and
// cannot drift from the cap the re-pack budget forces. It is stated as the whole of the
// answer ("nothing else") because the structured re-pack is an extract over this text: a
// preamble the seat adds becomes a finding-shaped string that survives as an unranked claim,
// which is noise the lead then has to triage.
//
// The FILLED-IN example is load-bearing and was added from a live run, not from review. With
// an abstract `severity | file:line | claim | why` template the 27B seat found both planted
// defects in a probe diff and then wrote `severe | file:16` — it had copied the placeholder
// name instead of the path and lost claim and why entirely. The example is deliberately a
// DIFFERENT defect class from anything a test diff is likely to plant: the first version used
// an off-by-one, the seat replied in the example's exact wording, and "found it" became
// indistinguishable from "parroted it".
//
// fieldSpec and exampleFinding are their own constants because they are read TWICE: once to
// build the prompt, and once by dropTemplateEchoes to discard those same two lines if the
// seat hands them back AS findings. Echoing the example is measured behaviour of this seat
// (see the CHANGELOG's 0.97.0 entry), so the guard must compare against the exact text the
// prompt shipped — a second copy of either string would stop matching the moment one was
// edited, and the guard would go quietly inert.
const (
	fieldSpec      = `<severity> | <path>:<line> | <claim> | <why>`
	exampleFinding = `moderate | internal/store/load.go:57 | the returned error is discarded with _ | a failed load reads as an empty store`
)

const promptFormatHead = `
Answer with ONE LINE PER DEFECT, at most `

const promptFormatTail = ` lines, most serious first, each with
FOUR fields separated by | and nothing else around them:

` + fieldSpec + `

A filled-in example of one line, for shape only — it is not about the diff below:

` + exampleFinding + `

<severity> is one of: severe, moderate, minor. <path> is the file's REAL path, copied from
the diff — never the literal word "file". <line> is its line number in the new file.
<claim> states the defect in under 15 words. <why> states the consequence in under 20 words.
Fill in all four every time; do not copy the placeholder names, and write nothing before or
after the lines. If the diff has no defects, answer with the single word: NONE

TASK:
`

// promptReminderHead/promptReminderTail repeat the field spec AFTER the diff.
//
// Not redundancy — arithmetic. The diff may run to MaxDiffBytes (256 KiB), which puts the
// original spec a quarter of a megabyte above the point where it has to be applied, and
// attention decay over a long window is this lane's own founding thesis. It would be
// incoherent to rest the whole design on lost-in-the-middle and then bury the one
// instruction that has ALREADY failed live (see promptFormatTail) at the far end of the
// context.
const promptReminderHead = `
REMINDER, now that you have read the diff — the answer format, repeated here because it was
stated a long way above and this is where it has to be applied:

` + fieldSpec + `

One line per defect, at most `

const promptReminderTail = ` lines, most serious first, nothing before or after them.
Use the file's REAL path from the diff. If the diff has no defects, answer with the single
word: NONE
`

// buildPrompt assembles the seat's entire instruction: header, format, task, diff. Nothing
// else reaches the reviewer — no history, no file list, no prior findings.
func buildPrompt(task, diff string) string {
	n := strconv.Itoa(DefaultMaxFindings)
	var b strings.Builder
	b.Grow(len(promptHeader) + len(promptFormatHead) + len(promptFormatTail) +
		len(promptReminderHead) + len(promptReminderTail) + len(task) + len(diff) + 32)
	b.WriteString(promptHeader)
	b.WriteString(promptFormatHead)
	b.WriteString(n)
	b.WriteString(promptFormatTail)
	b.WriteString(task)
	b.WriteString("\n\nDIFF:\n")
	b.WriteString(diff)
	b.WriteString("\n")
	// The format spec again, on the near side of the diff — see promptReminderHead.
	b.WriteString(promptReminderHead)
	b.WriteString(n)
	b.WriteString(promptReminderTail)
	return b.String()
}

// BuildContract assembles the complete, validated contract for one clean-context review.
//
// It takes no findings cap: the seat is always asked for DefaultMaxFindings, because that
// number is set by what the structured re-pack's token budget can carry, not by caller
// preference. A caller's max_findings NARROWS the published list and is applied
// delegator-side in Report, where it is checkable.
func BuildContract(task, diff string) (core.AgentContract, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return core.AgentContract{}, ErrNoTask
	}
	if strings.TrimSpace(diff) == "" {
		return core.AgentContract{}, ErrNoDiff
	}
	if len(diff) > MaxDiffBytes {
		return core.AgentContract{}, fmt.Errorf("%w: %d bytes exceeds the %d-byte ceiling — split it by path (git diff -- <dir>) and review each part, which also keeps each review inside the seat's context window",
			ErrDiffTooLarge, len(diff), MaxDiffBytes)
	}
	// PrepareContract mints schema_version/depth and clamps max_steps/timeout to the same
	// ceilings the wire decoder applies, then runs the full Validate — so a contract this
	// package builds obeys exactly the rules a hand-written one does. readRoot is unused
	// (no context_paths: there is nothing to inline), and Profile is left EMPTY on purpose
	// so the executing box's own agent_profile decides — a per-SEAT property, measured as
	// the single biggest lever on a small tier.
	return delegate.PrepareContract(delegate.SubtaskSpec{
		AgentContract: core.AgentContract{
			Goal:         buildPrompt(task, diff),
			OutputSchema: reviewOutputSchema,
			// Acceptance is deliberately empty — see the package comment.
		},
	}, "")
}

// Result is everything one review publishes: the findings the caller is shown, plus the
// three counts that say what is NOT in that list.
//
// The counts are not telemetry. A short or empty findings list is the shape a reader most
// easily misreads, and each count means something different about WHY it is short:
// DroppedUngrounded says the seat named a file the diff does not touch (it invented a path),
// DroppedEcho says it handed the prompt's own template back instead of reviewing, and
// TruncatedByCap says more was found than the caller asked to see. Counting one and
// swallowing the others would make the published list quietly unreadable — the same reason
// dropped-but-uncounted was wrong in the first place.
type Result struct {
	Findings          []Finding
	DroppedUngrounded int
	DroppedEcho       int
	TruncatedByCap    int
}

// Report turns the seat's raw finding lines into what the caller is shown: template echoes
// removed, parsed, grounded against the diff's own files, severity-ranked, capped — with a
// count for each of the three ways a line can fail to reach the caller.
func Report(lines []string, diff string, max int) Result {
	lines, echoed := dropTemplateEchoes(lines)
	kept, ungrounded := Ground(ParseFindings(lines), FilesInDiff(diff))
	ranked := rankFindings(kept, capFindings(max))
	return Result{
		Findings:          ranked,
		DroppedUngrounded: ungrounded,
		DroppedEcho:       echoed,
		// rankFindings reorders and truncates and does nothing else, so the difference
		// between what went in and what came out IS the cap's doing.
		TruncatedByCap: len(kept) - len(ranked),
	}
}

// dropTemplateEchoes removes lines that are the prompt's own field spec or worked example
// handed straight back, and counts them.
//
// This converts a human judgement into a machine check. The worked example is parseable and
// grounds against any diff touching a file with that base name, so an echo of it would reach
// the caller as an ordinary finding — and echoing the example is not hypothetical: it was
// MEASURED on this seat while building the lane (the first example described the same defect
// class as the planted one, and the reply came back in the example's exact wording). Choosing
// a neutral example makes an echo distinguishable to a person reading the output; it does
// nothing for the harness. This does.
//
// It is a byte-equality test after the same normalisation ParseFindings applies, never a
// similarity test: a finding that merely resembles the example is a finding, and dropping it
// would be the quality judgement this package's own comment argues against.
func dropTemplateEchoes(lines []string) ([]string, int) {
	out := make([]string, 0, len(lines))
	dropped := 0
	for _, ln := range lines {
		if t := normalizeLine(ln); t == fieldSpec || t == exampleFinding {
			dropped++
			continue
		}
		out = append(out, ln)
	}
	return out, dropped
}

// noneVerdictRe matches the NONE token promptFormatTail asks for when a diff has no defects.
var noneVerdictRe = regexp.MustCompile(`(?i)\bnone\b`)

// minCleanVerdictChars is the shortest raw answer that may stand in for the NONE token. It
// separates "the seat said something" from "the seat said nothing" — NOT "the seat said
// something good", which is a judgement this lane does not make. A one-sentence clean verdict
// ("no defects found in this diff") clears it; "ok" does not, and neither does "".
const minCleanVerdictChars = 16

// VerdictReadsClean reports whether the seat's OWN raw answer supports publishing an empty
// findings list as a genuine clean review rather than as a broken run.
//
// This closes the hole that made the two indistinguishable. The traced path: agent/loop.go
// returns stop_reason "done" the moment the model stops requesting tools, with no check that
// the final message has any CONTENT — and empty content is live-measured in this codebase
// (pipeline/agenttask.go's re-pack comment: a GBNF + thinking seat puts its answer in
// reasoning_content and leaves content empty). agenttask.go special-cases only "budget", so
// "done" with an empty Output reaches repackStructured, which extracts findings from an empty
// string and returns a schema-valid {"findings":[]}. Nothing downstream could tell that from
// a real clean review: steps:1 and stop_reason "done" describe both, and Output — the one
// field that differs — was consumed by the re-pack and thrown away.
//
// So the caller checks it here. This asks ONLY for the explicit "I looked and found nothing"
// signal the prompt already requests; it does not grade the answer.
func VerdictReadsClean(output string) bool {
	t := strings.TrimSpace(output)
	switch {
	case t == "":
		return false // the traced broken-run shape: the seat said nothing at all
	case noneVerdictRe.MatchString(t):
		return true // the token the prompt asks for
	default:
		// A seat that wrote a sentence instead of the token still reviewed something.
		return len(t) >= minCleanVerdictChars
	}
}

// capFindings resolves the caller's cap: unset or over the ceiling means DefaultMaxFindings,
// because that is all the seat was asked to produce.
func capFindings(max int) int {
	if max <= 0 || max > DefaultMaxFindings {
		return DefaultMaxFindings
	}
	return max
}

var sevRank = map[string]int{"severe": 0, "moderate": 1, "minor": 2}

// rankFindings orders severe-first and applies the caller's cap. A cap of 0 means no cap.
// An unrecognised severity sorts LAST rather than first: a bare map miss yields 0, which is
// severe's own rank, so a seat inventing "critical" would have outranked every real severe
// finding. SliceStable keeps input order within a rank, so the seat's own most-serious-first
// ordering survives inside each bucket.
func rankFindings(in []Finding, max int) []Finding {
	out := append([]Finding(nil), in...)
	rank := func(f Finding) int {
		if r, ok := sevRank[f.Severity]; ok {
			return r
		}
		return len(sevRank) // unknown or unstated: after everything named
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// listMarkers are the bullets a seat prefixes a list with despite being asked for bare
// lines. Stripped so "- severe | ..." parses as a severity rather than as free text.
const listMarkers = "-*•‣— \t"

// ParseFindings reads the seat's lines back into Findings, tolerantly.
//
// The governing rule is that NOTHING is dropped for being badly formatted: a line the seat
// wrote in its own shape survives as an unranked claim (Severity ""), because discarding it
// would turn a reviewer that did work into an empty findings list — a clean bill of health
// nobody issued. Only a blank line and the literal NONE (the "no defects" answer the prompt
// asks for) are dropped.
func ParseFindings(lines []string) []Finding {
	out := make([]Finding, 0, len(lines))
	for _, raw := range lines {
		s := normalizeLine(raw)
		if s == "" || strings.EqualFold(strings.TrimRight(s, "."), "none") {
			continue
		}
		parts := strings.Split(s, "|")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		f := Finding{}
		sev := strings.ToLower(strings.Trim(parts[0], " \t*_`"))
		switch {
		case len(parts) > 1 && isKnownSeverity(sev):
			f.Severity = sev
			parts = parts[1:]
		case len(parts) > 1 && !looksLikePath(parts[0]) && looksLikePath(parts[1]):
			// An UNRECOGNISED label sitting in the severity slot — "critical", "high",
			// "blocker", "P0". Small seats drift to these routinely, and leaving the
			// slot unconsumed used to SHRED the line: looksLikePath rejected the label
			// too, so it became the Claim and the real claim, path and why were rejoined
			// into Why. Worse, File came out empty, so Ground skipped the wreckage
			// (it only judges findings that name a file) and it reached the caller
			// uncounted, looking like a normal finding — breaking this function's own
			// promise that a badly formatted line survives as an unranked claim.
			//
			// The label is kept rather than discarded: rankFindings sorts any unknown
			// severity last, so it costs nothing and tells the reader what the seat
			// actually said. The next field being path-shaped is what makes this a
			// severity slot rather than a guess.
			f.Severity = sev
			parts = parts[1:]
		}
		if len(parts) > 1 && looksLikePath(parts[0]) {
			f.File, f.Line = splitFileLine(parts[0])
			parts = parts[1:]
		}
		// Whatever is left leads the claim. A two-field line ("severe | run.go:5", a shape
		// Run 1 of the live exercise actually emitted) lands here with the path still in
		// parts[0]: it becomes the Claim, with File empty. That is deliberate — the text is
		// preserved verbatim rather than half-parsed into a File the seat never confirmed.
		f.Claim = parts[0]
		if len(parts) > 1 {
			// Everything after the claim is the why, rejoined: a seat that used a pipe
			// inside its explanation should not lose the tail of it.
			f.Why = strings.TrimSpace(strings.Join(parts[1:], " | "))
		}
		out = append(out, f)
	}
	return out
}

// normalizeLine strips the decoration a seat adds around a line it was told to write bare:
// surrounding space, a bullet, an ordinal, wrapping backticks. Shared by ParseFindings and
// dropTemplateEchoes so the echo guard and the parser can never disagree about what a line
// "is" — a guard that normalises differently from the parser it protects is a guard that
// misses.
func normalizeLine(s string) string {
	s = strings.TrimLeft(strings.TrimSpace(s), listMarkers)
	// A numbered list ("1. severe | ...") — strip the ordinal, not a real digit-led claim,
	// so the dot/paren is required.
	if i := strings.IndexAny(s, ".)"); i > 0 && i <= 3 && isDigits(s[:i]) {
		s = strings.TrimSpace(s[i+1:])
	}
	return strings.TrimSpace(strings.Trim(s, "`"))
}

func isKnownSeverity(s string) bool { _, ok := sevRank[s]; return ok }

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// looksLikePath reports whether a field reads as a file reference rather than prose: no
// spaces, and it carries a separator or an extension. Conservative on purpose — misreading
// a short claim as a filename would move the claim into File and lose it.
func looksLikePath(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t") {
		return false
	}
	return strings.ContainsAny(s, "/\\") || strings.Contains(s, ".")
}

// splitFileLine separates "internal/run.go:42" into its path and line. A trailing ":N" is
// only taken as a line number when N is digits — a Windows drive letter or a bare path with
// no line stays intact.
func splitFileLine(s string) (string, int) {
	s = strings.Trim(s, "`()[]")
	if i := strings.LastIndex(s, ":"); i > 0 && isDigits(s[i+1:]) {
		n, err := strconv.Atoi(s[i+1:])
		if err == nil {
			return s[:i], n
		}
	}
	return s, 0
}

// FilesInDiff returns the BASENAMES of the files a unified diff touches, lowercased.
//
// Basenames rather than full paths, because the seat is quoting a path it read out of the
// diff and may quote it rooted differently (`b/internal/run.go`, `internal/run.go`, or bare
// `run.go`) — matching on the base name grounds all three. That is deliberately the LENIENT
// direction: this set only ever decides what to DROP, so being generous costs a little
// noise while being strict would delete real findings.
func FilesInDiff(diff string) map[string]bool {
	files := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if i := strings.IndexByte(p, '\t'); i >= 0 { // git appends a timestamp on some headers
			p = p[:i]
		}
		p = strings.Trim(p, `"`)
		if p == "" || p == "/dev/null" {
			return
		}
		p = strings.ReplaceAll(p, `\`, "/")
		p = strings.TrimPrefix(strings.TrimPrefix(p, "a/"), "b/")
		if base := path.Base(p); base != "" && base != "." && base != "/" {
			files[strings.ToLower(base)] = true
		}
	}
	for _, ln := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			if f := strings.Fields(ln); len(f) >= 4 {
				add(f[2])
				add(f[3])
			}
		case strings.HasPrefix(ln, "+++ "):
			add(ln[4:])
		case strings.HasPrefix(ln, "--- "):
			// A removed line whose own content began with "-- " also lands here. The
			// cost is one extra (harmless) name in a set that only ever widens what is
			// kept; the alternative — requiring a preceding @@ or diff header — would
			// drop real files out of a partial or hand-trimmed diff.
			add(ln[4:])
		}
	}
	return files
}

// Ground drops findings that name a file the diff never touched, returning what survived
// and how many were dropped. A finding naming NO file survives: the seat declined to point
// at a line, which is weaker evidence but not evidence of invention, and rankFindings
// already sorts an unstated severity last.
//
// Fails OPEN: an empty file set (a diff shape FilesInDiff could not read) means there is no
// grounding basis, so nothing is dropped. A grounding pass that deletes an entire review
// because it could not parse the headers would be worse than no pass at all.
func Ground(in []Finding, files map[string]bool) ([]Finding, int) {
	if len(files) == 0 {
		return in, 0
	}
	kept := make([]Finding, 0, len(in))
	dropped := 0
	for _, f := range in {
		if f.File != "" && !files[strings.ToLower(path.Base(strings.ReplaceAll(f.File, `\`, "/")))] {
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	return kept, dropped
}
