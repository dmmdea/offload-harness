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
	"log"
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
	// generated at least this many tokens. The wall of a call includes its
	// prefill, so a short answer under-reads the decode rate: at 128 the
	// first live probe (2026-09-10 21:01, a ~200-token answer over a 43 KB
	// prefill on the 27B) recorded 11.5 tok/s against a 30 tok/s seat, which
	// would have tripled the next estimate. At 1,024 tokens (≈ 34 s of decode
	// on that seat against ≈ 8 s of prefill) the under-read is bounded near
	// 20 %; the ledger-01 finals (9–19k tokens) read the seat exactly.
	minSampleTokens = 1024
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
	toolStepTokens  = 128
	stepOverheadSec = 6
)

// Seat is one seat's remembered numbers.
type Seat struct {
	// TokS is the effective decode rate (completion tokens per second of call
	// wall over completions of ≥ minSampleTokens), an EMA over runs.
	TokS float64 `json:"tok_s"`
	// PrefillTokS is the seat's measured PREFILL rate (prompt tokens per
	// second, an EMA over runs), observed from the loop's prefill accounting
	// (0.131.0, liveness walls). It sizes the stall allowance while the seat
	// is prefilling: a 214k-token prompt at 2,000 tok/s is 160 s of
	// legitimate silence. 0 = never measured (the policy assumes its own slow-seat rate, 100 tok/s).
	PrefillTokS float64 `json:"prefill_tok_s,omitempty"`
	// PrefillRates is the last prefillWindow SOLO prefill rates (tok/s), oldest first, and
	// PrefillSlowTokS the slowest of them (register F10). The EMA above is the average, and an
	// average is the wrong number for a STALL bound: a seat that heat-soaked for one run is
	// still averaged with the cool ones, so an allowance sized from the EMA is the cool
	// seat's and files a stall on a seat that is working. Like cold_load_sec, which is the
	// slowest of its window for the same reason, the bound must hold the slow case.
	// 0 / empty = no solo sample since the window existed (StallPrefillTokS falls back to the EMA).
	PrefillRates    []float64 `json:"prefill_rates,omitempty"`
	PrefillSlowTokS float64   `json:"prefill_slow_tok_s,omitempty"`
	// ColdLoadSec is the slowest of the last coldLoadWindow observed loads.
	ColdLoadSec float64   `json:"cold_load_sec,omitempty"`
	ColdLoads   []float64 `json:"cold_loads,omitempty"`
	// ColdLoadEnds is when each entry of ColdLoads ENDED, aligned with it (ADR 0066):
	// a load ends at one instant for everyone waiting on it, so an observation that
	// ends within coldLoadMergeWindow of ANY entry is that load seen by another run,
	// not another load. A zero end is unknown (an entry written without one) and
	// never merges.
	ColdLoadEnds []time.Time `json:"cold_load_ends,omitempty"`
	Samples      int         `json:"samples"`
	Updated      time.Time   `json:"updated"`
}

// prefillWindow is how many of the most recent solo prefill rates a seat keeps for its stall
// bound (chosen, not measured): one run folds in up to two samples (its aggregate and its
// largest first-delta sample), so five cover the last two or three runs, enough for a heat
// soak that lasts a few runs to be held and short enough that a seat that cooled down gets its
// tight allowance back within an hour of ordinary traffic.
const prefillWindow = 5

