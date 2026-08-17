package pipeline

// Call identity (memory-frontier Phase 0.1).
//
// WHY THIS EXISTS
//   The ledger recorded THAT a call happened but never WHAT it was about. Every
//   downstream question the harness wants to answer from telemetry — what is the
//   real duplicate rate, does prefix reuse actually fire, would a different tier
//   have decided differently, which exemplars are ever injected — needed either a
//   bespoke throwaway script or the input text itself, and the ledger has no input
//   text at all. Measured consequence: a 34-day, 1,747-row ledger could not answer
//   any of them.
//
// WHY HASHES AND NOT CONTENT
//   Storing full prompts was considered and rejected: it creates a permanent
//   redaction and multi-brand-isolation liability on an estate that works across
//   several brands. Fingerprints give the GROUPING power (are these two calls the
//   same? did the artifacts change between them?) with none of the payload.
//
// COST
//   Pure in-memory sha256 over a few hundred bytes per call — nanoseconds-to-
//   microseconds, no IO, no parse, no lock held across anything slow. This
//   deliberately respects reload.go's rule that the request hot path adds no
//   IO/parse; hashing already-resident strings is neither.

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/exemplars"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

// cacheKeyFor builds the exact-result cache key for a text-cascade call.
//
// It exists as a NAMED function taking the artifacts (built, shots) rather than
// pre-computed strings, because cache.Key is variadic: dropping an ingredient in
// a later refactor compiles clean, passes every existing test, and silently
// reinstates the stale-answer bug this change exists to close. There is no type
// backstop otherwise — this signature is it.
//
// It also removes a live ordering hazard from the first draft, where the key was
// built from meta fields assigned a few lines earlier: grouping those assignments
// after the key computation (an entirely ordinary cleanup) would have fed empty
// strings into the key and broken nothing visibly.
func cacheKeyFor(task core.TaskType, orig, paramsKey, model string, built tasks.Built, shots []exemplars.Pair) string {
	return cache.Key(
		string(task),
		orig,
		paramsKey,
		model,
		built.Grammar,
		templateCacheTag(built.System, built.Grammar, built.User, orig),
		exemplarCacheTag(shots),
	)
}

// idHashLen bounds a recorded fingerprint. 16 hex chars = 64 bits: collision
// probability stays negligible at any ledger size this estate will reach
// (~1e-9 at a million rows), while keeping the one-line JSONL records small —
// they must stay O_APPEND-atomic-small, which is what lets a reader run while
// the MCP server writes.
const idHashLen = 16

// shortHash returns the first idHashLen hex chars of sha256(s).
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:idHashLen]
}

// inputFingerprint hashes the ORIGINAL caller input (pre-packing), so repeats are
// countable across tiers, across runs, and across machines running identical
// models. Keyed on the original rather than the entry-tier packing for the same
// reason the cache key is (TO-3): the packing is a lossy view, the original is
// the request's identity.
func inputFingerprint(orig string) string {
	if orig == "" {
		return ""
	}
	return shortHash(orig)
}

// inputPlaceholder stands in for the caller's payload when fingerprinting a
// TEMPLATE. NUL-delimited because NUL cannot occur in a Go source literal or in
// generated GBNF, so it cannot collide with real template text.
const inputPlaceholder = "\x00INPUT\x00"

// promptPrefixFingerprint hashes the REAL KV PREFIX: the system block plus the
// user preamble that precedes the caller's input.
//
// Two review findings shaped this, both of which the first draft got wrong:
//
//  1. GRAMMAR IS NOT PROMPT TEXT. It ships to llama-server as a separate JSON
//     field, so it occupies zero tokens of KV prefix. Mixing it in produced BOTH
//     false positives (triage rows collapsing into one bucket because their fixed
//     enum grammar and constant System matched, while the real prompts diverged
//     at token ~2) and false negatives (extract rows splitting on a grammar change
//     that left the prefix identical). A metric whose gate is "ship the prefix
//     change only if reuse actually fires" cannot be measured with the wrong bytes.
//
//  2. THE USER PREAMBLE *IS* THE PREFIX. internal/tasks/prefixorder_test.go
//     exists precisely to guard that preamble's reusability, and defines the
//     prefix as System + User-up-to-the-input. This mirrors that definition
//     rather than inventing a second, disagreeing one.
//
// So: two calls sharing this value really are two calls whose KV prefix the
// server could reuse.
func promptPrefixFingerprint(system, userPreamble string) string {
	if system == "" && userPreamble == "" {
		return ""
	}
	return shortHash(system + "\x00" + userPreamble)
}

