package mediaremote

// How a call meets the queue (P0 plan S2; ADR 0083). A busy image call on a box with a fleet does not wait for ONE lane
// and does not loop: it becomes a ticket, and the call is only a way of waiting for it.
//
//   - A call that carries waiter_token=mq-... collects THAT ticket's result (or finds it still waiting and answers again).
//   - Any other call whose request is identical to a live ticket's attaches to it, with or without a token. That is what
//     ends a caller's loop of resends: the tenth identical request is the first one's ticket, not a tenth job.
//   - Otherwise the lane is asked (core.LaneProber). A free lane runs the call here, exactly as it always did. A busy one
//     makes the ticket, and the call waits for its first scan.
//
// How long a call waits depends on the door (Wait). The MCP doors answer inside their window (gpu_wait_ms, or wait_s) and
// the ticket keeps running in the process; the CLI blocks until the job is done and cancels a ticket that sent nothing
// when it is interrupted. A call whose window ends gets the answer that says where the ticket stands and why.

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// Wait says how a call takes an answer that is not final.
type Wait struct {
	// Window is how long the call blocks for its ticket: > 0 that long, 0 the config's gpu_wait_ms (the window every
	// other GPU door answers inside), < 0 until the job is done (the CLI). AtOnce answers after the ticket's first scan.
	Window time.Duration
	// Detach says the ticket outlives the call: an MCP door answers and the process goes on holding the ticket. Without
	// it (the CLI) the call IS the wait, and an interrupt cancels a ticket that has sent nothing.
	Detach bool
	// RosterFile is the config file whose delegate_remotes the ticket re-reads when the file changes (by mtime): an MCP
	// session never reloads its config, so a node added to the file joins the running session through this.
	RosterFile string
	// Notify, when set, is told one line whenever the ticket's standing changes (queued, started), for a human at a CLI.
	Notify func(string)
}

// AtOnce is a Window that answers right after the ticket's first scan.
const AtOnce = time.Nanosecond

// windowOf is how long a call blocks, < 0 for ever.
func windowOf(cfg config.Config, w Wait) time.Duration {
	switch {
	case w.Window < 0:
		return -1
	case w.Window > 0:
		return w.Window
	case cfg.GPUWaitMs <= 0:
		return 0
	}
	return time.Duration(cfg.GPUWaitMs) * time.Millisecond
}

// effectiveCfg is cfg with delegate_remotes re-read from the roster file when it changed.
func effectiveCfg(cfg config.Config, rosterFile string) config.Config {
	if remotes, ok := rosterFromFile(rosterFile); ok {
		cfg.DelegateRemotes = remotes
	}
	return cfg
}

func withoutParam(req core.Request, key string) core.Request {
	params := make(map[string]any, len(req.Params))
	for k, v := range req.Params {
		if k != key {
			params[k] = v
		}
	}
	req.Params = params
	return req
}

// runQueued is the busy-lane path of an auto image call on a box that has the lane and a fleet.
func (q *queueRT) runQueued(ctx context.Context, cfg config.Config, runner Runner, req core.Request, remotes []string, w Wait) core.Result {
	res, lt := q.admit(ctx, cfg, runner, req, remotes, w)
	if lt != nil {
		return q.await(ctx, cfg, w, lt)
	}
	return res
}

