// gate_cmd.go — the `gate` verb: the one measurement gate (internal/eval/gate.go) and its
// pre-registration, from the command line.
//
//	gate freeze   --prereg P --corpus C [--incumbent ARM] --out BASELINE
//	gate check    --baseline BASELINE [--expect-hash SHA256]
//	gate verdict  --baseline BASELINE --input INPUT [--require-adopt]
//	gate selftest --pairs PAIRS --scorer ID [--out RECEIPT]
//	gate schema
//
// Exit codes (every failure of `verdict` and of `selftest` that is not a finding is 2, so that 1
// can only mean a finding: a verdict that is not ADOPT, or a scorer that failed its self-test):
//
//	0  the command ran; for verdict, a verdict was produced (any of ADOPT, BLOCK, INCONCLUSIVE);
//	   for selftest, the scorer ranked every control pair and the receipt says passed
//	1  verdict --require-adopt and the verdict is not ADOPT; selftest and the scorer mis-ranked a
//	   pair (the result, the receipt with passed false, is still printed)
//	2  a refusal: a bad hash, a bad receipt, a malformed or unreadable input, a baseline that is
//	   not the frozen bytes, a freeze the gate cannot honour, a selftest that could not run (no
//	   pairs, an unknown scorer, a scorer that returned an error, a build that cannot be
//	   hashed), a result that could not be written, or a usage error
//
// -h and --help print the usage and exit 0, as the other verbs do whose flags are parsed with
// flag.ExitOnError.
//
// The verb reads the files it is named and writes at most one file: freeze's --out, which is
// created exclusively (a pre-registration is written once; a changed one is a new file with a new
// hash), or selftest's --out, which replaces what is there (a receipt is the latest answer, so a
// failing run must not leave an older passing one behind). It prints no timestamp. The gate reads
// no config; selftest alone reads one, to reach the seat of a scorer that calls one.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/compeval"
	"github.com/dmmdea/offload-harness/internal/eval"
)

// gateCompactionScorerID names the scorer of the compaction A/B (accept + entity recall over a
// summary of the transcript, see compactionOutcomeScorer).
const gateCompactionScorerID = "compaction-outcome"

// gateScorerIDs are the scorer ids `gate selftest --scorer` accepts, in the order the usage lists them.
var gateScorerIDs = []string{gateCompactionScorerID}

// gateScorerFor builds the scorer a self-test ranks the control pairs with and the cleanup to run
// when the self-test is over (nil when there is nothing to release). fs is the verb's flag set, for
// the --config a scorer that calls a seat needs. It is a variable so a test puts a fake where the seat
// would be: no test of the verb needs a seat.
var gateScorerFor = realGateScorer

func realGateScorer(id string, fs *flag.FlagSet) (compeval.Scorer, func(), error) {
	switch id {
	case gateCompactionScorerID:
		p, cleanup, err := openPipeline(loadCfg(fs))
		if err != nil {
			return nil, nil, err
		}
		return compactionOutcomeScorer(p.Run), cleanup, nil
	}
	return nil, nil, fmt.Errorf("unknown scorer %q (known: %s)", id, strings.Join(gateScorerIDs, ", "))
}

// gateBuildSHA is the hash of this binary, reported as gate_build_sha256. It is a variable so a
// test can pin it; it is not part of the era marker.
var gateBuildSHA = buildinfo.BuildSHA256

