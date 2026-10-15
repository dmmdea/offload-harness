package eval

import (
	_ "embed"
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// GateInputSchema is the JSON Schema (draft 2020-12) of the gate input document, the contract
// the Claude Code adapter reads (`local-offload gate schema` prints it). Its `outcome` enum is
// pinned to AllOutcomes by a test, so the two cannot drift.
//
//go:embed gate-input.schema.json
var GateInputSchema []byte

// Attempt is one attempt at one task.
//
// Unresolved cost rule: a nil metric value stays nil end to end and is NEVER written as 0. It
// marshals as JSON null (the key stays, the value is null); a metric key that is absent means
// the same thing. A cost that was not measured is not a free run.
type Attempt struct {
	Outcome AttemptOutcome `json:"outcome"`
	// Score is required for pass and task_fail; a cheat enters at the primary's worst score
	// whatever it carries; ignored for every other class.
	Score *float64 `json:"score,omitempty"`
	// Metrics are efficiency values by declared metric name (tokens, money, memory in GiB):
	// finite and not negative. A nil value is unresolved.
	Metrics map[string]*float64 `json:"metrics,omitempty"`
}

// Task is one declared task and the attempts made at it on one arm.
type Task struct {
	ID       string    `json:"id"`
	Attempts []Attempt `json:"attempts"`
}

// Stamps pin the run that produced an arm's scores: results from different stamps are
// different eras and are never compared. They come from the arms, not from the gate
// binary.
type Stamps struct {
	HarnessBuildSHA256 string `json:"harness_build_sha256"`
	SeatConfigSHA256   string `json:"seat_config_sha256"`
	EngineBuild        string `json:"engine_build"`
}

// Arm is one side of a comparison: the stamps of the run and its tasks.
type Arm struct {
	Stamps Stamps `json:"stamps"`
	Tasks  []Task `json:"tasks"`
}

// cloneArm deep-copies an arm and normalises nil slices to empty ones, so the canonical bytes
// of an arm do not depend on how the caller built it. The copy is independent of the original.
func cloneArm(a Arm) Arm {
	out := Arm{Stamps: a.Stamps, Tasks: make([]Task, 0, len(a.Tasks))}
	for _, t := range a.Tasks {
		nt := Task{ID: t.ID, Attempts: make([]Attempt, 0, len(t.Attempts))}
		for _, at := range t.Attempts {
			na := Attempt{Outcome: at.Outcome}
			if at.Score != nil {
				v := *at.Score
				na.Score = &v
			}
			if len(at.Metrics) > 0 {
				na.Metrics = make(map[string]*float64, len(at.Metrics))
				for k, p := range at.Metrics {
					if p == nil {
						na.Metrics[k] = nil
						continue
					}
					v := *p
					na.Metrics[k] = &v
				}
			}
			nt.Attempts = append(nt.Attempts, na)
		}
		out.Tasks = append(out.Tasks, nt)
	}
	return out
}

// sortedArm is cloneArm with the tasks in task-id order (the attempts keep their order).
func sortedArm(a Arm) Arm {
	out := cloneArm(a)
	sort.SliceStable(out.Tasks, func(i, j int) bool { return out.Tasks[i].ID < out.Tasks[j].ID })
	return out
}

// armsEqual compares two arms by their canonical JSON after sorting tasks by id: nil versus
// empty slices and maps do not matter, the data does.
func armsEqual(a, b Arm) bool {
	ja, errA := json.Marshal(sortedArm(a))
	jb, errB := json.Marshal(sortedArm(b))
	return errA == nil && errB == nil && string(ja) == string(jb)
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// timeTerm names the wall-time word a metric or primary name carries, "" when it carries none.
// INV-5: no pass rule, verdict or ranking carries a wall-time term, so a name that says seconds,
// latency, duration or the like is refused at freeze whatever class it hides behind. Words are
// matched whole (camelCase and punctuation split them), so "secondary_score" is not "second".
func timeTerm(name string) string {
	low := strings.ToLower(name)
	for _, stem := range []string{"latency", "walltime", "wallclock", "duration", "elapsed", "throughput", "speed"} {
		if strings.Contains(low, stem) {
			return stem
		}
	}
	var toks []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			toks = append(toks, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	var prev rune
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			if prev >= 'a' && prev <= 'z' {
				flush()
			}
			cur = append(cur, r)
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			cur = append(cur, r)
		default:
			flush()
		}
		prev = r
	}
	flush()
	for _, tok := range toks {
		switch tok {
		case "s", "sec", "secs", "second", "seconds", "ms", "millisecond", "milliseconds", "time", "wall", "tps":
			return tok
		}
	}
	return ""
}

func stampsComplete(s Stamps) bool {
	return s.HarnessBuildSHA256 != "" && s.SeatConfigSHA256 != "" && s.EngineBuild != ""
}

// validateArm refuses an arm the gate cannot honour: a task id the baseline never declared or a
// repeated one, an unknown outcome word, a scored attempt with no score (or one outside its
// class range), a non-finite number or a negative efficiency value. who names the arm in the
// message.
func validateArm(who string, a Arm, declared map[string]struct{}, p Primary) error {
	seen := make(map[string]struct{}, len(a.Tasks))
	lo, hi, bounded := p.scoreBounds()
	for _, t := range a.Tasks {
		if t.ID == "" {
			return refusef("%s: a task has an empty id", who)
		}
		if _, ok := declared[t.ID]; !ok {
			return refusef("%s: task %q is not in the baseline's declared tasks", who, t.ID)
		}
		if _, dup := seen[t.ID]; dup {
			return refusef("%s: task %q appears twice", who, t.ID)
		}
		seen[t.ID] = struct{}{}
		for i, at := range t.Attempts {
			if !at.Outcome.Valid() {
				return refusef("%s: task %q attempt %d has outcome %q, not one of %s", who, t.ID, i, string(at.Outcome), outcomeList())
			}
			if at.Outcome == OutcomePass || at.Outcome == OutcomeTaskFail {
				if at.Score == nil {
					return refusef("%s: task %q attempt %d is %s with no score", who, t.ID, i, at.Outcome)
				}
				if !finite(*at.Score) {
					return refusef("%s: task %q attempt %d has a non-finite score", who, t.ID, i)
				}
				if bounded && (*at.Score < lo || *at.Score > hi) {
					return refusef("%s: task %q attempt %d score %g is outside the %s range [%g, %g]", who, t.ID, i, *at.Score, p.Class, lo, hi)
				}
			}
			for name, m := range at.Metrics {
				if m != nil && !finite(*m) {
					return refusef("%s: task %q attempt %d metric %q is not finite", who, t.ID, i, name)
				}
				if m != nil && *m < 0 {
					return refusef("%s: task %q attempt %d metric %q is negative (%g): an efficiency value is a cost, finite and not below 0", who, t.ID, i, name, *m)
				}
			}
		}
	}
	return nil
}

func outcomeList() string {
	parts := make([]string, len(allOutcomes))
	for i, o := range allOutcomes {
		parts[i] = string(o)
	}
	return strings.Join(parts, "|")
}