// admit decides what a call is: a result already (the lane was free and the call ran, or it joined a ticket and waited for
// it), or a ticket this process just made and the caller is to wait for.
func (q *queueRT) admit(ctx context.Context, cfg config.Config, runner Runner, req core.Request, remotes []string, w Wait) (core.Result, *liveTicket) {
	st, ok := storeFor(cfg)
	if req.Task != core.TaskGenerateImage || !ok {
		return runner.Run(ctx, req), nil
	}
	key := reqKey(req)

	// A ticket named by its token. A token that names nothing (collected, swept, mistyped) is a new arrival.
	var inherit time.Time
	if tok := str(req.Params, "waiter_token"); strings.HasPrefix(tok, "mq-") {
		if validTicketID(tok) {
			if t, found := st.read(tok); found {
				if t.ReqKey != key {
					return contractDefer(fmt.Sprintf("waiter_token %s names the ticket of a different request: send the same request to collect it, or drop the token for a new one", tok), core.Meta{}), nil
				}
				if res, attached := q.attach(ctx, cfg, runner, req, remotes, w, st, t); attached {
					return res, nil
				}
				inherit = time.UnixMilli(t.Created) // its owner is gone and it had sent nothing: the new ticket keeps its arrival
			}
		}
		req = withoutParam(req, "waiter_token")
	}

	// A call that runs under a lease its process inherited has a card chosen for it (checked after a ticket named by its token
	// was collected: that needs no lane).
	if ambientLease() {
		return runner.Run(ctx, req), nil
	}
	if _, err := candidateBases(cfg, remotes); err != nil {
		return runner.Run(ctx, req), nil // a bad `remotes` is reported by the paths that use it, as before
	}
	local, okID, _ := localIdentity(cfg, str(req.Params, "family"))
	if !okID {
		return runner.Run(ctx, req), nil
	}
	if _, err := plan(req); err != nil {
		return runner.Run(ctx, req), nil // the lane that runs it reports its own contract
	}

	// An identical request is the ticket it already made.
	for _, t := range st.list() {
		if t.ReqKey != key || (t.Collected && terminalState(t.State)) {
			continue // not this request, or its result was taken: a repeat of the content is a new job
		}
		if res, attached := q.attach(ctx, cfg, runner, req, remotes, w, st, t); attached {
			return res, nil
		}
		// Its owner is gone and it had sent nothing: the new ticket keeps its arrival (and its place in the line).
		if inherit.IsZero() || time.UnixMilli(t.Created).Before(inherit) {
			inherit = time.UnixMilli(t.Created)
		}
	}

	// A lane that reads free runs the call here, as it always did. One the pipeline could not read (v.Unjudged) is not a free
	// lane: the call becomes a ticket, whose first scan looks at the fleet and runs the call here only if no node can take it.
	v := core.ProbeLane(ctx, runner, req)
	if v.Free && !v.Unjudged {
		return runner.Run(ctx, req), nil
	}
	lt, existing, err := q.create(ctx, cfg, runner, req, remotes, w, st, key, local, v, inherit)
	if err != nil {
		logQueue("no ticket could be made (%v): the call waits in the local line", err)
		return runner.Run(ctx, req), nil
	}
	if existing != nil {
		if res, attached := q.attach(ctx, cfg, runner, req, remotes, w, st, *existing); attached {
			return res, nil
		}
		return runner.Run(ctx, req), nil
	}
	return core.Result{}, lt
}

// create makes the ticket for a call whose lane is busy. A request identical to a ticket that appeared meanwhile (two
// processes asked at once) is not made twice: existing is that ticket.
func (q *queueRT) create(ctx context.Context, cfg config.Config, runner Runner, req core.Request, remotes []string, w Wait,
	st *store, key string, local localID, v core.LaneVerdict, inherit time.Time) (*liveTicket, *ticketFile, error) {
	req2 := req
	params := make(map[string]any, len(req.Params)+1)
	for k, val := range req.Params {
		params[k] = val
	}
	seed, given := posInt(params, "seed")
	if !given {
		seed = mintSeed()
		params["seed"] = seed
	}
	req2.Params = params
	pl, err := plan(req2) // the payload carries the fixed seed
	if err != nil {
		return nil, nil, err
	}

	created := inherit
	if created.IsZero() {
		created = time.Now()
	}
	carried := str(req.Params, "waiter_token")
	place, perr := core.LeavePlace(ctx, runner, req2, created)
	if perr == nil && place.Since.Before(created) {
		created = place.Since // a place the caller already held brings its arrival along
	}
	lt := &liveTicket{
		q: q, st: st, cfg: cfg, remotes: remotes, rosterFile: w.RosterFile, runner: runner, req: req2, pl: pl, local: local,
		done: make(chan struct{}), scanned: make(chan struct{}), rowSince: map[string]rowMemo{},
	}
	lt.ctx, lt.cancel = context.WithCancel(q.rootCtx)
	lt.first, lt.firstAt = &v, time.Now()
	lt.t = ticketFile{
		ID: newTicketID(), ReqKey: key, Created: created.UnixMilli(), Owner: q.owner, Beat: nowMs(),
		Task: string(req.Task), Family: local.name, Digest: local.recipe.Digest(), Out: str(req.Params, "out"), Seed: seed,
		State: stWaiting, LaneWhy: v.Why,
	}
	if perr == nil {
		lt.t.Place = place.Token
	} else {
		lt.t.PlaceNote, lt.placeTried = placeNote(perr), time.Now()
	}
	var existing *ticketFile
	err = st.withLock(func() error {
		for _, t := range st.list() {
			if t.ReqKey == key && !t.Collected && !(t.dead() && (t.State == stWaiting || t.State == stLocal)) {
				tt := t
				existing = &tt
				return nil
			}
		}
		return st.write(lt.t)
	})
	if err != nil || existing != nil {
		lt.cancel()
		if perr == nil && place.Token != carried {
			core.DropPlace(runner, place.Token) // the place was made for this ticket, which does not exist
		}
		return nil, existing, err
	}
	q.register(lt)
	return lt, nil, nil
}

