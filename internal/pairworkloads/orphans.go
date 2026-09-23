package pairworkloads

// The open-card register (0.133.1): a PAIR card whose producer died is closed
// by the harness, because nothing on PAIR's side can know the producer died.
//
// WHY. A card opens with an in-flight frame (queued / running) and closes with
// the terminal frame from the SAME process. When that process is killed in
// between (2026-09-22: a `local-offload delegate` CLI run killed by its parent
// after its running frame), PAIR's workload manager keeps the local-ingress
// record in its active set with no expiry and re-asserts it on every
// anti-entropy heartbeat, and PAIR's broker staleness sweep exempts records of
// its own origin — so the card read "Running" until PAIR restarted.
//
// HOW. Every in-flight frame writes one marker file under
// <state root>/pair-open/ (the machine-wide root seat-inflight and the GPU
// lease use) holding the frame's workloadInfo and the writing process's pid
// and start identity. The terminal frame removes it once delivered; a terminal
// frame that could not be delivered replaces it as a PENDING marker, which the
// next sweep resends as it is (its real verdict). A sweep — once in every
// harness process that emits, and every OrphanSweepInterval in fleet-serve —
// sends the terminal "failed" frame for each marker whose process is gone
// (dead pid, or a pid recycled by a different process), or that is older than
// OpenMaxAge whatever its pid says, then deletes it.
//
// RACING SWEEPERS. A sweeper CLAIMS a marker before it sends anything by
// creating `<marker>.lock` with O_EXCL (CREATE_NEW on Windows): of two
// sweepers that both judged a marker orphaned, exactly one create succeeds.
// The winner re-checks that the marker still exists (a sweeper that finished
// it removes the marker BEFORE its lock, so a late claimant sees it gone),
// posts, removes the marker, then the lock. One terminal frame per orphan.
//
// Not rename-to-claim: on Windows a rename is performed through a handle, and
// two sweepers that both opened the marker before either renamed it BOTH
// succeed (the second rename moves the file from the first claimant's name to
// its own) — the race test sent every card twice that way. Not delete-then-
// emit either: a lock can be UNDONE, so when PAIR is down the lock is removed,
// the marker stays, and the next sweep retries, where delete-then-emit loses
// the only record of the open card on the first failed post. A lock whose
// sweeper died is removed by a later pass, and the pass after that claims.
//
// SAFE BY CONSTRUCTION. Every register error is swallowed: a marker that cannot
// be written only means a card that cannot be closed after a crash — what
// happened before this register existed — and never a failed or slowed job. A
// disabled emitter writes and sweeps nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const (
	// openDirName is the register's directory under the machine-wide state
	// root, a sibling of seat-inflight.
	openDirName = "pair-open"
	// OpenMaxAge is the leak cap: a marker this old is closed whatever its pid
	// says. The longest harness job is a delegation's 900 s cap; a seat-watch
	// stretch closes after two idle polls. Past a day a marker is a leak.
	OpenMaxAge = 24 * time.Hour
	// openGiveUp bounds the retries of a claimed orphan whose terminal frame
	// cannot be delivered (PAIR down): past it the marker is dropped. PAIR
	// forgets its local-ingress records when it restarts, so an orphan PAIR
	// could not hear about for this long has no card left to close.
	openGiveUp = 2 * OpenMaxAge
	// openTmpMaxAge removes a half-written marker left by a crash mid-write.
	openTmpMaxAge = time.Hour
	// lockStale is the age past which a sweeper's claim is dead whatever its
	// pid says: a claim is held for one post, at most sendTimeout.
	lockStale = 5 * time.Minute
	// OrphanSweepInterval is fleet-serve's periodic sweep.
	OrphanSweepInterval = 45 * time.Second
	// OrphanError is the failure text an orphaned card is closed with.
	OrphanError = "harness process exited before the job finished"

	lockSuffix = ".lock"
	tmpInfix   = ".tmp-"
)

// openMarker is one register file.
type openMarker struct {
	PID       int   `json:"pid"`
	ProcStart int64 `json:"proc_start,omitempty"`
	WrittenMs int64 `json:"written_ms"`
	// Pending marks a terminal frame its producer could not deliver: the
	// sweep sends Info as it is, whatever the producer's liveness.
	Pending bool                       `json:"pending_terminal,omitempty"`
	Info    map[string]json.RawMessage `json:"workload_info"`
}

// registerDir resolves the register directory once; "" = no register.
func (e *Emitter) registerDir() string {
	e.openDirOnce.Do(func() {
		if d := strings.TrimSpace(e.cfg.OpenDir); d != "" {
			e.openDir = d
			return
		}
		root, err := gpulease.ResolveStateRoot(e.cfg.StateDir)
		if err != nil {
			return
		}
		e.openDir = filepath.Join(root, openDirName)
	})
	return e.openDir
}

// selfIdentity is this process's pid and start identity (0 when unreadable).
func (e *Emitter) selfIdentity() (int, int64) {
	e.selfOnce.Do(func() {
		if e.procStart != nil {
			if st, ok := e.procStart(os.Getpid()); ok {
				e.selfStart = st
			}
		}
	})
	return os.Getpid(), e.selfStart
}

