// Package eval is the harness's code-based quality evaluation: replay a curated
// per-task gold set through the live cascade and score with deterministic
// graders (grounding for extract/summarize, label-match for classify/triage).
// It is the keystone the prompt/exemplar optimizers (Phases 3-4) replay against.
package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/dmmdea/local-offload-pp-cli/internal/core"
	"github.com/dmmdea/local-offload-pp-cli/internal/grounding"
)

// Case is one gold-set instance with a defined success condition.
type Case struct {
	Task   string         `json:"task"`
	Input  string         `json:"input"`
	Params map[string]any `json:"params,omitempty"`
	Expect string         `json:"expect,omitempty"` // gold label/decision (classify/triage); optional for extract/summarize
}

// LoadCases reads a JSONL gold set; missing file => empty, no error.
// Empty and malformed (non-JSON / empty-Task) lines are skipped.
func LoadCases(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Case
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c Case
		if json.Unmarshal(line, &c) == nil && c.Task != "" {
			out = append(out, c)
		}
	}
	return out, sc.Err()
}

// Grade returns whether an ACCEPTED result is correct for the gold case.
// classify/triage: the model's chosen label/decision must equal Case.Expect.
// extract/summarize: graded by grounding against the source (no reference needed).
// Callers must only Grade accepted (res.OK && !res.Deferred) results.
func Grade(c Case, res core.Result) bool {
	switch core.TaskType(c.Task) {
	case core.TaskClassify:
		return strings.EqualFold(field(res.Data, "label"), c.Expect)
	case core.TaskTriage:
		return strings.EqualFold(field(res.Data, "decision"), c.Expect)
	case core.TaskExtract, core.TaskSummarize:
		g, ok := grounding.Check(core.TaskType(c.Task), c.Input, res.Data)
		return ok && g
	}
	return false
}

func field(data []byte, key string) string {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// Runner is satisfied by *pipeline.Pipeline (Run(ctx, req) core.Result).
type Runner interface {
	Run(ctx context.Context, req core.Request) core.Result
}

// Outcome is the per-case eval result.
type Outcome struct {
	Case        Case
	Accepted    bool
	Correct     bool // among accepted
	Deferred    bool
	TokensOut   int
	Margin      float64
	Escalations int
}

// Report aggregates outcomes for one task.
type Report struct {
	Task             string  `json:"task"`
	N                int     `json:"n"`
	Accepted         int     `json:"accepted"`
	AcceptedCorrect  int     `json:"accepted_correct"`
	Deferred         int     `json:"deferred"`
	AccuracyAccepted float64 `json:"accuracy_accepted"` // correct / accepted
	DeferRate        float64 `json:"defer_rate"`
	TokensOut        int     `json:"tokens_out"`
	AccPer1kTok      float64 `json:"acc_per_1k_tok"` // correct / (tokensOut/1000)
}

// Aggregate rolls outcomes up per task. Correct-defer is a valuable failure:
// it is NOT counted as an error (the frontier model handles it); only an
// accepted-wrong result is a true failure.
func Aggregate(outs []Outcome) map[string]Report {
	m := map[string]*Report{}
	for _, o := range outs {
		r := m[o.Case.Task]
		if r == nil {
			r = &Report{Task: o.Case.Task}
			m[o.Case.Task] = r
		}
		r.N++
		r.TokensOut += o.TokensOut
		switch {
		case o.Deferred:
			r.Deferred++
		case o.Accepted:
			r.Accepted++
			if o.Correct {
				r.AcceptedCorrect++
			}
		}
	}
	out := map[string]Report{}
	for k, r := range m {
		if r.Accepted > 0 {
			r.AccuracyAccepted = float64(r.AcceptedCorrect) / float64(r.Accepted)
		}
		if r.N > 0 {
			r.DeferRate = float64(r.Deferred) / float64(r.N)
		}
		if r.TokensOut > 0 {
			r.AccPer1kTok = float64(r.AcceptedCorrect) / (float64(r.TokensOut) / 1000.0)
		}
		out[k] = *r
	}
	return out
}

// SortedTasks returns the task keys of a report map in stable order.
func SortedTasks(m map[string]Report) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// RCPoint is one (confidence, correctness) observation for selective prediction.
type RCPoint struct {
	Confidence float64
	Correct    bool
}

// RiskCoverage computes the discrete risk-coverage curve with AURC and E-AURC.
// Field-standard selective-prediction metric (Geifman & El-Yaniv 2017; discrete
// form Ding et al. 2020): sort by confidence DESC; selective risk at coverage
// k/N = mean 0/1 loss over the k most-confident predictions; AURC = mean
// selective risk over all k. E-AURC subtracts the oracle AURC (perfect ranking:
// all correct first), isolating the reducible part. Lower = better.
func RiskCoverage(pts []RCPoint) (coverage, risk []float64, aurc, eaurc float64) {
	n := len(pts)
	if n == 0 {
		return nil, nil, 0, 0
	}
	sorted := make([]RCPoint, n)
	copy(sorted, pts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Confidence > sorted[j].Confidence })

	coverage = make([]float64, n)
	risk = make([]float64, n)
	cumLoss := 0.0
	for k := 0; k < n; k++ {
		if !sorted[k].Correct {
			cumLoss++
		}
		coverage[k] = float64(k+1) / float64(n)
		risk[k] = cumLoss / float64(k+1)
		aurc += risk[k]
	}
	aurc /= float64(n)

	nWrong := 0
	for _, p := range pts {
		if !p.Correct {
			nWrong++
		}
	}
	nCorrect := n - nWrong
	oracle, cum := 0.0, 0.0
	for k := 0; k < n; k++ {
		if k >= nCorrect {
			cum++
		}
		oracle += cum / float64(k+1)
	}
	oracle /= float64(n)
	eaurc = aurc - oracle
	return coverage, risk, aurc, eaurc
}

