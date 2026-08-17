package pipeline

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/exemplars"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

// baseBuilt is a stand-in for a task's assembled prompt. The literals do not
// matter; what matters is that each test mutates exactly ONE of them.
func baseBuilt() tasks.Built {
	return tasks.Built{
		System:  "You are a precise summarizer. Output ONLY a JSON object.",
		User:    "Summarize the text below. Provide a 1-2 sentence summary and up to 5 key points.\n\nTEXT:\nPAYLOAD",
		Grammar: `root ::= "{" ws "\"summary\"" ws ":" ws string "}"`,
	}
}

const baseInput = "PAYLOAD"

func keyOf(t *testing.T, b tasks.Built, shots []exemplars.Pair) string {
	t.Helper()
	return cacheKeyFor(core.TaskSummarize, baseInput, "params", "gemma-4-e4b", b, shots)
}

// TestCacheKeyBindsToTemplate is the regression guard for the bug this change
// exists to close: a prompt edit that did NOT change the cache key, so the cache
// went on serving pre-edit answers forever with no signal anywhere.
//
// It asserts the RELATION (mutate X => the key must change) rather than mirroring
// the key formula, deliberately: a test that recomputes the formula passes even
// when an ingredient is dropped from both sides.
func TestCacheKeyBindsToTemplate(t *testing.T) {
	shots := []exemplars.Pair{{Input: "ex-in-1", Output: "ex-out-1"}}
	base := keyOf(t, baseBuilt(), shots)

	// Determinism first: without it, every "differs" assertion below is vacuous.
	if again := keyOf(t, baseBuilt(), shots); again != base {
		t.Fatalf("cache key is not deterministic: %q vs %q", base, again)
	}

	cases := []struct {
		name    string
		mutate  func(*tasks.Built)
		shots   []exemplars.Pair
		because string
	}{
		{
			name:    "system prompt edited",
			mutate:  func(b *tasks.Built) { b.System += " Prefer short bullets." },
			shots:   shots,
			because: "editing a task's system prompt no longer invalidates the cache — the harness will serve pre-edit answers forever, with no signal anywhere",
		},
		{
			name:   "user instruction template edited",
			mutate: func(b *tasks.Built) { b.User = strings.Replace(b.User, "1-2 sentence", "3-4 sentence", 1) },
			shots:  shots,
			because: "the USER turn holds the real instruction template for the text tasks " +
				"(e.g. \"1-2 sentence summary\"); if it is not in the key, the most common kind of " +
				"prompt edit there is silently serves stale answers — this was the half-fixed hole caught in review",
		},
		{
			name:    "grammar changed",
			mutate:  func(b *tasks.Built) { b.Grammar += ` ws ::= " "` },
			shots:   shots,
			because: "a different grammar can yield a different output shape for the same prompt",
		},
		{
			name:    "exemplar swapped",
			mutate:  func(b *tasks.Built) {},
			shots:   []exemplars.Pair{{Input: "ex-in-2", Output: "ex-out-1"}},
			because: "a different exemplar set renders a different prompt",
		},
		{
			name:   "exemplar OUTPUT rewritten, input unchanged",
			mutate: func(b *tasks.Built) {},
			shots:  []exemplars.Pair{{Input: "ex-in-1", Output: "ex-out-REGENERATED"}},
			because: "injectExemplars writes the OUTPUT into the prompt, and the exemplar pool is " +
				"auto-grown, so regenerating selected.json with a better answer for an EXISTING input " +
				"changes the prompt; keying on exemplar inputs alone left exactly this stale-cache hole open",
		},
		{
			name:    "exemplar order reversed",
			mutate:  func(b *tasks.Built) {},
			shots:   []exemplars.Pair{{Input: "ex-in-B", Output: "o"}, {Input: "ex-in-A", Output: "o"}},
			because: "exemplars render in order, so order is part of the prompt's identity",
		},
		{
			name:    "exemplars removed entirely",
			mutate:  func(b *tasks.Built) {},
			shots:   nil,
			because: "a prompt with no exemplars is a different prompt",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := baseBuilt()
			tc.mutate(&b)
			got := keyOf(t, b, tc.shots)
			if got == base {
				t.Errorf("cache key UNCHANGED after %s.\n%s", tc.name, tc.because)
			}
		})
	}
}