const gateUsage = `usage: local-offload gate <freeze|check|verdict|selftest|schema> [flags]

  gate freeze  --prereg PREREG.json --corpus CORPUS [--incumbent ARM.json] --out BASELINE.json
      validate the pre-registration, hash the corpus, sort the task lists, and write the canonical
      baseline (created exclusively; an existing different file is refused). Prints
      {"baseline": <path>, "baseline_hash": <sha256>}.
  gate check   --baseline BASELINE.json [--expect-hash SHA256]
      parse the baseline, refuse it unless it is byte for byte what freeze writes, and print its
      sha256. --expect-hash also refuses a baseline whose hash is another one. A one-byte edit of a
      frozen baseline either fails check or changes its hash; an edit that leaves a valid file is
      another baseline, so bind a verdict to the hash freeze printed, never to a later check.
  gate verdict --baseline BASELINE.json --input INPUT.json [--require-adopt]
      judge the candidate against the incumbent under the baseline and print the result as JSON.
  gate selftest --pairs PAIRS.json --scorer ID [--out RECEIPT.json]
      rank every control pair (a JSON array of {"name", "good", "degraded"}) with the named model
      scorer, and print its self-test receipt: {"scorer_id", "pairs_hash" (the sha256 of the pairs
      file), "passed", "build_sha256" (the sha256 of this binary)}. A model judge's gate input carries
      that receipt as "receipt", and the judge's identity in the baseline is the scorer id. A scorer
      fails when it scores a pair's good side at or below its degraded side; the receipt is then still
      printed, with passed false, and the mis-ranking goes to stderr. --out also writes the receipt to
      a file (replacing one). Scorers: compaction-outcome (the compaction A/B's accept + entity-recall
      score through the configured seat; it reads --config).
  gate schema
      print the JSON Schema (draft 2020-12) of the gate input document.

Exit codes:
  0  the command ran; for verdict, a verdict was produced (ADOPT, BLOCK or INCONCLUSIVE); for
     selftest, the scorer ranked every pair
  1  verdict --require-adopt and the verdict is not ADOPT; selftest and the scorer mis-ranked a pair
     (the result is still printed)
  2  a refusal (bad hash, bad receipt, malformed input, a baseline that is not the frozen bytes,
     a freeze the gate cannot honour; for selftest, no pairs, an unknown scorer, a scorer that
     returned an error, or a build that cannot be hashed), a result that could not be written, or a
     usage error

A global --config <path> before the subcommand is accepted. Only selftest reads it, to reach the seat
of a scorer that calls one; the other subcommands ignore it: the gate reads no config.
`

// gateFail is a gate failure: every one ends the process with code 2, so that exit 1 stays free
// for "the verdict is not ADOPT".
func gateFail(format string, a ...any) error {
	return &exitCodeError{code: 2, msg: "gate: " + fmt.Sprintf(format, a...)}
}

func runGate(args []string) error { return gateMain(args, os.Stdout, os.Stderr) }

// gateMain is runGate with its streams passed in, so a test drives the whole verb in-process.
func gateMain(args []string, stdout, stderr io.Writer) error {
	// main.go hoists a global `--config <v>` / `--config=<v>` to the front of the verb's args, so
	// `local-offload --config x.json gate schema` arrives here as [--config x.json schema]. The gate
	// reads no config, but the subcommand is what follows it: take the subcommand from behind any
	// leading --config (the same hoist `gpu` uses), and let the subcommand's own flag set accept the
	// flag it gets back.
	sub, rest, ok := hoistGlobalConfig(args)
	if !ok {
		fmt.Fprint(stderr, gateUsage)
		return gateFail("a subcommand is required (freeze, check, verdict, selftest or schema)")
	}
	switch sub {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, gateUsage)
		return nil
	case "freeze":
		return gateFreeze(rest, stdout, stderr)
	case "check":
		return gateCheck(rest, stdout, stderr)
	case "verdict":
		return gateVerdict(rest, stdout, stderr)
	case "selftest":
		return gateSelftest(rest, stdout, stderr)
	case "schema":
		return gateSchema(rest, stdout, stderr)
	}
	fmt.Fprint(stderr, gateUsage)
	return gateFail("unknown subcommand %q (freeze, check, verdict, selftest or schema)", sub)
}

func gateFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("gate "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	// The gate reads no config, except that selftest hands this flag set to the scorer it builds, which
	// may call a seat. The flag is accepted by every subcommand because gateMain hands a leading global
	// --config back to the subcommand, and so that `gate schema --config x.json` parses too.
	fs.String("config", "", "read only by selftest (for a scorer that calls a seat); accepted and ignored by the other subcommands")
	return fs
}

// gateParse parses a subcommand's flags. help is true when -h was asked for (the flag set has
// printed the flags): the caller returns nil without running anything.
func gateParse(fs *flag.FlagSet, args []string) (help bool, err error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return false, gateFail("%v", err)
	}
	if fs.NArg() > 0 {
		return false, gateFail("%s: unexpected argument %q", fs.Name(), fs.Arg(0))
	}
	return false, nil
}

// decodeJSONStrict reads exactly one JSON document into v, refusing unknown fields and trailing
// data. A leading UTF-8 byte-order mark is dropped (Windows editors write one); the files read
// this way are not hashed, so that is the only leniency.
func decodeJSONStrict(raw []byte, v any) error {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON document")
	}
	return nil
}

