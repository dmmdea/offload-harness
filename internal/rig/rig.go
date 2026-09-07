// Package rig is the seat rigger's first slice (P3a of the envharness port,
// ADR 0036): a DETERMINISTIC classifier over the delegation-log corpus that
// puts every failed or deferred row on exactly one failure axis, and a triage
// report with weights over the rows eligible for each axis, the evidence
// (job ids) and the pre-authored remedy where the harness's closed rule
// vocabulary has one — and an honest "not a rule matter" bucket where it has
// none. It proposes nothing on its own, applies nothing, never calls the
// cloud; P3b (a proposer with a validation gate, the difficulty-zone and
// red-team objectives) waits for a trace corpus in the hundreds.
//
// Why a classifier first (design council 2026-09-07): of 649 bad rows in the
// corpus that day, ~70 % were wall timeouts and seat/infrastructure errors the
// rule vocabulary cannot touch, only 16 rows carried a trace, and a proposer
// built on an unvalidated diagnosis inherits every ambiguity. The published
// precedence order below is the contract; a fixture with one row per boundary
// case pins it.
package rig

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// Axis is one primary failure axis. The order of this list IS the precedence
// order: Classify evaluates the predicates top to bottom and stops at the
// first hit, so two runs on the same corpus never disagree.
type Axis string

const (
	AxisSeatInfra       Axis = "seat-infra"       // defer_class infrastructure: the seat/engine, not the loop
	AxisTimeout         Axis = "timeout"          // the contract's wall ran out
	AxisBudget          Axis = "budget"           // the step budget ran out
	AxisAbstention      Axis = "abstention"       // defer_class abstention: the seat declined
	AxisSchemaMiss      Axis = "schema-miss"      // the answer failed acceptance on shape: empty / short fields, or did not validate at all (sub invalid)
	AxisAnchorMiss      Axis = "anchor-miss"      // a schema-valid answer missed a content anchor (contains:/regex:/not_contains:) — off-document or generic
	AxisLoop            Axis = "loop"             // trace: the same tool >= 3 times running, or a rule fired
	AxisLongObservation Axis = "long-observation" // trace: one observation over the long threshold
	AxisToolMisuse      Axis = "tool-misuse"      // trace: >= 2 calls that failed or never ran
	AxisUnclassified    Axis = "unclassified"
)

// Precedence is the evaluation order, published so the report can print it.
var Precedence = []Axis{AxisSeatInfra, AxisTimeout, AxisBudget, AxisAbstention, AxisSchemaMiss, AxisAnchorMiss, AxisLoop, AxisLongObservation, AxisToolMisuse, AxisUnclassified}

// SubTwoStepGrounded is the sub-axis (of schema-miss and anchor-miss) the P2
// corpus read found: the contract carried context docs and the run stopped
// within two steps — the seat answered before it had read the document.
const SubTwoStepGrounded = "two-step-grounded"

// SubInvalid is the schema-miss sub-axis for an answer that did not validate
// against the output schema at all (the re-pack failed: wrong types, wrong
// shape) — distinct from a valid answer with empty fields.
const SubInvalid = "invalid"

// Subs lists the sub-axes each axis can carry, for the report.
var Subs = map[Axis][]string{AxisSchemaMiss: {SubTwoStepGrounded, SubInvalid}, AxisAnchorMiss: {SubTwoStepGrounded}}

// Thresholds are the classifier's constants, printed in the report so a
// reader knows what the labels mean.
const (
	LongObservationChars = 8000
	LoopSameToolRun      = 3
	ToolMisuseMin        = 2
	TwoStepMaxSteps      = 2
)

// Row is the slice of a delegation-log row the classifier reads.
type Row struct {
	TS                 int64                 `json:"ts"`
	JobID              string                `json:"job_id"`
	Node               string                `json:"node"`
	Seat               string                `json:"seat"`
	Deferred           bool                  `json:"deferred"`
	DeferClass         string                `json:"defer_class"`
	AcceptancePass     bool                  `json:"acceptance_pass"`
	WallMs             int64                 `json:"wall_ms"`
	Error              string                `json:"error"`
	Contract           core.AgentContract    `json:"contract"`
	Result             *core.AgentWireResult `json:"result"`
	AcceptanceFailures []string              `json:"acceptance_failures"`
	Arm                string                `json:"arm"`
}

