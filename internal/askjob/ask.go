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
	// ErrNoQuestion means the caller sent no question. Typed like the rest so a
	// surface can branch on it rather than string-matching a reason.
	ErrNoQuestion = errors.New("askjob: question is required")
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

// plainAnchorMinLen is the higher bar a NON-identifier token must clear to take one of the
// spare alternation slots. Identifiers keep anchorMinLen; ordinary words do not.
//
// Measured: with one bar for both pools, the handler fixture's check went from
// regex:(FleetMaxQueueDepth) to regex:(FleetMaxQueueDepth|accepted). "accepted" is eight
// characters of ordinary English, so a wrong answer — "requests are accepted until the
// cap" — passes the grounding branch without having read anything. Easing the check eases
// it for an UNCITED answer too, which is the verified:true-beside-nothing pathology from
// the other direction.
//
// Twelve because English word frequency falls off a cliff around there: `accepted`,
// `returned`, `different`, `following` are out, while a word long enough to survive
// (`provisioning`, `reconciliation`, `authentication`) is a domain term an answer only
// produces by reading the source. See docNameStems for the second, narrower door — length
// alone would also have excluded `buildinfo`, which is nine characters and the very case
// the top-up exists to serve.
const plainAnchorMinLen = 12

// anchorMaxLen is the longest token that can still be a NAME rather than a blob. identRe
// has no upper bound and identifierShaped answers yes to anything carrying a digit, so a
// lockfile hash, a checksum table, an integrity map, a minified bundle or a data URI puts
// 40-to-500-character strings into the pool as "identifiers". At count 1 the tie-break can
// seat one, and the result is an acceptance check hundreds of characters long that no
// answer will ever cite: a guaranteed false verified:false and an absurd `acceptance` field
// in the response. Reachable from an ordinary input — a config directory holding a
// lockfile — so it is bounded here rather than left to luck. Beyond this length it is a
// blob, not a name.
const anchorMaxLen = 40