func gateReadJSON(path, what string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return gateFail("%s: %v", what, err)
	}
	if err := decodeJSONStrict(raw, v); err != nil {
		return gateFail("%s %s: %v", what, path, err)
	}
	return nil
}

// printJSONLine writes v as one compact line (HTML escaping off: paths are paths).
func printJSONLine(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func gateFreeze(args []string, stdout, stderr io.Writer) error {
	fs := gateFlags("freeze", stderr)
	prereg := fs.String("prereg", "", "the pre-registration JSON (required)")
	corpus := fs.String("corpus", "", "the corpus file to hash (required; it is hashed, never copied)")
	incumbent := fs.String("incumbent", "", "an incumbent arm JSON to freeze into the baseline (optional)")
	out := fs.String("out", "", "where to write the canonical baseline (required; must not exist with other content)")
	if help, err := gateParse(fs, args); help || err != nil {
		return err
	}
	for name, val := range map[string]string{"--prereg": *prereg, "--corpus": *corpus, "--out": *out} {
		if val == "" {
			return gateFail("freeze: %s is required", name)
		}
	}
	var p eval.PreReg
	if err := gateReadJSON(*prereg, "pre-registration", &p); err != nil {
		return err
	}
	corpusBytes, err := os.ReadFile(*corpus)
	if err != nil {
		return gateFail("corpus: %v", err)
	}
	var arm *eval.Arm
	if *incumbent != "" {
		arm = &eval.Arm{}
		if err := gateReadJSON(*incumbent, "incumbent arm", arm); err != nil {
			return err
		}
	}
	_, raw, hash, err := eval.FreezeBaseline(p, corpusBytes, arm)
	if err != nil {
		return gateFail("%v", err)
	}
	if err := writeBaselineOnce(*out, raw); err != nil {
		return err
	}
	if err := printJSONLine(stdout, struct {
		Baseline     string `json:"baseline"`
		BaselineHash string `json:"baseline_hash"`
	}{*out, hash}); err != nil {
		return gateFail("write result: %v", err)
	}
	return nil
}

// writeBaselineOnce creates path with raw. A pre-registration is written once: an existing file
// with the same bytes is the same pre-registration and is left alone; one with other content is a
// different pre-registration and gets a new path.
func writeBaselineOnce(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err == nil {
		_, werr := f.Write(raw)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return gateFail("write %s: %v", path, werr)
		}
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return gateFail("write %s: %v", path, err)
	}
	existing, rerr := os.ReadFile(path)
	if rerr != nil {
		return gateFail("read %s: %v", path, rerr)
	}
	if !bytes.Equal(existing, raw) {
		return gateFail("freeze refused: %s already holds a different baseline (hash %.12s); a pre-registration is written once, so a changed one goes to a new --out", path, eval.BaselineHash(existing))
	}
	return nil
}

func gateCheck(args []string, stdout, stderr io.Writer) error {
	fs := gateFlags("check", stderr)
	baseline := fs.String("baseline", "", "the baseline file (required)")
	expect := fs.String("expect-hash", "", "refuse unless the baseline's sha256 is this one")
	if help, err := gateParse(fs, args); help || err != nil {
		return err
	}
	if *baseline == "" {
		return gateFail("check: --baseline is required")
	}
	raw, err := os.ReadFile(*baseline)
	if err != nil {
		return gateFail("baseline: %v", err)
	}
	_, hash, err := eval.CheckBaseline(raw)
	if err != nil {
		return gateFail("%v", err)
	}
	if *expect != "" && *expect != hash {
		return gateFail("check refused: the baseline hashes to %s, not the expected %s: this is not the file that was frozen", hash, *expect)
	}
	if _, err := fmt.Fprintln(stdout, hash); err != nil {
		return gateFail("write result: %v", err)
	}
	return nil
}

