// Package askjob turns a one-line question plus a file list into a FULL delegation
// contract — the whole of offload_ask's contribution.
//
// Why it exists: measured organic adoption of agent_delegate is ~0, and the cause is
// arithmetic, not discipline. At the moment of deciding, authoring a contract (goal +
// context + output_schema + a non-parrot acceptance check) costs far more than opening the
// files and reading them. Three rounds of steering pressure — prose rules, a nudge hook, a
// blocking gate — moved that number not at all, because none of them changed the cost.
// This does: question + paths in, {answer, evidence} out.
//
// The hard part is acceptance, and it is the only reason this package is more than a
// struct literal. A caller-free check must still be GROUNDED — anchored to content that
// appears ONLY in the supplied files — or it becomes exactly the PARROT-PASSABLE /
// UNGROUNDED pathology delegate.LintAcceptance exists to catch, the cross-seat retry
// silently never fires, and an echoed question reads back as a verified answer. So the
// anchor is mined from the files themselves, excluded against the FULL BUILT GOAL (not
// just the question — the goal's own boilerplate carries long words, and the lint measures
// against the goal), and when no distinctive anchor survives we REFUSE with a typed error
// rather than emit a check that would pass garbage. delegate.LintAcceptance returning zero
// warnings on a realistic file is this package's real acceptance test.
package askjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// Typed refusals. Every one of them is a REFUSAL BY DESIGN: the caller gets a named
// reason and does the work itself, which is strictly better than a contract that runs and
// verifies nothing (ErrNoAnchor) or one that dies downstream inside Validate with a
// message naming neither the file nor the fix (the cap errors).
var (
	// ErrNoAnchor means no token distinctive enough to ground an acceptance check was
	// found — every candidate was too short, or already appears in the goal.
	ErrNoAnchor = errors.New("askjob: no grounded anchor found in the supplied files")
	// ErrNoPaths means there is nothing to ground against. A question with no files is
	// agent_run's shape (it searches for its own files), not this one's.
	ErrNoPaths = errors.New("askjob: at least one path is required")
	// ErrTooManyPaths / ErrContextTooLarge are the wire caps, refused HERE with the
	// numbers and the fix rather than left to Validate's post-hoc message.
	ErrTooManyPaths    = errors.New("askjob: too many paths")
	ErrContextTooLarge = errors.New("askjob: attached files exceed the context cap")
)

// anchorMinLen is the shortest token that may serve as an anchor, counted in characters.
// Below it, tokens are ordinary English (`return`, `config`, `string`) that a plausible
// wrong answer contains by accident, so a check anchored on one verifies nothing.
const anchorMinLen = 8

// identRe matches source-identifier-shaped tokens of at least anchorMinLen characters.
//
// Deliberately NOT internal/grounding's reWord: that one is `[A-Za-z][A-Za-z0-9'\-]+`,
// built for prose-vs-source value checking — it breaks snake_case at the underscore
// (`fleet_max_queue_depth` becomes four common words, none of them distinctive) and admits
// apostrophes, which no identifier has. It is also unexported, alongside every helper
// around it. What is wanted here is the opposite bias: whole identifiers, underscores
// included, nothing else.
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{` + fmt.Sprint(anchorMinLen-1) + `,}`)

// askOutputSchema is the flat {answer, evidence} shape. Flat and string-only because the
// contract's structured re-pack is grammar-constrained through gbnf.FromJSONSchema, whose
// supported subset is flat objects — and because two fields is the whole promise: the
// answer, and the lines it rests on so the caller can spot-check without re-reading.
//
// An output_schema is not optional here even though a LOCAL placement would run without
// one: it is what makes the same contract eligible for a remote seat, and it is what
// nonempty:evidence has to read.
var askOutputSchema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"},"evidence":{"type":"string"}},"required":["answer","evidence"]}`)