// attach joins a call to the ticket t, which is on file. attached is false when the ticket is gone for good (its owner
// died before anything was sent): the caller then makes a new one.
func (q *queueRT) attach(ctx context.Context, cfg config.Config, runner Runner, req core.Request, remotes []string, w Wait, st *store, t ticketFile) (core.Result, bool) {
	q.mu.Lock()
	lt := q.live[t.ID]
	q.mu.Unlock()
	switch {
	case lt != nil:
		return q.await(ctx, cfg, w, lt), true
	case terminalState(t.State):
		return collectFile(st, t), true
	case !t.dead():
		return q.awaitFile(ctx, cfg, runner, req, remotes, w, st, t.ID), true
	case inFlightState(t.State) && t.Job == "":
		// Its owner ended while it was being sent, and the file (an older build's) names no job: there is nothing to poll, and the
		// job may be on the node. The answer says so rather than guessing either way.
		return deferRes(core.DeferClassInfrastructure, fmt.Sprintf("ticket %s was being sent to %s when its owner ended and records no job id, so it cannot be polled and may already be rendering there; check that node's jobs before sending the request again", t.ID, t.Node), core.ErrClassGPUQueued), true
	case inFlightState(t.State):
		if ad := q.adopt(ctx, cfg, runner, req, remotes, w, st, t); ad != nil {
			return q.await(ctx, cfg, w, ad), true
		}
		// Another process adopted it first, or it finished: attach to whatever it is now.
		if now, ok := st.read(t.ID); ok && !now.dead() {
			return q.awaitFile(ctx, cfg, runner, req, remotes, w, st, t.ID), true
		}
		return deferRes(core.DeferClassInfrastructure, fmt.Sprintf("ticket %s could not be adopted: its owner ended and the job could not be taken over; send the request again", t.ID), core.ErrClassGPUQueued), true
	}
	// Waiting or running locally under a dead owner: nothing was sent anywhere, so it is swept and the call starts over.
	st.remove(t.ID)
	if t.Place != "" {
		core.DropPlace(runner, t.Place)
	}
	return core.Result{}, false
}

