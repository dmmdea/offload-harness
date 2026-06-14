// Package confhead implements a per-task logistic correctness head.
// It reads the JSONL ledger, fits a binary logistic regression (pure Go,
// gradient descent, L2 regularisation) per task over ALL ledger features and
// metadata signals, and predicts p(correct) for an entry at inference time.
// Unlike the router (which predicts accepted-at-E2B), confhead predicts
// whether the model's answer was actually correct — it will be validated
// (Task 2) and, only if it lowers AURC, wired into the pipeline (Task 4).
package confhead

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/dmmdea/local-offload-pp-cli/internal/ledger"
)

// ---- Feature set -----------------------------------------------------------

// featNames is the FIXED ordered list of 10 features used by confhead.
// Order matters: weights are stored positionally against this list.
var featNames = []string{
	"len_chars", "n_words", "n_numbers", "n_caps",
	"has_code", "has_url",
	"margin", "truncated", "retries", "loginput",
}

// FeatureRow builds a feature map for one ledger entry.
// It copies the 6 Feat fields (absent keys default to 0 after standardise),
// then appends the 4 metadata signals: margin, truncated, retries, loginput.
// EXPORTED: Task 2 (out-of-fold validation) reuses this function directly.
func FeatureRow(e ledger.Entry) map[string]float64 {
	m := map[string]float64{}
	for k, v := range e.Feat {
		m[k] = v
	}
	m["margin"] = e.Margin
	if e.Truncated {
		m["truncated"] = 1
	} else {
		m["truncated"] = 0
	}
	m["retries"] = float64(e.Retries)
	m["loginput"] = math.Log1p(float64(e.InputChars))
	return m
}

// Label derives the binary correctness label for a ledger entry.
// ok=true only when at least one of Grounded or EscalatedAgreed is non-nil
// (i.e., a human or Opus ground-truth signal is available).
// y=1 if the answer was correct (grounded true OR escalation agreed); else 0.
// EXPORTED: Task 2 uses this to build the validation set.
func Label(e ledger.Entry) (y float64, ok bool) {
	if e.Grounded == nil && e.EscalatedAgreed == nil {
		return 0, false
	}
	ok = true
	if (e.Grounded != nil && *e.Grounded) || (e.EscalatedAgreed != nil && *e.EscalatedAgreed) {
		y = 1
	}
	return y, ok
}

// ---- JSON shapes -----------------------------------------------------------

// taskWeights holds everything needed to score one task at inference time.
type taskWeights struct {
	Features []string           `json:"features"` // always featNames (stored for readability)
	Weights  []float64          `json:"weights"`  // len = len(featNames)+1 (bias last)
	Means    map[string]float64 `json:"means"`
	Stds     map[string]float64 `json:"stds"`
}

// weightFile is the on-disk confhead-weights.json format.
type weightFile struct {
	Tasks map[string]taskWeights `json:"tasks"`
}

// ---- Model -----------------------------------------------------------------

// Model holds per-task logistic-regression weights loaded from disk (or Fit).
// A nil *Model is safe to call — Predict returns -1 (sentinel: no head).
type Model struct {
	tasks map[string]taskWeights
}

// Load reads confhead-weights.json from path. Returns nil if the file is absent
// or cannot be parsed (caller treats nil as "no correctness signal").
func Load(path string) *Model {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var wf weightFile
	if json.Unmarshal(data, &wf) != nil {
		return nil
	}
	return &Model{tasks: wf.Tasks}
}

// Predict returns p(correct) in [0,1] for a given task and feature vector.
// Returns -1 (sentinel) for a nil receiver or an unknown task, so callers can
// distinguish "no signal" from a confident low-probability prediction.
func (m *Model) Predict(task string, feat map[string]float64) float64 {
	if m == nil {
		return -1
	}
	tw, ok := m.tasks[task]
	if !ok {
		return -1
	}
	return predict(tw, feat)
}