func isTerminal(state string) bool { return state != "queued" && state != "running" }

// markerName is a job's marker file name: pid-scoped so two processes never
// share a file, and job-scoped so a job's queued and running frames rewrite
// one marker.
func markerName(pid int, jobID string) string {
	b := make([]byte, 0, len(jobID))
	for i := 0; i < len(jobID) && len(b) < 160; i++ {
		c := jobID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return fmt.Sprintf("%d-%s.json", pid, b)
}

// track updates the register for a frame about to be posted: an in-flight
// frame writes (or rewrites) the job's marker; a terminal frame forgets it and
// returns its path, which the caller removes once the post was attempted
// (untrack). Best-effort throughout.
func (e *Emitter) track(ev Event, info map[string]json.RawMessage) (removeAfterPost string) {
	if ev.JobID == "" {
		return ""
	}
	if isTerminal(ev.State) {
		e.openMu.Lock()
		p := e.open[ev.JobID]
		delete(e.open, ev.JobID)
		e.openMu.Unlock()
		return p
	}
	dir := e.registerDir()
	if dir == "" || info == nil {
		return ""
	}
	pid, start := e.selfIdentity()
	body, err := json.Marshal(openMarker{PID: pid, ProcStart: start, WrittenMs: e.now().UnixMilli(), Info: info})
	if err != nil {
		return ""
	}
	path := filepath.Join(dir, markerName(pid, ev.JobID))
	if writeAtomic(dir, path, body) {
		e.openMu.Lock()
		if e.open == nil {
			e.open = map[string]string{}
		}
		e.open[ev.JobID] = path
		e.openMu.Unlock()
	}
	return ""
}

// untrack settles a terminal frame's marker once its post was attempted
// ("" = the job had no marker). Delivered: the marker goes. NOT delivered (PAIR
// down or slow past sendTimeout): the marker is rewritten to hold the terminal
// frame itself, marked pending, and the next sweep — in any process, without
// waiting for this one to exit — delivers that frame with its real verdict.
// Removing it instead would drop the only record of a card PAIR still shows as
// running; leaving the in-flight marker would later close a finished job as
// "failed".
func (e *Emitter) untrack(path string, terminal map[string]json.RawMessage, postErr error) {
	if path == "" {
		return
	}
	if postErr == nil || terminal == nil {
		removeRetrying(path)
		return
	}
	pid, start := e.selfIdentity()
	body, err := json.Marshal(openMarker{PID: pid, ProcStart: start, WrittenMs: e.now().UnixMilli(), Pending: true, Info: terminal})
	if err != nil || !writeAtomic(filepath.Dir(path), path, body) {
		removeRetrying(path)
	}
}

// writeAtomic writes body to path via a temp file and a rename, so a sweeper
// never reads a half-written marker. false = nothing usable was written.
func writeAtomic(dir, path string, body []byte) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	tmp := path + tmpInfix + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	// Windows: a sweeper reading the previous version (queued, before this
	// running) holds it without delete sharing for a sub-millisecond read, and
	// the replace fails. Retry briefly; on the rare total miss the previous
	// version (same job, same card) stays.
	for i := 0; ; i++ {
		err := os.Rename(tmp, path)
		if err == nil {
			return true
		}
		if i >= 5 {
			_ = os.Remove(tmp)
			_, statErr := os.Stat(path)
			return statErr == nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// removeRetrying deletes a marker, retrying briefly: on Windows a sweeper's
// read of the file blocks the delete for its (sub-millisecond) duration, and a
// dropped removal would later close a finished card as failed.
func removeRetrying(path string) {
	for i := 0; i < 6; i++ {
		if err := os.Remove(path); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// SweepOrphansAsync runs one sweep in the background, once per emitter. Emit
// calls it, so every harness process that reports anything closes what a dead
// process left open; the process's Wait covers it.
func (e *Emitter) SweepOrphansAsync() {
	if !e.Enabled() {
		return
	}
	e.sweepOnce.Do(func() {
		e.inflight.Add(1)
		go func() {
			defer e.inflight.Done()
			e.SweepOrphans(context.Background())
		}()
	})
}

// orphaned reports whether a marker's producer is gone: its pid is dead, its
// pid now belongs to a different process, or the marker is past the leak cap.
func (e *Emitter) orphaned(m openMarker, now time.Time) bool {
	if m.PID <= 0 || now.Sub(time.UnixMilli(m.WrittenMs)) > OpenMaxAge {
		return true
	}
	if e.alive != nil && !e.alive(m.PID) {
		return true
	}
	if m.ProcStart != 0 && e.procStart != nil {
		if st, ok := e.procStart(m.PID); ok && st != m.ProcStart {
			return true // pid recycled; the producer is gone
		}
	}
	return false
}

// SweepOrphans closes every card whose producer is gone: it claims the
// marker, sends the terminal "failed" frame the producer never sent, and
// deletes the marker. It returns the number of frames delivered. A failed
// post releases the claim and ends the pass (PAIR is down; the next sweep
// retries). A disabled emitter does nothing.
func (e *Emitter) SweepOrphans(ctx context.Context) int {
	if !e.Enabled() {
		return 0
	}
	dir := e.registerDir()
	if dir == "" {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	now := e.now()
	selfPID, _ := e.selfIdentity()
	sent := 0
	for _, de := range entries {
		if ctx.Err() != nil {
			break
		}
		name := de.Name()
		path := filepath.Join(dir, name)
		switch {
		case de.IsDir():
			continue
		case strings.Contains(name, tmpInfix):
			if fi, err := de.Info(); err == nil && now.Sub(fi.ModTime()) > openTmpMaxAge {
				_ = os.Remove(path)
			}
			continue
		case strings.HasSuffix(name, lockSuffix):
			e.reapLock(path, selfPID)
			continue
		case !strings.HasSuffix(name, ".json"):
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // being written, or already closed by another sweeper
		}
		var m openMarker
		if json.Unmarshal(raw, &m) != nil || len(m.Info) == 0 {
			if fi, err := de.Info(); err == nil && now.Sub(fi.ModTime()) > OpenMaxAge {
				_ = os.Remove(path) // unreadable and old: nothing to close
			}
			continue
		}
		if !m.Pending && !e.orphaned(m, now) {
			continue
		}
		lock := path + lockSuffix
		if !claimLock(lock, selfPID) {
			continue // another sweeper holds it
		}
		if _, err := os.Stat(path); err != nil {
			// Closed by a sweeper that finished between our read and our
			// claim (it removes the marker before its lock).
			removeRetrying(lock)
			continue
		}
		body, err := sweepFrame(m, now)
		if err == nil {
			err = e.post(ctx, body)
		}
		if err != nil {
			if now.Sub(time.UnixMilli(m.WrittenMs)) > openGiveUp {
				removeRetrying(path)
				removeRetrying(lock)
				log.Printf("pairworkloads: dropped an orphaned PAIR card marker PAIR has not accepted for %s (%s): %v", openGiveUp, name, err)
				continue
			}
			removeRetrying(lock) // PAIR down: the marker stays for the next sweep
			break
		}
		removeRetrying(path) // the marker BEFORE the lock: see RACING SWEEPERS
		removeRetrying(lock)
		sent++
	}
	if sent > 0 {
		log.Printf("pairworkloads: closed %d PAIR card(s) whose harness process exited before the job finished", sent)
	}
	return sent
}

// claimLock creates lock exclusively, recording the claimant's pid.
func claimLock(lock string, pid int) bool {
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	_, _ = f.WriteString(strconv.Itoa(pid))
	_ = f.Close()
	return true
}

// reapLock removes a claim whose sweeper is gone (it died between claim and
// release) so a later pass can close the card. A claim is held for one post
// (at most sendTimeout), so a lock older than lockStale is dead whatever its
// pid says; a younger one is left while its sweeper — this process included —
// is alive.
func (e *Emitter) reapLock(lock string, selfPID int) {
	fi, err := os.Stat(lock)
	if err != nil {
		return
	}
	if e.now().Sub(fi.ModTime()) > lockStale {
		_ = os.Remove(lock)
		return
	}
	raw, err := os.ReadFile(lock)
	if err != nil {
		return
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if pid <= 0 || pid == selfPID || (e.alive != nil && e.alive(pid)) {
		return // live, or a claimant between create and write
	}
	_ = os.Remove(lock)
}

// sweepFrame is the frame a sweep sends for a marker: a pending terminal
// frame as its producer built it, or the "failed" frame of an orphan.
func sweepFrame(m openMarker, now time.Time) ([]byte, error) {
	if m.Pending {
		var state string
		_ = json.Unmarshal(m.Info["state"], &state)
		return frameBody(MethodFor(state), m.Info)
	}
	return frameBody(MethodFor("failed"), orphanInfo(m.Info, now))
}

// orphanInfo is the terminal workloadInfo for an orphaned card: the in-flight
// frame's identity and timestamps unchanged, state failed, completed now.
func orphanInfo(in map[string]json.RawMessage, now time.Time) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in)+3)
	for k, v := range in {
		out[k] = v
	}
	out["state"] = mustJSON("failed")
	out["error"] = mustJSON(CardError(OrphanError))
	out["completedAt"] = json.RawMessage(strconv.FormatInt(now.UnixMilli(), 10))
	return out
}

// OrphanSweepConfig is the emitter configuration fleet-serve's periodic sweep
// uses: the PAIR ingress, enabled when either PAIR key is on (a box that only
// serves vLLM seats still owns seat-watch cards).
func OrphanSweepConfig(cfg config.Config) Config {
	c := FromConfig(cfg)
	c.Enabled = cfg.PairWorkloadsEnabled || cfg.PairSeatActivityEnabled
	return c
}

// RunOrphanSweeper sweeps the register every interval until ctx ends
// (fleet-serve). It returns at once when the emitter is disabled.
func RunOrphanSweeper(ctx context.Context, e *Emitter, every time.Duration) {
	if !e.Enabled() {
		return
	}
	if every <= 0 {
		every = OrphanSweepInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		e.SweepOrphans(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
