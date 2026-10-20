package mediaremote

// The scanner and the tickets it runs (P0 plan S2 and S5; ADR 0083 and its Amendment 1). See ticket.go for what a ticket is.
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
//  3. Else the fleet, with S1's own filter and ranking (candidatesFor): the nodes whose recipe matches (a graph: that serve
//     the task on this release with one card and the card size asked for, graph.go), that hold no lease, that are not being
//     left alone for a bounce, minus the ones a ticket of THIS MACHINE has a job on (the claims), minus the ones that
//     refused THIS ticket at their door (a graph that names a file they do not hold: not-capable, remembered by the ticket)
//     and minus the ones an OLDER waiting ticket of this machine matches (first in line). The first node left is
//     claimed under the directory lock and the job is sent: LATE BINDING, since nothing is sent anywhere before a lane
//     is free, so no job sits parked on a node that frees later than another. A ticket with a FleetNote (a graph that reads
//     input files) skips this step altogether: it is only ever served by this machine's lane.
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
	"sync/atomic"
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
	req        core.Request // params cloned, the seed fixed (an image), the graph and manifest paths pointed at the ticket's own copies (a graph)
	pl         planned
	local      localID
	// need is what the call asks of a node beyond the recipe, read ONCE with the request (a graph's default VRAM floor reads the
	// graph, which for a ticket is its own copy, body-<id>-graph.json: a file in a directory that other processes sweep and an
	// operator may clear during the hours a ticket can wait, so the floor is not read again): needNow.
	need    need
	needSet bool

	ctx    context.Context
	cancel context.CancelFunc

	// notify tells a human at a CLI about a change of standing, once per line (say).
	notify  func(string)
	saidKey string

	running   bool // a goroutine is carrying the ticket (a local run, a send, a poll)
	collected bool
	canceled  bool
	// ended: finish has run, so the ticket's channel is closed. Checked under mu at the top of finish, whatever state the ticket
	// is in: a cancel ends a ticket without making its state terminal, and a send that was already under way must not end
	// it a second time.
	ended bool

	// lostN counts the runs on this machine's lane that the lane refused at once although it had read free, in a row, and
	// lostUntil is when the ticket asks that lane again (see startLocal).
	lostN     int
	lostUntil time.Time

	done chan struct{}
	res  core.Result

	scanned     chan struct{}
	scannedOnce sync.Once

	rowSince map[string]rowMemo

	// placeTried is when the ticket last asked the box for a place in its line.
	placeTried time.Time

	// notCapable is the nodes that refused THIS ticket at their door, by base: the identity of a graph is decided by the node
	// (it holds the files or it does not), so a refusal is a fact about the pair of this graph and that node, kept by the ticket
	// for notCapableAgain and never put on the node for anyone else (placer). In memory: the owning process asks, and the
	// cluster row it writes is what the other processes read.
	notCapable map[string]notCapableMemo

	// first is the lane reading the call made when it became a ticket, reused by the first scan if it is still fresh:
	// the card table is one nvidia-smi read, and the same question twice in a second is not a second question.
	first   *core.LaneVerdict
	firstAt time.Time
}

// verdictFresh is how long the reading a call made when it asked the lane may stand in for the first scan's: the age counts from
// the question (firstAt), not from the ticket, because the call can wait on its identity between the two. Chosen. A var so a
// test can shorten it, as firstScanBound.
var verdictFresh = 2 * time.Second

// notCapableAgain is how long a ticket leaves a node that refused it at the door (the graph names a file the node does not
// hold) before it asks that node again. Chosen, not measured: the longest pause the placer puts on any node (maxCooldown), because
// asking costs one POST of a graph (up to 900 KB) answered with no job and no lease, and an operator who places the missing file
// wants a ticket that waits for hours to find it within minutes, not to keep asking every scan.
const notCapableAgain = maxCooldown

// notCapableMemo is why a node refused a ticket and until when the ticket does not ask it again.
type notCapableMemo struct {
	why   string
	until time.Time
}

// needNow is the call's need, read when the ticket was made (or, for an adopted one, the first time it is asked).
func (lt *liveTicket) needNow() need {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if !lt.needSet {
		lt.need, lt.needSet = needOf(lt.req), true
	}
	return lt.need
}

