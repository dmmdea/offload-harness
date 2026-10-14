package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/eval"
)

const gateTestBuild = "0123456789abcdef"

func init() {
	// The verb reports the hash of its own binary; a test pins it so output is comparable.
	gateBuildSHA = func() string { return gateTestBuild }
}

// runGateCmd drives the verb in-process and returns what it printed and the exit code it maps to.
func runGateCmd(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	err := gateMain(args, &out, &errb)
	if err != nil {
		var line string
		code, line = exitStatus(err)
		errb.WriteString(line + "\n")
	}
	return out.String(), errb.String(), code
}

func gateWrite(t *testing.T, dir, name string, v any) string {
	t.Helper()
	path := filepath.Join(dir, name)
	var raw []byte
	switch x := v.(type) {
	case string:
		raw = []byte(x)
	case []byte:
		raw = x
	default:
		var err error
		if raw, err = json.MarshalIndent(v, "", "  "); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func gateStamps(tag string) eval.Stamps {
	return eval.Stamps{HarnessBuildSHA256: "h-" + tag, SeatConfigSHA256: "s-" + tag, EngineBuild: "e-" + tag}
}

func gateArm(tag string, n int, score float64) eval.Arm {
	a := eval.Arm{Stamps: gateStamps(tag)}
	for i := 0; i < n; i++ {
		s := score
		a.Tasks = append(a.Tasks, eval.Task{ID: fmt.Sprintf("t%03d", i), Attempts: []eval.Attempt{{Outcome: eval.OutcomePass, Score: &s}}})
	}
	return a
}

func gateTaskList(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("t%03d", i)
	}
	return ids
}

const gatePrereg = `{
  "mode": "superiority",
  "primary": {"name": "goal_reached", "class": "unit_interval"},
  "judge": {"kind": "deterministic", "identity": "acceptance-dsl"},
  "tasks": %s
}`

func gatePreregFor(n int) string {
	j, _ := json.Marshal(gateTaskList(n))
	return fmt.Sprintf(gatePrereg, j)
}

// gateHex is a 64-character lower-case hex string made from a seed word.
func gateHex(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

type frozen struct {
	dir, baseline, hash, corpus string
}

func freezeSuperiority(t *testing.T, n int, incumbent *eval.Arm) frozen {
	t.Helper()
	return freezeWith(t, gatePreregFor(n), incumbent)
}

// freezeModelJudge freezes a 16-task superiority baseline whose pre-registered judge is a model.
func freezeModelJudge(t *testing.T) frozen {
	t.Helper()
	j, _ := json.Marshal(gateTaskList(16))
	return freezeWith(t, fmt.Sprintf(`{"mode":"superiority","primary":{"name":"goal_reached","class":"unit_interval"},"judge":{"kind":"model","identity":"m"},"tasks":%s}`, j), nil)
}

func freezeWith(t *testing.T, preregJSON string, incumbent *eval.Arm) frozen {
	t.Helper()
	dir := t.TempDir()
	f := frozen{dir: dir, corpus: gateWrite(t, dir, "corpus.txt", "a redacted corpus\n")}
	prereg := gateWrite(t, dir, "prereg.json", preregJSON)
	f.baseline = filepath.Join(dir, "baseline.json")
	args := []string{"freeze", "--prereg", prereg, "--corpus", f.corpus, "--out", f.baseline}
	if incumbent != nil {
		args = append(args, "--incumbent", gateWrite(t, dir, "incumbent.json", *incumbent))
	}
	out, errs, code := runGateCmd(t, args...)
	if code != 0 {
		t.Fatalf("freeze failed (%d): %s", code, errs)
	}
	var printed struct {
		Baseline     string `json:"baseline"`
		BaselineHash string `json:"baseline_hash"`
	}
	if err := json.Unmarshal([]byte(out), &printed); err != nil {
		t.Fatalf("freeze must print JSON, got %q: %v", out, err)
	}
	if printed.Baseline != f.baseline || len(printed.BaselineHash) != 64 {
		t.Fatalf("freeze printed %+v", printed)
	}
	f.hash = printed.BaselineHash
	return f
}

func TestGateCmdFreezeCheckVerdictRoundTrip(t *testing.T) {
	inc := gateArm("inc", 16, 0)
	f := freezeSuperiority(t, 16, &inc)

	// check prints the hash freeze printed, and --expect-hash agrees.
	out, errs, code := runGateCmd(t, "check", "--baseline", f.baseline)
	if code != 0 || strings.TrimSpace(out) != f.hash {
		t.Fatalf("check: code %d out %q err %q, want hash %s", code, out, errs, f.hash)
	}
	if _, errs, code = runGateCmd(t, "check", "--baseline", f.baseline, "--expect-hash", f.hash); code != 0 {
		t.Fatalf("check --expect-hash with the right hash failed: %s", errs)
	}

	// verdict: the frozen incumbent is used, the candidate is clearly better.
	in := eval.GateInput{
		BaselineHash: f.hash,
		Candidate:    gateArm("cand", 16, 1),
		Judge:        eval.Judge{Kind: "deterministic", Identity: "acceptance-dsl"},
	}
	input := gateWrite(t, f.dir, "input.json", in)
	out1, errs, code := runGateCmd(t, "verdict", "--baseline", f.baseline, "--input", input)
	if code != 0 {
		t.Fatalf("verdict failed (%d): %s", code, errs)
	}
	var res eval.GateResult
	if err := json.Unmarshal([]byte(out1), &res); err != nil {
		t.Fatalf("verdict must print the result JSON: %v\n%s", err, out1)
	}
	if res.Verdict != eval.VerdictAdopt || res.KeepIncumbent || res.BaselineHash != f.hash || res.GateBuildSHA256 != gateTestBuild {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Stamps.Incumbent != gateStamps("inc") || res.Stamps.Candidate != gateStamps("cand") {
		t.Fatalf("stamps must come from the arms: %+v", res.Stamps)
	}
	// Run twice: byte-identical output, nothing time-dependent in it.
	out2, _, _ := runGateCmd(t, "verdict", "--baseline", f.baseline, "--input", input)
	if out1 != out2 {
		t.Fatalf("verdict output is not deterministic:\n%s\n%s", out1, out2)
	}
	// --require-adopt on an ADOPT is a plain success.
	if _, errs, code = runGateCmd(t, "verdict", "--baseline", f.baseline, "--input", input, "--require-adopt"); code != 0 {
		t.Fatalf("--require-adopt on ADOPT must exit 0, got %d: %s", code, errs)
	}
	// An incumbent in the input equal to the frozen one is accepted too.
	same := in
	same.Incumbent = &inc
	if _, errs, code = runGateCmd(t, "verdict", "--baseline", f.baseline, "--input", gateWrite(t, f.dir, "input-with-inc.json", same)); code != 0 {
		t.Fatalf("an equal incumbent must be accepted: %s", errs)
	}
	// The verb wrote nothing but its inputs and the baseline.
	entries, _ := os.ReadDir(f.dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"baseline.json", "corpus.txt", "incumbent.json", "input-with-inc.json", "input.json", "prereg.json"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("files in the work dir: %v, want %v", names, want)
	}
}

func TestGateCmdExitCodeMapping(t *testing.T) {
	inc := gateArm("inc", 16, 0.5)
	f := freezeSuperiority(t, 16, &inc)
	judge := eval.Judge{Kind: "deterministic", Identity: "acceptance-dsl"}

	// A tie: a verdict is produced (exit 0) and it is INCONCLUSIVE; --require-adopt makes it exit 1
	// but still prints the result.
	tie := gateWrite(t, f.dir, "tie.json", eval.GateInput{BaselineHash: f.hash, Candidate: gateArm("cand", 16, 0.5), Judge: judge})
	out, errs, code := runGateCmd(t, "verdict", "--baseline", f.baseline, "--input", tie)
	if code != 0 || !strings.Contains(out, `"verdict": "INCONCLUSIVE"`) {
		t.Fatalf("an INCONCLUSIVE verdict is still a produced verdict: code %d out %.80s err %s", code, out, errs)
	}
	out, errs, code = runGateCmd(t, "verdict", "--baseline", f.baseline, "--input", tie, "--require-adopt")
	if code != 1 || !strings.Contains(out, `"verdict": "INCONCLUSIVE"`) || !strings.Contains(errs, "require-adopt") {
		t.Fatalf("--require-adopt on a non-ADOPT must exit 1 and still print the result: code %d out %.60s err %s", code, out, errs)
	}
	// BLOCK under --require-adopt is 1 as well.
	worse := gateWrite(t, f.dir, "worse.json", eval.GateInput{BaselineHash: f.hash, Candidate: gateArm("cand", 16, 0), Judge: judge})
	_, _, code = runGateCmd(t, "verdict", "--baseline", f.baseline, "--input", worse, "--require-adopt")
	if code != 1 {
		t.Fatalf("BLOCK under --require-adopt must exit 1, got %d", code)
	}

	// Refusals are 2, with or without --require-adopt, and print no result.
	fm := freezeModelJudge(t)
	mj := eval.Judge{Kind: "model", Identity: "m"}
	goodReceipt := eval.JudgeReceipt{ScorerID: "m", PairsHash: gateHex("pairs"), BuildSHA: gateHex("build"), Passed: true}
	emptyHashes := goodReceipt
	emptyHashes.PairsHash, emptyHashes.BuildSHA = "", ""
	failing := goodReceipt
	failing.Passed = false
	type badCase struct {
		f  frozen
		in eval.GateInput
	}
	bad := map[string]badCase{
		"bad hash":              {f, eval.GateInput{BaselineHash: strings.Repeat("0", 64), Candidate: gateArm("cand", 16, 1), Judge: judge}},
		"model without receipt": {fm, eval.GateInput{BaselineHash: fm.hash, Candidate: gateArm("cand", 16, 1), Judge: mj}},
		"model failing receipt": {fm, eval.GateInput{BaselineHash: fm.hash, Candidate: gateArm("cand", 16, 1), Judge: mj, Receipt: &failing}},
		"model empty hashes":    {fm, eval.GateInput{BaselineHash: fm.hash, Candidate: gateArm("cand", 16, 1), Judge: mj, Receipt: &emptyHashes}},
		"unknown judge kind":    {f, eval.GateInput{BaselineHash: f.hash, Candidate: gateArm("cand", 16, 1), Judge: eval.Judge{Kind: "ensemble", Identity: "m"}}},
		"other judge identity":  {f, eval.GateInput{BaselineHash: f.hash, Candidate: gateArm("cand", 16, 1), Judge: eval.Judge{Kind: "deterministic", Identity: "another-scorer"}}},
		"other judge kind":      {fm, eval.GateInput{BaselineHash: fm.hash, Candidate: gateArm("cand", 16, 1), Judge: eval.Judge{Kind: "deterministic", Identity: "m"}}},
	}
	for name, bc := range bad {
		for _, extra := range [][]string{nil, {"--require-adopt"}} {
			args := append([]string{"verdict", "--baseline", bc.f.baseline, "--input", gateWrite(t, bc.f.dir, "bad.json", bc.in)}, extra...)
			out, errs, code := runGateCmd(t, args...)
			if code != 2 || out != "" || !strings.Contains(errs, "gate: refused") {
				t.Errorf("%s %v: want exit 2 with no stdout, got %d %q %q", name, extra, code, out, errs)
			}
		}
	}
	// Malformed input: not JSON, an unknown field, trailing data, a missing file.
	for name, raw := range map[string]string{
		"not json":      "this is not json",
		"unknown field": `{"baseline_hash": "` + f.hash + `", "extra": 1}`,
		"trailing data": `{"baseline_hash": "` + f.hash + `"} {}`,
		"empty":         ``,
	} {
		_, _, code := runGateCmd(t, "verdict", "--baseline", f.baseline, "--input", gateWrite(t, f.dir, "malformed.json", raw))
		if code != 2 {
			t.Errorf("malformed input (%s) must exit 2, got %d", name, code)
		}
	}
	if _, _, code := runGateCmd(t, "verdict", "--baseline", f.baseline, "--input", filepath.Join(f.dir, "missing.json")); code != 2 {
		t.Errorf("a missing input file must exit 2, got %d", code)
	}
	// A BOM written by a Windows editor does not matter for the input.
	withBOM := gateWrite(t, f.dir, "bom.json", append([]byte("\xef\xbb\xbf"), mustJSON(t, eval.GateInput{BaselineHash: f.hash, Candidate: gateArm("cand", 16, 1), Judge: judge})...))
	if _, errs, code := runGateCmd(t, "verdict", "--baseline", f.baseline, "--input", withBOM); code != 0 {
		t.Errorf("a BOM must be tolerated on the input: %d %s", code, errs)
	}
	// Usage errors are 2.
	for _, args := range [][]string{
		nil, {"nonsense"}, {"verdict"}, {"verdict", "--baseline", f.baseline}, {"check"}, {"freeze"},
		{"verdict", "--baseline", f.baseline, "--input", tie, "extra-positional"}, {"verdict", "--no-such-flag"},
		{"schema", "extra"},
	} {
		if _, _, code := runGateCmd(t, args...); code != 2 {
			t.Errorf("gate %v must exit 2, got %d", args, code)
		}
	}
	// Help is not an error.
	if out, _, code := runGateCmd(t, "help"); code != 0 || !strings.Contains(out, "Exit codes") {
		t.Errorf("help: code %d out %.40s", code, out)
	}
	if _, _, code := runGateCmd(t, "verdict", "-h"); code != 0 {
		t.Errorf("verdict -h must not fail, got %d", code)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	j, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestGateCmdCheckRefusals(t *testing.T) {
	f := freezeSuperiority(t, 16, nil)
	raw, _ := os.ReadFile(f.baseline)
	// A hand edit that breaks the canonical form is refused; so is CRLF (a checkout with
	// autocrlf would do this to the file).
	for name, edited := range map[string][]byte{
		"crlf":             bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n")),
		"extra space":      bytes.Replace(raw, []byte(`"mode": `), []byte(`"mode":  `), 1),
		"no final newline": bytes.TrimSuffix(raw, []byte("\n")),
		"truncated":        raw[:len(raw)/2],
	} {
		p := gateWrite(t, f.dir, "edited.json", edited)
		_, errs, code := runGateCmd(t, "check", "--baseline", p)
		if code != 2 {
			t.Errorf("%s: check must exit 2, got %d (%s)", name, code, errs)
		}
		if name != "truncated" && !strings.Contains(errs, "canonical form") {
			t.Errorf("%s: want the canonical-form refusal, got %q", name, errs)
		}
		if _, _, code := runGateCmd(t, "verdict", "--baseline", p, "--input", p); code != 2 {
			t.Errorf("%s: verdict must refuse the edited baseline, got %d", name, code)
		}
	}
	// An edit that leaves a valid, canonical baseline is a different pre-registration: its hash is
	// different, and --expect-hash (the hash freeze printed) refuses it (see also
	// TestGateCmdOneByteEditFailsCheckOrChangesTheHash).
	other := bytes.Replace(raw, []byte("goal_reached"), []byte("goal_reachee"), 1)
	p := gateWrite(t, f.dir, "other.json", other)
	out, _, code := runGateCmd(t, "check", "--baseline", p)
	if code != 0 || strings.TrimSpace(out) == f.hash {
		t.Fatalf("a valid edit is a valid baseline with another hash: code %d hash %q", code, out)
	}
	if _, errs, code := runGateCmd(t, "check", "--baseline", p, "--expect-hash", f.hash); code != 2 || !strings.Contains(errs, "not the file that was frozen") {
		t.Errorf("--expect-hash must refuse another baseline: %d %s", code, errs)
	}
	// A verdict input that names the old hash cannot be judged under the edited baseline.
	in := gateWrite(t, f.dir, "old-hash.json", eval.GateInput{BaselineHash: f.hash, Incumbent: ptr(gateArm("inc", 16, 0)), Candidate: gateArm("cand", 16, 1), Judge: eval.Judge{Kind: "deterministic", Identity: "x"}})
	if _, errs, code := runGateCmd(t, "verdict", "--baseline", p, "--input", in); code != 2 || !strings.Contains(errs, "baseline_hash") {
		t.Errorf("an input bound to another hash must be refused: %d %s", code, errs)
	}
	if _, _, code := runGateCmd(t, "check", "--baseline", filepath.Join(f.dir, "missing.json")); code != 2 {
		t.Errorf("a missing baseline must exit 2, got %d", code)
	}
}

func ptr[T any](v T) *T { return &v }

func TestGateCmdFreezeRefusalsAndWriteOnce(t *testing.T) {
	dir := t.TempDir()
	corpus := gateWrite(t, dir, "corpus.txt", "corpus\n")
	tasks := gateTaskList(20)
	tj, _ := json.Marshal(tasks)
	good := gateWrite(t, dir, "good.json", fmt.Sprintf(gatePrereg, tj))

	out := filepath.Join(dir, "baseline.json")
	stdout1, errs, code := runGateCmd(t, "freeze", "--prereg", good, "--corpus", corpus, "--out", out)
	if code != 0 {
		t.Fatalf("freeze: %d %s", code, errs)
	}
	// Freezing the same inputs again to the same path is the same pre-registration: fine, same hash.
	stdout2, errs, code := runGateCmd(t, "freeze", "--prereg", good, "--corpus", corpus, "--out", out)
	if code != 0 || stdout1 != stdout2 {
		t.Fatalf("an identical re-freeze must succeed with the same output: %d %s\n%s\n%s", code, errs, stdout1, stdout2)
	}
	// A different pre-registration at the same path is refused and the file is untouched.
	before, _ := os.ReadFile(out)
	different := gateWrite(t, dir, "different.json", fmt.Sprintf(`{"mode":"superiority","primary":{"name":"goal_reached","class":"unit_interval"},"judge":{"kind":"deterministic","identity":"acceptance-dsl"},"seed":2,"tasks":%s}`, tj))
	_, errs, code = runGateCmd(t, "freeze", "--prereg", different, "--corpus", corpus, "--out", out)
	after, _ := os.ReadFile(out)
	if code != 2 || !strings.Contains(errs, "written once") || !bytes.Equal(before, after) {
		t.Fatalf("a changed pre-registration must not overwrite: %d %s", code, errs)
	}

	// INV-5: seconds, latency, wall, duration and time are refused as efficiency classes, citing it,
	// and no baseline file is left behind.
	for _, class := range []string{"seconds", "latency", "wall", "duration", "time"} {
		pre := gateWrite(t, dir, "ni-"+class+".json", fmt.Sprintf(`{"mode":"non_inferiority","primary":{"name":"q","class":"judge_0_10"},"judge":{"kind":"deterministic","identity":"acceptance-dsl"},"efficiency":[{"name":"cost","class":%q}],"tasks":%s}`, class, tj))
		target := filepath.Join(dir, "never-"+class+".json")
		_, errs, code := runGateCmd(t, "freeze", "--prereg", pre, "--corpus", corpus, "--out", target)
		if code != 2 || !strings.Contains(errs, "INV-5") {
			t.Errorf("class %q: want exit 2 citing INV-5, got %d %s", class, code, errs)
		}
		if _, err := os.Stat(target); err == nil {
			t.Errorf("class %q: a refused freeze left a file behind", class)
		}
	}
	// Unknown pre-registration fields are refused (a typo must not silently take a default).
	typo := gateWrite(t, dir, "typo.json", fmt.Sprintf(`{"mode":"superiority","primary":{"name":"q","class":"unit_interval"},"judge":{"kind":"deterministic","identity":"acceptance-dsl"},"min_pair":4,"tasks":%s}`, tj))
	if _, _, code := runGateCmd(t, "freeze", "--prereg", typo, "--corpus", corpus, "--out", filepath.Join(dir, "typo-out.json")); code != 2 {
		t.Errorf("an unknown pre-registration field must be refused, got %d", code)
	}
	// Missing corpus, empty corpus, missing flags.
	for name, args := range map[string][]string{
		"missing corpus": {"freeze", "--prereg", good, "--corpus", filepath.Join(dir, "nope"), "--out", filepath.Join(dir, "o1.json")},
		"empty corpus":   {"freeze", "--prereg", good, "--corpus", gateWrite(t, dir, "empty.txt", ""), "--out", filepath.Join(dir, "o2.json")},
		"no out":         {"freeze", "--prereg", good, "--corpus", corpus},
	} {
		if _, _, code := runGateCmd(t, args...); code != 2 {
			t.Errorf("%s: want exit 2, got %d", name, code)
		}
	}
	// An incumbent arm that names an undeclared task is refused.
	arm := gateArm("inc", 2, 0)
	arm.Tasks[0].ID = "undeclared"
	_, errs, code = runGateCmd(t, "freeze", "--prereg", good, "--corpus", corpus, "--incumbent", gateWrite(t, dir, "badarm.json", arm), "--out", filepath.Join(dir, "o3.json"))
	if code != 2 || !strings.Contains(errs, "not in the baseline's declared tasks") {
		t.Errorf("incumbent with an undeclared task: %d %s", code, errs)
	}
}

func TestGateCmdSchemaMatchesTheEnum(t *testing.T) {
	out, errs, code := runGateCmd(t, "schema")
	if code != 0 || errs != "" {
		t.Fatalf("schema: %d %s", code, errs)
	}
	if out != string(eval.GateInputSchema) {
		t.Fatal("schema must print the embedded schema byte for byte")
	}
	var doc struct {
		Schema string `json:"$schema"`
		Defs   struct {
			Outcome struct {
				Enum []string `json:"enum"`
			} `json:"outcome"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("schema output is not JSON: %v", err)
	}
	var want []string
	for _, o := range eval.AllOutcomes() {
		want = append(want, string(o))
	}
	if !reflect.DeepEqual(doc.Defs.Outcome.Enum, want) {
		t.Fatalf("the schema's outcome enum %v != eval.AllOutcomes() %v", doc.Defs.Outcome.Enum, want)
	}
	if !strings.Contains(doc.Schema, "2020-12") {
		t.Fatalf("schema draft: %q", doc.Schema)
	}
}

// The global --config hoist (main.go hoistGlobalConfig) re-attaches `--config <v>` at the FRONT of
// the verb's args, so `local-offload --config x.json gate schema` reaches gateMain as
// [--config x.json schema]. The verb needs no config; it must accept and drop a leading --config
// instead of dispatching on it.
func TestGateCmdAcceptsALeadingGlobalConfig(t *testing.T) {
	f := freezeSuperiority(t, 16, nil)
	want := string(eval.GateInputSchema)
	lead := map[string][]string{
		"--config v":   {"--config", "x.json"},
		"--config=v":   {"--config=x.json"},
		"-config v":    {"-config", "x.json"},
		"-config=v":    {"-config=x.json"},
		"twice, mixed": {"--config", "x.json", "--config=y.json"},
	}
	for name, prefix := range lead {
		t.Run(name, func(t *testing.T) {
			out, errs, code := runGateCmd(t, append(append([]string{}, prefix...), "schema")...)
			if code != 0 || out != want {
				t.Fatalf("gate %v schema: code %d err %q (schema printed: %v)", prefix, code, errs, out == want)
			}
			out, errs, code = runGateCmd(t, append(append([]string{}, prefix...), "check", "--baseline", f.baseline, "--expect-hash", f.hash)...)
			if code != 0 || strings.TrimSpace(out) != f.hash {
				t.Fatalf("gate %v check: code %d out %q err %q", prefix, code, out, errs)
			}
		})
	}

	// The whole path main takes: hoist first, then the verb gets what is left.
	sub, rest, ok := hoistGlobalConfig([]string{"--config", "x.json", "gate", "schema"})
	if !ok || sub != "gate" {
		t.Fatalf("hoist: %q %v %v", sub, rest, ok)
	}
	if out, errs, code := runGateCmd(t, rest...); code != 0 || out != want {
		t.Fatalf("local-offload --config x.json gate schema: code %d err %q", code, errs)
	}

	// A --config after the subcommand still parses (the per-verb flag), and the verdict verb takes it too.
	if out, errs, code := runGateCmd(t, "schema", "--config", "x.json"); code != 0 || out != want {
		t.Fatalf("schema --config x.json: code %d err %q", code, errs)
	}

	// A usage error names the real mistake, not the config flag.
	_, errs, code := runGateCmd(t, "--config", "x.json", "nonsense")
	if code != 2 || !strings.Contains(errs, `"nonsense"`) || strings.Contains(errs, `"--config"`) {
		t.Fatalf("an unknown subcommand after --config: code %d err %q", code, errs)
	}
	// --config with no subcommand is a usage error, not a panic and not a verdict.
	for _, args := range [][]string{{"--config", "x.json"}, {"--config=x.json"}, {"--config"}} {
		if _, _, code := runGateCmd(t, args...); code != 2 {
			t.Errorf("gate %v must exit 2, got %d", args, code)
		}
	}
	// help still works behind it.
	if out, _, code := runGateCmd(t, "--config", "x.json", "help"); code != 0 || !strings.Contains(out, "Exit codes") {
		t.Errorf("--config x.json help: code %d", code)
	}
}

// A one-byte edit of a frozen baseline either fails `check` or changes its hash, and
// `gate check --expect-hash` and the input's baseline_hash refuse it.
func TestGateCmdOneByteEditFailsCheckOrChangesTheHash(t *testing.T) {
	f := freezeSuperiority(t, 16, nil)
	raw, _ := os.ReadFile(f.baseline)
	judge := eval.Judge{Kind: "deterministic", Identity: "acceptance-dsl"}
	bound := gateWrite(t, f.dir, "bound.json", eval.GateInput{BaselineHash: f.hash, Candidate: gateArm("cand", 16, 1), Incumbent: ptr(gateArm("inc", 16, 0)), Judge: judge})
	differing := func(a, b []byte) (n int) {
		if len(a) != len(b) {
			t.Fatalf("a one-byte edit keeps the length: %d vs %d", len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				n++
			}
		}
		return n
	}

	// 1. One byte that leaves a valid canonical document: seed 1 -> 2. check accepts it and
	// prints ANOTHER hash; --expect-hash and the bound input refuse it.
	valid := bytes.Replace(raw, []byte(`"seed": 1`), []byte(`"seed": 2`), 1)
	if differing(raw, valid) != 1 {
		t.Fatal("setup: the edit must change exactly one byte")
	}
	p := gateWrite(t, f.dir, "seed-2.json", valid)
	out, errs, code := runGateCmd(t, "check", "--baseline", p)
	if code != 0 || len(strings.TrimSpace(out)) != 64 || strings.TrimSpace(out) == f.hash {
		t.Fatalf("a valid one-byte edit changes the hash: code %d hash %q (frozen %s) err %q", code, out, f.hash, errs)
	}
	if _, errs, code = runGateCmd(t, "check", "--baseline", p, "--expect-hash", f.hash); code != 2 || !strings.Contains(errs, f.hash) {
		t.Fatalf("--expect-hash must refuse the edited baseline and name the expected hash: %d %s", code, errs)
	}
	if _, errs, code = runGateCmd(t, "verdict", "--baseline", p, "--input", bound); code != 2 || !strings.Contains(errs, "baseline_hash") {
		t.Fatalf("an input bound to the frozen hash must be refused under the edited baseline: %d %s", code, errs)
	}

	// 2. One byte that breaks the document: the first brace. check itself fails.
	broken := append([]byte("z"), raw[1:]...)
	if differing(raw, broken) != 1 {
		t.Fatal("setup: the edit must change exactly one byte")
	}
	pb := gateWrite(t, f.dir, "broken.json", broken)
	if _, _, code = runGateCmd(t, "check", "--baseline", pb); code != 2 {
		t.Fatalf("a one-byte edit that breaks the document must fail check, got %d", code)
	}
	if _, _, code = runGateCmd(t, "check", "--baseline", pb, "--expect-hash", f.hash); code != 2 {
		t.Fatalf("and fail it with --expect-hash too, got %d", code)
	}

	// 3. Control: the unedited baseline passes check, --expect-hash and the bound input.
	if out, _, code := runGateCmd(t, "check", "--baseline", f.baseline, "--expect-hash", f.hash); code != 0 || strings.TrimSpace(out) != f.hash {
		t.Fatalf("control: the frozen baseline must pass --expect-hash: %d %q", code, out)
	}
	if _, errs, code := runGateCmd(t, "verdict", "--baseline", f.baseline, "--input", bound); code != 0 {
		t.Fatalf("control: the bound input must be judged under the frozen baseline: %d %s", code, errs)
	}
}
