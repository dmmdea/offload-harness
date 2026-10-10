package mediaremote

// Overflow (P0 plan, S1; ADR 0082): a media call on a box that has the lane no longer waits for it while another node
// of the fleet stands idle with the same recipe.
//
// WHEN. Only an `auto` image call on a machine that has the lane and a fleet configured, that is not resuming a
// place in the local line (waiter_token), does not run under a lease its process inherited, and whose pipeline
// answers that the lane is NOT free right now (core.LaneProber; a runner that cannot say reads as free). Every other
// call takes the path it always took, byte for byte: a box with no delegate_remotes asks nothing and reads no node.
//
// WHERE. A node is a candidate when it serves image-gen, holds a family whose recipe digests to this machine's (the
// strict match of identity.go; the family is sent under the node's OWN name, with the digest, and the node refuses a
// mismatch with 412), carries refine when the caller sent refine=false, runs this release, has the family's route
// configured when it reports its routes, and holds no lease of any class (the delegator cannot know which card a
// node's lease sits on or which card the job would take, so it is conservative and the node's own grant is the
// authority). Candidates rank by the shorter queue, then config order.
//
// HOW IT ENDS. A node that refuses the POST (503, 429) or accepts the job and answers that another job holds its
// card (gpu_busy, gpu_queued) or that its lease cannot be taken (gpu_lease_unavailable: in all three nothing ran) is
// passed over, under a fresh job id, at most maxPlacements times, and is not offered another call for a while (a
// node that read idle in health but whose own grant refuses the job refuses every call alike, and each one would park
// for the node's whole gpu_wait_ms before the bounce came back). A node's own answer after it accepted the job is
// final: a render that failed is the call's result and is never placed elsewhere, and a job a node holds is never also
// run here (a media job cannot be recalled, ADR 0064). When nothing admits, the call runs here exactly as it always
// did, and if that ends in a deferral it carries a cluster[] block that says, per node, why the fleet could not take
// it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

// maxPlacements bounds the POST attempts of one overflowing call. Chosen, not measured: a call that three nodes have
// refused or bounced is better served by its own queue, which keeps its place.
const maxPlacements = 3

// backoffSteps is how long a node that failed to answer is left alone, by consecutive failure (the last step holds).
// Chosen transport constants, not resource numbers: a powered-off box costs one probe per step, not one per call.
var backoffSteps = []time.Duration{5 * time.Second, 15 * time.Second, time.Minute, 5 * time.Minute}

// maxCooldown caps the pause a node's Retry-After can put on it (the last backoff step; chosen, not measured).
const maxCooldown = 5 * time.Minute

// bounceSteps is how long a node that took a call and passed it back is left alone, by consecutive bounce (the last
// step holds). A node whose health reads idle but whose own grant refuses the job (a card the display rule keeps closed,
// a quarantined card, a host short of RAM, a card another process holds that no lease shows) refuses every call alike,
// and each call sent to it parks for the node's whole gpu_wait_ms (90 s by default) before the bounce comes back: three
// calls would cost 270 s of waiting for nothing. The first bounce pauses the node for a minute (a lane taken between the
// health read and the POST is the common cause and clears by itself), the second for the cap; a call the node serves
// forgets them. Chosen transport constants, not measured: this release has no node-published verdict to read instead
// (the place-now header of the plan, section 7, is the lasting fix).
var bounceSteps = []time.Duration{time.Minute, maxCooldown}

// Seams: tests replace the roster reader, this machine's release, the clock and the jitter; production never does.
var (
	probeRoster  = rosterprobe.Probe
	localVersion = buildinfo.Version
	placerNow    = time.Now
	hostnameFn   = os.Hostname
	placerJitter = func(d time.Duration) time.Duration { return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) }
)

// ClusterRow says, for one roster node, whether the fleet could take a call and why not: the answer a caller reads
// when the call stayed on its own queue.
type ClusterRow struct {
	Node string `json:"node"`
	// State is one of busy (it holds a lease), not-capable (it cannot render this recipe), unreachable, bounced (it
	// had the call and passed it back), refused (it answered the POST with a refusal) or skipped.
	State   string   `json:"state"`
	Why     string   `json:"why"`
	Differs []string `json:"differs,omitempty"`
}

// placerState is the process-wide memory of nodes that did not answer: a call after a call must not pay a probe bound
// for a box that is down.
type placerState struct {
	mu    sync.Mutex
	nodes map[string]*nodeBackoff
	// held is the nodes this process has a job on right now. Two calls of one process (an MCP server answers its
	// session's calls in parallel) that both read a node idle must not both send it a job: the second would sit in the
	// node's own queue for its whole window and come back as a bounce.
	held map[string]bool
}