// identRe matches source-identifier-shaped tokens of at least anchorMinLen characters.
//
// Deliberately NOT internal/grounding's reWord: that one is `[A-Za-z][A-Za-z0-9'\-]+`,
// built for prose-vs-source value checking — it breaks snake_case at the underscore
// (`fleet_max_queue_depth` becomes four common words, none of them distinctive) and admits
// apostrophes, which no identifier has. It is also unexported, alongside every helper
// around it. What is wanted here is the opposite bias: whole identifiers, underscores
// included, nothing else.
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{` + strconv.Itoa(anchorMinLen-1) + `,}`)

// anchorAlternatives is how many mined tokens ride the grounding check.
//
// Betting the whole check on ONE token was measured wrong three separate ways (see
// pickAnchors): the single highest-ranked token is frequently something a correct answer
// had no reason to quote, and with no cross-seat retry on this lane a false negative is
// terminal — the caller simply learns not to trust the verdict, which is the exact failure
// offload_ask exists to prevent. Three alternatives turn the check into the question
// actually worth asking ("did the answer cite ANYTHING that appears only in these files?")
// while staying ONE content check, still grounded, still non-parrot, and still deterministic.
const anchorAlternatives = 3

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
		return core.AgentContract{}, ErrNoQuestion
	}
	if len(paths) == 0 {
		return core.AgentContract{}, ErrNoPaths
	}
	// Deduped BEFORE the cap so a repeat spends neither a doc slot nor context bytes: the
	// same path twice is one document, and inlining it twice would double-charge the
	// 256 KiB ceiling and hand the seat the same file under two names.
	paths = dedupePaths(paths, readRoot)
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

	goal := buildGoal(question, len(docs))
	anchors, err := pickAnchors(goal, docs)
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
			// The regex is the grounding check — it can only pass if the answer
			// cites one of a handful of tokens that appear solely in the files.
			// nonempty:evidence is the tool's own promise made checkable: an
			// answer with an empty evidence field defeats the spot-check the
			// caller skipped re-reading the files for.
			Acceptance: []string{groundingCheck(anchors), "nonempty:evidence"},
		},
	}, readRoot)
}

// buildGoal writes the whole instruction the seat sees — it gets this goal and a directory
// holding exactly the attached docs, nothing else.
//
// The doc BASENAMES are deliberately NOT listed. Naming them saved the seat one list_dir
// step, and cost far more than it saved: every basename lands in the goal, and the goal is
// the anchor's disqualifier, so every identifier that is a substring of a filename stops
// being a citable anchor. Measured on the first live run — attaching buildinfo.go removed
// `buildinfo` from the pool, leaving only tokens the right answer never touched. The count
// is given instead, which steers the seat to read all of them without spending candidates.
func buildGoal(question string, docCount int) string {
	var b strings.Builder
	b.WriteString("Answer the QUESTION below using ONLY the attached files. ")
	b.WriteString("They are the only files you can read: list the directory, then read all ")
	b.WriteString(strconv.Itoa(docCount))
	b.WriteString(" of them before answering. ")
	b.WriteString("Put the direct answer in \"answer\", and in \"evidence\" quote the exact lines you relied on, each with the file it came from. ")
	b.WriteString("If the attached files do not answer the question, say so plainly in \"answer\" rather than inferring.\n\nQUESTION: ")
	b.WriteString(question)
	return b.String()
}

// groundingCheck renders the mined anchors as ONE regex acceptance check. It is only ever
// reached with a non-empty set: pickAnchors returns ErrNoAnchor instead of an empty slice,
// and BuildContract returns on that error two lines earlier — which matters, because
// "regex:()" would compile to a pattern matching everything and pass silently.
//
// regex: rather than contains: because the DSL has no "any of these" verb and a second
// contains: would be a second check the answer must ALSO satisfy — the opposite of what is
// wanted. The alternatives are QuoteMeta'd: identRe cannot currently emit a metacharacter,
// so this is defence against a future widening of the token shape rather than a live bug.
func groundingCheck(anchors []string) string {
	alts := make([]string, 0, len(anchors))
	for _, a := range anchors {
		alts = append(alts, regexp.QuoteMeta(a))
	}
	return "regex:(" + strings.Join(alts, "|") + ")"
}

// dedupePaths drops repeats while preserving caller order.
//
// Keyed on the path RE-ROOTED the way InlineContextPaths re-roots it, because a caller may
// legitimately name one file either way and "\\abs\\cfg.go" beside "./cfg.go" is one
// document, not two. Left undetected it produced exactly the harm the doc-name de-collision
// exists to prevent, only worse: cfg.go AND cfg-2.go, the same bytes handed to the seat
// twice under two names, charged twice against the 256 KiB ceiling.
//
// Cleaned but NOT case-folded: on Linux, Config.go and config.go are two different files,
// and folding would silently drop one the caller asked for. A path that cannot be re-rooted
// keeps its own cleaned form as the key — an unresolvable path must never be dropped here;
// InlineContextPaths is what refuses it, with a message naming it.
func dedupePaths(paths []string, readRoot string) []string {
	absRoot, err := filepath.Abs(readRoot) // "" resolves to the process working dir
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		key := p
		if err == nil && filepath.IsAbs(p) {
			if rel, rerr := filepath.Rel(absRoot, p); rerr == nil {
				key = rel
			}
		}
		key = filepath.Clean(key)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// pickAnchors returns up to anchorAlternatives tokens that appear in the docs and NOWHERE
// in the goal, MOST FREQUENT first, filling the slots with identifier-shaped tokens before
// topping up from plain ones (see the tier comment below).
//
// Most-frequent-wins, and the inversion is the whole point. The original rule was
// rarest-wins, on the reasoning that a token used once is likelier to be the specific thing
// the answer must cite. Measurement said the opposite, three independent times: on a
// realistic Go file it chose `delegate` out of a comment; on the first live run over
// buildinfo.go it chose `EncodeToString` and a correct, well-cited answer read
// verified:false; and reproduced on this package's own fixture the pool was
// {ErrQueueSaturated:3, defaultMaxQueueDepth:3, dispatchRetryBackoff:1} and rarity picked
// the retry-backoff constant while the two tokens the answer must quote both LOST for being
// more frequent. Within one file, centrality and frequency correlate — a name the file
// repeats is a name the file is ABOUT — so ranking by rarity ranks away from what a right
// answer will quote. Ties break lexicographically so the same question over the same files
// is always the same contract; a non-deterministic acceptance check would make a re-run
// unfalsifiable against its predecessor.
//
// Nothing about grounding or anti-parrot moves with the inversion: the goal exclusion still
// runs BEFORE ranking, so every candidate is still present in the docs and still absent
// from the goal, whatever order they come out in. The generic-boilerplate hazard the
// inversion could in principle introduce is closed twice over — the 8-character bound
// already excludes `error`, `string`, `import`, `package`, and the alternation means one
// weak alternative cannot fail a right answer on its own.
//
// The goal exclusion is a SUBSTRING test, case-insensitive, and both halves are
// load-bearing. Substring because that is precisely what delegate.LintAcceptance's
// parrot test does (strings.Contains(c.Goal, arg)); case-insensitive because it is
// STRICTER than the lint — a token differing from a goal word only in case is still
// something a parroting model emits for free, and over-excluding costs at most a refusal
// while under-excluding ships a check that verifies nothing.
func pickAnchors(goal string, docs []core.ContextDoc) ([]string, error) {
	lowerGoal := strings.ToLower(goal)
	stems := docNameStems(docs)
	shaped := map[string]int{} // identifier-shaped: fills the slots first
	plain := map[string]int{}  // everything else: tops up whatever is left over
	for _, d := range docs {
		for _, m := range identRe.FindAllString(d.Text, -1) {
			if len(m) > anchorMaxLen {
				continue // a blob, not a name (see anchorMaxLen)
			}
			if strings.Contains(lowerGoal, strings.ToLower(m)) {
				continue // already in the goal: a parrot passes it for free
			}
			if identifierShaped(m) {
				shaped[m]++
				continue
			}
			// An ordinary word needs more than length 8 to be worth citing: either it
			// is long enough to be a domain term, or it NAMES one of the attached
			// files, which makes it a proper noun of this corpus.
			if len(m) >= plainAnchorMinLen || stems[strings.ToLower(m)] {
				plain[m]++
			}
		}
	}
	if len(shaped) == 0 && len(plain) == 0 {
		return nil, fmt.Errorf("%w: nothing in the attached files is distinctive enough to anchor a check (identifiers need %d-%d characters, ordinary words %d unless they name one of the files) that does not also appear in the goal — your question plus the instructions the harness adds around it. There is nothing here a right answer could cite that a restatement of the question could not, so read the files yourself, or attach a file that names what you are asking about",
			ErrNoAnchor, anchorMinLen, anchorMaxLen, plainAnchorMinLen)
	}
	// Identifier-shaped tokens FILL the slots first, and the tier is not cosmetic:
	// measured while building this, ranking alone picked "delegate" out of a comment
	// ("a full delegate call is 8 subtasks") — grounded, lint-clean, and something no
	// correct answer to the question would ever cite. A prose word inside a comment is
	// not an identifier.
	//
	// But the tier TOPS UP rather than REPLACES. When fewer than anchorAlternatives
	// identifiers survive, the spare slots go to the best plain tokens instead of being
	// left empty — those went through the very same goal exclusion, so grounding and
	// anti-parrot are untouched. Replacing the pool outright silently dropped good
	// candidates: `buildinfo` is nine characters and not question-named, yet it never
	// reached the pool on the live run purely because buildinfo.go had shaped tokens.
	//
	// A spare slot is another branch of an OR, and that cuts BOTH ways — it eases passing
	// for a citing answer AND for an uncited one. It was first justified here as "can only
	// ease passing, never block it", which is only half the story and is why a plain
	// candidate must clear plainAnchorMinLen (or name one of the files) before it is
	// allowed anywhere near a slot.
	picked := rankByCentrality(shaped)
	if len(picked) > anchorAlternatives {
		picked = picked[:anchorAlternatives]
	}
	if spare := anchorAlternatives - len(picked); spare > 0 {
		topUp := rankByCentrality(plain)
		if len(topUp) > spare {
			topUp = topUp[:spare]
		}
		picked = append(picked, topUp...)
	}
	return picked, nil
}

// rankByCentrality orders one pool MOST FREQUENT first, ties broken lexicographically so
// the same question over the same files is always the same contract.
func rankByCentrality(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]] // MOST frequent first
		}
		return keys[i] < keys[j] // deterministic tie-break
	})
	return keys
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

// docNameStems is the set of attached file names with their extension dropped, lowercased.
//
// A token matching one is a proper noun of THIS corpus — `buildinfo` for buildinfo.go — and
// is exactly what a citing answer reaches for ("in package buildinfo", "buildinfo.go line
// 31"), which is why it earns a spare slot at a length ordinary words do not. The base
// anchorMinLen still applies, so a short stem like `cfg` never qualifies. Safe against a
// stem that happens to be a common word: it would be a file the CALLER chose to attach and
// name, so an answer citing this corpus would plausibly say it.
func docNameStems(docs []core.ContextDoc) map[string]bool {
	stems := make(map[string]bool, len(docs))
	for _, d := range docs {
		stem := strings.TrimSuffix(d.Name, filepath.Ext(d.Name))
		if stem != "" {
			stems[strings.ToLower(stem)] = true
		}
	}
	return stems
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
