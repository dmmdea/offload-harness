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

// Judge names the scorer that produced the scores. A deterministic scorer (the acceptance DSL, a
// regex, an exact match) needs no receipt; a model judge needs a passed self-test receipt for the
// same scorer, or the gate refuses.
type Judge struct {
	Kind     string `json:"kind"`     // "deterministic" | "model"
	Identity string `json:"identity"` // scorer id, e.g. "acceptance-dsl" or a model id
}

// JudgeReceipt is the self-test receipt `gate selftest` (a later change) produces for a model
// judge. Data only here.
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
// usable pairs that resolved it on both arms.
type EfficiencyResult struct {
	Name            string  `json:"name"`
	Class           string  `json:"class"`
	LowerIsBetter   bool    `json:"lower_is_better"`
	Delta           float64 `json:"delta"`
	Lo              float64 `json:"lo"`
	Hi              float64 `json:"hi"`
	N               int     `json:"n"`
	UnresolvedPairs int     `json:"unresolved_pairs"`
	Improves        bool    `json:"improves"`
}

// HeldOutResult is the point-estimate check on the held-out pairs (GATE-R5).
type HeldOutResult struct {
	N      int     `json:"n"`
	Delta  float64 `json:"delta"`
	Failed bool    `json:"failed"`
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
// half-width of the main bootstrap interval read three ways (GATE-R6).
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
	Verdict       string             `json:"verdict"`
	Reason        string             `json:"reason"`
	KeepIncumbent bool               `json:"keep_incumbent"`
	Mode          string             `json:"mode"`
	Margin        float64            `json:"margin"`
	Delta         float64            `json:"delta"`
	Lo            float64            `json:"lo"`
	Hi            float64            `json:"hi"`
	NUsed         int                `json:"n_used"`
	NDeclared     int                `json:"n_declared"`
	ExcludedPairs int                `json:"excluded_pairs"`
	ExcludedShare float64            `json:"excluded_share"`
	AttemptCounts AttemptCounts      `json:"attempt_counts"`
	Cheats        ArmCount           `json:"cheats"`
	Efficiency    []EfficiencyResult `json:"efficiency"`
	HeldOut       *HeldOutResult     `json:"held_out"`
	Power         PowerNote          `json:"power"`
	Stamps        ResultStamps       `json:"stamps"`
	Judge         Judge              `json:"judge"`
	EraMarker     string             `json:"era_marker"`
	GateVersion   string             `json:"gate_version"`
	CorpusHash    string             `json:"corpus_hash"`
	BaselineHash  string             `json:"baseline_hash"`
	BootstrapB    int                `json:"bootstrap_b"`
	Seed          int64              `json:"seed"`
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
	// task's usable attempts. A metric with no resolved value is absent: unresolved, never 0.
	metric map[string]float64
}

// armView is an arm scored against the declared task list.
type armView struct {
	tasks  map[string]armTask
	counts OutcomeCounts
}