// Bad reports whether the row is a failure the rigger classifies.
func (r Row) Bad() bool { return r.Deferred || !r.AcceptancePass }

// Verdict is one row's classification.
type Verdict struct {
	JobID    string `json:"job_id"`
	Axis     Axis   `json:"axis"`
	Sub      string `json:"sub,omitempty"`
	Evidence string `json:"evidence"` // the fact that decided, in the row's own words
}

var (
	wallTimeoutRe   = regexp.MustCompile(`(?i)wall timeout|context deadline exceeded|deadline`)
	stepBudgetRe    = regexp.MustCompile(`(?i)step budget exhausted`)
	schemaFieldRe   = regexp.MustCompile(`^(min_items|nonempty):`)
	anchorFieldRe   = regexp.MustCompile(`^(contains|not_contains|regex):`)
	schemaInvalidRe = regexp.MustCompile(`(?i)output failed schema|schema validation failed`)
	// dispatchErrRe: the delegator could not even place the job (a node's
	// 503 shed, a refused dispatch, a dial failure) — rows with an error but
	// no defer_class, because no node ever produced a result.
	dispatchErrRe  = regexp.MustCompile(`(?i)\b(HTTP|status) 5\d\d\b|\bshed\b|no idle execution slot|dispatch|connection refused|dial tcp|unreachable`)
	nonAlnumDigits = regexp.MustCompile(`\d+`)
)

