package mediaremote

// The scanner and the tickets it runs (P0 plan S2; ADR 0083). See ticket.go for what a ticket is.
//
// ONE SCANNER PER PROCESS, every scanEvery +/- scanSpread, started when the process has its first ticket and gone when
// it has none. Each scan visits the process's own waiting tickets, OLDEST FIRST, and decides; it never runs a job. A job
// runs in a goroutine of its own, one per ticket in flight, so a local run that waits ninety seconds in the lease line
// does not stall the scan of any other ticket or the touch that keeps each ticket's place alive (the place is held 30 s
// past its last touch, and the scan touches it every ~10 s).
//
// The decision for a waiting ticket:
//
//  1. Keep its place in the local line (core.PlaceKeeper): a touch, or a fresh place with the ticket's original arrival
//     time when the old one lapsed.
//  2. LOCAL FIRST. Ask the pipeline whether the ticket, resuming its place, would be served now (core.LaneProber; the
//     question a ticket asks is "would anyone who arrived after me go first", see pipeline.MediaLaneFree). If so, and no
//     older ticket took the lane in this scan, start the local run with the place as its waiter_token: the final local
//     run spends the place through the path every resumed call takes.
//  3. Else the fleet, with S1's own filter and ranking (candidatesFor): the nodes whose recipe matches, that hold no
//     lease, that are not being left alone for a bounce, minus the ones a ticket of THIS MACHINE has a job on (the claims)
//     and minus the ones an OLDER waiting ticket of this machine matches (first in line). The first node left is
//     claimed under the directory lock and the job is sent: LATE BINDING, since nothing is sent anywhere before a lane
//     is free, so no job sits parked on a node that frees later than another.
//
// A scan writes the ticket's own file (beat, matches, the answer) and touches its place. It opens no PAIR card, writes
// no ledger row and registers no lease waiter: that is what made the first design of this loop pollute the PAIR
// workload list (register C01), and TestScanNeverWritesCardsOrRows holds it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// honourOrder is the rule that an older waiting ticket of the machine is first in line for the nodes it matches. A var only
// so a test can prove the claim alone keeps two processes off one node; production never changes it.
var honourOrder = true

// stepBound bounds one ticket's decision inside a scan (its probes), so one wedged read cannot hold up the others.
const stepBound = 30 * time.Second

// localRowKey is the memo key of this machine's own lane in the row memory.
const localRowKey = "this machine"

// placeRetry is how long a ticket that could not get a place in the local line waits before it asks again (a host that
// keeps none will not start to, but a card table that would not read at creation will). Chosen.
const placeRetry = time.Minute

// rowMemo remembers since when a node has been in a state, for the cluster row's `since`.
type rowMemo struct {
	state string
	at    time.Time
}

// liveTicket is a ticket this process owns: the persisted record and the body the file does not keep.
type liveTicket struct {
	q  *queueRT
	st *store

	mu sync.Mutex
	t  ticketFile

	cfg        config.Config
	remotes    []string
	rosterFile string
	runner     Runner
	req        core.Request // params cloned, the seed fixed
	pl         planned
	local      localID

	ctx    context.Context
	cancel context.CancelFunc

	// notify tells a human at a CLI about a change of standing, once per line (say).
	notify  func(string)
	saidKey string

	running   bool // a goroutine is carrying the ticket (a local run, a send, a poll)
	collected bool
	canceled  bool

	done chan struct{}
	res  core.Result

	scanned     chan struct{}
	scannedOnce sync.Once

	rowSince map[string]rowMemo

	// placeTried is when the ticket last asked the box for a place in its line.
	placeTried time.Time

	// first is the lane reading the call made when it became a ticket, reused by the first scan if it is still fresh:
	// the card table is one nvidia-smi read, and the same question twice in a second is not a second question.
	first   *core.LaneVerdict
	firstAt time.Time
}

// verdictFresh is how long the reading a call made at creation may stand in for the first scan's. Chosen.
const verdictFresh = 2 * time.Second

// queueRT is one process's share of the queue.
type queueRT struct {
	owner ownerID

	mu       sync.Mutex
	live     map[string]*liveTicket
	scanning bool
	stopped  bool

	wg      sync.WaitGroup
	kick    chan struct{}
	rootCtx context.Context
	stopAll context.CancelFunc
}

func newQueueRT(o ownerID) *queueRT {
	ctx, cancel := context.WithCancel(context.Background())
	return &queueRT{owner: o, live: map[string]*liveTicket{}, kick: make(chan struct{}, 1), rootCtx: ctx, stopAll: cancel}
}

