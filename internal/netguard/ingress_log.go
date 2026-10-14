package netguard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// offenderLogConfig is what an offenderLog runs on. IngressGuard fills it from the package
// constants; a test fills it with small numbers and its own clock.
type offenderLogConfig struct {
	key          string                           // the config key a summary names
	logf         func(format string, args ...any) // nil discards everything
	now          func() time.Time                 // the clock
	maxOffenders int                              // distinct offenders logged one by one per window
	maxPerPeer   int                              // of those, at most this many from one peer
	window       time.Duration                    // how long a logged offender stays deduped
	summaryEvery time.Duration                    // at most one summary line per this
}

// offenderLog is the warn (and enforce) log of the ingress guard: it writes each distinct
// offender once per window and counts the rest.
//
// It exists so the operator can run warn until the log shows no legitimate client tripping the
// rule, then promote to enforce. So it must not go blind, and it must not grow without bound:
//
//   - the dedupe set is emptied when its window ends, so a legitimate client that trips the rule
//     after a flood is logged in the next window, and an offender still tripping it is written
//     once per window (a persistent violator is a line every few minutes, not a line per request);
//   - one peer gets at most maxPerPeer of the maxOffenders slots in a window, so a page that
//     varies the Host on every request cannot use up the slots the rest of the tailnet logs from;
//   - what does not fit is counted, and a summary line (at most one per summaryEvery) says how
//     many, quoting the latest. A count left over when a burst ends is written by a timer when
//     the interval ends, not held until another violation happens to arrive;
//   - the dedupe key is a hash of the whole offending value, so its size is fixed and two
//     different long values are two offenders.
type offenderLog struct {
	cfg offenderLogConfig

	mu          sync.Mutex
	seen        map[string]struct{}
	perPeer     map[string]int
	windowStart time.Time
	overflow    int
	lastLine    string
	lastSummary time.Time
	timer       *time.Timer // armed while a counted overflow waits for its summary
}

func newOffenderLog(cfg offenderLogConfig) *offenderLog {
	return &offenderLog{cfg: cfg, seen: map[string]struct{}{}, perPeer: map[string]int{}}
}

// once logs line unless this kind, peer and offender was already logged in this window.
func (l *offenderLog) once(kind, peer, offender, line string) {
	if l.cfg.logf == nil {
		return
	}
	sum := sha256.Sum256([]byte(offender))
	key := kind + "|" + peer + "|" + hex.EncodeToString(sum[:16])
	line = clipTo(line, maxLoggedLine)
	now := l.cfg.now()

	l.mu.Lock()
	l.rotateLocked(now)
	if _, dup := l.seen[key]; dup {
		l.mu.Unlock()
		return
	}
	if len(l.seen) < l.cfg.maxOffenders && l.perPeer[peer] < l.cfg.maxPerPeer {
		l.seen[key] = struct{}{}
		l.perPeer[peer]++
		l.mu.Unlock()
		l.cfg.logf("%s", line)
		return
	}
	l.overflow++
	l.lastLine = line
	summary := l.summaryLocked(now)
	if summary == "" && l.timer == nil {
		l.timer = time.AfterFunc(l.cfg.summaryEvery-now.Sub(l.lastSummary), l.flush)
	}
	l.mu.Unlock()
	if summary != "" {
		l.cfg.logf("%s", summary)
	}
}

// rotateLocked empties the dedupe set and the per-peer counts when the window has ended.
func (l *offenderLog) rotateLocked(now time.Time) {
	if l.windowStart.IsZero() {
		l.windowStart = now
		return
	}
	if l.cfg.window > 0 && now.Sub(l.windowStart) >= l.cfg.window {
		l.seen = map[string]struct{}{}
		l.perPeer = map[string]int{}
		l.windowStart = now
	}
}

// summaryLocked returns the summary line when a count is waiting and the interval since the
// last one has passed, and resets the count; "" otherwise.
func (l *offenderLog) summaryLocked(now time.Time) string {
	if l.overflow == 0 || now.Sub(l.lastSummary) < l.cfg.summaryEvery {
		return ""
	}
	s := fmt.Sprintf("ingress guard (%s): %d violation(s) beyond the log budget (%d offenders, %d from one peer, per %s) were not logged one by one since the last summary; the latest: %s",
		l.cfg.key, l.overflow, l.cfg.maxOffenders, l.cfg.maxPerPeer, l.cfg.window, l.lastLine)
	l.overflow, l.lastSummary = 0, now
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	return s
}

// flush is the timer's: it writes the count a burst left behind once the interval ends.
func (l *offenderLog) flush() {
	now := l.cfg.now()
	l.mu.Lock()
	l.timer = nil
	summary := l.summaryLocked(now)
	if summary == "" && l.overflow > 0 {
		// This clock says the interval is not over yet (a test clock does not follow real
		// time): wait out the rest of it rather than leave the count unwritten.
		l.timer = time.AfterFunc(l.cfg.summaryEvery-now.Sub(l.lastSummary), l.flush)
	}
	l.mu.Unlock()
	if summary != "" {
		l.cfg.logf("%s", summary)
	}
}