// refusedBy says whether base refused this ticket at its door and is not asked again yet, with the sentence for the row.
func (lt *liveTicket) refusedBy(base string) (string, bool) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	m, ok := lt.notCapable[normBase(base)]
	if !ok {
		return "", false
	}
	left := m.until.Sub(placerNow())
	if left <= 0 {
		delete(lt.notCapable, normBase(base))
		return "", false
	}
	return fmt.Sprintf("%s; this ticket asks it again in %s", m.why, left.Round(time.Second)), true
}

// queueRT is one process's share of the queue.
type queueRT struct {
	owner ownerID

	mu       sync.Mutex
	live     map[string]*liveTicket
	scanning bool
	stopped  bool

	wg sync.WaitGroup
	// keepers is how many place keepers run: one per live scanner, none after it ended.
	keepers atomic.Int32
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

// register makes lt visible to the scanner and, when the scanner is already running, wakes it. A scanner that this registration
// starts takes the ticket in its first scan (lt is in q.live before the goroutine exists) and is not woken: the wake-up is a token
// that waits in the channel, so one given to a scanner that was just started outlived the scan it was for and made the scanner scan
// the same ticket again the moment the first scan ended, a second lane question within milliseconds of the first.
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
		return
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
	kctx, stopKeep := context.WithCancel(q.rootCtx)
	defer stopKeep()
	q.wg.Add(1)
	q.keepers.Add(1)
	go q.keepLoop(kctx)
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

// keepLoop touches the place of every waiting ticket on its own rhythm (scanEvery), for as long as the scanner lives. A scan
// visits its tickets one after another and each visit asks the lane one question (a bounded card-table read: laneProbeBound,
// and one retry under laneProbeRetryBound when the first ran out of time, 14 s in all), so with enough waiting tickets the turn of the last one comes later than the 30 s the local line holds a
// place for; a place touched only at its ticket's turn would lapse and be left again, and the ticket would not hold its
// card against a later caller in between. A touch is one file write, and a place that already lapsed is left again by its
// ticket's next turn (keepPlace), with the ticket's own arrival.
func (q *queueRT) keepLoop(ctx context.Context) {
	defer q.wg.Done()
	defer q.keepers.Add(-1)
	tick := time.NewTicker(scanEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, lt := range q.snapshot() {
			lt.touchPlace()
		}
	}
}

// touchPlace refreshes the place of a waiting ticket (one that is running keeps its own).
func (lt *liveTicket) touchPlace() {
	lt.mu.Lock()
	place, waiting := lt.t.Place, lt.t.State == stWaiting && !lt.running && !lt.canceled
	lt.mu.Unlock()
	if waiting && place != "" {
		core.KeepPlace(lt.runner, place)
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
	cooling, lostN, coolLeft := !reuse && time.Now().Before(lt.lostUntil), lt.lostN, time.Until(lt.lostUntil)
	lt.mu.Unlock()
	paused := false
	switch {
	case reuse:
	case cs.localTaken:
		// An older ticket of this scan has the lane; this one cannot take it now, and its answer keeps the last reading.
		verdict, probed = core.LaneVerdict{Free: false, Why: "an older ticket of this machine is being started on the lane"}, false
	case cooling:
		// The lane read free and refused this ticket's run at once (a younger place on the card that a call without a wait
		// cannot pass, or a caller that took the card between the reading and the run). Asking again now would repeat the
		// run, and each refused run writes the pipeline's own deferred row: the lane is not asked, and not run, until the
		// pause is over. The fleet is looked at meanwhile.
		verdict, paused = core.LaneVerdict{Free: false, Why: fmt.Sprintf("this machine's lane read free but refused the last %s at once", plural(lostN, "run"))}, true
	default:
		verdict = core.ProbeLane(sctx, lt.runner, preq)
		if !verdict.Free {
			lt.mu.Lock()
			lt.lostN = 0 // the lane's state really changed: the next time it reads free is a new question
			lt.mu.Unlock()
		}
		// The place says what this reading found: a call short only of host RAM does not hold its cards against a request
		// that declares none (core.PlaceMarker), and any other reading takes that back.
		lt.mu.Lock()
		place := lt.t.Place
		lt.mu.Unlock()
		core.MarkPlace(lt.runner, place, verdict.HostRAMOnly())
	}
	// A lane that reads free runs the call here and now. A lane the pipeline could not read is not a free lane (the card
	// table is slowest to read exactly when a heavy render holds the card): the fleet is looked at in this same scan, and
	// the run here is the answer only when no node can take the call.
	if verdict.Free && !verdict.Unjudged {
		cs.localTaken = true
		lt.startLocal()
		return
	}

	// 3. the fleet (a ticket with a FleetNote is never offered to a node: no roster read, no row, no match)
	cfg := lt.liveCfg()
	var (
		cands []candidate
		rows  []rowAt
		berr  error
	)
	if !lt.localOnly() {
		var bases []string
		if bases, berr = candidateBases(cfg, lt.remotes); berr == nil {
			cands, rows = candidatesFor(sctx, cfg, bases, lt.local, lt.needNow())
		}
	}
	all := lt.st.list()
	claims := lt.st.claims()
	lt.mu.Lock()
	selfID, selfCreated := lt.t.ID, lt.t.Created
	lt.mu.Unlock()

	// The tickets of this machine that are older than this one and still waiting (their owners alive): each is first in
	// line for the nodes it matches, and one that has not been scanned yet is first in line for all of them.
	older := ticketsAhead(all, selfID, selfCreated)

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
		// A node that refused THIS ticket at its door is not one this ticket matches: an older ticket that cannot use a node
		// holds it against nobody.
		if why, no := lt.refusedBy(c.base); no {
			extra = append(extra, rowAt{idx: c.idx, row: ClusterRow{Node: c.shown, State: "not-capable", Why: why}})
			continue
		}
		matched = append(matched, normBase(c.base))
		if h, held := claims[normBase(c.base)]; held && h.Ticket != selfID {
			extra = append(extra, rowAt{idx: c.idx, row: ClusterRow{Node: c.shown, State: "in-use", Why: h.words()}})
			continue
		}
		if t, first := firstInLine(older, c.base); first {
			extra = append(extra, rowAt{idx: c.idx, row: firstInLineRow(c, t)})
			continue
		}
		if chosen == nil {
			chosen = &cands[i]
		}
	}
	sort.Strings(matched)

	local := localLane(verdict)
	if paused { // the countdown is for the answer's local row, not for the placement sentence a finished result keeps
		local.Why += fmt.Sprintf("; not asked again for %s", coolLeft.Round(time.Second))
	}
	if probed && (!verdict.Free || verdict.Unjudged) && verdict.Why != "" {
		lt.mu.Lock()
		lt.t.LaneWhy = verdict.Why
		lt.mu.Unlock()
	}
	lt.record(rows, extra, matched, local, all, berr)
	if chosen != nil {
		lt.startRemote(sctx, *chosen)
		return
	}
	if verdict.Free && !cs.localTaken { // the lane could not be read and no node can take the call: it runs here
		cs.localTaken = true
		lt.startLocal()
	}
}

// ticketsAhead are the tickets of this machine that are older than the one (selfID, selfCreated) and still waiting with their
// owners alive. An image's ticket and a graph's are the same kind of ticket here, so each yields to the older tickets of either
// task by this rule.
func ticketsAhead(all []ticketFile, selfID string, selfCreated int64) []ticketFile {
	var ahead []ticketFile
	for _, t := range all {
		if t.ID == selfID || t.State != stWaiting || t.dead() {
			continue
		}
		if t.Created < selfCreated || (t.Created == selfCreated && t.ID < selfID) {
			ahead = append(ahead, t)
		}
	}
	return ahead
}

// firstInLine says whether one of the tickets ahead is first in line for base, and which: a ticket that has not been scanned yet is
// first in line for every node (it has not said which it matches), a scanned one for the nodes it matched. One rule for every
// ticket (scan), an image's and a graph's.
func firstInLine(ahead []ticketFile, base string) (ticketFile, bool) {
	for _, t := range ahead {
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

// firstInLineRow is the cluster row of an idle node that an older ticket is first in line for.
func firstInLineRow(c candidate, t ticketFile) ClusterRow {
	return ClusterRow{Node: c.shown, State: "free", Why: fmt.Sprintf("idle, but ticket %s is older and first in line for it", t.ID)}
}

func plural(n int, what string) string {
	if n == 1 {
		return what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// localOnly says the ticket is never offered to a node (FleetNote: a graph that reads input files).
func (lt *liveTicket) localOnly() bool {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return lt.t.FleetNote != ""
}

// lanePlacement is the placement sentence of a result that ran on a node, from the reading of this machine's lane that sent it
// there (was: the call was sent earlier, by a process that has since ended). The lane is named for the call: image, graph.
func lanePlacement(req core.Request, why string, was bool) string {
	lane := laneName(req)
	if rest, unread := strings.CutPrefix(why, "not judged: "); unread {
		return "remote: this machine's " + lane + " lane could not be judged (" + rest + ")"
	}
	if was {
		return "remote: this machine's " + lane + " lane was not free (" + why + ")"
	}
	return "remote: this machine's " + lane + " lane is not free (" + why + ")"
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
			// The lane was taken between the question and the run, or refused a run that has no wait (gpu_wait_ms 0), and
			// nothing ran: the ticket goes back to waiting with its place. It does NOT ask the lane again at once: a lane
			// that reads free and refuses at once would be asked and refused in a loop, a pipeline deferred row per run.
			// The pause grows with each refusal in a row (the same steps as a node that does not answer), and step looks
			// at the fleet meanwhile. A refusal that took the run's whole wait (gpu_wait_ms > 0) already spaced the runs.
			lt.mu.Lock()
			lt.running = false
			lt.t.State = stWaiting
			lt.lostN++
			pause := backoffSteps[min(lt.lostN, len(backoffSteps))-1]
			lt.lostUntil = time.Now().Add(pause)
			n := lt.lostN
			lt.t.Local = &LocalLane{State: "busy", Why: fmt.Sprintf("the last run on this machine's lane was refused at once although it read free; not asked again for %s", pause)}
			lt.persistLocked()
			lt.mu.Unlock()
			logQueue("ticket %s: this machine's lane read free and refused the run at once (%d in a row); not asked again for %s, the fleet is looked at meanwhile", lt.t.ID, n, pause)
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
	// A line goes on record as said only when there is someone to say it to: a scan can reach a ticket before its waiter has set
	// Notify, and a key recorded then made the waiter's own first look at the same standing a repeat, so it was never told.
	if key != "" && fn != nil {
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
	jobID, jerr := newJobID()
	if jerr != nil {
		logQueue("ticket %s: no job id could be made: %v", lt.t.ID, jerr)
		lt.mu.Unlock()
		return
	}
	lt.running = true // one dispatch of a ticket at a time, from here until the job ends or the claim is refused
	cp := lt.t
	// The ticket names its job in the very write that claims the node. A process that ends between the claim and the POST
	// leaves a ticket an adopter can ask the node about; without the id there is nothing to ask, and a job the POST got
	// through would run with no record of where.
	cp.Job = jobID
	lt.mu.Unlock()
	// The claim waits for the directory lock: not with the ticket locked.
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
	if lt.canceled {
		// A cancel landed while the claim waited for the lock, and it told its caller that nothing was sent: the claim wrote
		// the ticket's file and the node's claim after the cancel removed them, so both go again and nothing is sent.
		lt.running = false
		lt.mu.Unlock()
		lt.st.releaseNode(c.base, cp.ID)
		lt.st.remove(cp.ID)
		return
	}
	lt.t.State, lt.t.Base, lt.t.Node, lt.t.Job = cp.State, cp.Base, cp.Node, cp.Job
	placement := lanePlacement(lt.req, lt.t.LaneWhy, false)
	lt.mu.Unlock()
	lt.q.wg.Add(1)
	go func() {
		defer lt.q.wg.Done()
		start := time.Now()
		actx, cancel := context.WithTimeout(lt.ctx, Budgets[lt.pl.fleetTask])
		defer cancel()
		h := core.BeginRemote(lt.runner, lt.req, RouteAuto)
		defer core.CloseOnPanic(h)
		a := attempt(actx, lt.cfg, lt.req, lt.pl, c, lt.local, start, jobID, &ticketHandle{RemoteAttribution: h, lt: lt})
		switch a.kind {
		case attemptDone:
			a.res.Meta.Placement = placement + "; ran on " + c.node
			h.Finish(a.res)
			lt.finish(a.res, false)
		case attemptFailed:
			res := placementDefer(a.err, placement)
			h.Finish(res)
			lt.finish(res, false)
		case attemptNotCapable: // the node's door said "not this request": nothing ran there, and it is not busy
			h.Discard("the node cannot run this request; the ticket goes on to the next lane that frees")
			// The refusal is kept BEFORE the ticket is let back into the line: a scan that found it waiting without the memory
			// would offer the same node the same graph again.
			lt.rememberNotCapable(c, a.row)
			lt.backToWaiting(c.base)
			lt.q.nudge()
		default: // the node passed the job back or was not reached: nothing ran there
			h.Discard("the ticket goes on to the next lane that frees")
			lt.backToWaiting(c.base)
			lt.rememberAttempt(c, a.row)
			lt.q.nudge()
		}
	}()
}

// backToWaiting puts a ticket whose job went nowhere (a node passed it back or was not reached, or an adopted job was never
// on its node) back in line: the node and the job id are let go and the next scan decides again. The ticket keeps its arrival
// and its place.
func (lt *liveTicket) backToWaiting(base string) {
	lt.mu.Lock()
	lt.running = false
	lt.t.State, lt.t.Base, lt.t.Node, lt.t.Job = stWaiting, "", "", ""
	id := lt.t.ID
	lt.persistLocked()
	lt.mu.Unlock()
	lt.st.releaseNode(base, id)
}

// rememberNotCapable keeps a node's refusal of THIS ticket (the graph names a file it does not hold) and writes its row: the ticket
// does not ask that node again for notCapableAgain, and the next scan, which comes at once, goes on to the next candidate.
func (lt *liveTicket) rememberNotCapable(c candidate, row ClusterRow) {
	lt.mu.Lock()
	if lt.notCapable == nil {
		lt.notCapable = map[string]notCapableMemo{}
	}
	lt.notCapable[normBase(c.base)] = notCapableMemo{why: row.Why, until: placerNow().Add(notCapableAgain)}
	lt.mu.Unlock()
	if why, ok := lt.refusedBy(c.base); ok {
		row.Why = why
	}
	lt.rememberAttempt(c, row)
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
	if lt.ended || terminalState(lt.t.State) {
		lt.mu.Unlock()
		return
	}
	lt.ended = true
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
	lt.st.removeBodies(id) // a graph ticket's copies: the result is on file and nothing will run or adopt the body again
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
	if state == stWaiting && !lt.canceled && !lt.ended {
		// Marked in the same hold of the lock as the look at the state: a scan that is claiming a node for this ticket
		// (state still waiting until the claim is taken) checks the mark under the lock before it commits to sending, so the
		// job is either sent, and then the state is placing and this answers false, or it is not sent.
		lt.canceled = true
		lt.mu.Unlock()
		lt.finish(deferRes(core.DeferClassCapacity, why, core.ErrClassGPUQueued), true)
		return true
	}
	lt.mu.Unlock()
	if state == stLocal {
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

// queueEnded says res is the ticket queue's own end of a call (a ticket that is over), not an answer of a lane. The queue files
// every such end under the held class gpu_queued, with a defer class the lanes' own held answers never carry: the ceiling
// (step: "waited ... without a lane anywhere", DeferClassBudget) and an adoption it could not make (attach,
// DeferClassInfrastructure). A lane's own held answer, a lane that read free and was taken a moment later, is gpu_busy with
// no defer class or gpu_queued with the capacity class (pipeline.deferForLease). The batch reads this to tell the two apart:
// the second is asked again, the first is not (it would start another ticket and wait another ceiling).
func queueEnded(res core.Result) bool {
	return core.CardHeld(res.Meta.ErrClass) && (res.DeferClass == core.DeferClassBudget || res.DeferClass == core.DeferClassInfrastructure)
}

// queueCeiling says res is the ceiling in particular: a ticket that waited ticketMaxWait without a lane anywhere (step).
func queueCeiling(res core.Result) bool {
	return core.CardHeld(res.Meta.ErrClass) && res.DeferClass == core.DeferClassBudget
}

// queueLog is where the queue's log lines go (the process's stderr; a test replaces it).
var queueLog = func(line string) { fmt.Fprintln(os.Stderr, line) }

func logQueue(format string, args ...any) {
	queueLog("media queue: " + fmt.Sprintf(format, args...))
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
