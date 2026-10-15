package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/compeval"
	"github.com/dmmdea/offload-harness/internal/eval"
)

// `gate selftest` ranks a file of control pairs with a named scorer and writes the receipt a model
// judge needs. These tests drive the verb in-process (gateMain), the way the other gate tests do,
// and replace the scorer factory with fakes: none of them needs a seat, a config or a child process.

const selftestPairs = `[
  {"name": "one", "good": "GOOD first", "degraded": "BAD first"},
  {"name": "two", "good": "GOOD second", "degraded": "BAD second"}
]
`

// selftestPairsSecondMisranks has a second pair no label-reading scorer can rank: neither side is labelled.
const selftestPairsSecondMisranks = `[
  {"name": "one", "good": "GOOD first", "degraded": "BAD first"},
  {"name": "two", "good": "plain second", "degraded": "plain other"}
]
`

func labelScore(s string) float64 {
	if strings.Contains(s, "GOOD") {
		return 1
	}
	return 0
}

// The fake scorers. ranker reads a label out of the text; the others are the two ways a scorer fails
// the self-test (it ranks backwards, it cannot tell the sides apart) and the way it cannot run.
var (
	rankerScorer   compeval.Scorer = func(_ context.Context, s string) (float64, error) { return labelScore(s), nil }
	invertedScorer compeval.Scorer = func(_ context.Context, s string) (float64, error) { return 1 - labelScore(s), nil }
	tiedScorer     compeval.Scorer = func(context.Context, string) (float64, error) { return 0.5, nil }
	brokenScorer   compeval.Scorer = func(context.Context, string) (float64, error) { return 0, errors.New("seat unreachable") }
)

// selftestSeam is what the fake factory saw.
type selftestSeam struct {
	cleanups int
	asked    []string // the scorer ids it was asked for
	configs  []string // the --config value the factory's flag set held at each call
}

// useScorers replaces the scorer factory for one test. An id it does not hold is an error, as the
// real factory's is.
func useScorers(t *testing.T, byID map[string]compeval.Scorer) *selftestSeam {
	t.Helper()
	seam := &selftestSeam{}
	prev := gateScorerFor
	gateScorerFor = func(id string, fs *flag.FlagSet) (compeval.Scorer, func(), error) {
		seam.asked = append(seam.asked, id)
		seam.configs = append(seam.configs, fs.Lookup("config").Value.String())
		s, ok := byID[id]
		if !ok {
			return nil, nil, fmt.Errorf("no scorer %q", id)
		}
		return s, func() { seam.cleanups++ }, nil
	}
	t.Cleanup(func() { gateScorerFor = prev })
	return seam
}

func pinBuild(t *testing.T, f func() string) {
	t.Helper()
	prev := gateBuildSHA
	gateBuildSHA = f
	t.Cleanup(func() { gateBuildSHA = prev })
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func parseReceipt(t *testing.T, out string) eval.JudgeReceipt {
	t.Helper()
	var r eval.JudgeReceipt
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("selftest must print the receipt as one JSON document, got %q: %v", out, err)
	}
	if _, err := dec.Token(); err == nil {
		t.Fatalf("selftest printed more than the receipt: %q", out)
	}
	return r
}

func selftestRun(t *testing.T, dir, pairs string, extra ...string) (stdout, stderr string, code int) {
	t.Helper()
	p := gateWrite(t, dir, "pairs.json", pairs)
	return runGateCmd(t, append([]string{"selftest", "--pairs", p}, extra...)...)
}

func TestGateSelftestPassPrintsTheReceiptAndExitsZero(t *testing.T) {
	useScorers(t, map[string]compeval.Scorer{"ranker": rankerScorer})
	dir := t.TempDir()
	out, errs, code := selftestRun(t, dir, selftestPairs, "--scorer", "ranker")
	if code != 0 || errs != "" {
		t.Fatalf("a passing self-test must exit 0 with nothing on stderr: %d %q", code, errs)
	}
	r := parseReceipt(t, out)
	want := eval.JudgeReceipt{ScorerID: "ranker", PairsHash: sha256Hex([]byte(selftestPairs)), Passed: true, BuildSHA: gateTestBuild}
	if r != want {
		t.Fatalf("receipt %+v, want %+v", r, want)
	}
	// Exactly the four fields the gate reads, under the names the gate reads them by.
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"build_sha256", "pairs_hash", "passed", "scorer_id"}) {
		t.Fatalf("receipt keys %v", keys)
	}
	// The verb wrote nothing: --out was not given.
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("selftest without --out must write no file, the directory holds %d entries", len(entries))
	}
}

