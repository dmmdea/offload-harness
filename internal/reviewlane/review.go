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

	// Also holds the claims of other findings that cited this same file and line and were
	// folded into this one (MergeSameLine). Additive and omitted when nothing was folded, so a
	// result with no stacked line is byte-identical to what it was before the field existed.
	Also []string `json:"also,omitempty"`
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
// five counts that say what is NOT in that list.
//
// The counts are not telemetry. A short or empty findings list is the shape a reader most
// easily misreads, and each count means something different about WHY it is short:
// DroppedUngrounded says the seat named a file the diff does not touch (it invented a path),
// DroppedEcho says it handed the prompt's own template back instead of reviewing,
// DroppedHollow says it wrote lines with no structure at all — no severity, no file, no why,
// only a claim (the 2026-10-09 report: four of them, each merely restating the diff),
// DroppedDuplicate says the same defect was reported more than once — or the same file and
// line was cited more than once in different words, in which case the extra claims ride on the
// kept finding in Also — and TruncatedByCap says more was found than the caller asked to see.
// Counting one and swallowing the others would make the published list quietly unreadable —
// the same reason dropped-but-uncounted was wrong in the first place.
type Result struct {
	Findings          []Finding
	DroppedUngrounded int
	DroppedEcho       int
	DroppedHollow     int
	DroppedDuplicate  int
	TruncatedByCap    int
}

// Filtered is how many of the seat's lines the filters that can EMPTY a list took: hollow,
// ungrounded and echoed. A duplicate always leaves its survivor and the cap keeps at least one,
// so those two never count here. Non-zero on an empty Findings means the seat DID write
// finding-shaped lines and the lane discarded every one.
func (r Result) Filtered() int { return r.DroppedHollow + r.DroppedUngrounded + r.DroppedEcho }

// Report turns the seat's raw finding lines into what the caller is shown: template echoes
// removed, parsed, hollow lines removed, grounded against the diff's own files,
// deduplicated, same-line restatements folded together, severity-ranked, capped — with a
// count for each of the five ways a line can fail to appear as its own finding.
//
// Dedupe and MergeSameLine run BEFORE capFindings on purpose (register D-90): the seat
// routinely restates the same defect — once per hunk it touches, once plainly and once with
// the file:line it already named repeated inside the claim text, or in three different words
// at one line — and applying the cap first would let those restatements of ONE finding crowd
// a genuinely different finding out of the published list. That is the same failure class
// TruncatedByCap's own doc names for an uncounted drop, just reached from the other side: a
// cap that counts what it hides but still hides the wrong thing because duplicates padded the
// queue ahead of it.
func Report(lines []string, diff string, max int) Result {
	lines, echoed := dropTemplateEchoes(lines)
	// Hollow lines go before grounding so neither filter ever sees the other's: a hollow line
	// names no file, and Ground only judges findings that do.
	parsed, hollow := DropHollow(ParseFindings(lines))
	kept, ungrounded := Ground(parsed, FilesInDiff(diff))
	deduped, duplicate := Dedupe(kept)
	merged, folded := MergeSameLine(deduped)
	ranked := rankFindings(merged, capFindings(max))
	return Result{
		Findings:          ranked,
		DroppedUngrounded: ungrounded,
		DroppedEcho:       echoed,
		DroppedHollow:     hollow,
		// One count for both ways a finding is absorbed into another: a restatement of the
		// same claim (Dedupe) and a different claim at the same file:line (MergeSameLine).
		DroppedDuplicate: duplicate + folded,
		// rankFindings reorders and truncates and does nothing else, so the difference
		// between what went in (post-merge) and what came out IS the cap's doing.
		TruncatedByCap: len(merged) - len(ranked),
	}
}

// claimFileLinePrefixRe matches a leading "<path>:<line>" (optionally followed by a colon)
// that a seat sometimes prepends to Claim itself — ParseFindings's own doc notes the two-field
// shape ("severe | run.go:5") that leaves the whole "run.go:5" sitting in Claim because there
// was no third field to hold it. Left in place, that text would make an otherwise identical
// finding fail to match its properly-parsed sibling on claim text alone.
var claimFileLinePrefixRe = regexp.MustCompile(`^(\S+):([0-9]+):?\s+`)

