// Package servedwindow decides what the harness believes about a seat's context window without asking
// the engine (ADR 0106). Three things can answer, and the order they are trusted in is the point:
//
//  1. what the engine last reported serving, remembered per seat in the seat-rates store (the number
//     the loop budgets against, measured by a probe or a loaded-only read);
//  2. what the serving config states (llama.cpp's -c divided by its slots), read from the file with no
//     engine call;
//  3. the hand-written agent_ctx_tokens key, which is only ever as right as its last edit.
//
// A node advertises Known's answer in /fleet/health, the run doors hand it to the loop as the window
// to fall back on when the probe cannot answer, and doctor compares it with the key. Known never
// calls the engine and never touches the network: it reads two small files, both memoised.
package servedwindow

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/seatrate"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
)

// The sources a Window names.
const (
	// SourceServed: the engine reported this window and nothing has reloaded the seat since.
	SourceServed = "served"
	// SourceServedUnconfirmed: the engine reported a window, but the seat has loaded again since (a vLLM
	// seat can size itself at load), so the report only ever lowers the answer.
	SourceServedUnconfirmed = "served-unconfirmed"
	// SourceServingConfig: the serving config's own flags state the window.
	SourceServingConfig = "serving_config"
	// SourceConfig: only the agent_ctx_tokens key is known (what the harness believed before ADR 0106).
	SourceConfig = "config"
)

// Window is what the harness believes about one seat's context window.
type Window struct {
	// Tokens is the window to advertise or budget against; 0 = not advertised (the key is unset) or
	// nothing is known.
	Tokens int
	// Source says where Tokens came from: one of the Source constants.
	Source string
	// Configured is the agent_ctx_tokens value the caller passed in (the hand-written key).
	Configured int
	// Why is a clause for an operator: what the answer rests on, or why a better source could not answer.
	Why string

	// value is the resolved window before the "an unset key is never advertised" rule is applied.
	value int
}

// ForRun is the window a run may fall back on when its probe cannot answer: the same answer as Tokens,
// except that a seat whose key is unset but whose engine or serving config is known still has a window
// to use (it is never advertised, see Known).
func (w Window) ForRun() int { return w.value }

// Facts is what the two files say about one seat, before the precedence is applied. Doctor prints it.
type Facts struct {
	// Path is the serving config read ("" = serving_config_path unset).
	Path string
	// Serving is the window the serving config states; ServingWhy is why it is 0 when it is.
	Serving    int
	ServingWhy string
	// Fingerprint is the hash of the serving-config entry that answers to the seat ("" = unreadable).
	Fingerprint string
	// Remembered is the window the engine last reported (0 = none) and RememberedAt when. RememberedOK
	// is true when that report was made against the entry as it is now; a report made against another
	// entry is not used.
	Remembered   int
	RememberedAt time.Time
	RememberedOK bool
	// Confirmed is true when no cold load of the seat ended after the report was made.
	Confirmed bool
}

// Look reads both files for seat. It never calls the engine.
func Look(cfg config.Config, seat string) Facts {
	var f Facts
	if strings.TrimSpace(seat) == "" {
		return f
	}
	f.Path = strings.TrimSpace(cfg.ServingConfigPath)
	f.Serving, f.ServingWhy, f.Fingerprint = readServing(f.Path, seat)
	root, err := gpulease.ResolveStateRoot(cfg.StateDir)
	if err != nil {
		return f
	}
	s := loadStore(seatrate.Path(root)).Get(seat)
	if s.CtxWindow <= 0 {
		return f
	}
	f.Remembered = s.CtxWindow
	f.RememberedAt = time.UnixMilli(s.CtxAtMs)
	f.RememberedOK = s.CtxKey == f.Fingerprint
	f.Confirmed = true
	for _, end := range s.ColdLoadEnds {
		if !end.IsZero() && end.After(f.RememberedAt) {
			f.Confirmed = false
			break
		}
	}
	return f
}

// Known resolves the window of seat from what the harness can read without loading anything.
// configured is the agent_ctx_tokens key. Precedence, with R the remembered engine report (valid only
// against the serving-config entry it was read under), Y the serving config's statement and C the key:
//
//   - R valid and no cold load of the seat since: R, "served";
//   - R valid but the seat has loaded since: the smaller of R and the first known of Y and C,
//     "served-unconfirmed" (an unconfirmed report only lowers);
//   - else Y when known: Y, "serving_config";
//   - else C, "config" (what the harness believed before, byte for byte).
//
// There is no clock-based expiry. A key of 0 stays "not advertised": Tokens is 0 whatever R and Y say,
// and ForRun still carries them for a run.
func Known(cfg config.Config, seat string, configured int) Window {
	w := Window{Source: SourceConfig, Configured: configured, value: configured}
	f := Look(cfg, seat)
	switch {
	case f.RememberedOK && f.Confirmed:
		w.Source, w.value = SourceServed, f.Remembered
		w.Why = fmt.Sprintf("the engine reported %d at %s", f.Remembered, f.RememberedAt.Format(time.RFC3339))
	case f.RememberedOK:
		v := f.Remembered
		bound := f.Serving // the first known of the serving config's statement and the key
		if bound <= 0 {
			bound = configured
		}
		if bound > 0 && bound < v {
			v = bound
		}
		w.Source, w.value = SourceServedUnconfirmed, v
		w.Why = fmt.Sprintf("the engine reported %d at %s, and the seat has loaded since", f.Remembered, f.RememberedAt.Format(time.RFC3339))
	case f.Serving > 0:
		w.Source, w.value = SourceServingConfig, f.Serving
		w.Why = fmt.Sprintf("the serving config at %s states it", f.Path)
	default:
		w.Why = f.ServingWhy
	}
	w.Tokens = w.value
	if configured <= 0 {
		w.Tokens = 0
	}
	return w
}