// pairs_hash is the hash of the file's bytes, not of what they decode to: the same pairs written with other
// whitespace, or behind a byte-order mark, are another file.
func TestGateSelftestPairsHashIsTheHashOfTheFileBytes(t *testing.T) {
	useScorers(t, map[string]compeval.Scorer{"ranker": rankerScorer})
	dir := t.TempDir()
	compact := `[{"name":"one","good":"GOOD first","degraded":"BAD first"}]`
	withBOM := "\xef\xbb\xbf" + compact
	seen := map[string]bool{}
	for name, body := range map[string]string{"indented": selftestPairs, "compact": compact, "bom": withBOM} {
		out, errs, code := selftestRun(t, dir, body, "--scorer", "ranker")
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", name, code, errs)
		}
		got := parseReceipt(t, out).PairsHash
		if got != sha256Hex([]byte(body)) {
			t.Errorf("%s: pairs_hash %s is not the SHA-256 of the file bytes", name, got)
		}
		seen[got] = true
	}
	if len(seen) != 3 {
		t.Errorf("three different files must have three different hashes, got %d", len(seen))
	}
}

func TestGateSelftestFailureStillPrintsTheReceiptAndExitsOne(t *testing.T) {
	useScorers(t, map[string]compeval.Scorer{"inverted": invertedScorer, "tied": tiedScorer, "ranker": rankerScorer})
	for name, tc := range map[string]struct{ scorer, pairs, wantErr string }{
		"ranks backwards":             {"inverted", selftestPairs, `"one"`},
		"cannot tell the sides apart": {"tied", selftestPairs, `"one"`},
		"the second pair mis-ranks":   {"ranker", selftestPairsSecondMisranks, `"two"`},
	} {
		dir := t.TempDir()
		out, errs, code := selftestRun(t, dir, tc.pairs, "--scorer", tc.scorer)
		if code != 1 {
			t.Errorf("%s: a failed self-test must exit 1, got %d (stderr %q)", name, code, errs)
		}
		r := parseReceipt(t, out)
		if r.Passed || r.ScorerID != tc.scorer || r.PairsHash != sha256Hex([]byte(tc.pairs)) || r.BuildSHA != gateTestBuild {
			t.Errorf("%s: the failed receipt must carry the same identity with passed false: %+v", name, r)
		}
		if !strings.Contains(errs, "FAILED") || !strings.Contains(errs, tc.wantErr) {
			t.Errorf("%s: the mis-ranking belongs on stderr and names the pair: %q", name, errs)
		}
	}
}