// nodeBackoff is what the placer remembers about a node it is leaving alone.
type nodeBackoff struct {
	// fails counts consecutive failures to reach the node; bounces counts consecutive calls it took and passed back
	// (only a call it serves clears them).
	fails, bounces int
	until          time.Time
	// state and cause say what the pause is for, in the cluster row's own terms: a node that ANSWERED must never read
	// as one that did not. shown is the node as the cluster row names it ("address (node id)"), known once it answered.
	state, cause, shown string
}

// pause is a node being left alone: for how much longer, and why.
type pause struct {
	left                time.Duration
	state, cause, shown string
}

// why is the cluster row's sentence for the pause.
func (z pause) why() string {
	lead := "not offered a call"
	if z.state == "unreachable" {
		lead = "not probed"
	}
	return fmt.Sprintf("%s: %s; looking again in %s", lead, z.cause, z.left.Round(time.Second))
}

var placer = &placerState{nodes: map[string]*nodeBackoff{}, held: map[string]bool{}}

func resetPlacer() {
	placer.mu.Lock()
	defer placer.mu.Unlock()
	placer.nodes = map[string]*nodeBackoff{}
	placer.held = map[string]bool{}
}

// hold takes base for one call of this process; false when another call already has a job there.
func (p *placerState) hold(base string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held[base] {
		return false
	}
	p.held[base] = true
	return true
}

// release gives base back.
func (p *placerState) release(base string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.held, base)
}

// backedOff reports whether base is being left alone, for how much longer and why.
func (p *placerState) backedOff(base string) (pause, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if nb := p.nodes[base]; nb != nil {
		if left := nb.until.Sub(placerNow()); left > 0 {
			return pause{left: left, state: nb.state, cause: nb.cause, shown: nb.shown}, true
		}
	}
	return pause{}, false
}

// entry is base's record, made when there is none. Callers hold p.mu.
func (p *placerState) entry(base string) *nodeBackoff {
	nb := p.nodes[base]
	if nb == nil {
		nb = &nodeBackoff{}
		p.nodes[base] = nb
	}
	return nb
}

// failed records one more consecutive failure to reach base and sets its pause.
func (p *placerState) failed(base string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	nb := p.entry(base)
	step := nb.fails
	if step >= len(backoffSteps) {
		step = len(backoffSteps) - 1
	}
	nb.fails++
	nb.until = placerNow().Add(backoffSteps[step])
	nb.state, nb.cause = "unreachable", "it did not answer a moment ago"
}