// StallPrefillTokS is the prefill rate the seat's STALL bound is sized from: the slowest of its
// recent solo samples, so the bound holds a seat that runs slower than its average (heat soak, a
// throttled card), else the EMA of a store written before the window existed. 0 = never measured.
func (s Seat) StallPrefillTokS() float64 {
	if s.PrefillSlowTokS > 0 {
		return s.PrefillSlowTokS
	}
	return s.PrefillTokS
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
		return s, fmt.Errorf("seatrate: %s is not readable (%w); starting empty", path, jerr)
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
// minPrefillSampleTokens / minPrefillSampleMS reject prefill samples too
// small to measure: a cached-prefix hit prefills a few hundred tokens in
// tens of milliseconds and says nothing about the seat's rate.
const (
	minPrefillSampleTokens = 1000
	minPrefillSampleMS     = 200
)

// ObservePrefill folds one run's prefill measurement (prompt tokens and the
// milliseconds the engine reported for them) into the seat's PrefillTokS, for a
// run that had the seat to itself: ObservePrefillLoad with load 1. Returns false
// when the sample is too small to count.
func (s *Store) ObservePrefill(seat string, promptTokens int64, promptMS float64, now time.Time) bool {
	return s.ObservePrefillLoad(seat, promptTokens, promptMS, 1, now)
}

// ObservePrefillLoad is ObservePrefill for a run that saw the seat shared by
// `load` requests (its own included; 1 = known solo, 0 = never observed). A
// sample timed while requests shared the seat is what a shared seat gives ONE
// request, never the seat's own rate — folded in, it lowers PrefillTokS, inflates
// every later stall allowance and ceiling, and swings the published rate with
// whatever traffic the run happened to meet (one fleet seat's prefill_tok_s:
// 255-1321 tok/s in one day, 2026-09-29). Only a KNOWN solo run's sample moves
// the rate (ADR 0066, register C-66); a shared seat's, and one nobody looked at,
// return false and change nothing.
func (s *Store) ObservePrefillLoad(seat string, promptTokens int64, promptMS float64, load int, now time.Time) bool {
	if load != 1 {
		return false
	}
	if s == nil || strings.TrimSpace(seat) == "" || promptTokens < minPrefillSampleTokens || promptMS < minPrefillSampleMS {
		return false
	}
	if s.Seats == nil {
		s.Seats = map[string]Seat{}
	}
	cur := s.Seats[seat]
	rate := float64(promptTokens) / (promptMS / 1000)
	if cur.PrefillTokS <= 0 {
		cur.PrefillTokS = rate
	} else {
		cur.PrefillTokS = emaWeight*rate + (1-emaWeight)*cur.PrefillTokS
	}
	// The stall bound's window (register F10): the slowest of the last prefillWindow solo
	// samples, kept beside the average.
	rates := append(append([]float64(nil), cur.PrefillRates...), math.Round(rate*10)/10)
	if len(rates) > prefillWindow {
		rates = rates[len(rates)-prefillWindow:]
	}
	cur.PrefillRates, cur.PrefillSlowTokS = rates, slowestRate(rates)
	cur.Updated = now
	s.Seats[seat] = cur
	return true
}

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
		loads, ends := coldLoadEntries(cur)
		loads = append(loads, math.Round(coldLoadSec*10)/10)
		ends = append(ends, time.Time{}) // this caller cannot say when the load ended
		if len(loads) > coldLoadWindow {
			loads, ends = loads[len(loads)-coldLoadWindow:], ends[len(ends)-coldLoadWindow:]
		}
		cur.ColdLoads, cur.ColdLoadEnds, cur.ColdLoadSec = loads, ends, slowest(loads)
	}
	cur.Updated = now
	s.Seats[seat] = cur
	return true
}

// coldLoadMergeWindow: two cold-load observations of one seat that END within
// this window are ONE load seen by two runs. A load ends at a single instant for
// every run waiting on it, and a seat cannot unload and load again in ten
// seconds (a load is minutes), so the window cannot merge two real loads.
const coldLoadMergeWindow = 10 * time.Second

// coldLoadEntries returns copies of a seat's cold-load window with its end times
// aligned: a window whose ends are missing or misaligned (written before the ends
// were kept, or by a caller that could not say) has none known.
func coldLoadEntries(cur Seat) ([]float64, []time.Time) {
	loads := append([]float64(nil), cur.ColdLoads...)
	ends := make([]time.Time, len(loads))
	if len(cur.ColdLoadEnds) == len(loads) {
		copy(ends, cur.ColdLoadEnds)
	}
	return loads, ends
}

// slowestRate is the lowest rate of the window (0 for none).
func slowestRate(rates []float64) float64 {
	m := 0.0
	for _, r := range rates {
		if r > 0 && (m == 0 || r < m) {
			m = r
		}
	}
	return m
}

// slowest is the longest load of the window (0 for none).
func slowest(loads []float64) float64 {
	m := 0.0
	for _, c := range loads {
		if c > m {
			m = c
		}
	}
	return m
}

