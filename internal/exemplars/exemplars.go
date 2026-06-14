// Package exemplars implements BM25-based few-shot exemplar mining with
// greedy-diverse k-center selection. It maintains per-task JSONL sidecars of
// verified-good (input, output) pairs and, on demand, selects a diverse ~50
// finalists subset and persists it for fast retrieval.
//
// Concurrency: Append uses O_APPEND for cross-process safety plus an in-process
// mutex per task file. Select and Retrieve are read-only after the selected.json
// is written and safe to call concurrently.
package exemplars

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Pair is one exemplar stored in the sidecar or returned by Retrieve.
type Pair struct {
	Input     string  `json:"input"`
	Output    string  `json:"output"`
	ParamsKey string  `json:"params_key,omitempty"`
	Margin    float64 `json:"margin,omitempty"`
}

// BM25 parameters.
const (
	bm25K1     = 1.5
	bm25B      = 0.75
	targetSize = 50
)

// fileMu serialises in-process appends per absolute path.
var (
	fileMuMu sync.Mutex
	fileMus  = map[string]*sync.Mutex{}
)

func lockFor(path string) *sync.Mutex {
	fileMuMu.Lock()
	defer fileMuMu.Unlock()
	if m, ok := fileMus[path]; ok {
		return m
	}
	m := &sync.Mutex{}
	fileMus[path] = m
	return m
}

// sidecarPath returns <dir>/<task>.jsonl
func sidecarPath(dir, task string) string { return filepath.Join(dir, task+".jsonl") }

// selectedPath returns <dir>/<task>.selected.json
func selectedPath(dir, task string) string { return filepath.Join(dir, task+".selected.json") }

// Append appends a verified-good (input, output) pair to the per-task sidecar.
// It is concurrency-safe across goroutines (mutex) and across processes (O_APPEND).
func Append(dir, task, paramsKey, input string, output []byte, margin float64) error {
	p := sidecarPath(dir, task)
	mu := lockFor(p)
	mu.Lock()
	defer mu.Unlock()

	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("exemplars.Append: open %s: %w", p, err)
	}
	defer f.Close()

	pair := Pair{
		Input:     input,
		Output:    string(output),
		ParamsKey: paramsKey,
		Margin:    margin,
	}
	line, err := json.Marshal(pair)
	if err != nil {
		return fmt.Errorf("exemplars.Append: marshal: %w", err)
	}
	line = append(line, '\n')
	_, err = f.Write(line)
	return err
}

// ---- tokenisation -------------------------------------------------------

func tokenise(s string) []string {
	words := strings.Fields(strings.ToLower(s))
	return words
}

func tokenSet(tokens []string) map[string]bool {
	s := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		s[t] = true
	}
	return s
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 1.0
	}
	return float64(inter) / float64(union)
}

// ---- BM25 ---------------------------------------------------------------

type bm25Index struct {
	docs    [][]string         // tokenised docs
	sets    []map[string]bool  // token sets per doc
	df      map[string]int     // document frequency
	avgDL   float64
	n       int
}

func buildIndex(docs [][]string) bm25Index {
	n := len(docs)
	df := map[string]int{}
	total := 0
	sets := make([]map[string]bool, n)
	for i, d := range docs {
		sets[i] = tokenSet(d)
		for t := range sets[i] {
			df[t]++
		}
		total += len(d)
	}
	avg := 0.0
	if n > 0 {
		avg = float64(total) / float64(n)
	}
	return bm25Index{docs: docs, sets: sets, df: df, avgDL: avg, n: n}
}

func (idx *bm25Index) score(docIdx int, query []string) float64 {
	dl := float64(len(idx.docs[docIdx]))
	score := 0.0
	qf := map[string]int{}
	for _, t := range query {
		qf[t]++
	}
	for t, qtf := range qf {
		_ = qtf
		df := float64(idx.df[t])
		if df == 0 {
			continue
		}
		idf := math.Log((float64(idx.n)-df+0.5)/(df+0.5) + 1)
		// term freq in document
		tf := 0
		for _, tok := range idx.docs[docIdx] {
			if tok == t {
				tf++
			}
		}
		num := float64(tf) * (bm25K1 + 1)
		den := float64(tf) + bm25K1*(1-bm25B+bm25B*dl/max(idx.avgDL, 1))
		score += idf * num / den
	}
	return score
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// ---- load sidecar -------------------------------------------------------

func loadSidecar(path string) ([]Pair, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var pairs []Pair
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var p Pair
		if json.Unmarshal(line, &p) != nil {
			continue // skip malformed
		}
		pairs = append(pairs, p)
	}
	return pairs, sc.Err()
}

// ---- Select -------------------------------------------------------------