// cooldown leaves base alone for d (a node's Retry-After, jittered once so several delegators do not return together).
func (p *placerState) cooldown(base string, d time.Duration, shown, cause string) {
	if d <= 0 {
		return
	}
	if d > maxCooldown {
		d = maxCooldown
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	nb := p.entry(base)
	if until := placerNow().Add(placerJitter(d)); until.After(nb.until) {
		nb.until, nb.state, nb.cause, nb.shown = until, "refused", cause, shown
	}
}

// bounced records that base took a call and passed it back (nothing ran there) and leaves it alone for the next
// bounceSteps step. It returns the pause it set, jittered once like a Retry-After.
func (p *placerState) bounced(base, shown, state, cause string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	nb := p.entry(base)
	step := nb.bounces
	if step >= len(bounceSteps) {
		step = len(bounceSteps) - 1
	}
	nb.bounces++
	d := placerJitter(bounceSteps[step])
	if until := placerNow().Add(d); until.After(nb.until) {
		nb.until, nb.state, nb.cause, nb.shown = until, state, cause, shown
	}
	return d
}

// ok records that base answered a health read: what held against it for not answering is over. A pause that is still
// running because the node ANSWERED (a Retry-After, a bounce) is not undone by a health read, and the bounce count only
// a served call clears: a node that reads idle and bounces every call must not start again at the first step each time
// its pause lapses and it is read.
func (p *placerState) ok(base string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	nb := p.nodes[base]
	if nb == nil {
		return
	}
	nb.fails = 0
	if nb.state == "unreachable" {
		nb.until, nb.state, nb.cause, nb.shown = time.Time{}, "", "", ""
	}
	if nb.bounces == 0 && !placerNow().Before(nb.until) {
		delete(p.nodes, base)
	}
}

// served records that base ran a call (whatever its result): it is not bouncing.
func (p *placerState) served(base string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if nb := p.nodes[base]; nb != nil {
		nb.bounces = 0
		if !placerNow().Before(nb.until) {
			delete(p.nodes, base)
		}
	}
}

// overflowOutcome is what an attempt to place a call on the fleet came to.
type overflowOutcome struct {
	// placed: a node took the call (or held it and failed); res is the call's result and nothing runs here.
	placed bool
	res    core.Result
	// rows say why the fleet did not take the call, per roster node, when it did not.
	rows []ClusterRow
}

// annotate adds the cluster block to a local result that is a deferral, and quotes it in the reason. A result that is
// not a deferral (the call ran here) and a call the fleet was never asked about are returned untouched.
func (o overflowOutcome) annotate(res core.Result) core.Result {
	if len(o.rows) == 0 || res.OK || !res.Deferred {
		return res
	}
	obj := map[string]any{}
	if len(res.Data) > 0 {
		if err := json.Unmarshal(res.Data, &obj); err != nil {
			return res // data that is not an object stays as the pipeline made it
		}
	}
	obj["cluster"] = o.rows
	if b, err := json.Marshal(obj); err == nil {
		res.Data = b
	}
	parts := make([]string, 0, len(o.rows))
	for _, r := range o.rows {
		parts = append(parts, r.Node+": "+r.Why)
	}
	res.Reason += "; the fleet could not take it either: " + strings.Join(parts, "; ")
	return res
}

// selfNodeID is this machine's node id the way fleet-serve derives its own (main.go fleetServeParams): fleet_node_id,
// else the OS hostname. A roster that lists this machine's own node names the lane that is busy: it matches the recipe
// perfectly, and sending the call back into it would only wait the node's window and bounce.
func selfNodeID(cfg config.Config) string {
	if id := strings.TrimSpace(cfg.FleetNodeID); id != "" {
		return id
	}
	if h, err := hostnameFn(); err == nil {
		return strings.TrimSpace(h)
	}
	return ""
}

// ambientLease reports whether this process runs under a lease it inherited (`gpu reserve -- local-offload ...`): its
// card was chosen for it. It is the trigger the pipeline's own check keys on (pipeline.ambientLeaseEnv), read here so
// the router never even asks the lane about such a call.
func ambientLease() bool {
	for _, k := range []string{"GPU_LEASE_DIR", "GPU_LEASE_EPOCH", "GPU_LEASE_CLASS"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}

// tryOverflow places the call on the fleet when its local lane is not free. The zero outcome means "run it here".
func tryOverflow(ctx context.Context, cfg config.Config, runner Runner, req core.Request, remotes []string) overflowOutcome {
	if req.Task != core.TaskGenerateImage || str(req.Params, "waiter_token") != "" || ambientLease() {
		return overflowOutcome{}
	}
	bases, err := candidateBases(cfg, remotes)
	if err != nil {
		return overflowOutcome{} // a bad `remotes` is reported by the paths that use it, as before
	}
	local, ok, _ := localIdentity(cfg, str(req.Params, "family"))
	if !ok {
		return overflowOutcome{}
	}
	pl, err := plan(req)
	if err != nil {
		return overflowOutcome{} // the lane that runs it reports its own contract
	}
	v := core.ProbeLane(ctx, runner, req)
	if v.Free {
		return overflowOutcome{}
	}
	return place(ctx, cfg, runner, req, pl, bases, local, v)
}

// candidate is a node that can take the call, with the name it knows the recipe by.
type candidate struct {
	base, node, shown string
	family            string
	idx, queue        int
}

// rowAt orders a cluster row by the roster's own order.
type rowAt struct {
	idx int
	row ClusterRow
}

func place(ctx context.Context, cfg config.Config, runner Runner, req core.Request, pl planned, bases []string, local localID, v core.LaneVerdict) overflowOutcome {
	cands, rows := candidatesFor(ctx, cfg, bases, local, needOf(req.Params))
	out := overflowOutcome{}
	flush := func(extra []rowAt) {
		all := append(append([]rowAt(nil), rows...), extra...)
		sort.SliceStable(all, func(i, j int) bool { return all[i].idx < all[j].idx })
		for _, r := range all {
			out.rows = append(out.rows, r.row)
		}
	}
	if len(cands) == 0 {
		flush(nil)
		return out
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Budgets[pl.fleetTask])
		defer cancel()
	}
	start := time.Now()
	placement := "remote: this machine's image lane is not free (" + v.Why + ")"
	h := core.BeginRemote(runner, req, RouteAuto)
	defer core.CloseOnPanic(h)

	var after []rowAt
	attempts := 0
	for _, c := range cands {
		if attempts >= maxPlacements {
			after = append(after, rowAt{c.idx, ClusterRow{Node: c.shown, State: "skipped", Why: fmt.Sprintf("not tried: a call is sent to at most %d nodes", maxPlacements)}})
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if !placer.hold(c.base) {
			after = append(after, rowAt{c.idx, ClusterRow{Node: c.shown, State: "busy", Why: "another call of this machine has a job there"}})
			continue
		}
		attempts++
		a := func() attemptResult {
			defer placer.release(c.base)
			return attempt(ctx, cfg, req, pl, c, local, start, h)
		}()
		switch a.kind {
		case attemptDone:
			a.res.Meta.Placement = placement + "; ran on " + c.node
			h.Finish(a.res)
			return overflowOutcome{placed: true, res: a.res}
		case attemptFailed:
			res := placementDefer(a.err, placement)
			h.Finish(res)
			return overflowOutcome{placed: true, res: res}
		case attemptBounce:
			after = append(after, rowAt{c.idx, a.row})
		case attemptOut:
			after = append(after, rowAt{c.idx, a.row})
		}
	}
	// Nothing took it. A handle that never reached a node writes nothing; one whose every attempt bounced was
	// re-armed by each Bounce and is not dispatched now.
	h.Discard("no node of the fleet took the call")
	flush(after)
	return out
}

// candidatesFor reads the roster and splits it into the nodes that can take the call, best first, and a row for every
// node that cannot, in the roster's order.
func candidatesFor(ctx context.Context, cfg config.Config, bases []string, local localID, n need) ([]candidate, []rowAt) {
	var (
		cands []candidate
		rows  []rowAt
		live  []string
	)
	idxOf := map[string]int{}
	for i, b := range bases {
		idxOf[b] = i
		if pz, backed := placer.backedOff(b); backed {
			name := pz.shown
			if name == "" {
				name = rosterprobe.Members([]string{b})[0].Shown()
			}
			rows = append(rows, rowAt{i, ClusterRow{Node: name, State: pz.state, Why: pz.why()}})
			continue
		}
		live = append(live, b)
	}
	self := selfNodeID(cfg)
	for _, r := range probeRoster(ctx, live, cfg.FleetAuthToken, healthTimeout) {
		i := idxOf[r.Base]
		if r.Err != nil {
			if !rosterprobe.IsRefusal(r.Err) && r.Source == rosterprobe.SourceProbe {
				placer.failed(r.Base)
			}
			why := r.Miss()
			state := "unreachable"
			if rosterprobe.IsRefusal(r.Err) {
				state = "refused"
			}
			rows = append(rows, rowAt{i, ClusterRow{Node: r.Shown(), State: state, Why: strings.TrimPrefix(why, r.Shown()+": ")}})
			continue
		}
		placer.ok(r.Base)
		v := r.View
		who := fmt.Sprintf("%s (%s)", r.Shown(), v.NodeID)
		if self != "" && strings.EqualFold(strings.TrimSpace(v.NodeID), self) {
			rows = append(rows, rowAt{i, ClusterRow{Node: who, State: "skipped", Why: "this machine's own node: the lane that is not free is the one it would use"}})
			continue
		}
		m, ok := matchNode(local, v, n)
		if !ok {
			rows = append(rows, rowAt{i, ClusterRow{Node: who, State: m.state, Why: m.why, Differs: m.differs}})
			continue
		}
		if v.LeaseHeld || len(v.Leases) > 0 || v.LeaseDraining {
			rows = append(rows, rowAt{i, ClusterRow{Node: who, State: "busy", Why: leaseWords(v)}})
			continue
		}
		id := v.NodeID
		if id == "" {
			id = r.Shown()
		}
		cands = append(cands, candidate{base: r.Base, node: id, shown: who, family: m.family, idx: i, queue: v.QueueDepth})
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].queue != cands[b].queue {
			return cands[a].queue < cands[b].queue
		}
		return cands[a].idx < cands[b].idx
	})
	return cands, rows
}