// defaultQ is the process's queue.
var defaultQ = newQueueRT(selfOwner())

// stop ends the scanner and every goroutine of the queue and waits for them (a test's cleanup; a process just exits).
func (q *queueRT) stop() {
	q.mu.Lock()
	q.stopped = true
	q.mu.Unlock()
	q.stopAll()
	q.wg.Wait()
}

// scannerAuto: tests that drive the scan by hand turn the scanner off.
var scannerAuto = true

// register makes lt visible to the scanner and wakes it.
func (q *queueRT) register(lt *liveTicket) {
	q.mu.Lock()
	q.live[lt.t.ID] = lt
	start := scannerAuto && !q.scanning && !q.stopped
	if start {
		q.scanning = true
		q.wg.Add(1)
	}
	q.mu.Unlock()
	if start {
		go q.scanLoop()
	}
	q.nudge()
}

func (q *queueRT) unregister(lt *liveTicket) {
	q.mu.Lock()
	delete(q.live, lt.t.ID)
	q.mu.Unlock()
}

func (q *queueRT) nudge() {
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

// snapshot returns the live tickets, oldest first.
func (q *queueRT) snapshot() []*liveTicket {
	q.mu.Lock()
	out := make([]*liveTicket, 0, len(q.live))
	for _, lt := range q.live {
		out = append(out, lt)
	}
	q.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].created(), out[j].created()
		if a != b {
			return a < b
		}
		return out[i].t.ID < out[j].t.ID
	})
	return out
}

func (lt *liveTicket) created() int64 {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return lt.t.Created
}

func spread(d time.Duration) time.Duration {
	f := 1 + scanSpread*(2*rand.Float64()-1)
	return time.Duration(float64(d) * f)
}