// predict returns the sigmoid score for one feature vector using stored weights.
func predict(tw taskWeights, feat map[string]float64) float64 {
	z := 0.0
	for i, name := range tw.Features {
		v := feat[name] // absent keys yield 0 (zero value)
		v = standardize(v, tw.Means[name], tw.Stds[name])
		z += tw.Weights[i] * v
	}
	// bias is the last weight
	z += tw.Weights[len(tw.Features)]
	return sigmoid(z)
}

// ---- Training --------------------------------------------------------------

const (
	// minRows is the minimum number of labelled, non-cache-hit rows required to
	// train a task head. 100 (vs router's 200) is justified by the richer feature
	// set enabling better sample efficiency; provisional until more data accrues.
	minRows   = 100
	lrDefault = 0.1
	iters     = 500
	l2        = 1e-3
)

// row is one labelled sample for logistic regression.
type row struct {
	feat map[string]float64
	y    float64 // 1 = correct, 0 = incorrect
}

// Fit trains one logistic-regression head per eligible task from the provided
// entries (in-memory; no file I/O). Task 2's out-of-fold scoring calls this
// directly with fold-split slices instead of going through Train.
// Entries that are cache hits or lack a correctness label are skipped.
// Tasks with fewer than minRows labelled rows are omitted from the returned Model.
func Fit(entries []ledger.Entry) *Model { return FitWithMinRows(entries, minRows) }

// FitWithMinRows is Fit parameterized by the minimum labeled-row threshold.
// Production (Fit) uses minRows=100; Task 2's out-of-fold validation passes a
// lower threshold so a head can still train on a ~80-row training fold (the
// paired-bootstrap CI is the real adoption guard). Behavior is otherwise
// identical to Fit.
func FitWithMinRows(entries []ledger.Entry, minRowsArg int) *Model {
	byTask := map[string][]row{}
	for _, e := range entries {
		if e.CacheHit {
			continue
		}
		y, ok := Label(e)
		if !ok {
			continue
		}
		task := strings.ToLower(e.Task)
		byTask[task] = append(byTask[task], row{feat: FeatureRow(e), y: y})
	}

	tasks := map[string]taskWeights{}
	for task, rows := range byTask {
		if len(rows) < minRowsArg {
			continue
		}

		means, stds := computeStats(rows, featNames)

		n := len(rows)
		d := len(featNames)
		X := make([][]float64, n)
		Y := make([]float64, n)
		for i, r := range rows {
			xi := make([]float64, d+1)
			for j, name := range featNames {
				xi[j] = standardize(r.feat[name], means[name], stds[name])
			}
			xi[d] = 1.0 // bias
			X[i] = xi
			Y[i] = r.y
		}

		w := gradientDescent(X, Y, d+1, lrDefault, iters, l2)

		tasks[task] = taskWeights{
			Features: featNames,
			Weights:  w,
			Means:    means,
			Stds:     stds,
		}
	}
	return &Model{tasks: tasks}
}