// ObserveColdLoad folds one observed seat load into the seat's cold-load window:
// coldLoadSec is the wall the observer waited, endedAt when the seat became
// ready for it (register C-66).
//
// Several runs on one box wait on the same load — a fan-out lands its subtasks
// on a cold seat together, and every one of them measures it — and the store used
// to append each observation: the window of five filled with copies of one load
// (seat-rates.json, 2026-09-29: [12.4, 234.6, 234.6, 203.5, 203.5]) and a run
// that joined the load near its end recorded the few seconds it saw as if that
// were the load (12.4 against a real 178-271 s). An observation that ends within
// coldLoadMergeWindow of ANY entry of the window is that load: it keeps the
// LONGEST measurement — the run that began waiting first, closest to the load's
// real start — instead of adding an entry. It merges against every entry, not
// just the newest: runs report when THEY end, not when the load did, so a run that
// waited on an older load and finishes minutes later must find its own load.
//
// A load not in the window is a new entry, placed in end order so a late report
// keeps the window in the order the loads happened; the window forgets its
// OLDEST load, and an observation older than everything in a full window belongs
// to a load that already left it and is dropped, never allowed to push a newer one
// out. An observation with no end time cannot merge and is appended as the newest.
// Returns whether anything changed.
func (s *Store) ObserveColdLoad(seat string, coldLoadSec float64, endedAt time.Time) bool {
	if s == nil || strings.TrimSpace(seat) == "" || coldLoadSec <= 0 {
		return false
	}
	if s.Seats == nil {
		s.Seats = map[string]Seat{}
	}
	cur := s.Seats[seat]
	sec := math.Round(coldLoadSec*10) / 10
	loads, ends := coldLoadEntries(cur)

	// The entry this observation is another view of: the nearest end inside the window.
	at := -1
	if !endedAt.IsZero() {
		var nearest time.Duration
		for i, e := range ends {
			gap := endedAt.Sub(e) // an unknown end (zero) is a gap of centuries: it merges with nothing
			if gap < 0 {
				gap = -gap
			}
			if gap <= coldLoadMergeWindow && (at < 0 || gap < nearest) {
				at, nearest = i, gap
			}
		}
	}
	if at >= 0 {
		if sec <= loads[at] {
			return false // the same load, and an earlier observer already measured more of it
		}
		loads[at] = sec
		if endedAt.After(ends[at]) {
			ends[at] = endedAt
		}
	} else {
		pos := len(loads) // no end time: the newest
		if !endedAt.IsZero() {
			pos = 0
			for pos < len(ends) && !ends[pos].After(endedAt) {
				pos++ // entries of unknown end sort first, as the oldest
			}
		}
		if len(loads) >= coldLoadWindow && pos == 0 {
			return false // a load that already left the window
		}
		loads = append(loads[:pos], append([]float64{sec}, loads[pos:]...)...)
		ends = append(ends[:pos], append([]time.Time{endedAt}, ends[pos:]...)...)
		if len(loads) > coldLoadWindow {
			loads, ends = loads[len(loads)-coldLoadWindow:], ends[len(ends)-coldLoadWindow:]
		}
	}
	cur.ColdLoads, cur.ColdLoadEnds, cur.ColdLoadSec = loads, ends, slowest(loads)
	if endedAt.After(cur.Updated) {
		cur.Updated = endedAt
	}
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
	tmp := fmt.Sprintf("%s.%d.%d.tmp", s.path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := renameOntoStore(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// renameStore is os.Rename. It is a variable only so a test can inject the denial Windows gives a
// rename over a file another process has open.
var renameStore = os.Rename

// renamePoll is the pause between attempts to rename the saved store onto its path. It is shorter than
// lockPoll because the window being waited out is a reader's open handle, which lasts microseconds.
const renamePoll = 5 * time.Millisecond

// renameOntoStore renames the temp file onto the store, asking again for up to lockWait when the rename
// is refused for a reason that clears on its own.
//
// WHY: the store is read WITHOUT the lock (Load, from the delegate, the fleet node and the pipeline, so
// that a reader never waits on a writer). On Windows os.Open grants FILE_SHARE_READ|FILE_SHARE_WRITE and
// never FILE_SHARE_DELETE, and a rename over a file has to delete the old directory entry, so a rename
// that lands while any reader has the store open fails with "Access is denied" although nothing is
// wrong: the reader lets go microseconds later. Update used to hand that error to its caller, which logs
// "seat-rates store not updated" and drops the run's sample (D-120's symptom, on the rename instead of
// the lock create). The wait stays inside Update's lock, so no other writer can slip in between.
// Anything that is not a denial, or a denial that outlasts lockWait, is returned as it is.
func renameOntoStore(tmp, path string) error {
	deadline := time.Now().Add(lockWait)
	for {
		err := renameStore(tmp, path)
		if err == nil || !transientRename(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(renamePoll)
	}
}

// transientRename reports whether a refused rename may be granted by asking again. A permission denial
// is the portable answer (Windows reports ERROR_ACCESS_DENIED as one); the Windows file in this package
// adds the sharing and lock violations, which Go does not map to a permission error.
func transientRename(err error) bool {
	return errors.Is(err, os.ErrPermission) || platformTransientRename(err)
}

const (
	// lockWait bounds how long Update waits for another process's lock; a
	// writer that cannot get in within it skips THIS observation (logged by
	// the caller) rather than blocking a run — the next run records again.
	lockWait = 2 * time.Second
	lockPoll = 20 * time.Millisecond
	// lockStale: a lock file older than this belongs to a process that died
	// between create and remove; it is taken over.
	lockStale = 30 * time.Second
)

// ErrLocked is returned by Update when it could not take the store's lock within
// lockWait: another writer held it for the whole window, or creating the lock file
// was refused for the whole window (on Windows a denial can be a delete still
// pending, so it is waited on like a held lock; a directory this process cannot
// write ends the same way). The second case wraps the refusal beside ErrLocked, so
// errors.Is(err, os.ErrPermission) tells the two apart. In both the store is
// untouched and the caller skips that one observation. A failure after the lock was
// taken (the save) is never ErrLocked.
var ErrLocked = errors.New("seatrate: store locked by another writer")

// openLock creates the lock file exclusively. It is a variable only so a test can inject the
// access-denied answer Windows gives while a deleted lock is still pending removal.
var openLock = func(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
}

// removeLock removes the lock file, both a stale one and the writer's own at the end of Update. A
// variable for the same reason: a test cannot make the removal fail the way a held handle does on
// Windows without it.
var removeLock = os.Remove

// releaseLock removes the lock file Update took. A removal that fails (on Windows a scanner or indexer
// holding the file open without delete sharing) leaves the lock behind until it is old enough to be taken
// over (lockStale), and every writer meanwhile waits lockWait and skips its sample. That is not Update's
// result: the save it ran under the lock may well have landed. So it is said here, once, naming the lock
// and the cause, instead of leaving the skipped samples with no explanation. A lock that is already gone
// is not a failure.
func releaseLock(lock string) {
	if err := removeLock(lock); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("seatrate: the lock %s could not be removed (%v): writers skip their samples until it is %s old", lock, err, lockStale)
	}
}

// Update applies fn to the store at path under an exclusive lock — load, fn,
// save as ONE step. tmp+rename alone protects a reader from a half-written
// file but not a writer from another writer: two processes (an MCP server
// and the CLI share one state root; two delegated subtasks land on one box)
// that both load, observe and save around the same moment each start from
// the same snapshot and the last save silently drops the other's sample
// (reviewer finding, 2026-09-10). The lock is an O_EXCL file beside the
// store, the same shape the GPU lease uses for the same reason.
func Update(path string, fn func(*Store)) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("seatrate: store has no path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lock := path + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		f, err := openLock(lock)
		if err == nil {
			f.Close()
			break
		}
		// Contention on Windows does not always look like "already exists": removing a
		// file whose handle is still open only marks it delete-PENDING, and an exclusive
		// create issued in that window fails with ACCESS_DENIED. That is another writer
		// finishing, so it waits like a held lock (the same rule gpulease's own claim and
		// epoch lock follow). A directory this process cannot write is refused with the
		// same denial every time and ends at the deadline below.
		if !errors.Is(err, os.ErrExist) && !errors.Is(err, os.ErrPermission) {
			return err
		}
		if st, serr := os.Stat(lock); serr == nil && time.Since(st.ModTime()) > lockStale {
			// A dead writer's lock; the next attempt takes it. Only a removal that worked
			// (or found the file already gone) retries at once: a stale lock that cannot be
			// removed waits out the deadline like any held lock instead of spinning.
			if rerr := removeLock(lock); rerr == nil || errors.Is(rerr, os.ErrNotExist) {
				continue
			}
		}
		if time.Now().After(deadline) {
			if errors.Is(err, os.ErrExist) {
				return ErrLocked
			}
			// Both causes stay in the chain: the caller skips the sample as for a held lock
			// (errors.Is ErrLocked), and an unwritable directory is still named.
			return fmt.Errorf("%w: the lock file could not be created within %s: %w", ErrLocked, lockWait, err)
		}
		time.Sleep(lockPoll)
	}
	defer releaseLock(lock)
	s, lerr := Load(path)
	if lerr != nil {
		// A corrupt store is replaced by what this run knows — the error is
		// returned beside the save so the caller can log the replacement.
		fn(s)
		if serr := s.Save(); serr != nil {
			return serr
		}
		return lerr
	}
	fn(s)
	return s.Save()
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
	// ThinkingOn: the seat thinks on EVERY step (agent_thinking / contract
	// thinking "on"). The estimate charges one think block like auto and says
	// it is a FLOOR: how much each tool step thinks is not measured yet.
	ThinkingOn bool
	TimeoutSec int
	// RepackBudget (register D-46 follow-up, 0.117.2) is the completion budget
	// of the structured re-pack the node runs when the contract carries an
	// output_schema and the final answer came back as prose: one more
	// completion of up to the final budget on the same seat. 0 = no schema, no
	// term. The term is an UPPER bound — a seat that answers in the object
	// shape skips the re-pack (directStructured) and pays nothing.
	RepackBudget int
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
	// RepackSec is the re-pack term inside TotalSec and MinTurnSec (0 when the
	// contract carries no output_schema).
	RepackSec int
	// OtherSec is everything inside TotalSec that is NOT the final answer and
	// its re-pack: the cold load, the one think block and the tool steps. It is
	// published separately because the final-budget fit (FitFinalBudget,
	// register D-95) has to subtract exactly those terms from the wall before
	// converting what is left into tokens — deriving it downstream would be a
	// second copy of this arithmetic, free to drift from this one.
	OtherSec int
	// TokS is the rate the estimate was computed at (0 when there was none),
	// carried so a caller that sizes anything else from the same seat reads the
	// number this note printed rather than re-resolving it.
	TokS float64
}

// FinalBudgetFor is the final answer's completion budget for a tool-step
// budget: 4× the step, capped at FinalBudgetCap, never below the step. ONE
// rule, read by the agent loop (agent.FinalBudgetFor), the fleet node's health
// advertisement and the delegator's retry floor, so the three never drift.
func FinalBudgetFor(stepBudget int) int {
	if stepBudget <= 0 {
		stepBudget = 1024
	}
	b := stepBudget * 4
	if b > FinalBudgetCap {
		b = FinalBudgetCap
	}
	if b < stepBudget {
		b = stepBudget
	}
	return b
}

// FinalBudgetCap bounds the final answer's completion budget: the visible
// answer to a 40 KB / seven-array extraction is a few thousand tokens, and a
// runaway non-thinking seat must still stop.
const FinalBudgetCap = 8192

// MinTurnFor is the least wall a retry on a seat is worth starting with: a
// cold load plus one final turn at the seat's rate, plus the re-pack term when
// the contract carries an output_schema (register D-46). 0 when the rate is
// unknown — the caller then falls back to whatever floor it has.
func MinTurnFor(coldLoadSec float64, finalBudget, repackBudget int, tokS float64) int {
	if tokS <= 0 {
		return 0
	}
	if finalBudget <= 0 {
		finalBudget = 1024
	}
	return int(math.Ceil(coldLoadSec + float64(finalBudget+repackBudget)/tokS))
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
	if in.ThinkingAuto || in.ThinkingOn {
		thinkSec = float64(stepBudget) / in.TokS
	}
	toolSteps := steps - 1
	stepsSec := float64(toolSteps) * (float64(toolStepTokens)/in.TokS + stepOverheadSec)
	var repackSec float64
	if in.RepackBudget > 0 {
		repackSec = float64(in.RepackBudget) / in.TokS
	}
	total := cold + thinkSec + stepsSec + finalSec + repackSec
	est := Estimate{
		TotalSec:   int(math.Ceil(total)),
		MinTurnSec: int(math.Ceil(cold + finalSec + repackSec)),
		RepackSec:  int(math.Ceil(repackSec)),
		OtherSec:   int(math.Ceil(cold + thinkSec + stepsSec)),
		TokS:       in.TokS,
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
	if in.ThinkingAuto || in.ThinkingOn {
		fmt.Fprintf(&b, " + one think block %d tok (%.0f s)", stepBudget, thinkSec)
	}
	fmt.Fprintf(&b, " + %d tool steps × (%d tok + %d s prefill) (%.0f s) + final %d tok (%.0f s)",
		toolSteps, toolStepTokens, stepOverheadSec, stepsSec, final, finalSec)
	if in.RepackBudget > 0 {
		fmt.Fprintf(&b, " + re-pack ≤ %d tok (%.0f s; output_schema set — skipped when the seat answers in the object shape)", in.RepackBudget, repackSec)
	}
	fmt.Fprintf(&b, " at %.1f tok/s (%s, %d samples); min_turn %d s", in.TokS, src, in.RateSamples, est.MinTurnSec)
	if in.ThinkingOn {
		b.WriteString(" — thinking on: every tool step may think as well, so this is a floor")
	}
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