func gateVerdict(args []string, stdout, stderr io.Writer) error {
	fs := gateFlags("verdict", stderr)
	baseline := fs.String("baseline", "", "the frozen baseline file (required)")
	input := fs.String("input", "", "the gate input JSON (required; `gate schema` prints its schema)")
	requireAdopt := fs.Bool("require-adopt", false, "exit 1 unless the verdict is ADOPT")
	if help, err := gateParse(fs, args); help || err != nil {
		return err
	}
	if *baseline == "" || *input == "" {
		return gateFail("verdict: --baseline and --input are required")
	}
	raw, err := os.ReadFile(*baseline)
	if err != nil {
		return gateFail("baseline: %v", err)
	}
	b, hash, err := eval.CheckBaseline(raw)
	if err != nil {
		return gateFail("%v", err)
	}
	var in eval.GateInput
	if err := gateReadJSON(*input, "gate input", &in); err != nil {
		return err
	}
	res, err := eval.Gate(b, hash, in)
	if err != nil {
		return gateFail("refused: %v", err)
	}
	res.GateBuildSHA256 = gateBuildSHA()
	j, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return gateFail("%v", err)
	}
	if _, err := stdout.Write(append(j, '\n')); err != nil {
		return gateFail("write result: %v", err)
	}
	if *requireAdopt && res.Verdict != eval.VerdictAdopt {
		return &exitCodeError{code: 1, msg: fmt.Sprintf("gate: verdict %s (--require-adopt): %s", res.Verdict, res.Reason)}
	}
	return nil
}

// gateSelftest ranks the control pairs with a named scorer and prints the receipt R7 asks of a model
// judge. A scorer that mis-ranks a pair has produced a finding: the receipt is printed with passed
// false and the exit is 1. A run that could not reach a finding (no pairs, an unknown scorer, a scorer
// that returned an error, a build that cannot be hashed) is a refusal, exit 2, with no receipt: a
// failed receipt would read as a statement about the scorer.
func gateSelftest(args []string, stdout, stderr io.Writer) error {
	fs := gateFlags("selftest", stderr)
	pairsPath := fs.String("pairs", "", "the control pairs: a JSON array of {name, good, degraded} (required)")
	scorerID := fs.String("scorer", "", "the scorer to rank them with (required; one of "+strings.Join(gateScorerIDs, ", ")+")")
	out := fs.String("out", "", "also write the receipt to this file, replacing one that is there")
	if help, err := gateParse(fs, args); help || err != nil {
		return err
	}
	if *pairsPath == "" || *scorerID == "" {
		return gateFail("selftest: --pairs and --scorer are required")
	}
	raw, err := os.ReadFile(*pairsPath)
	if err != nil {
		return gateFail("pairs: %v", err)
	}
	var pairs []compeval.ControlPair
	if err := decodeJSONStrict(raw, &pairs); err != nil {
		return gateFail("pairs %s: %v", *pairsPath, err)
	}
	if len(pairs) == 0 {
		return gateFail("pairs %s: no control pairs: a self-test with nothing to rank proves nothing", *pairsPath)
	}
	build := gateBuildSHA()
	if !isLowerHex64(build) {
		return gateFail("selftest: the running build cannot be hashed (got %q): a receipt names the build that ran it, and the gate refuses a build_sha256 that is not 64 lower-case hex characters", build)
	}
	score, cleanup, err := gateScorerFor(*scorerID, fs)
	if err != nil {
		return gateFail("selftest: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	// SelfTest returns one error for a mis-ranking and for a scorer that failed to score. Only the first
	// is a finding about the scorer, so the scorer is wrapped to tell them apart.
	var scoreErr error
	tracked := func(ctx context.Context, rendered string) (float64, error) {
		v, err := score(ctx, rendered)
		if err != nil && scoreErr == nil {
			scoreErr = err
		}
		return v, err
	}
	selfErr := compeval.SelfTest(context.Background(), tracked, pairs)
	if scoreErr != nil {
		return gateFail("selftest: scorer %q could not run, so there is no receipt: %v", *scorerID, selfErr)
	}
	sum := sha256.Sum256(raw)
	receipt := eval.JudgeReceipt{ScorerID: *scorerID, PairsHash: hex.EncodeToString(sum[:]), Passed: selfErr == nil, BuildSHA: build}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(receipt); err != nil {
		return gateFail("%v", err)
	}
	if *out != "" {
		if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
			return gateFail("write %s: %v", *out, err)
		}
	}
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		return gateFail("write result: %v", err)
	}
	if selfErr != nil {
		return &exitCodeError{code: 1, msg: "gate: selftest: " + selfErr.Error()}
	}
	return nil
}

// isLowerHex64 reports whether s is 64 lower-case hex characters, the form R7 requires of a receipt's
// hashes.
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func gateSchema(args []string, stdout, stderr io.Writer) error {
	fs := gateFlags("schema", stderr)
	if help, err := gateParse(fs, args); help || err != nil {
		return err
	}
	if _, err := stdout.Write(eval.GateInputSchema); err != nil {
		return gateFail("write result: %v", err)
	}
	return nil
}