// Classify puts one bad row on exactly one axis, in Precedence order. A row
// that is not bad classifies as "" (the caller filters). The predicates:
//
//  1. seat-infra: defer_class == "infrastructure" (or "config"/"contract" —
//     the box or the contract, never the loop); ALSO no defer_class at all
//     and the delegator's own error is a placement failure (a node's 503
//     shed, a refused dispatch, a dial failure) — no node produced a result.
//  2. timeout: the reason/error says wall timeout / deadline, or
//     defer_class == "budget" with stop_reason "error" and no step-budget text.
//  3. budget: "step budget exhausted" in the reason, or stop_reason "budget".
//  4. abstention: defer_class == "abstention" — except when the reason says
//     the OUTPUT FAILED SCHEMA, which is schema-miss / invalid (the seat
//     answered; the answer did not validate).
//  5. schema-miss: any acceptance failure that is min_items:/nonempty: —
//     sub two-step-grounded when the contract has context docs and steps <= 2.
//     5b. anchor-miss: any acceptance failure that is contains:/not_contains:/
//     regex: (a valid answer that missed a content anchor); same sub-axis.
//  6. loop (trace only): rules_fired > 0, or the same tool on >= 3
//     consecutive steps.
//  7. long-observation (trace only): any step's obs_chars > 8,000.
//  8. tool-misuse (trace only): >= 2 steps with status failed/none/unknown.
//  9. unclassified.
func Classify(r Row) Verdict {
	v := Verdict{JobID: r.JobID}
	if !r.Bad() {
		return v
	}
	res := r.Result
	reason := ""
	steps := 0
	stop := ""
	var trace []core.AgentTraceStep
	rulesFired := 0
	if res != nil {
		reason, steps, stop, trace, rulesFired = res.Reason, res.Steps, res.StopReason, res.Trace, res.RulesFired
	}
	if reason == "" {
		reason = r.Error
	}
	switch r.DeferClass {
	case core.DeferClassInfrastructure, core.DeferClassConfig, core.DeferClassContract:
		v.Axis, v.Evidence = AxisSeatInfra, "defer_class "+r.DeferClass+": "+clip(reason, 120)
		return v
	case "":
		// no node produced a result: the delegator's own error is the fact
		if r.Error != "" && dispatchErrRe.MatchString(r.Error) {
			v.Axis, v.Evidence = AxisSeatInfra, "dispatch: "+clip(r.Error, 120)
			return v
		}
	}
	if wallTimeoutRe.MatchString(reason) || (r.DeferClass == core.DeferClassCapacity && wallTimeoutRe.MatchString(reason)) {
		v.Axis, v.Evidence = AxisTimeout, clip(reason, 120)
		return v
	}
	if stepBudgetRe.MatchString(reason) || stop == "budget" {
		v.Axis, v.Evidence = AxisBudget, clip(firstNonEmpty(reason, "stop_reason budget"), 120)
		return v
	}
	if r.DeferClass == core.DeferClassAbstention {
		if schemaInvalidRe.MatchString(reason) {
			// the seat answered, the answer did not validate: a shape
			// failure, not a refusal — the defer class is the harness's
			// word for "no usable result", the reason says which
			v.Axis, v.Sub, v.Evidence = AxisSchemaMiss, SubInvalid, clip(reason, 120)
			return v
		}
		v.Axis, v.Evidence = AxisAbstention, "defer_class abstention: "+clip(reason, 120)
		return v
	}
	if r.DeferClass == core.DeferClassBudget && stop == "error" {
		// a budget-class defer whose reason did not say "step budget" is the
		// wall (the node's context deadline) — the corpus's dominant shape
		v.Axis, v.Evidence = AxisTimeout, "defer_class budget with stop_reason error: "+clip(reason, 100)
		return v
	}
	for _, f := range r.AcceptanceFailures {
		if schemaFieldRe.MatchString(f) {
			v.Axis, v.Evidence = AxisSchemaMiss, clip(f, 120)
			if len(r.Contract.Context) > 0 && steps <= TwoStepMaxSteps {
				v.Sub = SubTwoStepGrounded
			}
			return v
		}
	}
	for _, f := range r.AcceptanceFailures {
		if anchorFieldRe.MatchString(f) {
			v.Axis, v.Evidence = AxisAnchorMiss, clip(f, 120)
			if len(r.Contract.Context) > 0 && steps <= TwoStepMaxSteps {
				v.Sub = SubTwoStepGrounded
			}
			return v
		}
	}
	if len(trace) > 0 {
		if rulesFired > 0 {
			v.Axis, v.Evidence = AxisLoop, fmt.Sprintf("rules_fired %d", rulesFired)
			return v
		}
		if tool, run := longestSameToolRun(trace); run >= LoopSameToolRun {
			v.Axis, v.Evidence = AxisLoop, fmt.Sprintf("%s x%d consecutive", tool, run)
			return v
		}
		for _, s := range trace {
			if s.ObsChars > LongObservationChars {
				v.Axis, v.Evidence = AxisLongObservation, fmt.Sprintf("step %d %s obs_chars %d", s.Step, s.Tool, s.ObsChars)
				return v
			}
		}
		bad := 0
		var last core.AgentTraceStep
		for _, s := range trace {
			if s.Status != "committed" {
				bad++
				last = s
			}
		}
		if bad >= ToolMisuseMin {
			v.Axis, v.Evidence = AxisToolMisuse, fmt.Sprintf("%d non-committed calls, last %s %s", bad, last.Tool, last.Status)
			return v
		}
	}
	if len(r.AcceptanceFailures) > 0 {
		v.Axis, v.Evidence = AxisUnclassified, "acceptance: "+clip(r.AcceptanceFailures[0], 100)
		return v
	}
	v.Axis, v.Evidence = AxisUnclassified, clip(firstNonEmpty(reason, "no reason on the row"), 120)
	return v
}

func longestSameToolRun(trace []core.AgentTraceStep) (string, int) {
	bestTool, best, cur := "", 0, 0
	prev := ""
	for _, s := range trace {
		if s.Tool == prev {
			cur++
		} else {
			cur = 1
			prev = s.Tool
		}
		if cur > best {
			best, bestTool = cur, s.Tool
		}
	}
	return bestTool, best
}

// Remedy is the pre-authored lever for an axis: what the harness's closed
// vocabulary can do about it (Key), or the honest statement that it cannot.
type Remedy struct {
	Key       string `json:"key,omitempty"`   // config key / rule, "" when there is no lever
	Value     string `json:"value,omitempty"` // how the value is derived, in words
	Applies   string `json:"applies"`         // node config | contract | none
	Rationale string `json:"rationale"`
}

