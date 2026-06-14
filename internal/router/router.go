// Package router implements a per-task logistic entry-tier router.
// It reads the JSONL ledger, fits a binary logistic regression (pure Go,
// gradient descent, L2 regularisation) per task, and persists the weights
// so that the pipeline can call PreferLargerEntry to decide whether to skip
// the small E2B tier and enter at E4B directly.
package router

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/dmmdea/local-offload-pp-cli/internal/ledger"
)

// ---- JSON shapes --------------------------------------------------------

// taskWeights holds everything needed to score one task at inference time.
type taskWeights struct {
	Features []string             `json:"features"`  // ordered feature names
	Weights  []float64            `json:"weights"`   // len = len(Features)+1 (bias last)
	Means    map[string]float64   `json:"means"`
	Stds     map[string]float64   `json:"stds"`
}

// weightFile is the on-disk router-weights.json.
type weightFile struct {
	Tasks map[string]taskWeights `json:"tasks"`
}

// ---- Model --------------------------------------------------------------

// Model holds per-task logistic-regression weights loaded from disk.
// A nil *Model is safe to call — PreferLargerEntry returns false (keep default).
type Model struct {
	tasks map[string]taskWeights
}

// Load reads router-weights.json from path. Returns nil if the file is absent
// or cannot be parsed (caller should treat nil as "no routing preference").
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

// PreferLargerEntry returns true when the model predicts that P(accept at
// E2B) < 0.5 for the given task and feature vector, meaning the caller
// should skip E2B and enter at E4B. Returns false for unknown tasks or a
// nil receiver.
func (m *Model) PreferLargerEntry(task string, feat map[string]float64) bool {
	if m == nil {
		return false
	}
	tw, ok := m.tasks[task]
	if !ok {
		return false
	}
	p := predict(tw, feat)
	return p < 0.5
}

// predict returns P(accept at E2B) for one feature vector.
func predict(tw taskWeights, feat map[string]float64) float64 {
	z := 0.0
	for i, name := range tw.Features {
		v := feat[name]
		v = standardize(v, tw.Means[name], tw.Stds[name])
		z += tw.Weights[i] * v
	}
	// bias is the last weight
	z += tw.Weights[len(tw.Features)]
	return sigmoid(z)
}

// ---- Training -----------------------------------------------------------

const (
	minRows   = 200
	lrDefault = 0.1
	iters     = 500
	l2        = 1e-3
)

// row is one labelled sample for logistic regression.
type row struct {
	feat map[string]float64
	y    float64 // 1 = accepted at E2B, 0 = not
}

// Train reads the JSONL ledger at ledgerPath, fits one logistic-regression
// model per eligible task (triage/classify with >=minRows labelled rows),
// writes the results to outPath (router-weights.json), and returns a
// human-readable report.
func Train(ledgerPath, outPath string) (string, error) {
	entries, err := readLedger(ledgerPath)
	if err != nil {
		return "", fmt.Errorf("router.Train: read ledger: %w", err)
	}

	// Partition by task; only E2B entry rows are labelled.
	byTask := map[string][]row{}
	for _, e := range entries {
		task := strings.ToLower(e.Task)
		if task != "triage" && task != "classify" {
			continue
		}
		if e.ModelTier != "gemma4-e2b" {
			continue // only rows that entered at E2B are labelled
		}
		if len(e.Feat) == 0 {
			continue
		}
		y := 0.0
		if e.Escalations == 0 && !e.Deferred && (e.Grounded == nil || *e.Grounded) {
			y = 1.0
		}
		byTask[task] = append(byTask[task], row{feat: e.Feat, y: y})
	}

	wf := weightFile{Tasks: map[string]taskWeights{}}
	var sb strings.Builder

	for task, rows := range byTask {
		if len(rows) < minRows {
			fmt.Fprintf(&sb, "task=%s: only %d rows (need %d) — skipped\n", task, len(rows), minRows)
			continue
		}

		// Collect ordered feature names from the first row that has data.
		featNames := featOrder(rows[0].feat)

		// Compute per-feature mean and std.
		means, stds := computeStats(rows, featNames)

		// Build design matrix X (n x (d+1)) and label vector Y.
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

		// Fit logistic regression.
		w := gradientDescent(X, Y, d+1, lrDefault, iters, l2)

		tw := taskWeights{
			Features: featNames,
			Weights:  w,
			Means:    means,
			Stds:     stds,
		}
		wf.Tasks[task] = tw

		pos := countPositive(Y)
		fmt.Fprintf(&sb, "task=%s: %d rows (pos=%d neg=%d) trained OK\n",
			task, n, pos, n-pos)
	}

	// Write JSON.
	data, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		return sb.String(), fmt.Errorf("router.Train: marshal: %w", err)
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return sb.String(), fmt.Errorf("router.Train: write: %w", err)
	}
	fmt.Fprintf(&sb, "wrote %s\n", outPath)
	return sb.String(), nil
}

// ---- helpers ------------------------------------------------------------

func readLedger(path string) ([]ledger.Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []ledger.Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e ledger.Entry
		if json.Unmarshal(line, &e) != nil {
			continue // skip malformed
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// featOrder returns a deterministic sorted list of feature names.
func featOrder(feat map[string]float64) []string {
	known := []string{"len_chars", "n_words", "n_numbers", "n_caps", "has_code", "has_url"}
	// Include only keys that exist in the sample feat map; append any extras.
	present := map[string]bool{}
	for k := range feat {
		present[k] = true
	}
	var out []string
	for _, k := range known {
		if present[k] {
			out = append(out, k)
		}
	}
	// Any additional keys not in the canonical list.
	for k := range feat {
		inKnown := false
		for _, kk := range known {
			if kk == k {
				inKnown = true
				break
			}
		}
		if !inKnown {
			out = append(out, k)
		}
	}
	return out
}

func computeStats(rows []row, names []string) (means, stds map[string]float64) {
	means = map[string]float64{}
	stds = map[string]float64{}
	n := float64(len(rows))
	for _, name := range names {
		sum := 0.0
		for _, r := range rows {
			sum += r.feat[name]
		}
		m := sum / n
		means[name] = m
		ss := 0.0
		for _, r := range rows {
			d := r.feat[name] - m
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
			// dot product
			z := 0.0
			for j := 0; j < d; j++ {
				z += w[j] * X[i][j]
			}
			err := sigmoid(z) - Y[i]
			for j := 0; j < d; j++ {
				grad[j] += err * X[i][j]
			}
		}
		// update with L2 regularisation (skip bias term d-1 from regularisation)
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
