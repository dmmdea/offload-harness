package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// Verdicts.
const (
	VerdictAdopt        = "ADOPT"
	VerdictBlock        = "BLOCK"
	VerdictInconclusive = "INCONCLUSIVE"
)

// Judge names the scorer that produced the scores. It is frozen in the baseline and the input must
// name the same one. A deterministic scorer (the acceptance DSL, a regex, an exact match) needs no
// receipt; a model judge needs a passed self-test receipt for the same scorer, or the gate
// refuses.
type Judge struct {
	Kind     string `json:"kind"`     // "deterministic" | "model"
	Identity string `json:"identity"` // scorer id, e.g. "acceptance-dsl" or a model id
}

// JudgeReceipt is the self-test receipt `gate selftest` (a later change) produces for a model
// judge. Data only here: the gate checks that the scorer matches and that both hashes are 64
// lower-case hex characters, not what they hash.
type JudgeReceipt struct {
	ScorerID  string `json:"scorer_id"`
	PairsHash string `json:"pairs_hash"`
	Passed    bool   `json:"passed"`
	BuildSHA  string `json:"build_sha256"`
}

// GateInput is one comparison: a candidate arm against the incumbent arm under a frozen baseline.
type GateInput struct {
	BaselineHash string        `json:"baseline_hash"`
	Incumbent    *Arm          `json:"incumbent,omitempty"` // required unless the baseline froze one; if both, must be equal
	Candidate    Arm           `json:"candidate"`
	Judge        Judge         `json:"judge"`
	Receipt      *JudgeReceipt `json:"receipt,omitempty"`
}

// OutcomeCounts is the number of attempts of each outcome class on one arm, every class present
// (a zero is a zero, not an absent key).
type OutcomeCounts map[string]int

// AttemptCounts holds the per-arm outcome counts.
type AttemptCounts struct {
	Incumbent OutcomeCounts `json:"incumbent"`
	Candidate OutcomeCounts `json:"candidate"`
}

// ArmCount is one integer per arm.
type ArmCount struct {
	Incumbent int `json:"incumbent"`
	Candidate int `json:"candidate"`
}

// EfficiencyResult is the paired-bootstrap read of one declared efficiency metric over the
// usable pairs that resolved it on both arms. Delta, Lo and Hi are null when fewer than two pairs
// resolved the metric (N < 2): there is no interval to read, and a 0 would say "no change" where
// the truth is "no data". A measured zero is a number.
//
// With k declared metrics, Lo and Hi are the two-sided 1 - 0.05/k interval (Level), so that the
// chance some metric falsely improves stays near 2.5%; Improves reads that interval. Regresses
// reads the plain 95% interval.
type EfficiencyResult struct {
	Name            string   `json:"name"`
	Class           string   `json:"class"`
	LowerIsBetter   bool     `json:"lower_is_better"`
	Delta           *float64 `json:"delta"`
	Lo              *float64 `json:"lo"`
	Hi              *float64 `json:"hi"`
	N               int      `json:"n"`
	UnresolvedPairs int      `json:"unresolved_pairs"`
	Improves        bool     `json:"improves"`
	// OverUnresolvedCap is true when UnresolvedPairs over the usable pairs is above
	// max_excluded_share: the metric was not measured on enough pairs, so it cannot improve.
	OverUnresolvedCap bool `json:"over_unresolved_cap"`
	// Regresses is true when the plain 95% interval lies wholly on the worse side. It is read
	// wherever the metric has an interval (two or more resolved pairs), not only from min_pairs.
	Regresses bool `json:"regresses"`
	// Level is the two-sided level of Lo and Hi: 0.95, or 1 - 0.05/k for k declared metrics.
	Level float64 `json:"level"`
}