// userPreambleOf returns the leading, input-independent part of the user turn —
// the repo's own prefix definition (see internal/tasks/prefixorder_test.go).
func userPreambleOf(user, input string) string {
	if input == "" {
		return user
	}
	if i := strings.Index(user, input); i >= 0 {
		return user[:i]
	}
	// The input was packed/trimmed before rendering, so it does not appear
	// verbatim. Fall back to the whole turn rather than silently reporting an
	// empty prefix, which would read as "no prefix to reuse".
	return user
}

// templateCacheTag fingerprints the FULL template that produced a prompt —
// system, grammar, and the entire user turn with the caller's input elided.
//
// This is deliberately a DIFFERENT value from promptPrefixFingerprint, because
// the two answer different questions:
//
//	promptPrefixFingerprint -> "could the server have reused a KV prefix?"
//	                           (prefix only; grammar excluded — it is not prompt text)
//	templateCacheTag        -> "is this the same template that produced the cached
//	                           answer?" (EVERYTHING that shapes the output, including
//	                           grammar and any instructions AFTER the input)
//
// Conflating them is what left the headline bug half-open in the first draft:
// for the four text tasks the instruction template lives in Built.User (e.g.
// summarize's "Provide a 1-2 sentence summary and up to %d key points"), so
// hashing only System+Grammar meant editing that line changed the prompt, did
// NOT change the key, and the cache served pre-edit answers forever.
func templateCacheTag(system, grammar, user, input string) string {
	elided := user
	if input != "" {
		elided = strings.ReplaceAll(user, input, inputPlaceholder)
	}
	if system == "" && grammar == "" && elided == "" {
		return ""
	}
	return shortHash(system + "\x00" + grammar + "\x00" + elided)
}

// maxExemplarIDs caps how many ids a row records. The ledger's one-line records
// must stay small enough for O_APPEND to be atomic — that is what lets a reader
// run lock-free while the MCP server writes (see the ledger package comment), and
// it is why Reason is capped at 120 bytes. ExemplarIDs was the only unbounded
// field on Entry; raising ExemplarShots would have grown rows past the atomic
// threshold, and a torn line is then silently DROPPED by ReadAll — the row would
// vanish from the savings ledger entirely. Caught in review.
const maxExemplarIDs = 16

// exemplarFingerprints returns a stable short id per injected exemplar, for the
// TELEMETRY histogram ("which exemplars actually fire?" — unanswerable today).
//
// Deliberately hashes the INPUT only: a stable per-example identity is what makes
// the histogram groupable across a rewrite of that example's output.
//
// ⚠ This is NOT sufficient for the cache key — see exemplarCacheTag. The two
// consumers genuinely want different hashes, and conflating them reintroduced a
// stale-cache hole in the first draft of this change.
func exemplarFingerprints(shots []exemplars.Pair) []string {
	if len(shots) == 0 {
		return nil
	}
	if len(shots) > maxExemplarIDs {
		shots = shots[:maxExemplarIDs]
	}
	ids := make([]string, 0, len(shots))
	for _, s := range shots {
		ids = append(ids, shortHash(s.Input))
	}
	return ids
}

// exemplarCacheTag fingerprints the exemplar set AS RENDERED INTO THE PROMPT —
// input AND output, in order.
//
// Why this differs from exemplarFingerprints (review finding): injectExemplars
// writes both the INPUT and the OUTPUT of every shot into the prompt, and the
// exemplar pool is auto-grown — exemplars.Append adds a fresh pair on every good
// call with no dedupe, so the same input accumulates different outputs over time
// and a later Select can pick a different one. Hashing input-only meant that
// regenerating selected.json with a BETTER answer for an existing input changed
// the prompt but not the cache key, so the cache kept serving pre-regeneration
// answers indefinitely. That is the same bug class this change exists to close,
// and the first draft reintroduced it on the exemplar axis.
//
// Order is part of the identity because injectExemplars renders them in order.
func exemplarCacheTag(shots []exemplars.Pair) string {
	if len(shots) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range shots {
		b.WriteString(s.Input)
		b.WriteByte(0)
		b.WriteString(s.Output)
		b.WriteByte(0)
	}
	return shortHash(b.String())
}

