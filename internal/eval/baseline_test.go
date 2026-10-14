package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

var testCorpus = []byte("corpus line 1\ncorpus line 2\n")

func testStamps(tag string) Stamps {
	return Stamps{HarnessBuildSHA256: "h-" + tag, SeatConfigSHA256: "s-" + tag, EngineBuild: "e-" + tag}
}

func taskIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("t%03d", i)
	}
	return ids
}

func boolp(v bool) *bool { return &v }

func testPreReg(mode string) PreReg {
	p := PreReg{
		Mode:    mode,
		Primary: PreRegPrimary{Name: "answer_quality", Class: ClassJudge010},
		Judge:   Judge{Kind: "deterministic", Identity: scorerID},
		Tasks:   taskIDs(20),
		HeldOut: []string{"t019", "t018"},
	}
	if mode == ModeNonInferiority {
		p.Efficiency = []PreRegEfficiency{{Name: "forwarded_tokens", Class: EffTokens}}
	}
	return p
}

func refusalOf(t *testing.T, err error) *Refusal {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a *Refusal, got %T: %v", err, err)
	}
	return r
}

// testIncumbentArm is a fresh incumbent arm with its tasks out of id order and one unresolved metric.
func testIncumbentArm() Arm {
	return Arm{Stamps: testStamps("inc"), Tasks: []Task{
		{ID: "t002", Attempts: []Attempt{{Outcome: OutcomePass, Score: f64(8), Metrics: map[string]*float64{"forwarded_tokens": f64(100)}}}},
		{ID: "t000", Attempts: []Attempt{{Outcome: OutcomeInfraError}}},
		{ID: "t001", Attempts: []Attempt{{Outcome: OutcomePass, Score: f64(7.5), Metrics: map[string]*float64{"forwarded_tokens": nil}}}},
	}}
}

// Freezing twice from the same inputs is byte-identical, and check of the frozen bytes passes
// and reproduces them. The edit half is TestOneByteEditOfAFrozenBaselineFailsCheckOrChangesItsHash.
func TestFreezeThenCheckReproducesByteIdentically(t *testing.T) {
	inc := testIncumbentArm()
	b1, raw1, h1, err := FreezeBaseline(testPreReg(ModeNonInferiority), testCorpus, &inc)
	if err != nil {
		t.Fatal(err)
	}
	b2, raw2, h2, err := FreezeBaseline(testPreReg(ModeNonInferiority), testCorpus, &inc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw1, raw2) || h1 != h2 {
		t.Fatal("freezing the same inputs twice must give identical bytes and hash")
	}
	sum := sha256.Sum256(raw1)
	if h1 != hex.EncodeToString(sum[:]) {
		t.Fatalf("BaselineHash %s is not sha256 of the canonical bytes", h1)
	}
	if !bytes.HasSuffix(raw1, []byte("}\n")) || bytes.Contains(raw1, []byte("\r")) {
		t.Fatalf("canonical bytes must be LF only with one trailing newline: %q", raw1[len(raw1)-4:])
	}
	if b1.Incumbent.Tasks[0].ID != "t000" || b1.Incumbent.Tasks[2].ID != "t002" {
		t.Fatalf("freeze must sort the incumbent's tasks by id, got %v", b1.Incumbent.Tasks)
	}
	if inc.Tasks[0].ID != "t002" {
		t.Fatal("freeze mutated the caller's incumbent arm")
	}
	if b1.HeldOut[0] != "t018" || b2.HeldOut[1] != "t019" {
		t.Fatalf("held_out must be sorted, got %v", b1.HeldOut)
	}
	// The unresolved metric survives the freeze as null, not 0.
	if !bytes.Contains(raw1, []byte(`"forwarded_tokens": null`)) {
		t.Fatalf("the frozen incumbent lost its unresolved metric:\n%s", raw1)
	}

	got, h3, err := CheckBaseline(raw1)
	if err != nil {
		t.Fatalf("check of the frozen bytes: %v", err)
	}
	if h3 != h1 {
		t.Fatalf("check hash %s != freeze hash %s", h3, h1)
	}
	canon, _ := canonicalBaseline(got)
	if !bytes.Equal(canon, raw1) {
		t.Fatal("the baseline check returns does not re-marshal to the frozen bytes")
	}

	// A different pre-registration is a different hash.
	p := testPreReg(ModeNonInferiority)
	seed := int64(2)
	p.Seed = &seed
	_, _, h4, err := FreezeBaseline(p, testCorpus, &inc)
	if err != nil || h4 == h1 {
		t.Fatalf("a changed seed must change the hash (err %v)", err)
	}
	_, _, h5, _ := FreezeBaseline(testPreReg(ModeNonInferiority), []byte("another corpus"), &inc)
	if h5 == h1 {
		t.Fatal("a different corpus must change the hash")
	}
}