// TestCacheKeySameInputsSameKey pins the other direction: the key must NOT churn
// on identical inputs, or every call is a miss and the cache is pointless.
func TestCacheKeySameInputsSameKey(t *testing.T) {
	shots := []exemplars.Pair{{Input: "a", Output: "b"}, {Input: "c", Output: "d"}}
	a := keyOf(t, baseBuilt(), shots)
	b := keyOf(t, baseBuilt(), []exemplars.Pair{{Input: "a", Output: "b"}, {Input: "c", Output: "d"}})
	if a != b {
		t.Errorf("identical inputs produced different cache keys:\n  %q\n  %q", a, b)
	}
}

// TestPromptPrefixFingerprintTracksRealPrefix guards the TELEMETRY fingerprint,
// which answers a different question from the cache key: "could the server have
// reused a KV prefix?"
//
// Grammar is deliberately NOT an input here — it ships as a separate JSON field
// and occupies zero KV prefix. Including it previously produced false positives
// (rows collapsing into one bucket while the real prompts diverged) and false
// negatives (rows splitting on a grammar change that left the prefix identical).
func TestPromptPrefixFingerprintTracksRealPrefix(t *testing.T) {
	sys := "You are a classifier."
	pre := "Classify the text into exactly one of these labels: a,b.\n\nTEXT:\n"

	base := promptPrefixFingerprint(sys, pre)
	if base == "" {
		t.Fatal("expected a fingerprint for a non-empty prefix")
	}
	if promptPrefixFingerprint(sys, pre) != base {
		t.Error("prefix fingerprint is not deterministic")
	}
	if promptPrefixFingerprint(sys, pre+" ") == base {
		t.Error("a changed user preamble must change the prefix fingerprint — it IS the KV prefix")
	}
	if promptPrefixFingerprint(sys+"!", pre) == base {
		t.Error("a changed system block must change the prefix fingerprint")
	}
	if promptPrefixFingerprint("", "") != "" {
		t.Error("an empty prefix must fingerprint to empty so omitempty can omit it")
	}
}

// TestUserPreambleOf pins the prefix extraction, including the fallback that
// matters: when the input was packed/trimmed before rendering it does not appear
// verbatim, and returning "" there would read as "no prefix to reuse".
func TestUserPreambleOf(t *testing.T) {
	if got := userPreambleOf("INSTRUCTIONS\nTEXT:\nbody", "body"); got != "INSTRUCTIONS\nTEXT:\n" {
		t.Errorf("preamble = %q, want the text before the input", got)
	}
	if got := userPreambleOf("INSTRUCTIONS only", ""); got != "INSTRUCTIONS only" {
		t.Errorf("empty input must leave the turn intact, got %q", got)
	}
	whole := "INSTRUCTIONS\nTEXT:\ntrimmed-away"
	if got := userPreambleOf(whole, "not-present-verbatim"); got != whole {
		t.Errorf("a non-verbatim input must fall back to the whole turn, got %q", got)
	}
}

// TestIdentityHelpersOmitEmpty pins the empty-input returns. These look trivial
// but are load-bearing: they are what makes `omitempty` actually omit. If any
// started returning a hash-of-empty, every previously-omitted field would appear
// on every row and loupe's RowsWithIdentity denominator would silently inflate
// with rows carrying a fingerprint of nothing — corrupting the one number the
// plan's gates are read from.
func TestIdentityHelpersOmitEmpty(t *testing.T) {
	if got := inputFingerprint(""); got != "" {
		t.Errorf("inputFingerprint(\"\") = %q, want empty", got)
	}
	if got := exemplarFingerprints(nil); got != nil {
		t.Errorf("exemplarFingerprints(nil) = %v, want nil (a non-nil empty slice serialises as [] on EVERY row)", got)
	}
	if got := exemplarCacheTag(nil); got != "" {
		t.Errorf("exemplarCacheTag(nil) = %q, want empty", got)
	}
	if got := shortHash("x"); len(got) != idHashLen {
		t.Errorf("shortHash len = %d, want %d", len(got), idHashLen)
	}
}

// TestExemplarIDsCapped guards the ledger's O_APPEND-atomic-small invariant.
// ExemplarIDs was the only unbounded field on Entry; raising ExemplarShots would
// grow rows past the atomic threshold, and a torn line is then silently DROPPED
// by ReadAll — the row vanishes from the savings ledger entirely.
func TestExemplarIDsCapped(t *testing.T) {
	shots := make([]exemplars.Pair, maxExemplarIDs+7)
	for i := range shots {
		shots[i] = exemplars.Pair{Input: string(rune('a' + i%26)), Output: "o"}
	}
	if got := len(exemplarFingerprints(shots)); got != maxExemplarIDs {
		t.Errorf("exemplarFingerprints returned %d ids, want the cap %d", got, maxExemplarIDs)
	}
}