// contextHash fingerprints the DECISION ARTIFACTS in force for this call: the
// hot-reloadable learning files (router weights, thresholds, tier overrides,
// confhead + its thresholds) plus the prompt-template version.
//
// This is the field that turns every hot-reload into a free A/B. Rows split into
// buckets by which artifact set produced them, so the effect of adopting new
// router weights (or pruning exemplars, or editing a template) is attributable
// from telemetry instead of anecdotal. It also directly addresses a recorded
// injury on this estate: a config change that appeared to apply but did not, where
// the lesson was to prove a flip by IDENTITY rather than by behaviour.
//
// It reads p.learnHashes, which is SEEDED AT CONSTRUCTION (pipeline.New) and
// advanced by commitHash on every successful reload — so the value is meaningful
// from the first call, not only after the first reload tick.
func (p *Pipeline) contextHash() string {
	// Map each configured artifact path to a STABLE LOGICAL NAME.
	//
	// Review finding: hashing the absolute paths made the fingerprint
	// machine- and layout-dependent. Two boxes running byte-identical artifacts
	// produced different context hashes (C:\Users\... vs /home/...), and merely
	// relocating state_dir on one box opened a brand-new "the artifacts changed"
	// arm with zero artifact change — a documented recurring event on this estate.
	// The logical set is small and fixed, so name it explicitly.
	named := [...]struct {
		name string
		path string
	}{
		{"thresholds", p.cfg.ThresholdsPath},
		{"router", p.cfg.RouterWeightsPath},
		{"overrides", p.cfg.TierOverridesPath},
		{"confhead", p.cfg.ConfHeadPath},
		{"confhead_thresholds", p.cfg.ConfHeadThresholdsPath},
	}

	type part struct{ name, state string }
	parts := make([]part, 0, len(named))

	p.learnMu.RLock()
	for _, n := range named {
		if n.path == "" {
			continue // not configured — contributes nothing
		}
		// Three states, deliberately distinguishable (the house rule that a failed
		// probe is not proof of absence). learnHashes is advanced ONLY by
		// commitHash, which runs only after a SUCCESSFUL load — so a present but
		// corrupt artifact (loader returns nil, hash not advanced) reads as
		// "stale", not as a confident new arm. Hashing the raw file bytes instead
		// would have manufactured a fresh A/B arm for an artifact that silently
		// failed to load, inviting an analyst to attribute a behaviour change to
		// "the new weights" when the truth is "the weights are off".
		if h, ok := p.learnHashes[n.path]; ok && h != "" {
			parts = append(parts, part{n.name, "loaded:" + h})
		} else {
			parts = append(parts, part{n.name, "absent"})
		}
	}
	p.learnMu.RUnlock()

	if len(parts) == 0 {
		// "no artifacts configured" is a real, meaningful state — not missing data.
		// Returning "" would let omitempty erase the row from loupe's bucket set
		// entirely, making it indistinguishable from "not computed".
		return "none"
	}

	sort.Slice(parts, func(i, j int) bool { return parts[i].name < parts[j].name })
	var b strings.Builder
	for _, pt := range parts {
		b.WriteString(pt.name)
		b.WriteByte(0)
		b.WriteString(pt.state)
		b.WriteByte(0)
	}
	return shortHash(b.String())
}

// NOTE ON THE PROMPT TEMPLATE, deliberately NOT a hand-maintained version const.
//
// The plan called for a `TemplateVersion` constant to be added to the cache key
// and the context hash. A constant has one fatal property: it is bumped by hand,
// so the single failure it exists to prevent — someone edits a prompt and forgets
// — is exactly the failure it cannot catch. Worse, a stale constant is invisible.
//
// promptPrefixFingerprint(built.System, built.Grammar) is the same signal derived
// from the artifact itself: it changes precisely when the template changes, it
// cannot be forgotten, and it needs no upkeep. So the template identity is carried
// per-row by PromptPrefixSHA256 and mixed into the cache key directly, while
// contextHash above stays scoped to the hot-reloadable learning artifacts. No
// redundancy, no maintenance burden, and strictly stronger than the constant.