// A one-byte edit of a frozen baseline either fails `check` or changes its hash, and
// `gate check --expect-hash` and the input's baseline_hash refuse it.
//
// check alone cannot refuse an edit that leaves a valid canonical document (a letter inside a
// name, the value of min_pairs): that is another pre-registration, and it hashes to another hash.
// So the test does not assert that every edit fails check. For every one-byte edit it asserts the
// other half of the sentence: the edit fails check, or it is a valid baseline whose hash is not
// the frozen one AND a gate input that names the frozen hash is refused under it. The root test
// TestGateCmdOneByteEditFailsCheckOrChangesTheHash drives `--expect-hash` through the verb.
func TestOneByteEditOfAFrozenBaselineFailsCheckOrChangesItsHash(t *testing.T) {
	inc := testIncumbentArm()
	_, raw1, h1, err := FreezeBaseline(testPreReg(ModeNonInferiority), testCorpus, &inc)
	if err != nil {
		t.Fatal(err)
	}
	// An input bound to the frozen hash, as a verdict adapter holds it.
	bound := GateInput{BaselineHash: h1, Candidate: Arm{Stamps: testStamps("cand")}, Judge: detJudge()}
	var outright, rehashed int
	for i := 0; i < len(raw1); i++ {
		edited := append([]byte{}, raw1...)
		edited[i] ^= 0x01
		b, h, err := CheckBaseline(edited)
		if err != nil {
			outright++ // fails check
			continue
		}
		rehashed++ // a valid, canonical document: another pre-registration
		if h == h1 {
			t.Fatalf("a one-byte edit at offset %d left a valid baseline with the frozen hash", i)
		}
		// The input that names the frozen hash is refused under the edited baseline, with the
		// hash binding as the reason (not some later check).
		if _, gerr := Gate(b, h, bound); gerr == nil || !strings.Contains(gerr.Error(), "input baseline_hash") {
			t.Fatalf("the edit at offset %d must be refused by the input's baseline_hash binding, got %v", i, gerr)
		}
	}
	if outright == 0 || rehashed == 0 {
		t.Fatalf("expected both kinds of edit (fails check %d, valid but another hash %d)", outright, rehashed)
	}
	// The frozen bytes themselves are accepted by the same input's binding (the control).
	if b, h, err := CheckBaseline(raw1); err != nil || h != h1 {
		t.Fatalf("control: %v", err)
	} else if _, gerr := Gate(b, h, bound); gerr != nil && strings.Contains(gerr.Error(), "input baseline_hash") {
		t.Fatalf("control: the frozen baseline must pass the hash binding, got %v", gerr)
	}
	// Whitespace edits fail check too: this is a hash of bytes, not a parse.
	for _, edited := range [][]byte{
		bytes.Replace(raw1, []byte("\n"), []byte("\r\n"), -1),
		append(append([]byte{}, raw1...), '\n'),
		bytes.TrimSuffix(raw1, []byte("\n")),
		bytes.Replace(raw1, []byte("0.5"), []byte("0.50"), 1),
	} {
		_, _, err := CheckBaseline(edited)
		if err == nil || !strings.Contains(err.Error(), "canonical form") {
			t.Fatalf("a whitespace edit must be refused as non-canonical, got %v", err)
		}
	}
}