func (q *queueRT) scanLoop() {
	defer q.wg.Done()
	for {
		q.cycle(q.rootCtx)
		q.mu.Lock()
		if q.stopped || q.rootCtx.Err() != nil || len(q.live) == 0 {
			q.scanning = false
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()
		timer := time.NewTimer(spread(scanEvery))
		select {
		case <-q.rootCtx.Done():
			timer.Stop()
		case <-q.kick:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// cycleState is what the tickets of one scan share.
type cycleState struct {
	localTaken bool // an older ticket took this machine's lane in this scan
}

// cycle is one scan: sweep what the dead left, then decide for each waiting ticket of this process, oldest first.
func (q *queueRT) cycle(ctx context.Context) {
	tickets := q.snapshot()
	swept := map[string]bool{}
	cs := &cycleState{}
	for _, lt := range tickets {
		if ctx.Err() != nil {
			return
		}
		if !swept[lt.st.dir] {
			swept[lt.st.dir] = true
			for _, tok := range lt.st.sweep(lt.st.list()) {
				core.DropPlace(lt.runner, tok)
			}
		}
		lt.step(ctx, cs)
	}
}

// ---- the decision for one ticket ---------------------------------------------------------------------------------

// liveCfg is the config the ticket decides with: the call's own, with delegate_remotes re-read from the config file when
// that file changed (an MCP session never reloads its config, so a node added to the file would otherwise never be seen
// until the session restarted). Nothing else in cfg moves.
func (lt *liveTicket) liveCfg() config.Config {
	cfg := lt.cfg
	if remotes, ok := rosterFromFile(lt.rosterFile); ok {
		cfg.DelegateRemotes = remotes
	}
	return cfg
}

// probeReq is the request the lane is asked about and the local run is given: the ticket's own, carrying its place.
func (lt *liveTicket) probeReq() core.Request {
	req := lt.req
	params := make(map[string]any, len(req.Params)+1)
	for k, v := range req.Params {
		params[k] = v
	}
	delete(params, "waiter_token")
	lt.mu.Lock()
	if lt.t.Place != "" {
		params["waiter_token"] = lt.t.Place
	}
	lt.mu.Unlock()
	req.Params = params
	return req
}

// persistLocked writes lt.t. The caller holds lt.mu.
func (lt *liveTicket) persistLocked() {
	if lt.canceled {
		return
	}
	if err := lt.st.write(lt.t); err != nil {
		logQueue("ticket %s: its file could not be written: %v", lt.t.ID, err)
	}
}

// step decides for one ticket in this scan.
func (lt *liveTicket) step(ctx context.Context, cs *cycleState) {
	lt.mu.Lock()
	state, running := lt.t.State, lt.running
	if terminalState(state) || lt.canceled {
		lt.mu.Unlock()
		return
	}
	lt.t.Beat = nowMs()
	if running || state != stWaiting {
		lt.persistLocked() // the beat: an owner that is silent for ownerStale is dead
		lt.mu.Unlock()
		lt.markScanned()
		return
	}
	if age := time.Since(time.UnixMilli(lt.t.Created)); age > ticketMaxWait {
		lt.mu.Unlock()
		lt.finish(deferRes(core.DeferClassBudget, fmt.Sprintf("ticket %s waited %s without a lane anywhere and ended; send the request again", lt.t.ID, ticketMaxWait.Round(time.Minute)), core.ErrClassGPUQueued), false)
		return
	}
	lt.mu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, stepBound)
	defer cancel()

	lt.keepPlace(sctx)

	// 2. local first
	preq := lt.probeReq()
	var (
		verdict core.LaneVerdict
		probed  = true
	)
	lt.mu.Lock()
	reuse := lt.first != nil && time.Since(lt.firstAt) < verdictFresh
	if reuse {
		verdict = *lt.first
	}
	lt.first = nil
	lt.mu.Unlock()
	switch {
	case reuse:
	case cs.localTaken:
		// An older ticket of this scan has the lane; this one cannot take it now, and its answer keeps the last reading.
		verdict, probed = core.LaneVerdict{Free: false, Why: "an older ticket of this machine is being started on the lane"}, false
	default:
		verdict = core.ProbeLane(sctx, lt.runner, preq)
	}
	if verdict.Free && !cs.localTaken {
		cs.localTaken = true
		lt.startLocal()
		return
	}

	// 3. the fleet
	cfg := lt.liveCfg()
	bases, berr := candidateBases(cfg, lt.remotes)
	var (
		cands []candidate
		rows  []rowAt
	)
	if berr == nil {
		cands, rows = candidatesFor(sctx, cfg, bases, lt.local, needOf(lt.req.Params))
	}
	all := lt.st.list()
	claims := lt.st.claims()
	lt.mu.Lock()
	selfID, selfCreated := lt.t.ID, lt.t.Created
	lt.mu.Unlock()

	// The tickets of this machine that are older than this one and still waiting (their owners alive): each is first in
	// line for the nodes it matches, and one that has not been scanned yet is first in line for all of them.
	var older []ticketFile
	for _, t := range all {
		if t.ID == selfID || t.State != stWaiting || t.dead() {
			continue
		}
		if t.Created < selfCreated || (t.Created == selfCreated && t.ID < selfID) {
			older = append(older, t)
		}
	}
	firstIn := func(base string) (ticketFile, bool) {
		for _, t := range older {
			if !honourOrder {
				break
			}
			if !t.Scanned {
				return t, true
			}
			for _, m := range t.Matches {
				if m == normBase(base) {
					return t, true
				}
			}
		}
		return ticketFile{}, false
	}

	var (
		extra   []rowAt
		chosen  *candidate
		matched []string
	)
	for _, r := range rows {
		if r.matched {
			matched = append(matched, normBase(r.base))
		}
	}
	for i := range cands {
		c := cands[i]
		matched = append(matched, normBase(c.base))
		if h, held := claims[normBase(c.base)]; held && h.Ticket != selfID {
			extra = append(extra, rowAt{idx: c.idx, row: ClusterRow{Node: c.shown, State: "in-use",
				Why: fmt.Sprintf("a job of this machine is running there (ticket %s, since %s)", h.Ticket, time.UnixMilli(h.At).UTC().Format(time.RFC3339))}})
			continue
		}
		if t, first := firstIn(c.base); first {
			extra = append(extra, rowAt{idx: c.idx, row: ClusterRow{Node: c.shown, State: "free",
				Why: fmt.Sprintf("idle, but ticket %s is older and first in line for it", t.ID)}})
			continue
		}
		if chosen == nil {
			chosen = &cands[i]
		}
	}
	sort.Strings(matched)

	local := localLane(verdict)
	if probed && !verdict.Free && verdict.Why != "" {
		lt.mu.Lock()
		lt.t.LaneWhy = verdict.Why
		lt.mu.Unlock()
	}
	lt.record(rows, extra, matched, local, all, berr)
	if chosen != nil {
		lt.startRemote(sctx, *chosen)
	}
}

// markScanned says the first scan of the ticket is over (a call waits for it before it answers).
func (lt *liveTicket) markScanned() { lt.scannedOnce.Do(func() { close(lt.scanned) }) }

// keepPlace touches the ticket's place in the local line, and leaves a fresh one with the ticket's own arrival time when
// the old one is gone (a lapsed grace is only a touch away, but a place that was spent or pruned must be left again).
func (lt *liveTicket) keepPlace(ctx context.Context) {
	lt.mu.Lock()
	place, created := lt.t.Place, lt.t.Created
	lt.mu.Unlock()
	if place != "" && core.KeepPlace(lt.runner, place) {
		return
	}
	if place == "" && lt.noPlaceNote() != "" && time.Since(lt.placeTried) < placeRetry {
		return // this box keeps none; asking every scan would only repeat the answer
	}
	req := lt.req
	pl, err := core.LeavePlace(ctx, lt.runner, req, time.UnixMilli(created))
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.placeTried = time.Now()
	if err != nil {
		lt.t.Place = ""
		if lt.t.PlaceNote == "" {
			lt.t.PlaceNote = placeNote(err)
		}
		return
	}
	lt.t.Place, lt.t.PlaceNote = pl.Token, ""
}

func (lt *liveTicket) noPlaceNote() string {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return lt.t.PlaceNote
}

func placeNote(err error) string {
	s := err.Error()
	return strings.TrimPrefix(s, core.ErrNoMediaPlace.Error()+" ")
}

// localLane turns the pipeline's reading into the answer's word about this machine's lane.
func localLane(v core.LaneVerdict) *LocalLane {
	l := &LocalLane{State: "free", Why: v.Why, Ahead: v.Ahead}
	if v.Free {
		return l
	}
	l.State = "busy"
	if v.HostRAM != "" && len(v.Holders) == 0 && v.Ahead == 0 {
		l.State = "ram"
	}
	if len(v.Holders) > 0 {
		h := v.Holders[0]
		l.Holder = &RowHolder{Epoch: h.Epoch, Class: h.Class, Reason: h.Reason, RemainingSec: h.RemainingSec}
	}
	return l
}

// record merges what the scan learned into the ticket and saves it: the rows in the roster's order with their `since`,
// the nodes that match, the local lane, and the place in line.
func (lt *liveTicket) record(rows, extra []rowAt, matched []string, local *LocalLane, all []ticketFile, berr error) {
	lt.recordLocked(rows, extra, matched, local, all, berr)
	lt.say(standing(lt.snapshot()))
}

func (lt *liveTicket) recordLocked(rows, extra []rowAt, matched []string, local *LocalLane, all []ticketFile, berr error) {
	merged := append(append([]rowAt(nil), rows...), extra...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].idx < merged[j].idx })
	now := time.Now()
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if lt.rowSince == nil {
		lt.rowSince = map[string]rowMemo{}
	}
	cluster := make([]ClusterRow, 0, len(merged))
	seen := map[string]bool{}
	for _, r := range merged {
		row := r.row
		memo, ok := lt.rowSince[row.Node]
		if !ok || memo.state != row.State {
			memo = rowMemo{state: row.State, at: now}
			lt.rowSince[row.Node] = memo
		}
		seen[row.Node] = true
		row.Since = memo.at.UTC().Format(time.RFC3339)
		cluster = append(cluster, row)
	}
	seen[localRowKey] = true
	for node := range lt.rowSince { // a node that left the roster stops being remembered
		if !seen[node] {
			delete(lt.rowSince, node)
		}
	}
	if berr != nil {
		cluster = nil // no roster (or none that may be used by this call): the answer has no node rows, and says why below
		local.Why = strings.TrimSpace(local.Why + "; the fleet: " + berr.Error())
	}
	pos := 1
	for _, t := range all {
		if t.ID != lt.t.ID && (t.State == stWaiting || t.State == stLocal) && !t.dead() &&
			(t.Created < lt.t.Created || (t.Created == lt.t.Created && t.ID < lt.t.ID)) {
			pos++
		}
	}
	lt.t.Scanned, lt.t.Matches, lt.t.Cluster, lt.t.Position = true, matched, cluster, pos
	if local != nil {
		local.Since = rowSinceOf(lt.rowSince, localRowKey, local.State, now)
		lt.t.Local = local
	}
	lt.persistLocked()
	lt.markScanned()
}

func rowSinceOf(m map[string]rowMemo, node, state string, now time.Time) string {
	memo, ok := m[node]
	if !ok || memo.state != state {
		memo = rowMemo{state: state, at: now}
		m[node] = memo
	}
	return memo.at.UTC().Format(time.RFC3339)
}

// ---- running a ticket ----------------------------------------------------------------------------------------------

// startLocal runs the ticket on this machine's lane in a goroutine of its own.
func (lt *liveTicket) startLocal() {
	lt.mu.Lock()
	if lt.running || lt.t.State != stWaiting || lt.canceled {
		lt.mu.Unlock()
		return
	}
	lt.running = true
	lt.t.State = stLocal
	lt.t.Local = &LocalLane{State: "free", Why: "starting on this machine's lane"}
	lt.persistLocked()
	lt.mu.Unlock()
	lt.markScanned()
	lt.say(standing(lt.snapshot()))
	req := lt.probeReq()
	lt.q.wg.Add(1)
	go func() {
		defer lt.q.wg.Done()
		res := lt.runner.Run(lt.ctx, req)
		if lt.ctx.Err() != nil && !res.OK {
			lt.finish(res, true)
			return
		}
		if !res.OK && res.Deferred && core.CardHeld(res.Meta.ErrClass) {
			// The lane was taken between the question and the run, and nothing ran: the ticket goes back to waiting
			// with its place (a lost race only cost the run's own wait).
			lt.mu.Lock()
			lt.running = false
			lt.t.State = stWaiting
			lt.persistLocked()
			lt.mu.Unlock()
			lt.q.nudge()
			return
		}
		lt.finish(res, false)
	}()
}

// ticketHandle is the attribution handle of a ticket's remote attempt: the plain handle (the one ledger row and the one
// PAIR card of the call, opened at the node's acceptance) plus the record of where the job is.
type ticketHandle struct {
	core.RemoteAttribution
	lt *liveTicket
}

// Dispatched is called once the node has ACCEPTED the job: the ticket is placed, and the job id is on file for adoption.
func (h *ticketHandle) Dispatched(base, nodeID, jobID string) {
	h.RemoteAttribution.Dispatched(base, nodeID, jobID)
	h.lt.mu.Lock()
	h.lt.t.State, h.lt.t.Job = stPlaced, jobID
	h.lt.persistLocked()
	h.lt.mu.Unlock()
	h.lt.say(standing(h.lt.snapshot()))
}

// say tells the human waiting at a CLI (if there is one) where the ticket stands, once per change of standing (the key).
func (lt *liveTicket) say(key, line string) {
	lt.mu.Lock()
	fn, same := lt.notify, key == lt.saidKey
	if key != "" {
		lt.saidKey = key
	}
	lt.mu.Unlock()
	if fn != nil && line != "" && !same {
		fn(line)
	}
}

// Fetching is called when the node says the job is done and its outputs start to come back.
func (h *ticketHandle) Fetching() {
	h.lt.mu.Lock()
	if h.lt.t.State == stPlaced {
		h.lt.t.State = stFetching
		h.lt.persistLocked()
	}
	h.lt.mu.Unlock()
}

// startRemote claims the node and sends the job in a goroutine of its own.
func (lt *liveTicket) startRemote(ctx context.Context, c candidate) {
	lt.mu.Lock()
	if lt.running || lt.t.State != stWaiting || lt.canceled {
		lt.mu.Unlock()
		return
	}
	lt.running = true // one dispatch of a ticket at a time, from here until the job ends or the claim is refused
	cp := lt.t
	lt.mu.Unlock()
	// The claim waits for the directory lock, which a crashed holder can keep for a while: not with the ticket locked.
	ok, _, err := lt.st.claimNode(&cp, c.base, c.node, stPlacing)
	if err != nil || !ok {
		if err != nil {
			logQueue("ticket %s: node %s could not be claimed: %v", cp.ID, c.shown, err)
		}
		lt.mu.Lock()
		lt.running = false
		lt.mu.Unlock()
		return
	}
	lt.mu.Lock()
	lt.t.State, lt.t.Base, lt.t.Node = cp.State, cp.Base, cp.Node
	placement := "remote: this machine's image lane is not free (" + lt.t.LaneWhy + ")"
	lt.mu.Unlock()
	lt.q.wg.Add(1)
	go func() {
		defer lt.q.wg.Done()
		start := time.Now()
		actx, cancel := context.WithTimeout(lt.ctx, Budgets[lt.pl.fleetTask])
		defer cancel()
		h := core.BeginRemote(lt.runner, lt.req, RouteAuto)
		defer core.CloseOnPanic(h)
		a := attempt(actx, lt.cfg, lt.req, lt.pl, c, lt.local, start, &ticketHandle{RemoteAttribution: h, lt: lt})
		switch a.kind {
		case attemptDone:
			a.res.Meta.Placement = placement + "; ran on " + c.node
			h.Finish(a.res)
			lt.finish(a.res, false)
		case attemptFailed:
			res := placementDefer(a.err, placement)
			h.Finish(res)
			lt.finish(res, false)
		default: // the node passed the job back or was not reached: nothing ran there
			h.Discard("the ticket goes on to the next lane that frees")
			lt.mu.Lock()
			lt.running = false
			lt.t.State, lt.t.Base, lt.t.Node, lt.t.Job = stWaiting, "", "", ""
			lt.persistLocked()
			lt.mu.Unlock()
			lt.st.releaseNode(c.base, lt.t.ID)
			lt.rememberAttempt(c, a.row)
			lt.q.nudge()
		}
	}()
}

// rememberAttempt keeps what a refused or bounced attempt said about a node until the node is read again.
func (lt *liveTicket) rememberAttempt(c candidate, row ClusterRow) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	for i := range lt.t.Cluster {
		if lt.t.Cluster[i].Node == c.shown {
			lt.t.Cluster[i] = row
			lt.persistLocked()
			return
		}
	}
}