// Remedies maps every axis to its lever. This is a lookup table, not
// inference: the rigger's added value is the weight and the evidence list,
// never a proposal the author did not already hand-map.
var Remedies = map[Axis]Remedy{
	AxisSeatInfra:       {Applies: "none", Rationale: "the seat or the box answered wrong (HTTP errors, engine crashes, contention): fix the serving stack; no rule shapes this"},
	AxisTimeout:         {Applies: "contract", Key: "timeout_sec", Value: "raise toward the cap only when the row's steps show progress; otherwise the run was stuck, not slow", Rationale: "the wall ran out — a contract-side budget, not a rule"},
	AxisBudget:          {Applies: "contract", Key: "max_steps", Value: "the cap is 12; a run at the cap with no loop axis is a task too large for one contract — split it", Rationale: "the step budget ran out"},
	AxisAbstention:      {Applies: "contract", Key: "goal", Value: "name the context file and tell the seat to read it first; anchor acceptance on page-only tokens", Rationale: "the seat declined — usually a goal that reads as unanswerable"},
	AxisSchemaMiss:      {Applies: "node config", Key: "agent_seed_context_reads", Value: "true on the node (one read_file per context doc replayed before turn 1) when the sub-axis is two-step-grounded; otherwise anchor acceptance on the document", Rationale: "a schema-valid answer with empty or short fields — on grounded contracts stopping at two steps the seat answered before reading the document (the 2026-09-07 corpus finding, measured by the setup_ab A/B)"},
	AxisAnchorMiss:      {Applies: "contract", Key: "goal / acceptance", Value: "name the context file in the goal and tell the seat to read it first; anchor on tokens that exist only in the document (a regex alternation beats one contains:)", Rationale: "a valid answer that missed the content anchor — the seat answered generically or off-document; on grounded contracts stopping at two steps the seed key applies as for schema-miss"},
	AxisLoop:            {Applies: "node config", Key: "agent_env_rules.max_calls_per_tool", Value: "p95 of that tool's count on the seat's PASSING rows (a cap below what passing runs need converts loops into blocked reads)", Rationale: "the same tool ran again and again"},
	AxisLongObservation: {Applies: "node config", Key: "agent_env_rules.max_observation_tokens", Value: "p50 obs_chars of the seat's passing rows / 4", Rationale: "one tool result crowded the window"},
	AxisToolMisuse:      {Applies: "node config", Key: "agent_env_rules.rewrite_error", Value: "the most repeated failed-step note (trace.note, 0.113.26+) as the match, a one-line instruction as the text", Rationale: "calls that failed or never ran, repeatedly"},
	AxisUnclassified:    {Applies: "none", Rationale: "no predicate matched — read the row"},
}

// AxisReport is one axis's line in the report.
type AxisReport struct {
	Axis      Axis     `json:"axis"`
	Sub       string   `json:"sub,omitempty"`
	Hits      int      `json:"hits"`
	Eligible  int      `json:"eligible"` // rows this axis could have classified (trace axes: rows with a trace)
	Weight    float64  `json:"weight"`   // hits / eligible
	Examples  []string `json:"examples"` // up to 5 job ids, oldest first
	Evidence  []string `json:"evidence"` // the deciding facts of the examples
	Remedy    Remedy   `json:"remedy"`
	TopReason string   `json:"top_reason,omitempty"` // seat-infra / timeout: the most repeated reason text (digits folded)
}

// Report is the rigger's output for one seat and window.
type Report struct {
	Seat         string         `json:"seat"`
	Node         string         `json:"node,omitempty"`
	Since        string         `json:"since"`
	Until        string         `json:"until"`
	Rows         int            `json:"rows"`
	Bad          int            `json:"bad"`
	WithTrace    int            `json:"with_trace"`
	BadWithTrace int            `json:"bad_with_trace"`
	Precedence   []Axis         `json:"precedence"`
	Thresholds   map[string]int `json:"thresholds"`
	Axes         []AxisReport   `json:"axes"`
	SeatsSeen    []string       `json:"seats_seen,omitempty"`
}