func TestFreezeFillsDefaults(t *testing.T) {
	b, _, _, err := FreezeBaseline(testPreReg(ModeNonInferiority), testCorpus, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256(testCorpus))
	if b.GateVersion != GateVersion || GateVersion != "2" || b.MinPairs != 16 || b.BootstrapB != 10000 || b.Seed != 1 || b.MaxExcludedShare != 0.20 {
		t.Fatalf("defaults wrong: %+v", b)
	}
	if b.CorpusHash != want {
		t.Fatalf("corpus_hash %s != sha256 of the corpus %s", b.CorpusHash, want)
	}
	if !b.Primary.HigherIsBetter || b.Primary.Margin != 0.5 {
		t.Fatalf("judge_0_10 default margin must be 0.5 and higher_is_better true, got %+v", b.Primary)
	}
	if len(b.Efficiency) != 1 || !b.Efficiency[0].LowerIsBetter {
		t.Fatalf("efficiency lower_is_better defaults to true: %+v", b.Efficiency)
	}
	if b.Incumbent != nil {
		t.Fatal("no incumbent given, none frozen")
	}
	// Explicit values win over defaults, including zero ones that mean something.
	p := testPreReg(ModeNonInferiority)
	p.MinPairs, p.BootstrapB = 17, 3000
	seed, share := int64(0), 0.0
	p.Seed, p.MaxExcludedShare = &seed, &share
	b, _, _, err = FreezeBaseline(p, testCorpus, nil)
	if err != nil || b.MinPairs != 17 || b.BootstrapB != 3000 || b.Seed != 0 || b.MaxExcludedShare != 0 {
		t.Fatalf("explicit settings must be kept: %+v err %v", b, err)
	}
}

func TestFreezeMarginDefaultsAndSuperiority(t *testing.T) {
	for _, tc := range []struct {
		class  string
		margin *float64
		rng    []float64
		want   float64
	}{
		{ClassUnitInterval, nil, nil, 0.05},
		{ClassJudge010, nil, nil, 0.5},
		{ClassCustom, f64(2.5), []float64{0, 10}, 2.5},
		{ClassUnitInterval, f64(0.1), nil, 0.1},
	} {
		p := testPreReg(ModeNonInferiority)
		p.Primary.Class, p.Primary.Margin, p.Primary.Range = tc.class, tc.margin, tc.rng
		b, _, _, err := FreezeBaseline(p, testCorpus, nil)
		if err != nil || b.Primary.Margin != tc.want {
			t.Errorf("class %s margin %v: got %v err %v, want %v", tc.class, tc.margin, b.Primary.Margin, err, tc.want)
		}
	}
	// Superiority records the margin as 0 whatever was written.
	p := testPreReg(ModeSuperiority)
	p.Primary.Class, p.Primary.Margin = ClassUnitInterval, f64(0.3)
	b, _, _, err := FreezeBaseline(p, testCorpus, nil)
	if err != nil || b.Primary.Margin != 0 {
		t.Fatalf("superiority must record margin 0, got %v err %v", b.Primary.Margin, err)
	}
	// A custom class carries its own margin, in non-inferiority.
	p = testPreReg(ModeNonInferiority)
	p.Primary.Class, p.Primary.Range = ClassCustom, []float64{0, 10}
	if _, _, _, err = FreezeBaseline(p, testCorpus, nil); err == nil || !strings.Contains(err.Error(), "own margin") {
		t.Fatalf("custom without a margin must be refused, got %v", err)
	}
	// ... and a custom superiority needs none (the margin is ignored), but it does need its range.
	p = testPreReg(ModeSuperiority)
	p.Primary.Class, p.Primary.Range = ClassCustom, []float64{0, 10}
	if _, _, _, err = FreezeBaseline(p, testCorpus, nil); err != nil {
		t.Fatalf("custom superiority needs no margin: %v", err)
	}
	// A non-positive or non-finite margin is refused.
	for _, m := range []float64{0, -0.1} {
		p = testPreReg(ModeNonInferiority)
		p.Primary.Margin = f64(m)
		if _, _, _, err = FreezeBaseline(p, testCorpus, nil); err == nil {
			t.Errorf("margin %v must be refused", m)
		}
	}
}