// scoreArm applies GATE-R3 to one arm. Only pass, task_fail and cheat attempts are usable; a
// cheat enters as 0 whatever score it carries; a task's score is the mean of its usable
// attempts; a task with none is unusable on this arm. A declared task the arm lacks, or holds
// with no attempt, counts as one not_run attempt.
func scoreArm(a Arm, declared []string, effs []EfficiencyMetric) armView {
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
			continue
		}
		var sum float64
		var n int
		msum := make(map[string]float64, len(effs))
		mn := make(map[string]int, len(effs))
		for _, at := range t.Attempts {
			v.counts[string(at.Outcome)]++
			if !at.Outcome.Scored() {
				continue
			}
			if at.Outcome != OutcomeCheat {
				sum += *at.Score
			}
			n++
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

// Gate is the one measurement gate. It is pure: no I/O, no clock, no randomness beyond the
// baseline's seed. It returns a verdict or a refusal, never both.
//
// The rules (docs/systems/measurement-gate.md has them in plain words):
//
//	R1  the statistic is the paired bootstrap of the mean difference, resampling TASKS
//	R2  superiority: ADOPT iff lo > 0, BLOCK iff hi < 0; non-inferiority with margin m: BLOCK iff
//	    hi < -m, ADOPT iff lo > -m and a declared efficiency metric improves; else INCONCLUSIVE
//	R3  only pass, task_fail and cheat attempts score; a cheat is 0; excluded pairs are counted
//	R4  the incumbent wins every tie and every INCONCLUSIVE; fewer than min_pairs usable pairs, or
//	    too large an excluded share, is INCONCLUSIVE (the remedy is more tasks, not more attempts)
//	R5  held-out pairs may veto an ADOPT, never rescue anything
//	R6  everything the verdict depends on was frozen before the first candidate run
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

	// Judge (R7).
	switch in.Judge.Kind {
	case "deterministic", "model":
	default:
		return GateResult{}, refusef("judge kind %q is not \"deterministic\" or \"model\"", in.Judge.Kind)
	}
	if in.Judge.Identity == "" {
		return GateResult{}, refusef("judge identity is empty: the scorer must be named (the era marker and the receipt both depend on it)")
	}
	if in.Judge.Kind == "model" {
		switch {
		case in.Receipt == nil:
			return GateResult{}, refusef("a model judge needs a self-test receipt and none was given")
		case !in.Receipt.Passed:
			return GateResult{}, refusef("the self-test receipt of judge %q did not pass", in.Judge.Identity)
		case in.Receipt.ScorerID != in.Judge.Identity:
			return GateResult{}, refusef("receipt scorer_id %q is not the judge identity %q", in.Receipt.ScorerID, in.Judge.Identity)
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
		if err := validateArm(side.who, side.arm, declaredSet, b.Primary.Class); err != nil {
			return GateResult{}, err
		}
	}

	// Scoring (R3).
	incView := scoreArm(incArm, b.Tasks, b.Efficiency)
	candView := scoreArm(in.Candidate, b.Tasks, b.Efficiency)
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
		Mode:          b.Mode,
		Margin:        b.Primary.Margin,
		NUsed:         len(pairs),
		NDeclared:     len(declaredMain),
		ExcludedPairs: len(declaredMain) - len(pairs),
		AttemptCounts: AttemptCounts{Incumbent: incView.counts, Candidate: candView.counts},
		Cheats:        ArmCount{Incumbent: incView.counts[string(OutcomeCheat)], Candidate: candView.counts[string(OutcomeCheat)]},
		Stamps:        ResultStamps{Incumbent: incArm.Stamps, Candidate: in.Candidate.Stamps},
		Judge:         in.Judge,
		GateVersion:   b.GateVersion,
		CorpusHash:    b.CorpusHash,
		BaselineHash:  baselineHash,
		BootstrapB:    b.BootstrapB,
		Seed:          b.Seed,
	}
	res.ExcludedShare = float64(res.ExcludedPairs) / float64(res.NDeclared)
	incScores := make([]float64, len(pairs))
	candScores := make([]float64, len(pairs))
	for k, p := range pairs {
		incScores[k], candScores[k] = p.inc.score, p.cand.score
	}
	res.Delta, res.Lo, res.Hi = BootstrapDeltaMean(incScores, candScores, b.BootstrapB, b.Seed)

	// Efficiency (R2): one paired bootstrap per declared metric, over the pairs that resolved it
	// on both arms; the same B and seed as the primary.
	res.Efficiency = make([]EfficiencyResult, 0, len(b.Efficiency))
	anyImproves := false
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
		er := EfficiencyResult{Name: e.Name, Class: e.Class, LowerIsBetter: e.LowerIsBetter, N: len(incV), UnresolvedPairs: len(pairs) - len(incV)}
		er.Delta, er.Lo, er.Hi = BootstrapDeltaMean(incV, candV, b.BootstrapB, b.Seed)
		if er.N >= b.MinPairs {
			if e.LowerIsBetter {
				er.Improves = er.Hi < 0
			} else {
				er.Improves = er.Lo > 0
			}
		}
		if er.Improves {
			anyImproves = true
		}
		res.Efficiency = append(res.Efficiency, er)
	}

	// Verdict (R2, R4).
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
			if anyImproves {
				res.Verdict, res.Reason = VerdictAdopt, fmt.Sprintf("non-inferior: lo=%s above -margin=%s, and a declared efficiency metric improved", fmtF(res.Lo), fmtF(-m))
			} else {
				res.Verdict, res.Reason = VerdictInconclusive, "non-inferior but no declared efficiency metric improved"
			}
		default:
			res.Verdict, res.Reason = VerdictInconclusive, fmt.Sprintf("the 95%% interval does not clear the margin: lo=%s is not above -margin=%s", fmtF(res.Lo), fmtF(-m))
		}
	}

	// Held-out (R5): a point estimate over the usable held-out pairs. It can only veto an ADOPT.
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
		ho := &HeldOutResult{N: n}
		threshold := 0.0
		if b.Mode == ModeNonInferiority {
			threshold = -b.Primary.Margin
		}
		if n > 0 {
			ho.Delta = sum / float64(n)
		}
		ho.Failed = n == 0 || ho.Delta < threshold
		res.HeldOut = ho
		if res.Verdict == VerdictAdopt && ho.Failed {
			res.Verdict, res.Reason = VerdictBlock, "held-out check failed; reject without re-tuning"
		}
	}
	res.KeepIncumbent = res.Verdict != VerdictAdopt

	res.Power = powerNote(res.NUsed, res.Delta, res.Lo, res.Hi, b)
	res.EraMarker = eraMarker(b.GateVersion, incArm.Stamps, in.Candidate.Stamps, in.Judge.Identity)
	return res, nil
}

// powerNote reads the main interval three ways (GATE-R6). It adds no statistic.
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
