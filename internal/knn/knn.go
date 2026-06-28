// Package knn implements a zero-training k-nearest-neighbor entry-tier pre-filter
// over embeddinggemma vectors of past inputs. It is a bridge before the LR router
// (internal/router) has enough rows to train: it predicts whether the small E2B
// tier will accept an input, from the E2B-accept labels the shadow-labeling
// flywheel already manufactures, embedded at drain time. Brute-force cosine;
// fail-open; nil-safe.
package knn

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Row is one labeled substrate point: the embedding of a past input and whether
// the small E2B tier accepted it.
type Row struct {
	Task   string    `json:"task"`
	Vec    []float64 `json:"vec"`
	Accept bool      `json:"accept"`
}

// Index holds per-task labeled vectors loaded from a JSONL substrate.
// A nil *Index is safe to query (PreferLargerEntry returns no preference).
type Index struct {
	byTask map[string][]Row
}

var appendMu sync.Mutex

// Load reads the JSONL substrate at path. Returns nil if the file is absent or
// unreadable (caller treats nil as "no kNN preference"); malformed lines are
// skipped. A present-but-empty file yields a non-nil, empty Index.
func Load(path string) *Index {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	ix := &Index{byTask: map[string][]Row{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Row
		if json.Unmarshal(line, &r) != nil || r.Task == "" || len(r.Vec) == 0 {
			continue
		}
		ix.byTask[r.Task] = append(ix.byTask[r.Task], r)
	}
	return ix
}

// Append writes one row as a JSON line (O_APPEND, concurrency-safe). It creates
// the parent directory if needed.
func Append(path string, r Row) error {
	appendMu.Lock()
	defer appendMu.Unlock()
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	val, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(val, '\n'))
	return err
}

// PreferLargerEntry returns (skip, ok). skip=true means the kNN predicts E2B will
// NOT accept this input — the fraction of the k nearest same-task neighbors that
// accepted at E2B is below threshold — so the caller should skip E2B and enter at
// a larger tier. ok=false signals no usable signal (nil receiver, unknown task,
// empty query, or fewer than minNeighbors usable rows): the caller keeps the
// default entry. Rows whose vector dimension differs from the query are ignored.
func (ix *Index) PreferLargerEntry(task string, query []float64, k, minNeighbors int, threshold float64) (skip bool, ok bool) {
	if ix == nil || len(query) == 0 {
		return false, false
	}
	rows := ix.byTask[task]
	if len(rows) == 0 {
		return false, false
	}
	type scored struct {
		sim    float64
		accept bool
	}
	var cand []scored
	for _, r := range rows {
		if len(r.Vec) != len(query) {
			continue // dimension mismatch — ignore
		}
		cand = append(cand, scored{sim: cosine(query, r.Vec), accept: r.Accept})
	}
	if len(cand) < minNeighbors {
		return false, false
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].sim > cand[j].sim })
	if k > len(cand) {
		k = len(cand)
	}
	if k <= 0 { // defensive: a non-positive k would divide by zero below
		return false, false
	}
	accepted := 0
	for i := 0; i < k; i++ {
		if cand[i].accept {
			accepted++
		}
	}
	frac := float64(accepted) / float64(k)
	return frac < threshold, true
}

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