// HeldOutResult is the point-estimate check on the held-out pairs (rule R5).
type HeldOutResult struct {
	N     int     `json:"n"`
	Delta float64 `json:"delta"`
	// Failed is true only when N > 0 and the held-out delta is below the threshold.
	Failed bool `json:"failed"`
	// Missing is true when no held-out pair is usable (N == 0): there is no evidence, so an
	// ADOPT is held back as INCONCLUSIVE rather than judged on nothing.
	Missing bool `json:"missing"`
}

// Projected is the half-width of the main interval projected to 16, 24 and 120 usable tasks.
type Projected struct {
	N16  float64 `json:"16"`
	N24  float64 `json:"24"`
	N120 float64 `json:"120"`
}

// TaskCount is a number of tasks, or the word "unbounded" when no finite number follows from the
// interval (superiority with no observed effect, fewer than two pairs).
type TaskCount struct {
	N         int
	Unbounded bool
}

// PowerNote says how far the corpus is from a conclusive size. It adds no statistic: it is the
// half-width of the main bootstrap interval read three ways (the power note of rule R4).
type PowerNote struct {
	HalfWidth   float64   `json:"half_width"`
	Projected   Projected `json:"projected"`
	TasksNeeded TaskCount `json:"tasks_needed"`
	TasksToAdd  TaskCount `json:"tasks_to_add"`
}

// ResultStamps are the stamps of the two runs.
type ResultStamps struct {
	Incumbent Stamps `json:"incumbent"`
	Candidate Stamps `json:"candidate"`
}

// GateResult is the gate's whole output. Field order is the struct's, and nothing in it is a
// timestamp: the same input twice gives byte-identical JSON.
type GateResult struct {
	Verdict       string        `json:"verdict"`
	Reason        string        `json:"reason"`
	KeepIncumbent bool          `json:"keep_incumbent"`
	Mode          string        `json:"mode"`
	Margin        float64       `json:"margin"`
	Delta         float64       `json:"delta"`
	Lo            float64       `json:"lo"`
	Hi            float64       `json:"hi"`
	NUsed         int           `json:"n_used"`
	NDeclared     int           `json:"n_declared"`
	ExcludedPairs int           `json:"excluded_pairs"`
	ExcludedShare float64       `json:"excluded_share"`
	AttemptCounts AttemptCounts `json:"attempt_counts"`
	Cheats        ArmCount      `json:"cheats"`
	// ImputedAttempts is the number of candidate attempts that entered a task's mean at the
	// worst score because they were timeout, infra_error, judge_error or not_run (a cheat stays
	// in Cheats). UnusableAttempts is, per arm, the count of timeout, infra_error, judge_error
	// and not_run attempts, the placeholders for missing tasks included.
	ImputedAttempts  int                `json:"imputed_attempts"`
	UnusableAttempts ArmCount           `json:"unusable_attempts"`
	Efficiency       []EfficiencyResult `json:"efficiency"`
	HeldOut          *HeldOutResult     `json:"held_out"`
	Power            PowerNote          `json:"power"`
	Stamps           ResultStamps       `json:"stamps"`
	Judge            Judge              `json:"judge"`
	EraMarker        string             `json:"era_marker"`
	GateVersion      string             `json:"gate_version"`
	CorpusHash       string             `json:"corpus_hash"`
	BaselineHash     string             `json:"baseline_hash"`
	BootstrapB       int                `json:"bootstrap_b"`
	Seed             int64              `json:"seed"`
	// GateBuildSHA256 is the hash of the binary that ran the gate, set by the verb (Gate itself
	// is pure). It is not part of the era marker. Absent means unknown.
	GateBuildSHA256 string `json:"gate_build_sha256,omitempty"`
}

// MarshalJSON writes the number, or the word "unbounded".
func (c TaskCount) MarshalJSON() ([]byte, error) {
	if c.Unbounded {
		return []byte(`"unbounded"`), nil
	}
	return json.Marshal(c.N)
}