// Select builds a BM25 index over the sidecar for `task`, then runs a
// greedy k-center diverse pick targeting ~50 finalists, and writes the result
// to <dir>/<task>.selected.json. It returns a human-readable report.
func Select(dir, task string) (string, error) {
	pairs, err := loadSidecar(sidecarPath(dir, task))
	if err != nil {
		return "", fmt.Errorf("exemplars.Select: read sidecar: %w", err)
	}
	if len(pairs) == 0 {
		return "exemplars.Select: no pairs in sidecar", nil
	}

	// Build a "centroid" document = all inputs concatenated for IDF reference.
	allTokens := make([][]string, len(pairs))
	for i, p := range pairs {
		allTokens[i] = tokenise(p.Input)
	}
	idx := buildIndex(allTokens)

	// Score each doc against the centroid query (union of all tokens, once each).
	centroidTokens := make([]string, 0, len(idx.df))
	for t := range idx.df {
		centroidTokens = append(centroidTokens, t)
	}

	scores := make([]float64, len(pairs))
	for i := range pairs {
		scores[i] = idx.score(i, centroidTokens)
	}

	// Greedy k-center: start with the highest-scoring doc, then iteratively pick
	// the candidate with the highest BM25 score that is most dissimilar (lowest
	// max-Jaccard) to the already-selected set.
	k := targetSize
	if k > len(pairs) {
		k = len(pairs)
	}

	selected := make([]int, 0, k)
	// seed: highest centroid score
	best := 0
	for i := 1; i < len(pairs); i++ {
		if scores[i] > scores[best] {
			best = i
		}
	}
	selected = append(selected, best)
	inSelected := map[int]bool{best: true}

	for len(selected) < k {
		// For each candidate, compute min dissimilarity to selected set
		// (1 - maxJaccard). We want to maximise dissimilarity and break ties by BM25.
		bestCand := -1
		bestDis := -1.0
		bestScore := -1.0
		for i := range pairs {
			if inSelected[i] {
				continue
			}
			// max similarity to any already-selected
			maxJac := 0.0
			for _, s := range selected {
				j := jaccard(idx.sets[i], idx.sets[s])
				if j > maxJac {
					maxJac = j
				}
			}
			dis := 1.0 - maxJac
			if dis > bestDis || (dis == bestDis && scores[i] > bestScore) {
				bestDis = dis
				bestCand = i
				bestScore = scores[i]
			}
		}
		if bestCand < 0 {
			break
		}
		selected = append(selected, bestCand)
		inSelected[bestCand] = true
	}

	out := make([]Pair, len(selected))
	for i, idx2 := range selected {
		out[i] = pairs[idx2]
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("exemplars.Select: marshal: %w", err)
	}
	dst := selectedPath(dir, task)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return "", fmt.Errorf("exemplars.Select: write %s: %w", dst, err)
	}

	return fmt.Sprintf("exemplars.Select: task=%s total=%d selected=%d written=%s",
		task, len(pairs), len(selected), dst), nil
}

// ---- Retrieve -----------------------------------------------------------

// Retrieve loads <dir>/<task>.selected.json, BM25-ranks the entries against
// `input`, and returns the top-k diverse results. Never errors; missing file
// or any I/O issue returns nil.
func Retrieve(dir, task, input string, k int) []Pair {
	data, err := os.ReadFile(selectedPath(dir, task))
	if err != nil {
		return nil
	}
	var pool []Pair
	if json.Unmarshal(data, &pool) != nil {
		return nil
	}
	if len(pool) == 0 {
		return nil
	}
	if k <= 0 {
		return nil
	}

	// Tokenise and build index over pool.
	docs := make([][]string, len(pool))
	for i, p := range pool {
		docs[i] = tokenise(p.Input)
	}
	idx := buildIndex(docs)
	query := tokenise(input)

	// Score each entry.
	scores := make([]float64, len(pool))
	for i := range pool {
		scores[i] = idx.score(i, query)
	}

	// Greedy pick k entries by BM25 score, skipping near-duplicates (Jaccard > 0.8).
	if k > len(pool) {
		k = len(pool)
	}
	picked := make([]Pair, 0, k)
	pickedSets := make([]map[string]bool, 0, k)
	used := make([]bool, len(pool))

	for len(picked) < k {
		// pick highest-scoring unused
		best := -1
		for i := range pool {
			if used[i] {
				continue
			}
			if best < 0 || scores[i] > scores[best] {
				best = i
			}
		}
		if best < 0 {
			break
		}
		used[best] = true
		// diversity gate: skip if too similar to any already-picked
		tooSim := false
		for _, ps := range pickedSets {
			if jaccard(idx.sets[best], ps) > 0.8 {
				tooSim = true
				break
			}
		}
		if tooSim {
			continue
		}
		picked = append(picked, pool[best])
		pickedSets = append(pickedSets, idx.sets[best])
	}

	return picked
}