// INV-5: no efficiency class names time; the message cites it.
func TestFreezeRefusesWallTimeEfficiencyClasses(t *testing.T) {
	for _, class := range []string{"seconds", "latency", "wall", "duration", "time", "wall_time", "ms", "tok_s", "", "Tokens"} {
		p := testPreReg(ModeNonInferiority)
		p.Efficiency = []PreRegEfficiency{{Name: "forwarded_tokens", Class: class}}
		_, _, _, err := FreezeBaseline(p, testCorpus, nil)
		r := refusalOf(t, err)
		if !strings.Contains(r.Reason, "INV-5") {
			t.Errorf("class %q: the refusal must cite INV-5, got %q", class, r.Reason)
		}
	}
	for _, class := range []string{EffTokens, EffMoney, EffMemoryGiB} {
		p := testPreReg(ModeNonInferiority)
		p.Efficiency = []PreRegEfficiency{{Name: "x_" + class, Class: class}}
		if _, _, _, err := FreezeBaseline(p, testCorpus, nil); err != nil {
			t.Errorf("class %s must be allowed: %v", class, err)
		}
	}
	// A wall-time name behind an allowed class is the same bypass.
	for _, name := range []string{"latency_tokens", "tokens_per_second", "wallTime"} {
		p := testPreReg(ModeNonInferiority)
		p.Efficiency = []PreRegEfficiency{{Name: name, Class: EffTokens}}
		if _, _, _, err := FreezeBaseline(p, testCorpus, nil); err == nil || !strings.Contains(err.Error(), "INV-5") {
			t.Errorf("metric name %q must be refused citing INV-5, got %v", name, err)
		}
	}
	p := testPreReg(ModeSuperiority)
	p.Primary.Name = "response_latency"
	if _, _, _, err := FreezeBaseline(p, testCorpus, nil); err == nil || !strings.Contains(err.Error(), "INV-5") {
		t.Errorf("a primary named for latency must be refused citing INV-5, got %v", err)
	}
}

func TestFreezeRefusals(t *testing.T) {
	inc := Arm{Stamps: testStamps("inc"), Tasks: []Task{{ID: "t000", Attempts: []Attempt{{Outcome: OutcomePass, Score: f64(5)}}}}}
	for _, tc := range []struct {
		name   string
		mutate func(p *PreReg)
		corpus []byte
		arm    *Arm
		want   string
	}{
		{"non-inferiority with no efficiency", func(p *PreReg) { p.Efficiency = nil }, testCorpus, nil, "what it saves"},
		{"unknown mode", func(p *PreReg) { p.Mode = "equivalence" }, testCorpus, nil, "mode"},
		{"unknown primary class", func(p *PreReg) { p.Primary.Class = "percent" }, testCorpus, nil, "primary class"},
		{"higher_is_better false", func(p *PreReg) { p.Primary.HigherIsBetter = boolp(false) }, testCorpus, nil, "higher_is_better"},
		{"empty primary name", func(p *PreReg) { p.Primary.Name = "" }, testCorpus, nil, "primary.name"},
		{"min_pairs 1", func(p *PreReg) { p.MinPairs = 1 }, testCorpus, nil, "min_pairs"},
		{"min_pairs negative", func(p *PreReg) { p.MinPairs = -4 }, testCorpus, nil, "min_pairs"},
		{"bootstrap_b negative", func(p *PreReg) { p.BootstrapB = -1 }, testCorpus, nil, "bootstrap_b"},
		{"share above 1", func(p *PreReg) { s := 1.5; p.MaxExcludedShare = &s }, testCorpus, nil, "max_excluded_share"},
		{"share below 0", func(p *PreReg) { s := -0.1; p.MaxExcludedShare = &s }, testCorpus, nil, "max_excluded_share"},
		{"empty corpus", func(p *PreReg) {}, nil, nil, "corpus is empty"},
		{"no tasks", func(p *PreReg) { p.Tasks = nil; p.HeldOut = nil }, testCorpus, nil, "tasks is empty"},
		{"duplicate task", func(p *PreReg) { p.Tasks = append(p.Tasks, "t003") }, testCorpus, nil, "sorted and unique"},
		{"empty task id", func(p *PreReg) { p.Tasks = append(p.Tasks, "") }, testCorpus, nil, "empty id"},
		{"held-out not declared", func(p *PreReg) { p.HeldOut = []string{"zzz"} }, testCorpus, nil, "not in tasks"},
		{"duplicate held-out", func(p *PreReg) { p.HeldOut = []string{"t001", "t001"} }, testCorpus, nil, "held_out must be sorted and unique"},
		{"everything held out", func(p *PreReg) { p.HeldOut = p.Tasks }, testCorpus, nil, "every declared task is held out"},
		{"duplicate efficiency name", func(p *PreReg) {
			p.Efficiency = append(p.Efficiency, PreRegEfficiency{Name: "forwarded_tokens", Class: EffMoney})
		}, testCorpus, nil, "declared twice"},
		{"incumbent task not declared", func(p *PreReg) {}, testCorpus, &Arm{Stamps: testStamps("inc"), Tasks: []Task{{ID: "nope", Attempts: nil}}}, "not in the baseline's declared tasks"},
		{"incumbent scored with no score", func(p *PreReg) {}, testCorpus, &Arm{Stamps: testStamps("inc"), Tasks: []Task{{ID: "t000", Attempts: []Attempt{{Outcome: OutcomePass}}}}}, "no score"},
		{"incumbent duplicate task", func(p *PreReg) {}, testCorpus, &Arm{Stamps: testStamps("inc"), Tasks: []Task{{ID: "t000"}, {ID: "t000"}}}, "sorted by id and unique"},
		{"incumbent without stamps", func(p *PreReg) {}, testCorpus, &Arm{Tasks: inc.Tasks}, "stamps are incomplete"},
		{"incumbent score outside the class range", func(p *PreReg) {}, testCorpus, &Arm{Stamps: testStamps("inc"), Tasks: []Task{{ID: "t000", Attempts: []Attempt{{Outcome: OutcomePass, Score: f64(11)}}}}}, "outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPreReg(ModeNonInferiority)
			tc.mutate(&p)
			_, raw, hash, err := FreezeBaseline(p, tc.corpus, tc.arm)
			if err == nil {
				t.Fatalf("want a refusal containing %q, froze %d bytes", tc.want, len(raw))
			}
			r := refusalOf(t, err)
			if !strings.Contains(r.Reason, tc.want) {
				t.Fatalf("refusal %q does not contain %q", r.Reason, tc.want)
			}
			if raw != nil || hash != "" {
				t.Fatal("a refused freeze must return no bytes and no hash")
			}
		})
	}
}