func TestGateSelftestRefusalsExitTwoAndPrintNoReceipt(t *testing.T) {
	useScorers(t, map[string]compeval.Scorer{"ranker": rankerScorer, "broken": brokenScorer})
	dir := t.TempDir()
	good := gateWrite(t, dir, "good.json", selftestPairs)
	for _, tc := range []struct {
		name string
		args []string
		want string // a word of the reason, so a refusal is a refusal for the right cause
	}{
		{"no flags", []string{"selftest"}, "--pairs"},
		{"no --pairs", []string{"selftest", "--scorer", "ranker"}, "--pairs"},
		{"no --scorer", []string{"selftest", "--pairs", good}, "--scorer"},
		{"extra positional", []string{"selftest", "--pairs", good, "--scorer", "ranker", "extra"}, `"extra"`},
		{"unknown flag", []string{"selftest", "--pairs", good, "--scorer", "ranker", "--bogus"}, "bogus"},
		{"missing pairs file", []string{"selftest", "--pairs", filepath.Join(dir, "missing.json"), "--scorer", "ranker"}, "missing.json"},
		{"pairs is a directory", []string{"selftest", "--pairs", dir, "--scorer", "ranker"}, "pairs"},
		{"pairs is not JSON", []string{"selftest", "--pairs", gateWrite(t, dir, "a.json", "not json"), "--scorer", "ranker"}, "a.json"},
		{"pairs is an object", []string{"selftest", "--pairs", gateWrite(t, dir, "b.json", `{"name":"x"}`), "--scorer", "ranker"}, "b.json"},
		{"pairs has an unknown field", []string{"selftest", "--pairs", gateWrite(t, dir, "c.json", `[{"name":"x","good":"a","degraded":"b","extra":1}]`), "--scorer", "ranker"}, "c.json"},
		{"pairs has trailing data", []string{"selftest", "--pairs", gateWrite(t, dir, "d.json", `[] []`), "--scorer", "ranker"}, "d.json"},
		{"pairs file is empty", []string{"selftest", "--pairs", gateWrite(t, dir, "e.json", ""), "--scorer", "ranker"}, "e.json"},
		{"empty pair list", []string{"selftest", "--pairs", gateWrite(t, dir, "f.json", `[]`), "--scorer", "ranker"}, "no control pairs"},
		{"null pair list", []string{"selftest", "--pairs", gateWrite(t, dir, "g.json", `null`), "--scorer", "ranker"}, "no control pairs"},
		{"unknown scorer", []string{"selftest", "--pairs", good, "--scorer", "no-such-scorer"}, "no-such-scorer"},
		{"scorer that errors", []string{"selftest", "--pairs", good, "--scorer", "broken"}, "seat unreachable"},
		{"unwritable --out", []string{"selftest", "--pairs", good, "--scorer", "ranker", "--out", filepath.Join(dir, "no-such-dir", "r.json")}, "no-such-dir"},
	} {
		out, errs, code := runGateCmd(t, tc.args...)
		trimmed := strings.TrimSpace(errs)
		last := trimmed[strings.LastIndex(trimmed, "\n")+1:] // the error line comes after any usage text
		if code != 2 || out != "" || !strings.HasPrefix(last, "gate: ") || !strings.Contains(last, tc.want) {
			t.Errorf("%s: want exit 2, no stdout and a last stderr line \"gate: ...\" that says %q, got %d %q %q", tc.name, tc.want, code, out, errs)
		}
	}
}

// A scorer that errors has said nothing about whether it can rank: that is no receipt at all, not a
// failed one, because a failed receipt would read as a finding about the scorer.
func TestGateSelftestAScorerThatCannotRunIsNotAFailedReceipt(t *testing.T) {
	seam := useScorers(t, map[string]compeval.Scorer{"broken": brokenScorer})
	out, errs, code := selftestRun(t, t.TempDir(), selftestPairs, "--scorer", "broken")
	if code != 2 || out != "" || !strings.Contains(errs, "seat unreachable") {
		t.Fatalf("a scorer error must exit 2 with no receipt and its own words: %d %q %q", code, out, errs)
	}
	if seam.cleanups != 1 {
		t.Fatalf("the scorer's cleanup must still run, ran %d times", seam.cleanups)
	}
}

func TestGateSelftestRunsTheScorersCleanupOnceAndOnlyWhenThereIsOne(t *testing.T) {
	seam := useScorers(t, map[string]compeval.Scorer{"ranker": rankerScorer, "inverted": invertedScorer})
	dir := t.TempDir()
	selftestRun(t, dir, selftestPairs, "--scorer", "ranker")
	selftestRun(t, dir, selftestPairs, "--scorer", "inverted")
	if seam.cleanups != 2 {
		t.Fatalf("one cleanup per scorer built, passing or failing: %d", seam.cleanups)
	}
	selftestRun(t, dir, selftestPairs, "--scorer", "no-such-scorer")
	if seam.cleanups != 2 {
		t.Fatalf("a scorer that was never built has no cleanup: %d", seam.cleanups)
	}
	// The factory is not reached for a pairs file the verb already refused: nothing is opened for it.
	before := len(seam.asked)
	selftestRun(t, dir, `[]`, "--scorer", "ranker")
	selftestRun(t, dir, `not json`, "--scorer", "ranker")
	if len(seam.asked) != before {
		t.Fatalf("a refused pairs file must not build a scorer, the factory was asked %d more times", len(seam.asked)-before)
	}
}