// TestContextHashDeterministic is the guard on sort.Slice in contextHash.
//
// Go randomises map iteration deliberately. Without the sort the fingerprint
// changes per call, loupe's context buckets shatter into one arm per permutation,
// and the "every hot-reload is a free A/B" property degrades into noise — with no
// error, no panic, and no other failing test.
func TestContextHashDeterministic(t *testing.T) {
	cfg := config.Config{
		ThresholdsPath:         "/s/thresholds.json",
		RouterWeightsPath:      "/s/router.json",
		TierOverridesPath:      "/s/overrides.json",
		ConfHeadPath:           "/s/confhead.json",
		ConfHeadThresholdsPath: "/s/confthr.json",
	}
	p := &Pipeline{cfg: cfg, learnHashes: map[string]string{
		"/s/thresholds.json": "h1", "/s/router.json": "h2", "/s/overrides.json": "h3",
		"/s/confhead.json": "h4", "/s/confthr.json": "h5",
	}}
	first := p.contextHash()
	for i := 0; i < 50; i++ {
		if got := p.contextHash(); got != first {
			t.Fatalf("contextHash is non-deterministic (call %d: %q != %q) — map iteration order is leaking", i, got, first)
		}
	}

	// Insertion order must not matter either.
	q := &Pipeline{cfg: cfg, learnHashes: map[string]string{}}
	for _, k := range []string{"/s/confthr.json", "/s/confhead.json", "/s/overrides.json", "/s/router.json", "/s/thresholds.json"} {
		q.learnHashes[k] = map[string]string{
			"/s/thresholds.json": "h1", "/s/router.json": "h2", "/s/overrides.json": "h3",
			"/s/confhead.json": "h4", "/s/confthr.json": "h5",
		}[k]
	}
	if q.contextHash() != first {
		t.Error("contextHash depends on map insertion order")
	}
}

// TestContextHashIsPathIndependent pins the machine-portability fix: two boxes
// running byte-identical artifacts under different state dirs must land in the
// SAME A/B arm. Hashing absolute paths forged a phantom "the artifacts changed"
// arm on every state_dir move and on every cross-machine comparison.
func TestContextHashIsPathIndependent(t *testing.T) {
	win := &Pipeline{
		cfg: config.Config{ThresholdsPath: `C:\Users\d\.local-offload\thresholds.json`, RouterWeightsPath: `C:\Users\d\.local-offload\router.json`},
		learnHashes: map[string]string{
			`C:\Users\d\.local-offload\thresholds.json`: "h1",
			`C:\Users\d\.local-offload\router.json`:     "h2",
		},
	}
	nix := &Pipeline{
		cfg: config.Config{ThresholdsPath: "/home/d/.local-offload/thresholds.json", RouterWeightsPath: "/home/d/.local-offload/router.json"},
		learnHashes: map[string]string{
			"/home/d/.local-offload/thresholds.json": "h1",
			"/home/d/.local-offload/router.json":     "h2",
		},
	}
	if win.contextHash() != nix.contextHash() {
		t.Error("identical artifacts under different paths produced different context hashes — " +
			"cross-machine comparison and any state_dir move would forge a phantom A/B arm")
	}
}

// TestContextHashDistinguishesLoadFailure is the "unknown deserves its own
// branch" guard. learnHashes advances ONLY on a successful load, so an artifact
// that is present but failed to parse must NOT read as a confident new arm —
// otherwise an analyst attributes a behaviour change to "the new weights" when
// the truth is "the weights are off".
func TestContextHashDistinguishesLoadFailure(t *testing.T) {
	cfg := config.Config{ThresholdsPath: "/s/t.json", RouterWeightsPath: "/s/r.json"}
	loaded := &Pipeline{cfg: cfg, learnHashes: map[string]string{"/s/t.json": "h1", "/s/r.json": "h2"}}
	failed := &Pipeline{cfg: cfg, learnHashes: map[string]string{"/s/t.json": "h1"}} // router never loaded

	if loaded.contextHash() == failed.contextHash() {
		t.Error("a failed artifact load is indistinguishable from a successful one")
	}

	// No artifacts configured is a real state, not missing data: "" would let
	// omitempty erase the row from loupe's bucket set entirely.
	none := &Pipeline{cfg: config.Config{}, learnHashes: map[string]string{}}
	if got := none.contextHash(); got != "none" {
		t.Errorf("contextHash with no configured artifacts = %q, want %q", got, "none")
	}
}