// BuildContract assembles a complete, validated, remote-eligible contract from a question
// and a file list. readRoot confines every read; empty means the process working dir.
//
// The ORDER matters and is the correction to the brief: the goal is built BEFORE the
// anchor is mined, because the anchor's disqualifier is the goal, not the question.
func BuildContract(question string, paths []string, readRoot string) (core.AgentContract, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return core.AgentContract{}, errors.New("askjob: question is required")
	}
	if len(paths) == 0 {
		return core.AgentContract{}, ErrNoPaths
	}
	// Counted before any I/O: an over-ask should cost nothing to refuse.
	if len(paths) > core.AgentContextMaxDocs {
		return core.AgentContract{}, fmt.Errorf("%w: %d paths exceeds the %d-doc contract cap — narrow the list, or split the question",
			ErrTooManyPaths, len(paths), core.AgentContextMaxDocs)
	}

	// One confined reader for the whole delegator (os.Root containment, 128 KiB per
	// file, an error naming the offending path) — see delegate.InlineContextPaths.
	docs, err := delegate.InlineContextPaths(paths, readRoot)
	if err != nil {
		return core.AgentContract{}, err
	}
	deCollideNames(docs)

	// Same arithmetic Validate applies (name+text over all docs), refused here so the
	// message names the real ceiling and the real total instead of arriving from two
	// layers down after the contract looked fine.
	total := 0
	for _, d := range docs {
		total += len(d.Name) + len(d.Text)
	}
	if total > core.AgentContextMaxBytes {
		return core.AgentContract{}, fmt.Errorf("%w: %d bytes over the %d-byte cap — attach fewer or smaller files",
			ErrContextTooLarge, total, core.AgentContextMaxBytes)
	}

	goal := buildGoal(question, docs)
	anchor, err := pickAnchor(goal, docs)
	if err != nil {
		return core.AgentContract{}, err
	}

	// PrepareContract mints schema_version/depth, clamps max_steps/timeout to the same
	// ceilings the wire decoder applies, and runs the full Validate — the delegator's
	// one intake, so a contract this package builds obeys exactly the rules a
	// hand-written one does. Profile is left EMPTY on purpose: it is a property of the
	// executing SEAT, resolved from that box's agent_profile (measured: 0% -> 72% on a
	// small tier), and pinning it here would override the per-tier decision.
	return delegate.PrepareContract(delegate.SubtaskSpec{
		AgentContract: core.AgentContract{
			Goal:         goal,
			Context:      docs,
			OutputSchema: askOutputSchema,
			// contains:<anchor> is the grounding check — it can only pass if the
			// answer cites something that appears solely in the files.
			// nonempty:evidence is the tool's own promise made checkable: an
			// answer with an empty evidence field defeats the spot-check the
			// caller skipped re-reading the files for.
			Acceptance: []string{"contains:" + anchor, "nonempty:evidence"},
		},
	}, readRoot)
}

// buildGoal writes the whole instruction the seat sees — it gets the goal and a directory
// of the attached docs, nothing else. The names are listed because the seat would
// otherwise spend a step on list_dir to find them, and a bounded question should not cost
// a step to discover its own inputs.
func buildGoal(question string, docs []core.ContextDoc) string {
	names := make([]string, 0, len(docs))
	for _, d := range docs {
		names = append(names, d.Name)
	}
	var b strings.Builder
	b.WriteString("Answer the QUESTION below using ONLY the attached files. Read them first. ")
	b.WriteString("Put the direct answer in \"answer\", and in \"evidence\" quote the exact lines you relied on, each with the file it came from. ")
	b.WriteString("If the attached files do not answer the question, say so plainly in \"answer\" rather than inferring.\n\n")
	b.WriteString("ATTACHED FILES: ")
	b.WriteString(strings.Join(names, ", "))
	b.WriteString("\n\nQUESTION: ")
	b.WriteString(question)
	return b.String()
}