// adopt takes over a placed ticket whose owner died: this process polls the recorded job and fetches its outputs. It never
// sends the request again, so the job renders once. nil when another process took it first or it is not adoptable.
func (q *queueRT) adopt(ctx context.Context, cfg config.Config, runner Runner, req core.Request, remotes []string, w Wait, st *store, t ticketFile) *liveTicket {
	local, ok, _ := localIdentity(cfg, str(req.Params, "family"))
	if !ok {
		local = localID{}
	}
	pl, err := plan(withSeed(req, t.Seed))
	if err != nil {
		return nil
	}
	var taken bool
	err = st.withLock(func() error {
		cur, ok := st.read(t.ID)
		if !ok || !cur.dead() || !inFlightState(cur.State) {
			return nil
		}
		cur.Owner, cur.Beat = q.owner, nowMs()
		if err := st.write(cur); err != nil {
			return err
		}
		t, taken = cur, true
		return nil
	})
	if err != nil || !taken {
		return nil
	}
	lt := &liveTicket{
		q: q, st: st, cfg: cfg, remotes: remotes, rosterFile: w.RosterFile, runner: runner, req: withSeed(req, t.Seed), pl: pl, local: local,
		done: make(chan struct{}), scanned: make(chan struct{}), rowSince: map[string]rowMemo{}, running: true, t: t,
	}
	lt.ctx, lt.cancel = context.WithCancel(q.rootCtx)
	// The node is in use again, under the new owner.
	if ok, _, cerr := st.claimNode(&lt.t, t.Base, t.Node, t.State); cerr != nil || !ok {
		logQueue("ticket %s: adopted, but its node could not be claimed again (%v)", t.ID, cerr)
	}
	q.register(lt)
	lt.markScanned()
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		start := time.Now()
		placement := lanePlacement(t.LaneWhy, true)
		actx, cancel := context.WithTimeout(lt.ctx, Budgets[lt.pl.fleetTask])
		defer cancel()
		// A ticket that was only being sent when its owner ended may never have reached the node: ask it once. A node that
		// denies the job (404) was never given it (or lost it before it ran), so there is nothing to wait for and the ticket
		// goes back in line with its arrival; a node that holds it, or does not answer, is polled as any placed ticket.
		if t.State == stPlacing {
			if _, gerr := getJSON[jobWire](actx, cfg, t.Base+"/fleet/jobs/"+t.Job); gerr != nil {
				var se *httpStatusError
				if errors.As(gerr, &se) && se.code == http.StatusNotFound {
					logQueue("ticket %s: its owner ended while it was being sent to %s and the node does not hold job %s; the ticket goes back in line", t.ID, t.Node, t.Job)
					lt.backToWaiting(t.Base)
					lt.q.nudge()
					return
				}
			}
		}
		// The attribution handle opens no card (Dispatched is never called): the call writes its one ledger row and the card
		// the ended process opened is closed by the sweep that closes a dead process's cards.
		h := core.BeginRemote(runner, lt.req, RouteAuto)
		defer core.CloseOnPanic(h)
		res, err := collect(actx, cfg, lt.req, lt.pl, t.Base, t.Node, t.Job, start, &ticketHandle{RemoteAttribution: h, lt: lt})
		if err == nil && passedBack(res) {
			// The node took the job and passed it back (another job held its card): nothing ran, as for a live owner's job. The
			// ticket re-enters the line and the node is left alone for a while, instead of the caller being handed a gpu_busy
			// failure for a job that can still run.
			h.Discard("the node passed the adopted job back; the ticket goes on to the next lane that frees")
			placer.bounced(t.Base, t.Node, "bounced", "it took the last call and passed it back: another job held its card")
			lt.backToWaiting(t.Base)
			lt.q.nudge()
			return
		}
		if err != nil {
			res = placementDefer(err, placement)
		} else if res.OK {
			res.Meta.Placement = placement + "; ran on " + t.Node + " (the job was taken over from a process that ended)"
		}
		h.Finish(res)
		lt.finish(res, false)
	}()
	return lt
}

func withSeed(req core.Request, seed int) core.Request {
	params := make(map[string]any, len(req.Params)+1)
	for k, v := range req.Params {
		params[k] = v
	}
	params["seed"] = seed
	req.Params = params
	return req
}

// deliver hands the ticket's result to a call that waited for it. The first call to take it marks the file collected, and the
// file stays for collectedKeep so a call of another process that waits on the same ticket, or one that names its token, still
// gets the result; a request that only repeats the content starts a new job (the result was taken).
func (q *queueRT) deliver(lt *liveTicket) core.Result {
	lt.mu.Lock()
	res, first, canceled := lt.res, !lt.collected, lt.canceled
	lt.collected = true
	id := lt.t.ID
	lt.mu.Unlock()
	if first && !canceled {
		lt.st.markCollected(id)
	}
	return res
}