// claimPunctRe strips everything but letters, digits and whitespace, so two claims that differ
// only in a stray period, backtick or dash normalise to the same key.
var claimPunctRe = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)

// normalizeClaim canonicalises a Claim for duplicate comparison: lowercase, whitespace
// collapsed to single spaces, punctuation stripped, and a leading file:line prefix removed
// when the leading token actually looks like a path (looksLikePath) rather than an ordinary
// word that happens to contain a colon and a number ("step 2: ...").
func normalizeClaim(claim string) string {
	c := strings.TrimSpace(claim)
	if m := claimFileLinePrefixRe.FindStringSubmatch(c); m != nil && looksLikePath(m[1]) {
		c = strings.TrimSpace(c[len(m[0]):])
	}
	c = strings.ToLower(c)
	c = claimPunctRe.ReplaceAllString(c, " ")
	return strings.Join(strings.Fields(c), " ")
}

// baseFileKey normalises a finding's File field the way Ground and Dedupe both compare on:
// lowercase, Windows separators folded to '/', and only the base name — the seat is quoting a
// path it read out of the diff and may root it differently (`b/internal/run.go`,
// `internal/run.go`, bare `run.go`), and all three name the same file.
func baseFileKey(file string) string {
	return strings.ToLower(path.Base(strings.ReplaceAll(file, `\`, "/")))
}

// Dedupe collapses repeated findings that name the same defect more than once — the seat
// re-describing it a line or two off, with different punctuation, or with the file:line it
// already reported repeated inside the claim text — into one, keeping the strongest report and
// counting the rest as DroppedDuplicate. See Report's comment for why this runs before the cap.
//
// The key is (file, normalised claim); within that key, lines merge using a CHAINED ±2
// tolerance rather than exact equality — sorted ascending, a gap of more than 2 between
// consecutive lines starts a new cluster — because a seat citing the same defect one line off
// is not a second defect. Within a cluster the most severe report wins; a severity tie keeps
// whichever occurrence came first in the seat's own answer (lowest original index), so the
// surviving finding never depends on map or sort iteration order. A claim worded differently
// has a different key and is not this function's to merge; when it cites the same file and
// line it is MergeSameLine's.
func Dedupe(in []Finding) ([]Finding, int) {
	type item struct {
		idx  int
		f    Finding
		line int
	}
	groups := map[string][]item{}
	order := make([]string, 0, len(in))
	for i, f := range in {
		key := baseFileKey(f.File) + "\x00" + normalizeClaim(f.Claim)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], item{idx: i, f: f, line: f.Line})
	}

	keep := make(map[int]bool, len(in))
	dropped := 0
	resolveCluster := func(cluster []item) {
		best := cluster[0]
		bestRank := severityRank(best.f.Severity)
		for _, it := range cluster[1:] {
			r := severityRank(it.f.Severity)
			if r < bestRank || (r == bestRank && it.idx < best.idx) {
				best, bestRank = it, r
			}
		}
		keep[best.idx] = true
		dropped += len(cluster) - 1
	}
	for _, key := range order {
		items := groups[key]
		sort.SliceStable(items, func(a, b int) bool { return items[a].line < items[b].line })
		start := 0
		for i := 1; i < len(items); i++ {
			if items[i].line-items[i-1].line > 2 {
				resolveCluster(items[start:i])
				start = i
			}
		}
		resolveCluster(items[start:])
	}

	out := make([]Finding, 0, len(keep))
	for i, f := range in {
		if keep[i] {
			out = append(out, f)
		}
	}
	return out, dropped
}

// MergeSameLine folds findings that cite the SAME file and line into one and counts the rest
// as duplicates. The kept finding is the most severe (a severity tie keeps whichever came
// first in the seat's answer, so the choice never depends on map or sort order) and the
// others' claims ride on it in Also, in the seat's order.
//
// Dedupe cannot see these: it keys on the normalised claim, so a seat that restates one
// defect in different words passes through as several findings. The live report (2026-10-09,
// a 48 KB diff) had three stacked on a single line of one file, each a rewording of the same
// issue. They are one place to look, so the caller reads that place once — but nothing the
// reviewer said is thrown away, because what differs between them is exactly the wording, and
// a line can also hold two genuinely different defects. Every claim stays readable.
//
// Findings merge only on an exact file and line. The file is compared the way Ground and Dedupe
// compare it (base name, because the seat may root a path differently), which shares their
// blind spot: two touched files with one base name and one line number would merge, and the
// claim still rides along, so nothing is lost when they do. Line 0 means "the seat did not say
// where", and a finding with no line (or no file) is never merged on that alone: two findings
// that both failed to name a place are not the same place.
//
// It runs after Dedupe and before the cap, for the reason Report gives (D-90): folding a stack
// frees slots for genuinely different findings, where capping first would let the stack crowd
// them out.
func MergeSameLine(in []Finding) ([]Finding, int) {
	type spot struct {
		file string
		line int
	}
	at := map[spot][]int{} // input indexes citing each spot, ascending
	for i, f := range in {
		if f.File == "" || f.Line <= 0 {
			continue
		}
		k := spot{baseFileKey(f.File), f.Line}
		at[k] = append(at[k], i)
	}
	folded := map[int]bool{}   // indexes absorbed into another finding
	also := map[int][]string{} // kept index -> the claims it absorbed, in the seat's order
	dropped := 0
	for _, idx := range at {
		if len(idx) < 2 {
			continue
		}
		keep := idx[0]
		for _, i := range idx[1:] {
			// Strictly more severe only: idx is ascending, so an equal rank keeps the earlier.
			if severityRank(in[i].Severity) < severityRank(in[keep].Severity) {
				keep = i
			}
		}
		for _, i := range idx {
			if i == keep {
				continue
			}
			folded[i] = true
			dropped++
			if c := strings.TrimSpace(in[i].Claim); c != "" {
				also[keep] = append(also[keep], c)
			}
		}
	}
	out := make([]Finding, 0, len(in)-dropped)
	for i, f := range in {
		if folded[i] {
			continue
		}
		if extra := also[i]; len(extra) > 0 {
			f.Also = append(append([]string(nil), f.Also...), extra...)
		}
		out = append(out, f)
	}
	return out, dropped
}

// DropHollow removes findings that carry no structure at all — no known severity, no file
// and no why, only a claim — and counts them as DroppedHollow.
//
// It exists because ParseFindings keeps every line it cannot read, and a small seat that
// ignores the line format entirely produces nothing BUT such lines. The live report (2026-10-09,
// a 228-line diff on a small seat) was four findings with severity "", file "", line 0 and why "",
// each claim merely restating the diff, published as a successful review. A hollow line names
// no place to look, no consequence and no rank, so it cannot be triaged; it is the lane's version
// of an echo — text shaped like a finding that reviews nothing.
//
// Dropping is safe only because it is COUNTED and because an emptied list now defers instead of
// publishing (publishReview: a list emptied by filtering is not a review). That pair is what
// ParseFindings' old "nothing is dropped for being badly formatted" rule was protecting — a
// clean bill of health nobody issued — and it holds for every line that carries ANY structure:
// a known severity, a file or a why still survives however badly the rest is formatted. An
// unrecognised label alone ("critical | run.go") is not structure; it ranks last by design and
// is exactly the claim-only shape this drops.
func DropHollow(in []Finding) ([]Finding, int) {
	out := make([]Finding, 0, len(in))
	dropped := 0
	for _, f := range in {
		if !isKnownSeverity(f.Severity) && f.File == "" && f.Why == "" {
			dropped++
			continue
		}
		out = append(out, f)
	}
	return out, dropped
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

// noneLineRe and noDefectRe are the two AFFIRMATIVE shapes of "I looked and found nothing":
// the bare NONE token promptFormatTail asks for, standing alone on its own line, or an
// explicit no-defects statement.
//
// Both were tightened after the lane REVIEWED ITS OWN DIFF and flagged the first version:
//
//   - the token test was `\bnone\b` anywhere in the answer, so "I tried but none of the
//     tools worked" read as a clean verdict. It is now anchored to a whole line;
//   - the fallback was a length test — any answer of 16+ characters. The seat's own finding:
//     "I could not read the diff" is 25 characters and would have published an empty findings
//     list instead of deferring. Verified by running the function on that exact string.
//
// Length was never affirmative evidence of anything; it only said the seat had said SOMETHING.
// This gate exists because the dangerous failure is a broken run reading as clean, so it now
// requires a positive signal and defers on everything else. That direction costs a re-read
// when a seat phrases a clean verdict some third way; the other direction costs a false
// all-clear on work nobody reviewed.
var (
	noneLineRe = regexp.MustCompile(`(?im)^\W*none\W*$`)
	noDefectRe = regexp.MustCompile(`(?i)\bno\s+(defects?|issues?|problems?|bugs?|findings?|errors?)\b`)
)

// VerdictReadsClean reports whether the seat's OWN raw answer supports publishing an empty
// findings list as a genuine clean review rather than as a broken run.
//
// This closes the hole that made the two indistinguishable. The traced path (until 0.115.8):
// agent/loop.go returned stop_reason "done" the moment the model stopped requesting tools,
// with no check that the final message had any CONTENT — and empty content is live-measured
// in this codebase (a thinking seat spending its whole budget in the think block).
// agenttask.go special-cased only "budget", so "done" with an empty Output reached
// repackStructured, which extracted findings from an empty string and returned a
// schema-valid {"findings":[]}. Since 0.115.8 the loop names that stop (reasoning_starved /
// empty) and the node defers it before any re-pack; this check remains the last line for a
// seat that produced text which re-packed to nothing.
//
// So the caller checks it here. This asks ONLY for the explicit "I looked and found nothing"
// signal the prompt already requests; it does not grade the answer.
func VerdictReadsClean(output string) bool {
	t := strings.TrimSpace(output)
	if t == "" {
		return false // the traced broken-run shape: the seat said nothing at all
	}
	// An affirmative verdict, or nothing. A seat that reports a FAILURE ("I could not read
	// the diff") must land here as not-clean, which is why neither branch is a length test.
	return noneLineRe.MatchString(t) || noDefectRe.MatchString(t)
}

// What cut the structuring step short, as published in a result's `salvaged` field.
const (
	// SalvagedOutputTruncated: the seat's final answer ended on the completion budget, so what
	// it wrote is a PARTIAL list. Its complete lines are read; the one cut mid-way is dropped.
	SalvagedOutputTruncated = "output_truncated"
	// SalvagedWall: the loop finished and the clock ended the structured re-pack — skipped for
	// lack of wall, or clamped and cut by the time left. The answer is complete.
	SalvagedWall = "wall"
	// SalvagedRepackFlattened: the re-pack ran and returned, but it had flattened the findings
	// (a bare sentence apiece, or one pipe line split into its fields) and the seat's own answer
	// held strictly more findings once read. See RawLines.
	SalvagedRepackFlattened = "repack_flattened"
)

// Salvage reads a DEFERRED agent result whose seat had already written the review and was
// stopped only by the clock on the structuring step, and returns the lines to run through
// Report. kind is "" for every other result, which the caller handles exactly as it always did.
//
// It answers a measured waste (the harness ledger, the 26 hours to 2026-10-09): of five review
// defers, three read "output failed schema: re-pack skipped: the final answer was cut at the
// completion budget" and a fourth "structured re-pack skipped: 0 s left to the wall", on a 27B
// seat and on a 9B one. In every case the seat had written review lines and the lane threw them
// away. The re-pack is a convenience for this lane, not a requirement: the answer format is
// LINE-oriented, ParseFindings already reads raw lines, and the schema's one field is those
// same lines split on newlines. So an answer the re-pack could not reach is still readable, and
// each line then passes through every filter a structured one does (echo, hollow, grounding,
// dedupe, the cap). The raw text itself is still never published, only what survives them.
//
// It keys on structure, not prose: OutputTruncated (the node's own flag for a cut final) and
// SchemaMiss with the budget class (the node's flag for a finished answer whose structuring the
// CLOCK ended: a re-pack skipped, clamped or cut by the time left; a canceled one is excluded,
// because nobody is waiting for it). Everything else stays a defer: a gpu-busy or other
// capacity defer, a seat or stack failure (infrastructure), a re-pack that answered the wrong
// shape (abstention), and any defer that holds no answer. Those are the seat or the stack
// failing, not the clock cutting short an answer that was fine.
//
// A cut answer's last line is dropped. The text after its final newline is where the budget
// stopped the seat, so it is a fragment ("severe | run.go:5 | off-by-one in the l"), and a
// fragment can parse into a plausible finding with the wrong claim. A finished answer keeps
// every line.
func Salvage(w core.AgentWireResult) (kind string, lines []string) {
	if !w.Deferred || len(w.Structured) != 0 || strings.TrimSpace(w.Output) == "" {
		return "", nil
	}
	switch {
	case w.OutputTruncated:
		return SalvagedOutputTruncated, answerLines(w.Output, true)
	case w.SchemaMiss && w.DeferClass == core.DeferClassBudget && !strings.HasPrefix(w.Reason, core.RepackCanceledReason):
		// A finished answer: read it the way a delivered one is, so the shapes a seat is seen
		// to write (RawLines) are one reader's business and the two paths cannot diverge.
		return SalvagedWall, RawLines(w.Output)
	}
	return "", nil
}

// Survivors is how many findings outlived every filter, before the cap hid any: what a reading
// of the seat's answer is worth, and the number two readings of the same answer are compared on.
func (r Result) Survivors() int { return len(r.Findings) + r.TruncatedByCap }

// RawLines reads the lines of a seat's RAW final answer, the text the structured re-pack is only
// a copy of. It exists because the copy is the weak link: on 2026-10-09 three review reports came
// back as findings with every field empty but the claim, and a live probe of the same prompt on
// the same fleet seats found two different ways the re-pack gets there.
//
//   - The grammar-lane re-pack is told only `"findings" (array of strings)`, so it SPLITS lines:
//     one perfectly formed line (`severe | file:3764 | claim | why`) from a qwen3.6-35b-a3b seat
//     came back as its claim and its why in two list items, and on two mimo-9b seats five
//     well-formed lines came back as ten bare strings, no re-issue involved. The severity and the
//     location were gone. This is the ordinary path on those seats, not an edge.
//   - A mimo-9b seat repeated a line, the loop's repetition guard read that as a cut final, and
//     the cut-final re-issue told it to "return the same JSON object that was asked for". This
//     lane never asked for JSON, so the seat invented one, {"findings":[{"severity", "path",
//     "line", "claim", "why"}, ...]}, and the re-pack kept one string per object: its claim.
//
// In both, the raw answer is strictly richer than what the re-pack made of it. So the door reads
// both and keeps the richer reading (see the door's use of Survivors); the re-pack stays the
// default because it also strips a preamble, and a faithful one reads identically.
//
// Two shapes are recognised. Pipe lines, the format the prompt asks for, are the answer split on
// newlines. A JSON document (fenced or bare, {"findings":[...]} or a bare array) whose items are
// strings or objects is rendered back into pipe lines, one per item, so ParseFindings and every
// filter see exactly what they see for a seat that followed the format. The keys read are the ones
// the prompt's own placeholders spell: severity, path (or file), line, claim, why.
func RawLines(output string) []string {
	if lines := jsonFindingLines(output); len(lines) > 0 {
		return lines
	}
	return strings.Split(output, "\n")
}

// jsonFindingLines returns the findings of the JSON document a seat answered with, one pipe line
// each, or nil when the answer holds no such document. The document must open a LINE (after an
// optional fence), so a bracket inside a pipe-line claim ("indexes xs[len(xs)]") can never hijack a
// plain answer, and it must yield at least one non-empty line, so a stray array in prose reads as
// the prose it is.
func jsonFindingLines(output string) []string {
	off := 0
	for _, ln := range strings.SplitAfter(output, "\n") {
		t := strings.TrimLeft(ln, " \t")
		if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			var doc any
			if json.NewDecoder(strings.NewReader(output[off+len(ln)-len(t):])).Decode(&doc) != nil {
				return nil
			}
			var items []any
			switch d := doc.(type) {
			case map[string]any:
				items, _ = d["findings"].([]any)
			case []any:
				items = d
			}
			var lines []string
			for _, it := range items {
				var line string
				switch v := it.(type) {
				case string:
					line = v
				case map[string]any:
					line = objectLine(v)
				}
				if strings.TrimSpace(line) != "" {
					lines = append(lines, line)
				}
			}
			return lines
		}
		off += len(ln)
	}
	return nil
}

// lineSuffixRe matches a trailing ":<line>" on a path.
var lineSuffixRe = regexp.MustCompile(`:\d+$`)

// objectLine renders one finding object as the pipe line the prompt asks for, leaving out the
// parts it lacks so the line still reads in the shapes ParseFindings knows: "sev | file:line |
// claim | why", "sev | claim | why", "file:line | claim | why", or the bare claim (which is hollow
// and dropped). A pipe inside a value would shift the fields, so it is written as a slash.
func objectLine(o map[string]any) string {
	f := make(map[string]string, len(o))
	for k, v := range o {
		switch t := v.(type) {
		case string:
			f[strings.ToLower(k)] = strings.TrimSpace(t)
		case float64:
			f[strings.ToLower(k)] = strconv.Itoa(int(t))
		}
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v := f[k]; v != "" {
				return strings.TrimSpace(strings.ReplaceAll(v, "|", "/"))
			}
		}
		return ""
	}
	sev, file, claim, why := pick("severity"), pick("path", "file"), pick("claim"), pick("why")
	if line := pick("line"); file != "" && line != "" && line != "0" && !lineSuffixRe.MatchString(file) {
		file += ":" + line
	}
	var parts []string
	for _, p := range []string{sev, file, claim, why} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " | ")
}

// answerLines splits a seat's raw final answer into its lines. cut means the answer was ended
// by the completion budget, so whatever follows its last newline is a fragment and is dropped:
// the final element of the split, which is "" when the text ended on a newline (nothing lost)
// and the cut line otherwise.
func answerLines(output string, cut bool) []string {
	lines := strings.Split(output, "\n")
	if cut {
		lines = lines[:len(lines)-1]
	}
	return lines
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

// severityRank orders a severity label low-to-high (severe first). An unrecognised or unstated
// label ranks AFTER everything named rather than defaulting to 0 (severe's own rank via a bare
// map miss) — a seat inventing "critical" must not outrank a real severe finding. Shared by
// rankFindings and Dedupe so a duplicate cluster's "most severe" and the published list's
// "severe first" can never disagree about what severe means.
func severityRank(sev string) int {
	if r, ok := sevRank[sev]; ok {
		return r
	}
	return len(sevRank)
}

// rankFindings orders severe-first and applies the caller's cap. A cap of 0 means no cap.
// SliceStable keeps input order within a rank, so the seat's own most-serious-first ordering
// survives inside each bucket.
func rankFindings(in []Finding, max int) []Finding {
	out := append([]Finding(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return severityRank(out[i].Severity) < severityRank(out[j].Severity) })
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
// asks for) are dropped HERE. The rule still holds for every line that carries any structure
// (a known severity, a file or a why); the one shape it no longer protects is a bare claim,
// which Report drops through DropHollow — counted, and safe because a list emptied by
// filtering now defers instead of reading as a clean review.
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
		if f.File != "" && !files[baseFileKey(f.File)] {
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	return kept, dropped
}