// Train reads the JSONL ledger at ledgerPath plus the correctness-label sidecar
// at labelsPath, merges them, calls Fit, writes the model to outPath
// (confhead-weights.json), and returns a human-readable per-task report.
//
// The sidecar (written by the pipeline via ledger.AppendLabel) carries
// cascade-agreement labels for classify/triage — tasks grounding can't label —
// so the head can cover them once escalation traffic accrues. An empty
// labelsPath (or a missing file) behaves like "no sidecar". The report notes how
// many rows per task came from the sidecar.
func Train(ledgerPath, labelsPath, outPath string) (string, error) {
	entries, err := ledger.ReadAll(ledgerPath)
	if err != nil {
		return "", fmt.Errorf("confhead.Train: read ledger: %w", err)
	}
	var labels []ledger.Entry
	if labelsPath != "" {
		labels, err = ledger.ReadLabelFile(labelsPath)
		if err != nil {
			return "", fmt.Errorf("confhead.Train: read labels sidecar: %w", err)
		}
	}
	all := append(entries, labels...)

	// Count labelled rows per task for the report (incl. skipped tasks); track
	// how many came from the sidecar so the report can attribute them.
	byTaskRows := map[string][]row{}
	sidecarRows := map[string]int{}
	countInto := func(es []ledger.Entry, fromSidecar bool) {
		for _, e := range es {
			if e.CacheHit {
				continue
			}
			y, ok := Label(e)
			if !ok {
				continue
			}
			task := strings.ToLower(e.Task)
			byTaskRows[task] = append(byTaskRows[task], row{feat: FeatureRow(e), y: y})
			if fromSidecar {
				sidecarRows[task]++
			}
		}
	}
	countInto(entries, false)
	countInto(labels, true)

	m := Fit(all)

	wf := weightFile{Tasks: map[string]taskWeights{}}
	for task, tw := range m.tasks {
		wf.Tasks[task] = tw
	}

	data, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		return "", fmt.Errorf("confhead.Train: marshal: %w", err)
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return "", fmt.Errorf("confhead.Train: write: %w", err)
	}

	var sb strings.Builder
	// Report trained tasks first.
	for task, tw := range m.tasks {
		rows := byTaskRows[task]
		n := len(rows)
		Y := make([]float64, n)
		for i, r := range rows {
			Y[i] = r.y
		}
		pos := countPositive(Y)
		_ = tw
		fmt.Fprintf(&sb, "task=%s: %d rows (pos=%d neg=%d, %d from sidecar) trained OK\n", task, n, pos, n-pos, sidecarRows[task])
	}
	// Report skipped tasks.
	for task, rows := range byTaskRows {
		if _, trained := m.tasks[task]; trained {
			continue
		}
		fmt.Fprintf(&sb, "task=%s: only %d rows (%d from sidecar, need %d) — skipped\n", task, len(rows), sidecarRows[task], minRows)
	}
	return sb.String(), nil
}

// ---- helpers ---------------------------------------------------------------

func computeStats(rows []row, names []string) (means, stds map[string]float64) {
	means = map[string]float64{}
	stds = map[string]float64{}
	n := float64(len(rows))
	for _, name := range names {
		sum := 0.0
		for _, r := range rows {
			sum += r.feat[name]
		}
		mn := sum / n
		means[name] = mn
		ss := 0.0
		for _, r := range rows {
			d := r.feat[name] - mn
			ss += d * d
		}
		sd := math.Sqrt(ss / n)
		if sd < 1e-9 {
			sd = 1.0 // constant feature — no-op after standardisation
		}
		stds[name] = sd
	}
	return
}

func standardize(v, mean, std float64) float64 {
	return (v - mean) / std
}

func sigmoid(z float64) float64 {
	return 1.0 / (1.0 + math.Exp(-z))
}

// gradientDescent fits logistic regression returning weight vector of length d.
func gradientDescent(X [][]float64, Y []float64, d int, lr float64, maxIter int, lambda float64) []float64 {
	n := len(X)
	w := make([]float64, d)
	for iter := 0; iter < maxIter; iter++ {
		grad := make([]float64, d)
		for i := 0; i < n; i++ {
			z := 0.0
			for j := 0; j < d; j++ {
				z += w[j] * X[i][j]
			}
			e := sigmoid(z) - Y[i]
			for j := 0; j < d; j++ {
				grad[j] += e * X[i][j]
			}
		}
		// L2 regularisation (bias term at d-1 is not regularised)
		for j := 0; j < d; j++ {
			reg := 0.0
			if j < d-1 {
				reg = lambda * w[j]
			}
			w[j] -= lr * (grad[j]/float64(n) + reg)
		}
	}
	return w
}

func countPositive(Y []float64) int {
	c := 0
	for _, y := range Y {
		if y == 1.0 {
			c++
		}
	}
	return c
}
