// Package seatrate remembers, per planner seat, the two numbers a wall has to
// be sized from — the seat's effective decode rate and its cold-load time —
// and turns them into a wall ESTIMATE for a contract (register D-03).
//
// The numbers come from real runs: every agent run measures the wall of each
// planner completion (calls[].ms) and the admission warm-up measures a cold
// load when it waits for one. The store is machine-local (the GPU-lease state
// root): a seat's rate is a fact about THIS box's cards, never something to
// replicate. A missing or corrupt store is no memory, never an error — the
// estimate then says so and the run proceeds under the contract's wall exactly
// as before. Sizing is PUBLISHED (wall_estimate_sec, min_turn_sec, wall_note),
// not imposed: raising a contract's wall silently would change production
// placement behind the caller's back.
package seatrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileName is the store's file name under the state root.
const FileName = "seat-rates.json"

const (
	// minSampleTokens: a completion counts toward the rate only when it
	// generated at least this many tokens. Tool-call completions (25–60
	// tokens on the Qube 27B) are dominated by the prefill of a growing
	// transcript and would drag the "decode" rate to a fraction of itself.
	minSampleTokens = 128
	// emaWeight is the weight of the newest run's rate.
	emaWeight = 0.3
	// coldLoadWindow: cold_load_sec is the MAX of this many most recent loads —
	// a wall must hold the slow load, not the average one.
	coldLoadWindow = 5
	// toolStepTokens / stepOverheadSec are the per-step constants of the
	// estimate: a tool step's completion (25–285 tokens measured on the 27B,
	// 2026-09-10) and the prefill of the transcript it re-sends (~6 s at 200k
	// tokens on the 27B, measured: 213,785 prompt tokens over 12 steps in a
	// 381 s wall of which ~300 s were the final answer). Named here so the
	// note can print them and a reader can refute them.
	toolStepTokens = 128
	stepOverheadSec = 6
)

// Seat is one seat's remembered numbers.
type Seat struct {
	// TokS is the effective decode rate (completion tokens per second of call
	// wall over completions of ≥ minSampleTokens), an EMA over runs.
	TokS float64 `json:"tok_s"`
	// ColdLoadSec is the slowest of the last coldLoadWindow observed loads.
	ColdLoadSec float64   `json:"cold_load_sec,omitempty"`
	ColdLoads   []float64 `json:"cold_loads,omitempty"`
	Samples     int       `json:"samples"`
	Updated     time.Time `json:"updated"`
}

// Store is the per-seat memory. Zero value = empty.
type Store struct {
	Seats map[string]Seat `json:"seats"`
	path  string
}

// Path is the store file under stateRoot.
func Path(stateRoot string) string { return filepath.Join(stateRoot, FileName) }

// Load reads the store at path. A missing or unreadable file yields an empty
// store and a nil error; a corrupt one yields an empty store and the parse
// error, for the caller to log — never to fail a run on.
func Load(path string) (*Store, error) {
	s := &Store{Seats: map[string]Seat{}, path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return s, nil
	}
	var on Store
	if jerr := json.Unmarshal(data, &on); jerr != nil {
		return s, fmt.Errorf("seatrate: %s is not readable (%v); starting empty", path, jerr)
	}
	if on.Seats != nil {
		s.Seats = on.Seats
	}
	return s, nil
}

// Get returns the remembered numbers for seat (zero when unknown).
func (s *Store) Get(seat string) Seat {
	if s == nil || s.Seats == nil {
		return Seat{}
	}
	return s.Seats[seat]
}

// Observe folds one run's measurement in: tokS (0 = no rate sample this run)
// and coldLoadSec (0 = the seat was already loaded). Returns whether anything
// changed.
func (s *Store) Observe(seat string, tokS, coldLoadSec float64, now time.Time) bool {
	if s == nil || strings.TrimSpace(seat) == "" || (tokS <= 0 && coldLoadSec <= 0) {
		return false
	}
	if s.Seats == nil {
		s.Seats = map[string]Seat{}
	}
	cur := s.Seats[seat]
	if tokS > 0 {
		if cur.Samples == 0 || cur.TokS <= 0 {
			cur.TokS = tokS
		} else {
			cur.TokS = emaWeight*tokS + (1-emaWeight)*cur.TokS
		}
		cur.Samples++
	}
	if coldLoadSec > 0 {
		cur.ColdLoads = append(cur.ColdLoads, math.Round(coldLoadSec*10)/10)
		if len(cur.ColdLoads) > coldLoadWindow {
			cur.ColdLoads = cur.ColdLoads[len(cur.ColdLoads)-coldLoadWindow:]
		}
		cur.ColdLoadSec = 0
		for _, c := range cur.ColdLoads {
			if c > cur.ColdLoadSec {
				cur.ColdLoadSec = c
			}
		}
	}
	cur.Updated = now
	s.Seats[seat] = cur
	return true
}

