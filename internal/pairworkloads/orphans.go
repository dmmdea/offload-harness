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
// RELAYED MARKERS (relay.go). A card another box's process opened through this box's relay has a
// marker with pid 0 and Remote set, named `0-<job>.remote`, not `.json`: a harness built before the
// relay sweeps this directory too and reads a pid <= 0 marker as orphaned (see remoteSuffix).
//
// ITS OWN POST. A terminal frame is parked as a pending marker BEFORE it is posted (track), so the
// verdict outlives a producer killed mid-post. A sweeper that reads that marker while the post is in
// flight cannot tell it from a failed post and sent the same frame a second time. The sweeper that does
// is the producer's own: every emitter sweeps once, on its first Emit, from a goroutine, and a short
// call (a deferral, a refused dispatch) is opened and closed before that goroutine has read the
// register whenever the scheduler runs it late (on one processor every time: a duplicate terminal
// frame in three tests of three packages on a 4-vCPU CI runner, 2026-10-10). The emitter therefore
// remembers the markers it has a post in flight for (Emitter.posting) and its sweeps leave those alone;
// a post that fails parks the frame again and clears the mark, so the next sweep sends it as before.
// The mark is per emitter: a sweeper of another process, or fleet-serve's periodic one (an emitter of
// its own), that reads the marker in those few milliseconds still resends an identical terminal frame,
// which PAIR merges as an equal-rank no-op.
//
// WHOSE MARKER. A marker records the ingress URL its card was posted to, and a
// sweep closes only the markers of ITS OWN ingress (a marker with no endpoint
// predates the field and counts as DefaultEndpoint). Closing is posting to the
// sweeper's endpoint and deleting on success, so a sweeper that closed another
// ingress's marker would close nothing real and destroy the only record of the
// card: a test's httptest emitter did exactly that to live markers (2026-10-02,
// a 31.9 h ghost card). A foreign marker is left untouched: not locked, not
// posted, not even age-dropped.
//
// REJECTED IS NOT DOWN. PAIR answering a close with HTTP 400, 413 or 422 (a frame
// it will never accept; 401/403/404/405 describe the route, not the frame, and
// stay retryable) is different from PAIR being unreachable (transport error or
// 5xx): the first drops that one marker and the pass goes on to the next, the
// second releases the claim and skips the rest of that endpoint's markers for the pass, since
// they would fail the same way (the other endpoints' markers go on). Treating both as "down" let one rejected marker starve
// every marker behind it until the 48 h give-up.
//
// SAFE BY CONSTRUCTION. Every register error is swallowed: a marker that cannot
// be written only means a card that cannot be closed after a crash — what
// happened before this register existed — and never a failed or slowed job. A
// disabled emitter writes and sweeps nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
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
	// remoteSuffix ends the file name of a RELAYED card's marker (pid 0, producer on another box).
	// It is deliberately NOT ".json": a harness built before the relay sweeps this same directory
	// (a long-lived MCP server or CLI the upgrade did not restart), reads every ".json" marker with
	// pid <= 0 as orphaned and posts "failed" for a job that is still running, and it ignores the
	// unknown "remote" field. Those binaries skip every file that is not ".json", so the suffix
	// keeps a relayed marker out of their reach while this build's sweep reads it.
	remoteSuffix = ".remote"
)

// openMarker is one register file.
type openMarker struct {
	PID       int   `json:"pid"`
	ProcStart int64 `json:"proc_start,omitempty"`
	WrittenMs int64 `json:"written_ms"`
	// Pending marks a terminal frame its producer could not deliver: the
	// sweep sends Info as it is, whatever the producer's liveness.
	Pending bool `json:"pending_terminal,omitempty"`
	// Endpoint is the ingress URL the card was posted to (the writing
	// emitter's own). A sweep closes only the markers of its own endpoint: a
	// marker without one predates this field and belongs to DefaultEndpoint.
	Endpoint string `json:"endpoint,omitempty"`
	// Remote marks a card whose producer is ANOTHER box's process: a frame a card relay delivered
	// to this member (relay.go). PID is 0 and ProcStart 0 on it, because a pid of this box says
	// nothing about that producer; it closes by its terminal relayed frame or by RelayOpenMaxAge.
	Remote bool `json:"remote,omitempty"`
	// Relay is set on a marker the RELAYING box wrote: the node hint the member needs to place the
	// card, kept so a sweep can rebuild the relay body (Endpoint is then the relay's route URL).
	Relay *relayMeta                 `json:"relay,omitempty"`
	Info  map[string]json.RawMessage `json:"workload_info"`
}