// UnmarshalJSON reads what MarshalJSON writes.
func (c *TaskCount) UnmarshalJSON(b []byte) error {
	var word string
	if err := json.Unmarshal(b, &word); err == nil {
		if word != "unbounded" {
			return fmt.Errorf("task count %q is not a number or \"unbounded\"", word)
		}
		*c = TaskCount{Unbounded: true}
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*c = TaskCount{N: n}
	return nil
}

// maxProjectedTasks caps the projection: past it the corpus growth is not a plan, so the answer
// is "unbounded".
const maxProjectedTasks = 1e9

// armTask is one task on one arm after scoring.
type armTask struct {
	usable bool
	score  float64
	// metric holds the mean of the resolved values of each declared efficiency metric over the
	// task's attempts that ran: on the candidate arm every attempt but a not_run (a pass,
	// task_fail, cheat, timeout, infra_error or judge_error spent what it recorded); on the
	// incumbent arm the attempts that were scored (an unfinished one is skipped). A metric with no
	// resolved value is absent: unresolved, never 0.
	metric map[string]float64
}

// armView is an arm scored against the declared task list.
type armView struct {
	tasks  map[string]armTask
	counts OutcomeCounts
	// attempts is every attempt of the arm over the declared tasks, one not_run placeholder
	// included for each declared task the arm lacks or holds with no attempt.
	attempts int
	// unusable is the number of those that are timeout, infra_error, judge_error or not_run.
	unusable int
	// imputed is the number of unusable attempts that entered a score at the worst score
	// (candidate arm only).
	imputed int
}

// scoreArm applies the scoring rule (R3) to one arm. worst is the bad end of the primary's score
// range.
//
// Candidate arm (imputeUnusable): every attempt enters its task's mean. A pass or task_fail
// enters with its score, a cheat at worst, and a timeout, infra_error, judge_error or not_run at
// worst too, so a candidate cannot improve its mean by not finishing what it would fail. A
// declared task the arm lacks, or holds with no attempt, is one not_run attempt, so every
// declared task is usable. The cost of an attempt follows whether it ran: a pass, task_fail,
// cheat, timeout, infra_error or judge_error spent what it recorded and contributes it (a null
// stays unresolved, never 0), while a not_run, explicit or the placeholder for a missing task,
// never ran and carries nothing. Otherwise a candidate could hide what its timeouts burned.
//
// Incumbent arm: a cheat enters at worst, and a timeout, infra_error, judge_error or not_run
// attempt is skipped, its cost included; a task with no remaining attempt is unusable on this
// arm and its pair is excluded. The asymmetry is deliberate: the incumbent is not the one being
// adopted and keeps every tie, so only the candidate pays for the attempts it did not finish.
func scoreArm(a Arm, declared []string, effs []EfficiencyMetric, worst float64, imputeUnusable bool) armView {
	byID := make(map[string]Task, len(a.Tasks))
	for _, t := range a.Tasks {
		byID[t.ID] = t
	}
	v := armView{tasks: make(map[string]armTask, len(declared)), counts: OutcomeCounts{}}
	for _, o := range allOutcomes {
		v.counts[string(o)] = 0
	}
	for _, id := range declared {
		t, ok := byID[id]
		if !ok || len(t.Attempts) == 0 {
			v.counts[string(OutcomeNotRun)]++
			v.attempts++
			v.unusable++
			if imputeUnusable {
				v.imputed++
				v.tasks[id] = armTask{usable: true, score: worst, metric: map[string]float64{}}
			}
			continue
		}
		var sum float64
		var n int
		msum := make(map[string]float64, len(effs))
		mn := make(map[string]int, len(effs))
		for _, at := range t.Attempts {
			v.counts[string(at.Outcome)]++
			v.attempts++
			switch at.Outcome {
			case OutcomePass, OutcomeTaskFail:
				sum += *at.Score
				n++
			case OutcomeCheat:
				sum += worst
				n++
			case OutcomeTimeout, OutcomeInfraError, OutcomeJudgeError:
				v.unusable++
				if !imputeUnusable {
					continue // the incumbent's unfinished attempt is skipped, cost included
				}
				// The candidate's unfinished attempt ran: it enters the score at the worst and,
				// like a cheat, the cost it recorded is spent, so it falls through to the metric
				// loop below. A cost it did not record (null) stays unresolved there.
				v.imputed++
				sum += worst
				n++
			default: // not_run: it never ran, so it carries no cost on either arm
				v.unusable++
				if imputeUnusable {
					v.imputed++
					sum += worst
					n++
				}
				continue
			}
			for _, e := range effs {
				if p := at.Metrics[e.Name]; p != nil {
					msum[e.Name] += *p
					mn[e.Name]++
				}
			}
		}
		if n == 0 {
			continue
		}
		task := armTask{usable: true, score: sum / float64(n), metric: map[string]float64{}}
		for _, e := range effs {
			if mn[e.Name] > 0 {
				task.metric[e.Name] = msum[e.Name] / float64(mn[e.Name])
			}
		}
		v.tasks[id] = task
	}
	return v
}

func fmtF(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// plainQuantile is the tail mass of the plain two-sided 95% interval; with k declared efficiency
// metrics each is read at 1 - 0.05/k (Bonferroni), so its tail mass is plainQuantile / k.
const plainQuantile = 0.025

// efficiencyQuantiles are the two percentiles a metric's interval is read at, and its level. One
// metric reads the same 0.025 and 0.975 as BootstrapDeltaMean, to the bit.
func efficiencyQuantiles(k int) (qLo, qHi, level float64) {
	if k <= 1 {
		return plainQuantile, 0.975, 0.95
	}
	q := plainQuantile / float64(k)
	return q, 1 - q, 1 - 0.05/float64(k)
}

// overflowMsg is the refusal for scores or costs whose bootstrap sums are not finite numbers.
const overflowMsg = "the scores or costs are too large to average: a bootstrap sum overflowed to a number that is not finite (overflow), so the gate refuses rather than report a verdict about it"

// Gate is the one measurement gate. It is pure: no I/O, no clock, no randomness beyond the
// baseline's seed. It returns a verdict or a refusal, never both.
//
// The rules (docs/systems/measurement-gate.md has them in plain words, numbered the same):
//
//	R1  the statistic is the paired bootstrap of the mean difference, resampling TASKS
//	R2  superiority: ADOPT iff lo > 0, BLOCK iff hi < 0; non-inferiority with margin m: BLOCK iff
//	    hi < -m, ADOPT iff lo > -m and a declared efficiency metric improves and none got
//	    significantly worse or went unmeasured; else INCONCLUSIVE
//	R3  a candidate attempt that did not finish scores the worst score, an incumbent's is
//	    skipped, a cheat scores the worst score; excluded pairs and unusable attempts are counted
//	R4  the incumbent wins every tie and every INCONCLUSIVE; fewer than min_pairs usable pairs, or
//	    too large an excluded share, is INCONCLUSIVE (the remedy is more tasks, not more attempts)
//	R5  held-out pairs may veto an ADOPT, never rescue anything
//	R6  everything the verdict depends on, the judge included, was frozen before the first
//	    candidate run
//	R7  a model judge needs a passed self-test receipt for the same scorer
//	R8  the era marker pins the runs; results from different eras are never compared
func Gate(b Baseline, baselineHash string, in GateInput) (GateResult, error) {
	if in.BaselineHash != baselineHash {
		return GateResult{}, refusef("input baseline_hash %.12s does not match the baseline handed in (%.12s): the verdict would not be about this pre-registration", in.BaselineHash, baselineHash)
	}
	if err := b.validate(); err != nil {
		return GateResult{}, err
	}
	canon, err := canonicalBaseline(b)
	if err != nil {
		return GateResult{}, err
	}
	if BaselineHash(canon) != baselineHash {
		return GateResult{}, refusef("the baseline handed in does not match its hash %.12s: it was edited after it was frozen", baselineHash)
	}

	// Judge (R6, R7): the one the baseline froze, with a receipt when it is a model.
	switch in.Judge.Kind {
	case "deterministic", "model":
	default:
		return GateResult{}, refusef("judge kind %q is not \"deterministic\" or \"model\"", in.Judge.Kind)
	}
	if in.Judge.Identity == "" {
		return GateResult{}, refusef("judge identity is empty: the scorer must be named (the era marker and the receipt both depend on it)")
	}
	if in.Judge.Kind != b.Judge.Kind {
		return GateResult{}, refusef("judge kind %q is not the frozen judge kind %q: the scorer is part of the pre-registration, so a model judge cannot be declared deterministic to skip its receipt", in.Judge.Kind, b.Judge.Kind)
	}
	if in.Judge.Identity != b.Judge.Identity {
		return GateResult{}, refusef("judge identity %q is not the frozen judge identity %q: scores from another scorer are not about this pre-registration", in.Judge.Identity, b.Judge.Identity)
	}
	if in.Judge.Kind == "model" {
		switch {
		case in.Receipt == nil:
			return GateResult{}, refusef("a model judge needs a self-test receipt and none was given")
		case !in.Receipt.Passed:
			return GateResult{}, refusef("the self-test receipt of judge %q did not pass", in.Judge.Identity)
		case in.Receipt.ScorerID != in.Judge.Identity:
			return GateResult{}, refusef("receipt scorer_id %q is not the judge identity %q", in.Receipt.ScorerID, in.Judge.Identity)
		case len(in.Receipt.PairsHash) != 64 || !isLowerHex(in.Receipt.PairsHash):
			return GateResult{}, refusef("receipt pairs_hash must be 64 lower-case hex characters (the SHA-256 of the pairs the self-test ran on)")
		case len(in.Receipt.BuildSHA) != 64 || !isLowerHex(in.Receipt.BuildSHA):
			return GateResult{}, refusef("receipt build_sha256 must be 64 lower-case hex characters (the SHA-256 of the build that ran the self-test)")
		}
	}

	// Incumbent and era (R8).
	var incArm Arm
	switch {
	case b.Incumbent == nil && in.Incumbent == nil:
		return GateResult{}, refusef("no incumbent: the input carries none and the baseline froze none")
	case b.Incumbent != nil && in.Incumbent != nil:
		if in.Incumbent.Stamps != b.Incumbent.Stamps {
			return GateResult{}, refusef("era mismatch: the input incumbent's stamps differ from the frozen incumbent's; results from different eras are never compared, so re-run the incumbent in the new era")
		}
		if !armsEqual(*in.Incumbent, *b.Incumbent) {
			return GateResult{}, refusef("the input incumbent is not equal to the incumbent frozen in the baseline")
		}
		incArm = *b.Incumbent
	case b.Incumbent != nil:
		incArm = *b.Incumbent
	default:
		incArm = *in.Incumbent
	}
	declaredSet := make(map[string]struct{}, len(b.Tasks))
	for _, id := range b.Tasks {
		declaredSet[id] = struct{}{}
	}
	for _, side := range []struct {
		who string
		arm Arm
	}{{"incumbent", incArm}, {"candidate", in.Candidate}} {
		if !stampsComplete(side.arm.Stamps) {
			return GateResult{}, refusef("%s: stamps are incomplete (harness_build_sha256, seat_config_sha256 and engine_build are all required: they pin the era)", side.who)
		}
		if err := validateArm(side.who, side.arm, declaredSet, b.Primary); err != nil {
			return GateResult{}, err
		}
	}

	// Scoring (R3).
	worst := b.Primary.worst()
	incView := scoreArm(incArm, b.Tasks, b.Efficiency, worst, false)
	candView := scoreArm(in.Candidate, b.Tasks, b.Efficiency, worst, true)
	type pair struct{ inc, cand armTask }
	var pairs []pair
	declaredMain := b.nonHeldOut()
	for _, id := range declaredMain {
		i, c := incView.tasks[id], candView.tasks[id]
		if i.usable && c.usable {
			pairs = append(pairs, pair{i, c})
		}
	}
	res := GateResult{
		Mode:             b.Mode,
		Margin:           b.Primary.Margin,
		NUsed:            len(pairs),
		NDeclared:        len(declaredMain),
		ExcludedPairs:    len(declaredMain) - len(pairs),
		AttemptCounts:    AttemptCounts{Incumbent: incView.counts, Candidate: candView.counts},
		Cheats:           ArmCount{Incumbent: incView.counts[string(OutcomeCheat)], Candidate: candView.counts[string(OutcomeCheat)]},
		ImputedAttempts:  candView.imputed,
		UnusableAttempts: ArmCount{Incumbent: incView.unusable, Candidate: candView.unusable},
		Stamps:           ResultStamps{Incumbent: incArm.Stamps, Candidate: in.Candidate.Stamps},
		Judge:            in.Judge,
		GateVersion:      b.GateVersion,
		CorpusHash:       b.CorpusHash,
		BaselineHash:     baselineHash,
		BootstrapB:       b.BootstrapB,
		Seed:             b.Seed,
	}
	// The excluded share is the largest of three ratios: the pairs lost over the declared tasks,
	// and each arm's unusable attempts over its attempts. Each is a different way to lose
	// evidence, and the cap applies to the worst of them.
	res.ExcludedShare = max(
		float64(res.ExcludedPairs)/float64(res.NDeclared),
		float64(incView.unusable)/float64(incView.attempts),
		float64(candView.unusable)/float64(candView.attempts),
	)
	incScores := make([]float64, len(pairs))
	candScores := make([]float64, len(pairs))
	for k, p := range pairs {
		incScores[k], candScores[k] = p.inc.score, p.cand.score
	}
	res.Delta, res.Lo, res.Hi = BootstrapDeltaMean(incScores, candScores, b.BootstrapB, b.Seed)
	if !finite(res.Delta) || !finite(res.Lo) || !finite(res.Hi) {
		return GateResult{}, refusef("%s", overflowMsg)
	}

	// Efficiency (R2): one paired bootstrap per declared metric, over the pairs that resolved it
	// on both arms, with the same B and seed as the primary. With k declared metrics each one is
	// read on the two-sided 1 - 0.05/k interval (Bonferroni), so that "some metric improved"
	// keeps a false-improve rate near 2.5% instead of k times that. A regression is read on the
	// plain 95% interval of the same replicates: more vetoes is the conservative side.
	k := len(b.Efficiency)
	qLo, qHi, level := efficiencyQuantiles(k)
	res.Efficiency = make([]EfficiencyResult, 0, k)
	anyImproves := false
	var regressed string          // the first metric that regressed
	var overCap *EfficiencyResult // the first metric that was not measured on enough pairs
	for _, e := range b.Efficiency {
		var incV, candV []float64
		for _, p := range pairs {
			iv, iok := p.inc.metric[e.Name]
			cv, cok := p.cand.metric[e.Name]
			if iok && cok {
				incV = append(incV, iv)
				candV = append(candV, cv)
			}
		}
		er := EfficiencyResult{Name: e.Name, Class: e.Class, LowerIsBetter: e.LowerIsBetter, N: len(incV), UnresolvedPairs: len(pairs) - len(incV), Level: level}
		er.OverUnresolvedCap = len(pairs) > 0 && float64(er.UnresolvedPairs)/float64(len(pairs)) > b.MaxExcludedShare
		if er.N >= 2 { // fewer than two resolved pairs have no interval: delta, lo and hi stay null
			reps, d, _ := bootstrapReps(incV, candV, b.BootstrapB, b.Seed)
			lo, hi := percentile(reps, qLo), percentile(reps, qHi)
			plainLo, plainHi := percentile(reps, plainQuantile), percentile(reps, 0.975)
			if !finite(d) || !finite(lo) || !finite(hi) || !finite(plainLo) || !finite(plainHi) {
				return GateResult{}, refusef("efficiency metric %q: %s", e.Name, overflowMsg)
			}
			er.Delta, er.Lo, er.Hi = &d, &lo, &hi
			// A regression is read wherever there is an interval (two or more resolved pairs), so a
			// metric resolved on 2 to min_pairs-1 pairs cannot hide a ten-times regression. Only an
			// improvement needs min_pairs resolved pairs.
			if e.LowerIsBetter {
				er.Regresses = plainLo > 0
			} else {
				er.Regresses = plainHi < 0
			}
			if er.N >= b.MinPairs {
				if e.LowerIsBetter {
					er.Improves = hi < 0
				} else {
					er.Improves = lo > 0
				}
			}
		}
		if er.OverUnresolvedCap {
			er.Improves = false // a metric that was not measured on enough pairs cannot improve
		}
		anyImproves = anyImproves || er.Improves
		if er.Regresses && regressed == "" {
			regressed = er.Name
		}
		res.Efficiency = append(res.Efficiency, er)
	}
	for i := range res.Efficiency {
		if res.Efficiency[i].OverUnresolvedCap {
			overCap = &res.Efficiency[i]
			break
		}
	}

	// Verdict (R2, R4). Superiority reports the efficiency metrics and ignores them here.
	switch {
	case res.NUsed < b.MinPairs:
		res.Verdict, res.Reason = VerdictInconclusive, fmt.Sprintf("n=%d below min_pairs=%d", res.NUsed, b.MinPairs)
	case res.ExcludedShare > b.MaxExcludedShare:
		res.Verdict, res.Reason = VerdictInconclusive, fmt.Sprintf("excluded_share=%s above max_excluded_share=%s", fmtF(res.ExcludedShare), fmtF(b.MaxExcludedShare))
	case b.Mode == ModeSuperiority:
		switch {
		case res.Lo > 0:
			res.Verdict, res.Reason = VerdictAdopt, "candidate better: the 95% interval lies above 0"
		case res.Hi < 0:
			res.Verdict, res.Reason = VerdictBlock, "candidate worse: the 95% interval lies below 0"
		default:
			res.Verdict, res.Reason = VerdictInconclusive, "the 95% interval spans 0"
		}
	default: // non-inferiority
		m := b.Primary.Margin
		switch {
		case res.Hi < -m:
			res.Verdict, res.Reason = VerdictBlock, fmt.Sprintf("candidate worse than the margin: hi=%s below -margin=%s", fmtF(res.Hi), fmtF(-m))
		case res.Lo > -m:
			switch {
			case overCap != nil:
				res.Verdict, res.Reason = VerdictInconclusive, fmt.Sprintf("declared efficiency metric %s has %d of %d pairs unresolved, above max_excluded_share=%s: a metric that was not measured cannot be traded away", overCap.Name, overCap.UnresolvedPairs, res.NUsed, fmtF(b.MaxExcludedShare))
			case regressed != "":
				res.Verdict, res.Reason = VerdictInconclusive, fmt.Sprintf("declared efficiency metric %s got significantly worse", regressed)
			case anyImproves:
				res.Verdict, res.Reason = VerdictAdopt, fmt.Sprintf("non-inferior: lo=%s above -margin=%s, and a declared efficiency metric improved", fmtF(res.Lo), fmtF(-m))
			default:
				res.Verdict, res.Reason = VerdictInconclusive, "non-inferior but no declared efficiency metric improved"
			}
		default:
			res.Verdict, res.Reason = VerdictInconclusive, fmt.Sprintf("the 95%% interval does not clear the margin: lo=%s is not above -margin=%s", fmtF(res.Lo), fmtF(-m))
		}
	}

	// Held-out (R5): a point estimate over the usable held-out pairs, scored like the main
	// slice. It can only veto an ADOPT; with no usable held-out pair an ADOPT is held back as
	// missing evidence, which is not a failed check.
	if len(b.HeldOut) > 0 {
		var sum float64
		var n int
		for _, id := range b.HeldOut {
			i, c := incView.tasks[id], candView.tasks[id]
			if i.usable && c.usable {
				sum += c.score - i.score
				n++
			}
		}
		ho := &HeldOutResult{N: n, Missing: n == 0}
		threshold := 0.0
		if b.Mode == ModeNonInferiority {
			threshold = -b.Primary.Margin
		}
		if n > 0 {
			ho.Delta = sum / float64(n)
			if !finite(ho.Delta) {
				return GateResult{}, refusef("held-out: %s", overflowMsg)
			}
		}
		ho.Failed = n > 0 && ho.Delta < threshold
		res.HeldOut = ho
		switch {
		case res.Verdict == VerdictAdopt && ho.Failed:
			res.Verdict, res.Reason = VerdictBlock, "held-out check failed; reject without re-tuning"
		case res.Verdict == VerdictAdopt && ho.Missing:
			res.Verdict, res.Reason = VerdictInconclusive, "held-out evidence missing"
		}
	}
	res.KeepIncumbent = res.Verdict != VerdictAdopt

	res.Power = powerNote(res.NUsed, res.Delta, res.Lo, res.Hi, b)
	res.EraMarker = eraMarker(b.GateVersion, incArm.Stamps, in.Candidate.Stamps, in.Judge.Identity)
	return res, nil
}

// powerNote reads the main interval three ways (the power note of rule R4). It adds no statistic.
func powerNote(n int, delta, lo, hi float64, b Baseline) PowerNote {
	hw := (hi - lo) / 2
	pn := PowerNote{
		HalfWidth: hw,
		Projected: Projected{
			N16:  hw * math.Sqrt(float64(n)/16),
			N24:  hw * math.Sqrt(float64(n)/24),
			N120: hw * math.Sqrt(float64(n)/120),
		},
		TasksNeeded: TaskCount{Unbounded: true},
		TasksToAdd:  TaskCount{Unbounded: true},
	}
	if n < 2 {
		return pn // no interval to project from
	}
	yard := 0.0 // the effect the interval has to resolve
	if b.Mode == ModeNonInferiority {
		yard = b.Primary.Margin
	} else if math.Abs(delta) > 0 {
		yard = math.Abs(delta)
	}
	if yard <= 0 {
		return pn
	}
	need := math.Ceil(float64(n) * (hw / yard) * (hw / yard))
	if !finite(need) || need > maxProjectedTasks {
		return pn
	}
	pn.TasksNeeded = TaskCount{N: int(need)}
	pn.TasksToAdd = TaskCount{N: max(0, int(need)-n)}
	return pn
}

// eraMarker is the hex SHA-256 of the compact JSON of the gate version, both runs' stamps and the
// judge identity, in that field order and nothing else. Results with different era markers are
// never compared: a comparison re-runs the incumbent in the new era.
func eraMarker(version string, inc, cand Stamps, judgeIdentity string) string {
	doc := struct {
		GateVersion     string `json:"gate_version"`
		IncumbentStamps Stamps `json:"incumbent_stamps"`
		CandidateStamps Stamps `json:"candidate_stamps"`
		JudgeIdentity   string `json:"judge_identity"`
	}{version, inc, cand, judgeIdentity}
	j, _ := json.Marshal(doc) // a struct of strings cannot fail to marshal
	sum := sha256.Sum256(j)
	return hex.EncodeToString(sum[:])
}