type attemptKind int

const (
	attemptDone   attemptKind = iota // the node ran it (its result, ok or its own deferral, is the call's)
	attemptBounce                    // nothing ran there: another job holds its card, or it was full
	attemptOut                       // this node is out for this call and nothing ran there (it refused the recipe, or was not reached)
	attemptFailed                    // the node had the job and the call failed: final
)

type attemptResult struct {
	kind attemptKind
	res  core.Result
	err  error
	row  ClusterRow
}

// attempt sends the call to c under a fresh job id and brings the result back.
func attempt(ctx context.Context, cfg config.Config, req core.Request, pl planned, c candidate, local localID, start time.Time, h core.RemoteAttribution) attemptResult {
	payload := make(map[string]any, len(pl.payload)+2)
	for k, v := range pl.payload {
		payload[k] = v
	}
	// The node renders the family under ITS name for this recipe, never the caller's (a name binds different files on
	// different nodes), and the digest lets it refuse a recipe it no longer holds.
	delete(payload, "family")
	if c.family != "" {
		payload["family"] = c.family
	}
	payload["recipe_digest"] = local.recipe.Digest()
	raw, err := json.Marshal(payload)
	if err != nil {
		return attemptResult{kind: attemptFailed, err: &contractError{fmt.Sprintf("the %s request cannot be encoded for the wire: %v", pl.fleetTask, err)}}
	}
	jobID, err := newJobID()
	if err != nil {
		return attemptResult{kind: attemptFailed, err: &placementError{core.DeferClassInfrastructure, "job id: " + err.Error()}}
	}
	body, err := json.Marshal(map[string]any{"job_id": jobID, "task_type": pl.fleetTask, "payload": json.RawMessage(raw)})
	if err != nil {
		return attemptResult{kind: attemptFailed, err: &contractError{fmt.Sprintf("the %s request cannot be encoded for the wire: %v", pl.fleetTask, err)}}
	}
	if len(body) > maxDispatchBody {
		return attemptResult{kind: attemptFailed, err: &contractError{fmt.Sprintf("the %s request is %d bytes, over the %d bytes a fleet dispatch carries", pl.fleetTask, len(body), maxDispatchBody)}}
	}
	res, serr := sendAndFetch(ctx, cfg, req, pl, c.base, c.node, "/fleet/dispatch", body, jobID, start, h, true)
	if serr == nil {
		switch {
		case !res.OK && core.CardHeld(res.Meta.ErrClass):
			// The node accepted the job and answered that another job holds its card: nothing ran. Its card closes
			// quiet, the node is left alone for a while (it read idle and refused anyway: it will again), and the call
			// goes on, under a fresh job id.
			h.Bounce(res)
			left := placer.bounced(c.base, c.shown, "bounced", "it took the last call and passed it back: another job held its card")
			return attemptResult{kind: attemptBounce, row: ClusterRow{Node: c.shown, State: "bounced",
				Why: fmt.Sprintf("had the call and passed it back: %s; not offered another call for %s", res.Reason, left.Round(time.Second))}}
		case !res.OK && res.Meta.ErrClass == core.ErrClassGPULeaseUnavailable:
			// The node accepted the job and could not take its lease (a lease location it cannot use, a host-RAM need no
			// state of it admits): nothing ran there either, and a call the fleet could still serve is not ended by one
			// node's fault.
			h.Bounce(res)
			left := placer.bounced(c.base, c.shown, "refused", "it took the last call and could not take its lease")
			return attemptResult{kind: attemptBounce, row: ClusterRow{Node: c.shown, State: "refused",
				Why: fmt.Sprintf("took the call and could not take its lease, so nothing ran: %s; not offered another call for %s", res.Reason, left.Round(time.Second))}}
		}
		placer.served(c.base)
		return attemptResult{kind: attemptDone, res: res}
	}
	// A refusal AT THE DOOR means the node never took the job, so nothing ran there and the call moves on, whatever the
	// status (a full node, a recipe it no longer holds, a request or a token it rejects: each is in the cluster block
	// if the call ends up waiting). A POST that was sent and got no answer is ambiguous (the node may hold the job), so
	// it is final, like anything after acceptance.
	var pe *postError
	if errors.As(serr, &pe) && ctx.Err() == nil {
		switch {
		case pe.neverReached:
			placer.failed(c.base)
			return attemptResult{kind: attemptOut, row: ClusterRow{Node: c.shown, State: "unreachable", Why: "did not take the connection: " + pe.Error()}}
		case pe.status == 503 || pe.status == 429:
			placer.cooldown(c.base, pe.retryAfter, c.shown, fmt.Sprintf("it answered %d to the last dispatch and asked to be left alone", pe.status))
			return attemptResult{kind: attemptBounce, row: ClusterRow{Node: c.shown, State: "refused", Why: fmt.Sprintf("answered %d to the dispatch: %s", pe.status, pe.Error())}}
		case pe.status == 412:
			return attemptResult{kind: attemptOut, row: ClusterRow{Node: c.shown, State: "refused", Why: "refused the recipe (412): its files changed since it published them; " + pe.Error()}}
		case pe.status != 0:
			return attemptResult{kind: attemptOut, row: ClusterRow{Node: c.shown, State: "refused", Why: fmt.Sprintf("answered %d to the dispatch: %s", pe.status, pe.Error())}}
		}
	}
	return attemptResult{kind: attemptFailed, err: serr}
}