func TestCheckBaselineRefusals(t *testing.T) {
	_, raw, _, err := FreezeBaseline(testPreReg(ModeNonInferiority), testCorpus, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Structural: not JSON, trailing data, unknown field, wrong version.
	for name, edited := range map[string][]byte{
		"empty":         nil,
		"not json":      []byte("hello"),
		"trailing data": append(append([]byte{}, raw...), []byte("{}")...),
		"unknown field": bytes.Replace(raw, []byte(`"mode"`), []byte(`"extra": 1,`+"\n  "+`"mode"`), 1),
		"bom":           append([]byte("\xef\xbb\xbf"), raw...),
	} {
		if _, _, err := CheckBaseline(edited); err == nil {
			t.Errorf("%s: check must refuse", name)
		} else {
			refusalOf(t, err)
		}
	}
	// A canonical file that breaks a freeze rule is still refused: build it by hand.
	var b Baseline
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(b *Baseline){
		"seconds class":       func(b *Baseline) { b.Efficiency[0].Class = "seconds" },
		"higher_is_better":    func(b *Baseline) { b.Primary.HigherIsBetter = false },
		"zero margin":         func(b *Baseline) { b.Primary.Margin = 0 },
		"gate version":        func(b *Baseline) { b.GateVersion = "99" },
		"unsorted tasks":      func(b *Baseline) { b.Tasks[0], b.Tasks[1] = b.Tasks[1], b.Tasks[0] },
		"short corpus hash":   func(b *Baseline) { b.CorpusHash = "abc" },
		"held-out undeclared": func(b *Baseline) { b.HeldOut = []string{"zzz"} },
	} {
		c := b
		c.Efficiency = append([]EfficiencyMetric{}, b.Efficiency...)
		c.Tasks = append([]string{}, b.Tasks...)
		c.HeldOut = append([]string{}, b.HeldOut...)
		mut(&c)
		hand, err := canonicalBaseline(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := CheckBaseline(hand); err == nil {
			t.Errorf("%s: a canonical but invalid baseline must be refused", name)
		}
	}
	// null lists are not what freeze writes.
	nulls := bytes.Replace(raw, []byte(`"held_out": [
    "t018",
    "t019"
  ]`), []byte(`"held_out": null`), 1)
	if bytes.Equal(nulls, raw) {
		t.Fatal("test setup: the held_out block was not found in the canonical bytes")
	}
	if _, _, err := CheckBaseline(nulls); err == nil || !strings.Contains(err.Error(), "canonical form") {
		t.Errorf("a null list must be refused as non-canonical, got %v", err)
	}
}