// markerEndpoint is the ingress a marker's card lives on: the endpoint it
// recorded, or DefaultEndpoint for a legacy marker that recorded none.
func markerEndpoint(m openMarker) string {
	if strings.TrimSpace(m.Endpoint) == "" {
		return DefaultEndpoint
	}
	return m.Endpoint
}

// sameEndpoint reports whether two ingress URLs name the same ingress: scheme,
// host (case-insensitive) and port (a scheme's default port is the same as none)
// and path. An unparseable URL is compared as its trimmed text.
func sameEndpoint(a, b string) bool {
	return endpointKey(a) == endpointKey(b)
}

func endpointKey(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	host := strings.ToLower(u.Hostname())
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host + u.EscapedPath()
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
	return markerStem(pid, jobID) + ".json"
}

// remoteMarkerName is the file name of a relayed card's marker: the pid-0 name with remoteSuffix
// in place of ".json" (see remoteSuffix: an older harness on this box must not read it).
func remoteMarkerName(jobID string) string {
	return markerStem(0, jobID) + remoteSuffix
}

func markerStem(pid int, jobID string) string {
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
	return fmt.Sprintf("%d-%s", pid, b)
}

// track updates the register for a frame about to be posted: an in-flight
// frame writes (or rewrites) the job's marker; a terminal frame forgets it and
// returns its path, which the caller removes once the post was attempted
// (untrack). Best-effort throughout.
func (e *Emitter) track(ev Event, pl sendPlan) (removeAfterPost string) {
	if ev.JobID == "" {
		return ""
	}
	if isTerminal(ev.State) {
		e.openMu.Lock()
		p := e.open[ev.JobID]
		delete(e.open, ev.JobID)
		e.openMu.Unlock()
		if p != "" && pl.info != nil {
			// The verdict is on disk before it is posted. The marker on disk is still the in-flight one
			// until untrack settles it after the post, and a process killed in between (an MCP client
			// that kills its door right after the reply) would leave the sweep to close the card
			// "harness process exited before the job finished" over a job that finished, whatever its
			// outcome. As a pending marker the sweep sends the verdict itself, which is also what it
			// does for a post that fails. This emitter's own sweeps skip the marker until the post
			// settles (markPosting runs BEFORE the marker is written, so a sweep that can read it finds
			// the mark); a sweeper of another process that reads it in that window resends an identical
			// terminal frame, which PAIR merges as an equal-rank no-op.
			e.markPosting(p)
			e.parkTerminal(p, pl)
		}
		if p == "" && pl.remote {
			// A relayed card's marker is found by name, not by this process's memory: the member may
			// have restarted since the in-flight frame while the producer, on another box, did not.
			if dir := e.registerDir(); dir != "" {
				p = filepath.Join(dir, remoteMarkerName(ev.JobID))
			}
		}
		if p == "" && pl.relay != nil {
			// A terminal frame through a relay with no marker of its own (its in-flight frames never
			// went out: the relay was down or just demoted, or another process sent them) still gets
			// one if the post fails, so the sweep retries the frame instead of losing it: the card
			// then appears, closed, once the relay answers. A delivered frame removes it again.
			if dir := e.registerDir(); dir != "" {
				pid, _ := e.selfIdentity()
				p = filepath.Join(dir, markerName(pid, ev.JobID))
			}
		}
		return p
	}
	dir := e.registerDir()
	if dir == "" || pl.info == nil {
		return ""
	}
	pid, start := e.selfIdentity()
	if pl.remote {
		pid, start = 0, 0
	}
	body, err := json.Marshal(openMarker{PID: pid, ProcStart: start, WrittenMs: e.now().UnixMilli(), Endpoint: pl.url, Remote: pl.remote, Relay: pl.relay, Info: pl.info})
	if err != nil {
		return ""
	}
	path := filepath.Join(dir, markerName(pid, ev.JobID))
	if pl.remote {
		path = filepath.Join(dir, remoteMarkerName(ev.JobID))
	}
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
//
// A frame PAIR REJECTED (HTTP 400, 413 or 422) is the exception: no resend can change that
// answer, so a pending marker would only be retried and refused until the give-up
// and would end every sweep pass in the meantime. The marker is dropped, with a
// log line.
func (e *Emitter) untrack(path string, pl sendPlan, postErr error) {
	terminal := pl.info
	if path == "" {
		return
	}
	defer e.unmarkPosting(path) // after the marker is removed or parked again, never before
	if postErr == nil || terminal == nil {
		removeRetrying(path)
		return
	}
	var rej *rejectedError
	if errors.As(postErr, &rej) {
		log.Printf("pairworkloads: PAIR rejected the terminal frame of card %s (HTTP %d); dropped its marker", infoJobID(terminal, path), rej.status)
		removeRetrying(path)
		return
	}
	if !e.parkTerminal(path, pl) {
		removeRetrying(path)
	}
}

// parkTerminal rewrites the marker at path as a PENDING terminal marker: the frame in pl, which the
// sweep sends as it is, whatever the liveness of its producer. false = it could not be written (the
// marker on disk, if any, is then untouched).
func (e *Emitter) parkTerminal(path string, pl sendPlan) bool {
	pid, start := e.selfIdentity()
	if pl.remote {
		pid, start = 0, 0
	}
	body, err := json.Marshal(openMarker{PID: pid, ProcStart: start, WrittenMs: e.now().UnixMilli(), Pending: true, Endpoint: pl.url, Remote: pl.remote, Relay: pl.relay, Info: pl.info})
	return err == nil && writeAtomic(filepath.Dir(path), path, body)
}

// markPosting records that this emitter is about to park, and then post, the terminal frame of the
// marker at path; unmarkPosting clears it when the post has settled; isPosting is what its sweeps ask.
// Keyed by file name (the register is one directory per emitter) and counted, so two frames that name
// one marker never clear each other's mark early.
func (e *Emitter) markPosting(path string) {
	e.openMu.Lock()
	defer e.openMu.Unlock()
	if e.posting == nil {
		e.posting = map[string]int{}
	}
	e.posting[filepath.Base(path)]++
}

func (e *Emitter) unmarkPosting(path string) {
	e.openMu.Lock()
	defer e.openMu.Unlock()
	name := filepath.Base(path)
	if n := e.posting[name]; n > 1 {
		e.posting[name] = n - 1
	} else {
		delete(e.posting, name)
	}
}

func (e *Emitter) isPosting(name string) bool {
	e.openMu.Lock()
	defer e.openMu.Unlock()
	return e.posting[name] > 0
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
	if m.Remote {
		// The producer is another box's process: no pid of this box can say it is gone, so a
		// relayed card closes by its terminal frame or by the age cap, and only by those.
		return now.Sub(time.UnixMilli(m.WrittenMs)) > RelayOpenMaxAge
	}
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
// deletes the marker. It returns the number of frames delivered. It leaves
// every marker of another endpoint alone. A marker whose closing frame cannot
// be built, and a post PAIR REJECTED (HTTP 400, 413 or 422), drop that marker
// and go on; any other failed post releases the claim and skips every later
// marker of the SAME endpoint for the rest of the pass (that ingress or relay
// is down; the next sweep retries), while the markers of the other endpoints
// go on: with several relays, one dead relay must not starve the healthy ones'
// cards or the local ingress's. A disabled emitter does nothing.
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
	down := map[string]bool{} // endpoint keys that failed a post in this pass
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
		case !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, remoteSuffix):
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
		if !e.ownsEndpoint(markerEndpoint(m)) {
			continue // another ingress's card: not ours to post, lock, delete or age-drop
		}
		if m.Pending && e.isPosting(name) {
			// This emitter parked that verdict and its own post is still in flight: sending it again
			// would double the card's close. A post that fails parks the frame again (untrack) and
			// clears the mark, so the next sweep sends it.
			continue
		}
		if !m.Pending && !e.orphaned(m, now) {
			continue
		}
		epKey := endpointKey(markerEndpoint(m))
		if down[epKey] {
			continue // its ingress failed a post earlier in this pass: the next sweep retries it
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
		build := e.sweepFrameFn
		if build == nil {
			build = sweepFrame
		}
		body, err := build(m, now)
		if err != nil {
			// The closing frame cannot even be built from this marker, so no
			// PAIR, up or down, can ever accept it: the same permanent verdict
			// as a rejected post. Treating it as an unreachable PAIR would
			// release the lock and end the pass at this marker every sweep
			// until the 48 h give-up, starving every marker behind it.
			log.Printf("pairworkloads: the close of orphaned card %s cannot be built (%v); dropped its marker", infoJobID(m.Info, name), err)
			removeRetrying(path)
			removeRetrying(lock)
			continue
		}
		if err = e.postMarker(ctx, m, body); err != nil {
			var rej *rejectedError
			if errors.As(err, &rej) {
				// PAIR will never accept this frame: drop the one marker, go on.
				log.Printf("pairworkloads: PAIR rejected the close of orphaned card %s (HTTP %d); dropped its marker", infoJobID(m.Info, name), rej.status)
				removeRetrying(path)
				removeRetrying(lock)
				continue
			}
			if now.Sub(time.UnixMilli(m.WrittenMs)) > openGiveUp {
				removeRetrying(path)
				removeRetrying(lock)
				log.Printf("pairworkloads: dropped an orphaned PAIR card marker PAIR has not accepted for %s (%s): %v", openGiveUp, name, err)
				continue
			}
			removeRetrying(lock) // this endpoint is down: the marker stays for the next sweep
			down[epKey] = true
			continue
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

// rejectedError is a close PAIR answered with an HTTP 400, 413 or 422: the frame itself is
// refused, which no retry changes. It is distinct from every other post failure
// (transport error, 5xx), where PAIR may yet accept the same frame.
type rejectedError struct {
	endpoint string
	status   int
}

func (r *rejectedError) Error() string {
	return fmt.Sprintf("pairworkloads: %s answered %d", r.endpoint, r.status)
}

// rejection is whether an HTTP status is a verdict on the FRAME: 400, 413 and
// 422 say this body will never be accepted. The rest of 4xx describes the route,
// the auth, the service on the port or a moment (401, 403, 404, 405 from a PAIR
// mid-deploy or a wrong endpoint; 408, 429 "try again"), so it stays with the
// retryable failures: dropping on it would delete the only record of every open
// card, one marker after another, while the 48 h give-up bounds the loss.
func rejection(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// infoJobID names a frame's job for a log line, falling back to fallback.
func infoJobID(info map[string]json.RawMessage, fallback string) string {
	var id string
	if json.Unmarshal(info["id"], &id) != nil || id == "" {
		return fallback
	}
	return id
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
	method, info := MethodFor("failed"), orphanInfo(m.Info, now)
	if m.Pending {
		var state string
		_ = json.Unmarshal(m.Info["state"], &state)
		method, info = MethodFor(state), m.Info
	}
	if m.Relay != nil {
		// A card opened through a relay is closed through it, with the node hint the member needs.
		return relayFrameBody(method, info, *m.Relay)
	}
	return frameBody(method, info)
}

// ownsEndpoint reports whether a marker's endpoint is one this emitter closes cards on: its own
// ingress, or (relay.go) a relay member it is configured for.
func (e *Emitter) ownsEndpoint(ep string) bool {
	return sameEndpoint(ep, e.cfg.Endpoint) || e.relayOwns(ep)
}

// postMarker delivers a sweep's frame for m to the endpoint m's card lives on: the ingress, or the
// relay route its marker names.
func (e *Emitter) postMarker(ctx context.Context, m openMarker, body []byte) error {
	if m.Relay != nil {
		return e.postRelay(ctx, markerEndpoint(m), body)
	}
	return e.post(ctx, body)
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