// finish ends the ticket with its result: the file keeps the result for a caller that collects it later, the node and the
// local place are let go, and every call waiting for the ticket is woken. canceled removes the ticket.
func (lt *liveTicket) finish(res core.Result, canceled bool) {
	lt.mu.Lock()
	if terminalState(lt.t.State) {
		lt.mu.Unlock()
		return
	}
	base, id, place := lt.t.Base, lt.t.ID, lt.t.Place
	lt.running = false
	lt.res = res
	if canceled {
		lt.canceled = true
		lt.st.remove(id)
	} else {
		if res.OK {
			lt.t.State = stDone
		} else {
			lt.t.State = stFailed
		}
		if b, err := json.Marshal(res); err == nil {
			lt.t.Result = b
		}
		lt.persistLocked()
	}
	lt.mu.Unlock()
	if base != "" {
		lt.st.releaseNode(base, id)
	}
	if place != "" {
		core.DropPlace(lt.runner, place)
	}
	lt.q.unregister(lt)
	lt.markScanned()
	close(lt.done)
}

// cancelUndispatched ends a ticket that has sent nothing anywhere (the CLI's Ctrl-C): waiting, or running locally in this
// process (the render dies with the process that was asked to stop). A job already on a node cannot be recalled (ADR 0064),
// so the ticket stays on file for the next identical call to adopt. It reports whether the ticket was ended.
func (lt *liveTicket) cancelUndispatched(why string) bool {
	lt.mu.Lock()
	state := lt.t.State
	lt.mu.Unlock()
	switch state {
	case stWaiting:
		lt.finish(deferRes(core.DeferClassCapacity, why, core.ErrClassGPUQueued), true)
		return true
	case stLocal:
		lt.cancel()
		return true
	}
	return false
}

