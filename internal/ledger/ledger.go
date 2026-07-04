// Package ledger records per-call telemetry to an append-only JSONL file and
// reports estimated tokens (and dollars) kept out of Opus — how the harness
// proves it earns its keep.
//
// JSONL, not a locked key-value store, on purpose: the long-running MCP server
// and an occasional `local-offload ledger` invocation must both touch the
// ledger at once. Writers append with O_APPEND (atomic for the small one-line
// records on both POSIX and Windows); the reader takes no lock and tolerates a
// not-yet-complete trailing line. This is what lets the savings report run while
// the MCP server is live (the bbolt version could not — exclusive file lock).
package ledger

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// Entry is one offload call. The self-learning fields (margin..feat) are written
// by the pipeline from core.Meta; old lines without them parse fine (zero values).
type Entry struct {
	TS        int64   `json:"ts"`
	Task      string  `json:"task"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	LatencyMs int64   `json:"latency_ms"`
	TokPerSec float64 `json:"tok_per_s"`
	CacheHit  bool    `json:"cache_hit"`
	Deferred  bool    `json:"deferred"`
	// --- self-learning signals (Phase 0 enrichment) ---
	Margin          float64            `json:"margin,omitempty"`
	ModelTier       string             `json:"model_tier,omitempty"`
	Escalations     int                `json:"escalations,omitempty"`
	Reasoning       bool               `json:"reasoning,omitempty"` // produced by the terminal reasoning tier (a reclaimed deferral)
	Retries         int                `json:"retries,omitempty"`
	Truncated       bool               `json:"truncated,omitempty"`
	Grounded        *bool              `json:"grounded,omitempty"`
	EscalatedAgreed *bool              `json:"escalated_agreed,omitempty"`
	ErrClass        string             `json:"err_class,omitempty"`
	InputChars      int                `json:"input_chars,omitempty"`
	Feat            map[string]float64 `json:"feat,omitempty"`
	// Reason is the human-readable defer reason (LO-8), set only on deferred
	// entries and truncated to maxReasonLen on write. Old ledger lines without
	// the field parse fine (empty string).
	Reason string `json:"reason,omitempty"`
}

// maxReasonLen bounds a recorded defer reason so a long upstream error can't
// bloat the one-line ledger records (they must stay O_APPEND-atomic-small).
const maxReasonLen = 120

// truncateReason caps s at maxReasonLen bytes, backing off to a rune boundary
// so a multibyte character is never split.
func truncateReason(s string) string {
	if len(s) <= maxReasonLen {
		return s
	}
	cut := s[:maxReasonLen]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// Ledger appends entries to a JSONL file. The mutex serializes in-process
// writes; cross-process safety relies on O_APPEND atomicity for small lines.
type Ledger struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// Open opens (creating if needed) the JSONL ledger for appending. It does NOT
// take an exclusive lock, so multiple processes can append concurrently.
func Open(path string) (*Ledger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Ledger{f: f, path: path}, nil
}

// Close releases the append handle.
func (l *Ledger) Close() error {
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}

// Record appends one entry as a single JSON line.
func (l *Ledger) Record(e Entry) error {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	e.Reason = truncateReason(e.Reason)
	val, err := json.Marshal(e)
	if err != nil {
		return err
	}
	val = append(val, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err = l.f.Write(val); err != nil {
		return err
	}
	// fsync each entry so a crash can't lose recorded savings — sub-ms on NVMe,
	// negligible against multi-second model inference on the call path.
	return l.f.Sync()
}

// labelMu serializes sidecar appends across concurrent in-process callers (the
// long-running MCP server may label several escalations at once). Cross-process
// safety relies on O_APPEND atomicity for the small one-line records.
var labelMu sync.Mutex

// AppendLabel appends e as ONE JSON line to a correctness-label sidecar file
// (NOT the main ledger — kept separate so the router/calibration/savings report
// stay pristine). Only the confhead reads it. It creates the parent dir and
// stamps TS if unset; concurrent callers are serialized by labelMu.
func AppendLabel(path string, e Entry) error {
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	val, err := json.Marshal(e)
	if err != nil {
		return err
	}
	val = append(val, '\n')
	labelMu.Lock()
	defer labelMu.Unlock()
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
	_, err = f.Write(val)
	return err
}

// ReadLabelFile loads every parseable entry from a correctness-label sidecar
// (mirrors ReadAll). A missing file returns (nil, nil); blank/malformed lines
// are skipped.
func ReadLabelFile(path string) ([]Entry, error) {
	return ReadAll(path)
}

// Summary aggregates the ledger.
type Summary struct {
	Calls             int            `json:"calls"`
	CacheHits         int            `json:"cache_hits"`
	Deferred          int            `json:"deferred"`
	Completed         int            `json:"completed"`
	ReasoningReclaims int            `json:"reasoning_reclaims"` // completed via the terminal reasoning tier (deferrals it reclaimed before Opus)
	TokensSaved       int            `json:"tokens_saved"`       // input tokens kept out of Opus on completed/cache calls
	TokensOut         int            `json:"tokens_out"`
	// EstValueKeptLocal is the estimated Opus-INPUT value of the tokens kept
	// local (tokens_saved x opus_input_price_per_mtok / 1M). It is an estimate
	// of avoided cloud input pricing, NOT literal billed dollars saved (LO-12:
	// the old est_dollar_saved name presented it as money in the bank).
	EstValueKeptLocal float64 `json:"est_value_kept_local"`
	// Deprecated: the same number under the old, misleading name — kept
	// emitted for one release for consumers of the JSON; remove in v0.7.
	EstDollarSaved float64        `json:"est_dollar_saved"`
	ByTask         map[string]int `json:"by_task"`
}

// Summarize aggregates this ledger's file since `since` (unix; 0 = all).
func (l *Ledger) Summarize(since int64, opusPricePerMTok float64) (Summary, error) {
	return SummarizeFile(l.path, since, opusPricePerMTok)
}

// ReadAll loads every parseable entry from a JSONL ledger (lock-free; skips
// malformed/partial lines). Missing file => empty slice, no error.
func ReadAll(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// ReasonCount is one aggregated defer reason for the ledger report.
type ReasonCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// TopDeferReasons aggregates the defer reasons of entries since `since`
// (unix; 0 = all) and returns the topN most frequent, ties broken
// alphabetically for deterministic output. Deferred entries written before
// the reason field existed count under "(unrecorded)" so the denominator
// stays honest. Lock-free like SummarizeFile; a missing file returns nil.
func TopDeferReasons(path string, since int64, topN int) ([]ReasonCount, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, e := range entries {
		if !e.Deferred || e.CacheHit {
			continue
		}
		if since > 0 && e.TS < since {
			continue
		}
		r := e.Reason
		if r == "" {
			r = "(unrecorded)"
		}
		counts[r]++
	}
	if len(counts) == 0 {
		return nil, nil // nil (not an empty slice) so the JSON field stays omitempty
	}
	out := make([]ReasonCount, 0, len(counts))
	for r, n := range counts {
		out = append(out, ReasonCount{Reason: r, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Reason < out[j].Reason
	})
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out, nil
}

// SummarizeFile reads a JSONL ledger without any lock — safe to call while
// another process is appending (a partial final line is skipped). A missing
// file reports an empty summary (nothing offloaded yet), not an error.
func SummarizeFile(path string, since int64, opusPricePerMTok float64) (Summary, error) {
	s := Summary{ByTask: map[string]int{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) != nil {
			continue // malformed or a not-yet-complete trailing line
		}
		if since > 0 && e.TS < since {
			continue
		}
		s.Calls++
		s.ByTask[e.Task]++
		switch {
		case e.CacheHit:
			s.CacheHits++
			s.TokensSaved += e.TokensIn // a cache hit also saved Opus those tokens
		case e.Deferred:
			s.Deferred++
		default:
			s.Completed++
			s.TokensSaved += e.TokensIn
			s.TokensOut += e.TokensOut
			if e.Reasoning {
				s.ReasoningReclaims++ // a completed reasoning entry = a deferral reclaimed before Opus
			}
		}
	}
	// Same math as ever — only the CLAIM changed (LO-12): this is the est.
	// Opus-input value of locally-kept tokens, not billed savings.
	s.EstValueKeptLocal = float64(s.TokensSaved) / 1_000_000 * opusPricePerMTok
	s.EstDollarSaved = s.EstValueKeptLocal // Deprecated alias; remove in v0.7
	return s, sc.Err()
}