// collectFile hands over the result a finished ticket kept.
func collectFile(st *store, t ticketFile) core.Result {
	var res core.Result
	if len(t.Result) == 0 || json.Unmarshal(t.Result, &res) != nil {
		res = deferRes(core.DeferClassInfrastructure, fmt.Sprintf("ticket %s finished but kept no readable result; send the request again", t.ID), "")
	}
	st.markCollected(t.ID)
	return res
}

// await waits for the ticket this process owns, as long as the door allows.
func (q *queueRT) await(ctx context.Context, cfg config.Config, w Wait, lt *liveTicket) core.Result {
	var expire <-chan time.Time
	if win := windowOf(cfg, w); win >= 0 {
		timer := time.NewTimer(win)
		defer timer.Stop()
		expire = timer.C
	}
	poll := time.NewTicker(attachPoll)
	defer poll.Stop()
	if w.Notify != nil {
		lt.mu.Lock()
		lt.notify = w.Notify
		lt.mu.Unlock()
	}
	tell := func() {
		if w.Notify != nil {
			lt.say(standing(lt.snapshot()))
		}
	}
	tell()
	for {
		select {
		case <-lt.done:
			return q.deliver(lt)
		case <-expire:
			expire = nil
			select {
			case <-lt.scanned:
			case <-lt.done:
			case <-ctx.Done():
			case <-time.After(firstScanBound):
			}
			select {
			case <-lt.done:
				return q.deliver(lt)
			default:
			}
			snap := lt.snapshot()
			if terminalState(snap.State) {
				<-lt.done // finished between the two looks: its result is the answer
				return q.deliver(lt)
			}
			return queuedAnswer(snap)
		case <-ctx.Done():
			if w.Detach {
				return deferRes(core.DeferClassCapacity, fmt.Sprintf("the call ended before its job did; ticket %s keeps running in this process and a repeat of the request, or waiter_token=%s, collects it", lt.t.ID, lt.t.ID), core.ErrClassGPUQueued)
			}
			if lt.cancelUndispatched(fmt.Sprintf("interrupted: ticket %s was canceled before anything was sent to a lane", lt.t.ID)) {
				select {
				case <-lt.done:
					return q.deliver(lt)
				case <-time.After(5 * time.Second):
				}
			}
			snap := lt.snapshot()
			return deferRes(core.DeferClassCapacity, fmt.Sprintf("interrupted while ticket %s was on %s (job %s): a job already on a node cannot be recalled; run the same request again to collect it", snap.ID, snap.Node, snap.Job), core.ErrClassGPUQueued)
		case <-poll.C:
			tell()
		}
	}
}

// awaitFile waits for a ticket another process owns, by reading its file.
func (q *queueRT) awaitFile(ctx context.Context, cfg config.Config, runner Runner, req core.Request, remotes []string, w Wait, st *store, id string) core.Result {
	var expire <-chan time.Time
	if win := windowOf(cfg, w); win >= 0 {
		timer := time.NewTimer(win)
		defer timer.Stop()
		expire = timer.C
	}
	poll := time.NewTicker(attachPoll)
	defer poll.Stop()
	scanBound := time.Now().Add(firstScanBound)
	expired := false
	toldKey := ""
	for {
		t, ok := st.read(id)
		switch {
		case !ok:
			return deferRes(core.DeferClassCapacity, fmt.Sprintf("ticket %s is gone (collected by another call, or swept after its owner ended); send the request again", id), core.ErrClassGPUQueued)
		case terminalState(t.State):
			return collectFile(st, t)
		case t.dead():
			// The owner ended while this call waited: start over (adopt the job, or make a new ticket).
			if res, attached := q.attach(ctx, cfg, runner, req, remotes, w, st, t); attached {
				return res
			}
			return q.runQueued(ctx, cfg, runner, req, remotes, w)
		case w.Notify != nil:
			if key, line := standing(t); line != "" && key != toldKey {
				toldKey = key
				w.Notify(line)
			}
		}
		if expired && (t.Scanned || time.Now().After(scanBound)) {
			return queuedAnswer(t)
		}
		select {
		case <-expire:
			expire, expired = nil, true
		case <-ctx.Done():
			return deferRes(core.DeferClassCapacity, fmt.Sprintf("the call ended before ticket %s did; it keeps its place and a repeat of the request, or waiter_token=%s, collects it", id, id), core.ErrClassGPUQueued)
		case <-poll.C:
		}
	}
}