// pickAnchor returns the anchor: the rarest sufficiently-long token that appears in the
// docs and NOWHERE in the goal, preferring identifier-shaped tokens over prose (see the
// tier comment below).
//
// Rarest-wins because a token used once is far likelier to be the specific thing the
// answer must cite than a token used everywhere. Ties break lexicographically so the same
// question over the same files is always the same contract — a non-deterministic
// acceptance check would make a re-run unfalsifiable against its predecessor.
//
// The goal exclusion is a SUBSTRING test, case-insensitive, and both halves are
// load-bearing. Substring because that is precisely what delegate.LintAcceptance's
// parrot test does (strings.Contains(c.Goal, arg)); case-insensitive because it is
// STRICTER than the lint — a token differing from a goal word only in case is still
// something a parroting model emits for free, and over-excluding costs at most a refusal
// while under-excluding ships a check that verifies nothing.
func pickAnchor(goal string, docs []core.ContextDoc) (string, error) {
	lowerGoal := strings.ToLower(goal)
	counts := map[string]int{}
	shaped := map[string]int{}
	for _, d := range docs {
		for _, m := range identRe.FindAllString(d.Text, -1) {
			if strings.Contains(lowerGoal, strings.ToLower(m)) {
				continue // already in the goal: a parrot passes it for free
			}
			counts[m]++
			if identifierShaped(m) {
				shaped[m]++
			}
		}
	}
	// Identifier-shaped tokens FIRST, and this tier is not cosmetic. Measured while
	// building this: on a realistic Go file, rarity alone picked "delegate" out of a
	// comment ("a full delegate call is 8 subtasks") — grounded, lint-clean, and
	// something no correct answer to the question would ever cite. The brief asks for
	// the rarest sufficiently-long IDENTIFIER, and a prose word inside a comment is not
	// one. The wider pool stays as a FALLBACK because attaching prose (markdown, a log,
	// a spec) is a legitimate use of this tool, and refusing every such file outright
	// would be a worse failure than a weaker anchor.
	if len(shaped) > 0 {
		counts = shaped
	}
	if len(counts) == 0 {
		return "", fmt.Errorf("%w: no token of %d+ characters appears in the attached files without also appearing in the question or the file names — there is nothing here a right answer could cite that a restatement of the question could not, so read the files yourself or attach one that names what you are asking about",
			ErrNoAnchor, anchorMinLen)
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] < counts[keys[j]]
		}
		return keys[i] < keys[j] // deterministic tie-break
	})
	return keys[0], nil
}

// deCollideNames makes the doc names unique IN PLACE, keeping them flat.
//
// Two attached files sharing a base name is an ordinary caller shape — config.go lives in
// most packages — and Validate rejects the pair outright, because at materialization the
// second write would shadow the first and the sub-agent would read one of them with nobody
// knowing which. So the collision is resolved here, by position: the second config.go
// becomes config-2.go. Position, not content, so the mapping is stable across runs.
//
// Uniqueness is judged the way Validate judges it — case-folded, trailing spaces and dots
// stripped — because those are the shapes that name ONE file on at least one platform the
// fleet runs on.
func deCollideNames(docs []core.ContextDoc) {
	seen := make(map[string]bool, len(docs))
	for i := range docs {
		name := docs[i].Name
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		for n := 2; seen[normalizeName(name)]; n++ {
			name = stem + "-" + strconv.Itoa(n) + ext
		}
		seen[normalizeName(name)] = true
		docs[i].Name = name
	}
}

// identifierShaped reports whether a token reads as a source IDENTIFIER rather than an
// ordinary word: it carries an underscore, a digit, or an internal capital. Deliberately
// conservative — a single leading capital is not enough, because that is also every
// sentence-initial word in a comment.
func identifierShaped(tok string) bool {
	for i, r := range tok {
		switch {
		case r == '_', r >= '0' && r <= '9':
			return true
		case i > 0 && r >= 'A' && r <= 'Z':
			return true
		}
	}
	return false
}

// normalizeName mirrors core's duplicate-detection key (agentwire.go normalizeDocName).
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimRight(name, " ."))
}