// ErrNoRows is returned when the window holds no rows for the seat; the
// error names the seats that WERE seen so a typo is caught by name.
var ErrNoRows = errors.New("rig: no rows for the seat in the window")

// ReadShards reads every delegation-log shard whose day falls in [since,
// until] from dir (the harness's delegation-log directory) and returns the
// rows in file order. Unparseable lines are skipped and counted.
func ReadShards(dir string, since, until time.Time) ([]Row, int, error) {
	var rows []Row
	skipped := 0
	// Shards are named by the writer's LOCAL date (time.Now().Format), so the
	// day walk runs on local calendar days — a UTC truncation lands on the
	// wrong file west of Greenwich.
	first := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, since.Location())
	for d := first; !d.After(until); d = d.AddDate(0, 0, 1) {
		p := filepath.Join(dir, d.Format("2006-01-02")+".jsonl")
		f, err := os.Open(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, skipped, fmt.Errorf("rig: %w", err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			var r Row
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				skipped++
				continue
			}
			t := time.Unix(r.TS, 0)
			if t.Before(since) || t.After(until) {
				continue
			}
			rows = append(rows, r)
		}
		f.Close()
	}
	return rows, skipped, nil
}

// Build classifies the rows for one seat (alias match on the row's seat,
// optional node) and assembles the report. Deterministic for a fixed input:
// rows are processed in order, examples are the first five hits.
func Build(rows []Row, seat, node string, since, until time.Time) (Report, error) {
	rep := Report{Seat: seat, Node: node, Since: since.UTC().Format(time.RFC3339), Until: until.UTC().Format(time.RFC3339),
		Precedence: Precedence, Thresholds: map[string]int{"long_observation_chars": LongObservationChars, "loop_same_tool_run": LoopSameToolRun, "tool_misuse_min": ToolMisuseMin, "two_step_max_steps": TwoStepMaxSteps}}
	seen := map[string]bool{}
	var mine []Row
	for _, r := range rows {
		seen[r.Seat] = true
		if r.Seat != seat || (node != "" && r.Node != node) {
			continue
		}
		mine = append(mine, r)
	}
	for s := range seen {
		rep.SeatsSeen = append(rep.SeatsSeen, s)
	}
	sort.Strings(rep.SeatsSeen)
	if len(mine) == 0 {
		return rep, fmt.Errorf("%w %q (seats seen: %s)", ErrNoRows, seat, strings.Join(rep.SeatsSeen, ", "))
	}
	rep.Rows = len(mine)
	type bucket struct {
		hits     int
		examples []string
		evidence []string
		reasons  map[string]int
	}
	buckets := map[string]*bucket{}
	key := func(a Axis, sub string) string {
		if sub == "" {
			return string(a)
		}
		return string(a) + "/" + sub
	}
	for _, r := range mine {
		hasTrace := r.Result != nil && len(r.Result.Trace) > 0
		if hasTrace {
			rep.WithTrace++
		}
		if !r.Bad() {
			continue
		}
		rep.Bad++
		if hasTrace {
			rep.BadWithTrace++
		}
		v := Classify(r)
		get := func(k string) *bucket {
			b := buckets[k]
			if b == nil {
				b = &bucket{reasons: map[string]int{}}
				buckets[k] = b
			}
			return b
		}
		// count the axis, and the sub-axis when present (the parent counts it too)
		b := get(key(v.Axis, ""))
		if v.Sub != "" {
			get(key(v.Axis, v.Sub))
		}
		b.hits++
		if len(b.examples) < 5 {
			b.examples = append(b.examples, r.JobID)
			b.evidence = append(b.evidence, v.Evidence)
		}
		b.reasons[nonAlnumDigits.ReplaceAllString(clip(v.Evidence, 80), "N")]++
		if v.Sub != "" {
			sb := buckets[key(v.Axis, v.Sub)]
			sb.hits++
			if len(sb.examples) < 5 {
				sb.examples = append(sb.examples, r.JobID)
				sb.evidence = append(sb.evidence, v.Evidence)
			}
		}
	}
	traceAxes := map[Axis]bool{AxisLoop: true, AxisLongObservation: true, AxisToolMisuse: true}
	for _, a := range Precedence {
		for _, sub := range append([]string{""}, Subs[a]...) {
			b := buckets[key(a, sub)]
			if b == nil {
				if sub != "" {
					continue
				}
				b = &bucket{reasons: map[string]int{}}
			}
			elig := rep.Bad
			if traceAxes[a] {
				elig = rep.BadWithTrace
			}
			ar := AxisReport{Axis: a, Sub: sub, Hits: b.hits, Eligible: elig, Examples: b.examples, Evidence: b.evidence, Remedy: Remedies[a]}
			if elig > 0 {
				ar.Weight = float64(b.hits) / float64(elig)
			}
			if a == AxisSeatInfra || a == AxisTimeout || a == AxisUnclassified {
				ar.TopReason = topKey(b.reasons)
			}
			rep.Axes = append(rep.Axes, ar)
		}
	}
	return rep, nil
}