// snapshot copies the ticket's persisted record.
func (lt *liveTicket) snapshot() ticketFile {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	t := lt.t
	t.Cluster = append([]ClusterRow(nil), lt.t.Cluster...)
	return t
}

// standing is the one line for a human about where a ticket stands, and a key that changes when the standing does: the line of
// a waiting ticket carries reasons whose numbers move every scan, and a human is told once, not every scan.
func standing(t ticketFile) (key, line string) {
	switch t.State {
	case stLocal:
		return "local", fmt.Sprintf("ticket %s: started on this machine's lane", t.ID)
	case stPlacing, stPlaced, stFetching:
		return "node/" + t.Node, fmt.Sprintf("ticket %s: started on %s", t.ID, t.Node)
	case stWaiting:
		if t.Scanned {
			return fmt.Sprintf("queued/%d", t.Position), fmt.Sprintf("ticket %s: queued (position %d); it starts on the first matching lane that frees: %s", t.ID, t.Position, clusterSummary(t))
		}
	}
	return "", ""
}

// clusterSummary is the per-node why, in one sentence.
func clusterSummary(t ticketFile) string {
	var parts []string
	if t.Local != nil {
		why := t.Local.Why
		if why == "" {
			why = t.Local.State
		}
		parts = append(parts, "this machine: "+why)
	}
	for _, r := range t.Cluster {
		parts = append(parts, r.Node+": "+r.Why)
	}
	if t.PlaceNote != "" {
		parts = append(parts, "no seniority in this machine's own line ("+t.PlaceNote+")")
	}
	return strings.Join(parts, "; ")
}

// nextText is what the answer tells a caller to do.
func nextText(id string) string {
	return fmt.Sprintf("the harness holds this job and starts it on the first matching lane that frees; do not call again or resend; a repeat of the request attaches to this ticket; call with waiter_token=%s only to collect the result (a finished result is kept for %.0f hours)", id, doneOKKeep.Hours())
}

// queuedAnswer is the one answer that says where a ticket stands and why: the ticket, its place in this machine's queue,
// this machine's lane and every roster node with the reason it cannot run the job now.
func queuedAnswer(t ticketFile) core.Result {
	cluster := t.Cluster
	if cluster == nil {
		cluster = []ClusterRow{}
	}
	data := map[string]any{
		"queued": true, "scope": "cluster", "ticket": t.ID, "waiter_token": t.ID, "position": maxInt(t.Position, 1),
		"cluster": cluster, "next": nextText(t.ID),
	}
	if t.Local != nil {
		data["local"] = t.Local
	}
	if t.State != stWaiting {
		data["state"] = t.State
		if t.Node != "" {
			data["node"] = t.Node
		}
	}
	head := fmt.Sprintf("queued as %s (position %d): %s", t.ID, maxInt(t.Position, 1), clusterSummary(t))
	switch t.State {
	case stLocal:
		head = fmt.Sprintf("ticket %s is running on this machine's lane", t.ID)
	case stPlacing, stPlaced, stFetching:
		head = fmt.Sprintf("ticket %s is running on %s (remote job %s)", t.ID, t.Node, t.Job)
	}
	res := core.Deferf(head+". "+nextText(t.ID), "", core.Meta{ErrClass: core.ErrClassGPUQueued})
	res.DeferClass = core.DeferClassCapacity
	if b, err := json.Marshal(data); err == nil {
		res.Data = b
	}
	return res
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// mintSeed is a random positive seed in 1..1e9, the range the pipeline mints its own from.
func mintSeed() int {
	n, err := crand.Int(crand.Reader, big.NewInt(1_000_000_000))
	if err != nil {
		return 1
	}
	return int(n.Int64()) + 1
}