func TestGateSelftestOutWritesTheReceiptAndReplacesAStaleOne(t *testing.T) {
	useScorers(t, map[string]compeval.Scorer{"ranker": rankerScorer, "inverted": invertedScorer})
	dir := t.TempDir()
	dest := filepath.Join(dir, "receipt.json")

	out, errs, code := selftestRun(t, dir, selftestPairs, "--scorer", "ranker", "--out", dest)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	onDisk, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != out {
		t.Fatalf("the file and stdout must be the same bytes:\nfile:   %q\nstdout: %q", onDisk, out)
	}
	if !parseReceipt(t, string(onDisk)).Passed {
		t.Fatal("the written receipt must be the passing one")
	}

	// The same path after a run that fails: the passing receipt on disk must not outlive it.
	out, _, code = selftestRun(t, dir, selftestPairs, "--scorer", "inverted", "--out", dest)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	onDisk, _ = os.ReadFile(dest)
	if string(onDisk) != out || parseReceipt(t, string(onDisk)).Passed {
		t.Fatalf("a failing run must overwrite the stale passing receipt with its own: %q", onDisk)
	}
}

// build_sha256 is the build that ran the self-test. The gate refuses a receipt whose hash is not 64
// lower-case hex, so a build that cannot be hashed gets no receipt rather than one that is refused later.
func TestGateSelftestNeedsAHashOfTheRunningBuild(t *testing.T) {
	useScorers(t, map[string]compeval.Scorer{"ranker": rankerScorer})
	for name, v := range map[string]string{
		"unreadable":   "",
		"short":        "0123456789abcdef",
		"upper case":   strings.ToUpper(gateHex("x")),
		"not hex":      strings.Repeat("g", 64),
		"one too many": gateHex("x") + "0",
	} {
		pinBuild(t, func() string { return v })
		out, errs, code := selftestRun(t, t.TempDir(), selftestPairs, "--scorer", "ranker")
		if code != 2 || out != "" || !strings.Contains(errs, "build") {
			t.Errorf("%s: want exit 2, no receipt and a word about the build, got %d %q %q", name, code, out, errs)
		}
	}
}

// With nothing pinned the hash is the one of the executable that is running, read from its file.
func TestGateSelftestBuildHashIsTheRunningExecutablesHash(t *testing.T) {
	useScorers(t, map[string]compeval.Scorer{"ranker": rankerScorer})
	pinBuild(t, buildinfo.BuildSHA256)
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("no executable path on this platform: %v", err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	out, errs, code := selftestRun(t, t.TempDir(), selftestPairs, "--scorer", "ranker")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if got, want := parseReceipt(t, out).BuildSHA, sha256Hex(raw); got != want {
		t.Fatalf("build_sha256 %s is not the SHA-256 of the running executable %s", got, want)
	}
}

// The scorer's configuration reaches the factory, from before the subcommand (the global flag) or after it.
func TestGateSelftestPassesTheConfigPathToTheScorerFactory(t *testing.T) {
	seam := useScorers(t, map[string]compeval.Scorer{"ranker": rankerScorer})
	dir := t.TempDir()
	pairs := gateWrite(t, dir, "pairs.json", selftestPairs)
	for _, args := range [][]string{
		{"--config", "front.json", "selftest", "--pairs", pairs, "--scorer", "ranker"},
		{"selftest", "--pairs", pairs, "--scorer", "ranker", "--config", "after.json"},
		{"selftest", "--pairs", pairs, "--scorer", "ranker"},
	} {
		if _, errs, code := runGateCmd(t, args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errs)
		}
	}
	if want := []string{"front.json", "after.json", ""}; !reflect.DeepEqual(seam.configs, want) {
		t.Fatalf("the factory saw configs %q, want %q", seam.configs, want)
	}
}

// Without the fake, the real factory knows one scorer, and says so when asked for another. It builds
// nothing for an id it does not know (no config is read, no directory made).
func TestGateSelftestRealFactoryKnowsOnlyTheCompactionOutcomeScorer(t *testing.T) {
	out, errs, code := selftestRun(t, t.TempDir(), selftestPairs, "--scorer", "no-such-scorer")
	if code != 2 || out != "" || !strings.Contains(errs, `"no-such-scorer"`) || !strings.Contains(errs, "compaction-outcome") {
		t.Fatalf("an unknown scorer must exit 2 and name the known one: %d %q %q", code, out, errs)
	}
	if !reflect.DeepEqual(gateScorerIDs, []string{"compaction-outcome"}) {
		t.Fatalf("known scorers: %v", gateScorerIDs)
	}
}