// OpPoint is one cascade operating point: average cost and achieved quality.
type OpPoint struct {
	Label   string
	Cost    float64 // avg cost per query (e.g. tokens-out)
	Quality float64 // accuracy in [0,1]
}

// DeferralCurve computes cascade cost-quality summaries over operating points
// (Jitkrittum et al. 2024, arXiv:2410.10347; Cost-Aware Contrastive Routing
// arXiv:2508.12491). Points sorted by cost; cost normalized to [0,1] over the
// observed range. AUDC = trapezoidal area under quality-vs-normalized-cost
// (higher = better quality per cost). Peak = max quality. QNC = smallest
// normalized cost at which peak quality is reached (lower = matches its best
// quality more cheaply).
func DeferralCurve(points []OpPoint) (audc, qnc, peak float64) {
	if len(points) == 0 {
		return 0, 0, 0
	}
	ps := make([]OpPoint, len(points))
	copy(ps, points)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Cost < ps[j].Cost })

	cMin, cMax := ps[0].Cost, ps[len(ps)-1].Cost
	span := cMax - cMin
	norm := func(c float64) float64 {
		if span == 0 {
			return 0
		}
		return (c - cMin) / span
	}
	for _, p := range ps {
		if p.Quality > peak {
			peak = p.Quality
		}
	}
	if span == 0 {
		audc = peak
	} else {
		for i := 1; i < len(ps); i++ {
			x0, x1 := norm(ps[i-1].Cost), norm(ps[i].Cost)
			audc += (x1 - x0) * (ps[i-1].Quality + ps[i].Quality) / 2
		}
	}
	qnc = 1.0
	for _, p := range ps { // sorted by cost asc: first to reach peak is cheapest
		if p.Quality >= peak-1e-9 {
			qnc = norm(p.Cost)
			break
		}
	}
	return audc, qnc, peak
}

// Run replays each gold case through r and grades the accepted ones.
func Run(ctx context.Context, r Runner, cases []Case) []Outcome {
	outs := make([]Outcome, 0, len(cases))
	for _, c := range cases {
		res := r.Run(ctx, core.Request{Task: core.TaskType(c.Task), Input: c.Input, Params: c.Params})
		o := Outcome{Case: c, TokensOut: res.Meta.TokensOut, Margin: res.Meta.Margin, Escalations: res.Meta.Escalations}
		switch {
		case res.Deferred:
			o.Deferred = true
		case res.OK:
			o.Accepted = true
			o.Correct = Grade(c, res)
		}
		outs = append(outs, o)
	}
	return outs
}
