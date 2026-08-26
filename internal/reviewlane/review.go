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
// explicitly (see promptFormat) and parsing it delegator-side is the honest version of the
// same thing — and ParseFindings keeps whatever it cannot parse rather than dropping it.
var reviewOutputSchema = json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array","items":{"type":"string"}}},"required":["findings"]}`)

// promptHeader is the whole of the reviewer's instructions. It says "you have no prior
// knowledge" in as many words: the isolation is not merely a fact of how the seat is
// invoked, it is what the reviewer is asked to lean on.
const promptHeader = `You are reviewing a code diff. You have NO prior knowledge of this work — you were not
present for it, and nothing beyond this message is available to you. Judge ONLY the diff
below, against the stated task.

Report concrete defects: logic errors, off-by-one and boundary mistakes, unhandled errors,
missing edge cases, security problems, and changes that contradict the task. Do NOT comment
on style, naming, or formatting. Do NOT invent anything you cannot see in the diff — a
defect you cannot point at a changed line is not a finding.
`

// promptFormat is the line shape ParseFindings reads back, split around the finding count
// so the number the seat is asked for is DefaultMaxFindings itself and cannot drift from
// the cap the re-pack budget forces. It is stated as the whole of the answer ("nothing
// else") because the structured re-pack is an extract over this text: a preamble the seat
// adds becomes a finding-shaped string that survives as an unranked claim, which is noise
// the lead then has to triage.
const promptFormatHead = `
Answer with ONE LINE PER DEFECT, at most `

const promptFormatTail = ` lines, most serious first, in EXACTLY this format
and nothing else:

severity | file:line | claim | why

severity is one of: severe, moderate, minor. file is a path that appears in the diff. line
is the line number in the new file. claim states the defect in under 15 words; why states
the consequence in under 20 words. If the diff has no defects, answer with the single word:
NONE

TASK:
`

// buildPrompt assembles the seat's entire instruction: header, format, task, diff. Nothing
// else reaches the reviewer — no history, no file list, no prior findings.
func buildPrompt(task, diff string) string {
	var b strings.Builder
	b.Grow(len(promptHeader) + len(promptFormatHead) + len(promptFormatTail) + len(task) + len(diff) + 32)
	b.WriteString(promptHeader)
	b.WriteString(promptFormatHead)
	b.WriteString(strconv.Itoa(DefaultMaxFindings))
	b.WriteString(promptFormatTail)
	b.WriteString(task)
	b.WriteString("\n\nDIFF:\n")
	b.WriteString(diff)
	b.WriteString("\n")
	return b.String()
}

// BuildContract assembles the complete, validated contract for one clean-context review.
//
// maxFindings is accepted for symmetry with the tool's own argument but does not change the
// contract: the seat is always asked for at most DefaultMaxFindings (see that constant),
// and the caller's cap is applied delegator-side in Report. Taking it here keeps the
// argument's meaning in one place — "how many findings do I want back" — rather than
// splitting it across two layers.
func BuildContract(task, diff string, maxFindings int) (core.AgentContract, error) {
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
	_ = maxFindings // applied in Report; see the doc comment above.

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

// Report turns the seat's raw finding lines into what the caller is shown: parsed, grounded
// against the diff's own files, severity-ranked, and capped. The second return is how many
// findings named a file the diff never touched — surfaced, never silently swallowed.
func Report(lines []string, diff string, max int) ([]Finding, int) {
	kept, dropped := Ground(ParseFindings(lines), FilesInDiff(diff))
	return rankFindings(kept, capFindings(max)), dropped
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
		s := strings.TrimSpace(raw)
		s = strings.TrimLeft(s, listMarkers)
		// A numbered list ("1. severe | ...") — strip the ordinal, not a real digit-led
		// claim, so the dot/paren is required.
		if i := strings.IndexAny(s, ".)"); i > 0 && i <= 3 && isDigits(s[:i]) {
			s = strings.TrimSpace(s[i+1:])
		}
		s = strings.TrimSpace(strings.Trim(s, "`"))
		if s == "" || strings.EqualFold(strings.TrimRight(s, "."), "none") {
			continue
		}
		parts := strings.Split(s, "|")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		f := Finding{}
		if sev := strings.ToLower(strings.Trim(parts[0], " \t*_`")); len(parts) > 1 && isKnownSeverity(sev) {
			f.Severity = sev
			parts = parts[1:]
		}
		if len(parts) > 1 && looksLikePath(parts[0]) {
			f.File, f.Line = splitFileLine(parts[0])
			parts = parts[1:]
		}
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