// Remember records that the engine serving seat reported a window of n tokens, against the serving-config
// entry as it is now. It is best effort: an unwritable store, an unresolvable state root or a nameless
// seat is logged at most and never fails the caller. Call it from the place that read the window.
func Remember(cfg config.Config, seat string, n int) {
	if n <= 0 || strings.TrimSpace(seat) == "" {
		return
	}
	root, err := gpulease.ResolveStateRoot(cfg.StateDir)
	if err != nil {
		return
	}
	if f := Look(cfg, seat); f.Remembered == n && f.RememberedOK && f.Confirmed {
		return // already what the store says, and nothing has loaded since: no write
	}
	_, _, key := readServing(strings.TrimSpace(cfg.ServingConfigPath), seat)
	now := time.Now()
	if uerr := seatrate.Update(seatrate.Path(root), func(s *seatrate.Store) {
		s.ObserveWindow(seat, n, key, now)
	}); uerr != nil {
		log.Printf("served window: seat-rates store not updated: %v", uerr)
	}
}

// servingRead is one memoised reading of a serving config for one seat.
type servingRead struct {
	size   int64
	mtime  time.Time
	n      int
	why    string
	finger string
}

var (
	memoMu sync.Mutex
	memo   = map[string]servingRead{}
)

// memoQuiet is how long a file must have been unchanged before its (mtime, size) is trusted as its
// identity: a rewrite inside the same timestamp tick would otherwise be served stale.
const memoQuiet = 2 * time.Second

// readServing reads the window the serving config states for seat and the entry's fingerprint, keyed on
// the file's mtime and size like servingcfg's reader so a health cadence costs a stat.
func readServing(path, seat string) (n int, why, fingerprint string) {
	if path == "" {
		return 0, "serving_config_path is unset", ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Sprintf("serving_config_path %s cannot be read: %v", path, err), ""
	}
	k := path + "\x00" + seat
	memoMu.Lock()
	c, hit := memo[k]
	memoMu.Unlock()
	if hit && c.size == fi.Size() && c.mtime.Equal(fi.ModTime()) {
		return c.n, c.why, c.finger
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Sprintf("serving_config_path %s cannot be read: %v", path, err), ""
	}
	r := servingRead{size: fi.Size(), mtime: fi.ModTime()}
	r.n, r.why = servingtmpl.SeatCtx(string(text), seat)
	r.finger = servingtmpl.SeatEntryFingerprint(string(text), seat)
	if time.Since(fi.ModTime()) > memoQuiet {
		memoMu.Lock()
		if len(memo) > 64 {
			memo = map[string]servingRead{}
		}
		memo[k] = r
		memoMu.Unlock()
	}
	return r.n, r.why, r.finger
}

// storeRead is one memoised reading of the seat-rates store.
type storeRead struct {
	size  int64
	mtime time.Time
	store *seatrate.Store
}

var (
	storeMu   sync.Mutex
	storeMemo = map[string]storeRead{}
)

// loadStore reads the seat-rates store, keyed on the file's mtime and size so a health cadence (and every
// layer seat of a composite box) costs a stat. The store is replaced by a rename, so a write always
// changes at least the mtime; a file touched inside memoQuiet is read again rather than trusted.
func loadStore(path string) *seatrate.Store {
	fi, err := os.Stat(path)
	if err != nil {
		s, _ := seatrate.Load(path)
		return s
	}
	storeMu.Lock()
	c, hit := storeMemo[path]
	storeMu.Unlock()
	if hit && c.size == fi.Size() && c.mtime.Equal(fi.ModTime()) {
		return c.store
	}
	s, _ := seatrate.Load(path)
	if time.Since(fi.ModTime()) > memoQuiet {
		storeMu.Lock()
		if len(storeMemo) > 8 {
			storeMemo = map[string]storeRead{}
		}
		storeMemo[path] = storeRead{size: fi.Size(), mtime: fi.ModTime(), store: s}
		storeMu.Unlock()
	}
	return s
}