func topKey(m map[string]int) string {
	best, bestN := "", 0
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic tie-break
	for _, k := range keys {
		if m[k] > bestN {
			best, bestN = k, m[k]
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf("%s (x%d)", best, bestN)
}

// Markdown renders the report for a human: the seat, the window, the counts,
// one line per axis with weight, remedy and examples.
func Markdown(rep Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# rig — seat %s", rep.Seat)
	if rep.Node != "" {
		fmt.Fprintf(&b, " on %s", rep.Node)
	}
	fmt.Fprintf(&b, "\n\nwindow %s → %s · rows %d · bad %d · with trace %d (bad %d)\n\n", rep.Since, rep.Until, rep.Rows, rep.Bad, rep.WithTrace, rep.BadWithTrace)
	fmt.Fprintf(&b, "precedence: %s\n\n", joinAxes(rep.Precedence))
	fmt.Fprintf(&b, "| axis | hits / eligible | weight | remedy | examples |\n|---|---|---|---|---|\n")
	for _, a := range rep.Axes {
		name := string(a.Axis)
		if a.Sub != "" {
			name += " / " + a.Sub
		}
		rem := a.Remedy.Rationale
		if a.Remedy.Key != "" {
			rem = fmt.Sprintf("`%s` (%s): %s", a.Remedy.Key, a.Remedy.Applies, a.Remedy.Value)
		} else {
			rem = "not a rule matter — " + rem
		}
		ex := strings.Join(a.Examples, ", ")
		if a.TopReason != "" {
			ex += " · top: " + a.TopReason
		}
		fmt.Fprintf(&b, "| %s | %d / %d | %.0f%% | %s | %s |\n", name, a.Hits, a.Eligible, a.Weight*100, rem, ex)
	}
	b.WriteString("\nThe rigger proposes nothing by itself: a remedy is a pre-authored lever for its axis, to be validated on the seat before adoption (P3b).\n")
	return b.String()
}

func joinAxes(a []Axis) string {
	s := make([]string, len(a))
	for i, x := range a {
		s[i] = string(x)
	}
	return strings.Join(s, " → ")
}

func clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Verdicts classifies every bad row of the seat (and node) in order — the
// per-row companion of Build's per-axis report, for an agreement check
// against independent labels or for reading one job's verdict.
func Verdicts(rows []Row, seat, node string) []Verdict {
	var out []Verdict
	for _, r := range rows {
		if r.Seat != seat || (node != "" && r.Node != node) || !r.Bad() {
			continue
		}
		out = append(out, Classify(r))
	}
	return out
}

// ParseSince turns a window spec into its start instant: "<N>d", "<N>h", an
// RFC3339 instant, or "" (= 7d). Shared by the CLI verb and the MCP door so
// the two agree on what a window means.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "7d"
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if len(s) >= 2 {
		if n, err := strconv.Atoi(s[:len(s)-1]); err == nil && n > 0 {
			switch s[len(s)-1] {
			case 'd':
				return now.Add(-time.Duration(n) * 24 * time.Hour), nil
			case 'h':
				return now.Add(-time.Duration(n) * time.Hour), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("rig: since %q: want <N>d, <N>h or an RFC3339 instant", s)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