// Save writes the store atomically (temp file + rename) so concurrent
// processes (the MCP servers and the CLI share one state root) never read a
// half-written file. The directory is created when missing.
func (s *Store) Save() error {
	if s == nil || s.path == "" {
		return errors.New("seatrate: store has no path")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", s.path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Call is the per-completion fact the rate is measured from.
type Call struct {
	CompletionTokens int
	Ms               int64
}

// Rate derives a run's effective decode rate from its completions: tokens per
// second of call wall over completions of ≥ minSampleTokens. samples is how
// many completions qualified; 0 means no rate (tokS 0).
func Rate(calls []Call) (tokS float64, samples int) {
	var tokens int
	var ms int64
	for _, c := range calls {
		if c.CompletionTokens < minSampleTokens || c.Ms <= 0 {
			continue
		}
		tokens += c.CompletionTokens
		ms += c.Ms
		samples++
	}
	if samples == 0 || ms <= 0 {
		return 0, 0
	}
	return math.Round(float64(tokens)/(float64(ms)/1000)*10) / 10, samples
}

// Input is what the estimate is computed from.
type Input struct {
	Seat         string
	TokS         float64 // 0 = unknown
	RateSamples  int
	RateSource   string // "store" | "config" | ""
	ColdLoadSec  float64
	MaxSteps     int
	StepBudget   int // planner tokens per tool step
	FinalBudget  int // the final answer's budget
	ThinkingAuto bool
	TimeoutSec   int
}

// Estimate is the published sizing.
type Estimate struct {
	// TotalSec is the wall the contract is estimated to need on this seat.
	TotalSec int
	// MinTurnSec is a cold load plus ONE turn at the final budget — the least
	// wall a retry is worth starting with (register D-46).
	MinTurnSec int
	// Note names the arithmetic, or why there is none.
	Note string
	// Below is true when the contract's wall is under TotalSec.
	Below bool
}

// Compute derives the estimate. With no rate it returns only the note.
func Compute(in Input) Estimate {
	seat := in.Seat
	if seat == "" {
		seat = "the seat"
	}
	if in.TokS <= 0 {
		note := fmt.Sprintf("no decode-rate sample for %s yet (the first run that generates ≥ %d tokens in one completion records one; or set agent_seat_tok_s)", seat, minSampleTokens)
		if in.ColdLoadSec > 0 {
			note += fmt.Sprintf("; cold load %.0f s known", in.ColdLoadSec)
		}
		return Estimate{Note: note}
	}
	steps := in.MaxSteps
	if steps < 1 {
		steps = 1
	}
	stepBudget := in.StepBudget
	if stepBudget <= 0 {
		stepBudget = 1024
	}
	final := in.FinalBudget
	if final <= 0 {
		final = stepBudget
	}
	cold := in.ColdLoadSec
	finalSec := float64(final) / in.TokS
	var thinkSec float64
	if in.ThinkingAuto {
		thinkSec = float64(stepBudget) / in.TokS
	}
	toolSteps := steps - 1
	stepsSec := float64(toolSteps) * (float64(toolStepTokens)/in.TokS + stepOverheadSec)
	total := cold + thinkSec + stepsSec + finalSec
	est := Estimate{
		TotalSec:   int(math.Ceil(total)),
		MinTurnSec: int(math.Ceil(cold + finalSec)),
	}
	src := in.RateSource
	if src == "" {
		src = "store"
	}
	var b strings.Builder
	if in.TimeoutSec > 0 && in.TimeoutSec < est.TotalSec {
		est.Below = true
		fmt.Fprintf(&b, "wall %d s is BELOW the estimate %d s for %s: ", in.TimeoutSec, est.TotalSec, seat)
	} else if in.TimeoutSec > 0 {
		fmt.Fprintf(&b, "wall %d s vs estimate %d s for %s: ", in.TimeoutSec, est.TotalSec, seat)
	} else {
		fmt.Fprintf(&b, "estimate %d s for %s: ", est.TotalSec, seat)
	}
	fmt.Fprintf(&b, "cold load %.0f s", cold)
	if in.ThinkingAuto {
		fmt.Fprintf(&b, " + one think block %d tok (%.0f s)", stepBudget, thinkSec)
	}
	fmt.Fprintf(&b, " + %d tool steps × (%d tok + %d s prefill) (%.0f s) + final %d tok (%.0f s) at %.1f tok/s (%s, %d samples); min_turn %d s",
		toolSteps, toolStepTokens, stepOverheadSec, stepsSec, final, finalSec, in.TokS, src, in.RateSamples, est.MinTurnSec)
	est.Note = b.String()
	return est
}

// Names lists the remembered seats, sorted — for status output.
func (s *Store) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Seats))
	for k := range s.Seats {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