func deferRes(class, reason, errClass string) core.Result {
	res := core.Deferf(reason, "", core.Meta{ErrClass: errClass})
	res.DeferClass = class
	return res
}

func logQueue(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "media queue: "+format+"\n", args...)
}

// ---- the roster file ----------------------------------------------------------------------------------------------

// rosterCache remembers the roster read from each config file by its mtime and size.
var rosterCache = struct {
	mu sync.Mutex
	m  map[string]rosterEntry
}{m: map[string]rosterEntry{}}

type rosterEntry struct {
	mtime   time.Time
	size    int64
	remotes []string
}

// rosterFromFile returns the delegate_remotes of the config file at path, re-read only when the file's mtime or size
// changed. ok is false when there is no path or the file cannot be read as JSON: the caller then keeps the roster it has
// (a half-written file must not empty a running session's fleet).
func rosterFromFile(path string) ([]string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, false
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return nil, false
	}
	rosterCache.mu.Lock()
	defer rosterCache.mu.Unlock()
	if e, ok := rosterCache.m[path]; ok && e.mtime.Equal(fi.ModTime()) && e.size == fi.Size() {
		return e.remotes, true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}) // a UTF-8 byte-order mark
	var doc struct {
		Remotes []string `json:"delegate_remotes"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil, false
	}
	rosterCache.m[path] = rosterEntry{mtime: fi.ModTime(), size: fi.Size(), remotes: doc.Remotes}
	return doc.Remotes, true
}