// The usage text names the verb, its flags, the scorers and the new meaning of exit 1.
func TestGateSelftestIsInTheUsage(t *testing.T) {
	out, _, code := runGateCmd(t, "help")
	if code != 0 {
		t.Fatalf("help: %d", code)
	}
	for _, want := range []string{"gate selftest", "--pairs", "--scorer", "--out", "compaction-outcome", "self-test"} {
		if !strings.Contains(out, want) {
			t.Errorf("the usage must mention %q", want)
		}
	}
	if !strings.Contains(out, "freeze|check|verdict|selftest|schema") {
		t.Error("the usage line must list selftest among the subcommands")
	}
	_, errs, code := runGateCmd(t, "bogus")
	if code != 2 || !strings.Contains(errs, "selftest") {
		t.Errorf("the unknown-subcommand message must list selftest: %d %q", code, errs)
	}
	_, errs, code = runGateCmd(t)
	if code != 2 || !strings.Contains(errs, "selftest") {
		t.Errorf("the missing-subcommand message must list selftest: %d %q", code, errs)
	}
	if out, errs, code := runGateCmd(t, "selftest", "-h"); code != 0 || !strings.Contains(out+errs, "pairs") {
		t.Errorf("selftest -h: %d %q %q", code, out, errs)
	}
}

// The receipt this verb writes is the one the gate reads: a passing one lets a model-judged
// comparison through, a failing one does not, and one for another scorer does not.
func TestGateSelftestReceiptRoundTripsThroughVerdict(t *testing.T) {
	useScorers(t, map[string]compeval.Scorer{"m": rankerScorer})
	fm := freezeModelJudge(t) // the pre-registered judge is {model, "m"}
	judge := eval.Judge{Kind: "model", Identity: "m"}
	verdictWith := func(r *eval.JudgeReceipt) (string, string, int) {
		in := eval.GateInput{BaselineHash: fm.hash, Incumbent: ptr(gateArm("inc", 16, 0)), Candidate: gateArm("cand", 16, 1), Judge: judge, Receipt: r}
		return runGateCmd(t, "verdict", "--baseline", fm.baseline, "--input", gateWrite(t, fm.dir, "input.json", in))
	}

	// Without a receipt the gate refuses; that is what the verb exists to fix.
	if _, errs, code := verdictWith(nil); code != 2 || !strings.Contains(errs, "needs a self-test receipt") {
		t.Fatalf("a model judge without a receipt must be refused: %d %q", code, errs)
	}

	// A passing receipt, taken from the --out file, is accepted.
	dest := filepath.Join(fm.dir, "receipt.json")
	if _, errs, code := selftestRun(t, fm.dir, selftestPairs, "--scorer", "m", "--out", dest); code != 0 {
		t.Fatalf("selftest: %d %s", code, errs)
	}
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	pass := parseReceipt(t, string(raw))
	out, errs, code := verdictWith(&pass)
	if code != 0 {
		t.Fatalf("a passing receipt from gate selftest must be accepted by gate verdict: %d %s", code, errs)
	}
	var res eval.GateResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Verdict != eval.VerdictAdopt || res.Judge != judge {
		t.Fatalf("verdict: %v %+v", err, res)
	}

	// A failing receipt is printed with exit 1 and refused by the gate with exit 2.
	useScorers(t, map[string]compeval.Scorer{"m": invertedScorer})
	out, _, code = selftestRun(t, fm.dir, selftestPairs, "--scorer", "m")
	if code != 1 {
		t.Fatalf("selftest of an inverted scorer: exit %d", code)
	}
	fail := parseReceipt(t, out)
	if out, errs, code := verdictWith(&fail); code != 2 || out != "" || !strings.Contains(errs, "did not pass") {
		t.Fatalf("a failing receipt must be refused with exit 2: %d %q %q", code, out, errs)
	}

	// A passing receipt for another scorer is refused: the receipt is about the scorer it names.
	useScorers(t, map[string]compeval.Scorer{"other": rankerScorer})
	out, _, code = selftestRun(t, fm.dir, selftestPairs, "--scorer", "other")
	if code != 0 {
		t.Fatalf("selftest of the other scorer: exit %d", code)
	}
	other := parseReceipt(t, out)
	if _, errs, code := verdictWith(&other); code != 2 || !strings.Contains(errs, "is not the judge identity") {
		t.Fatalf("a receipt for another scorer must be refused: %d %q", code, errs)
	}
}
