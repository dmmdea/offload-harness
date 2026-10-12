package mediaremote

// Placement on the fleet (P0 plan S1, S2, S4 and S5; ADR 0082, ADR 0083): which node can take a call whose own lane is
// busy, and what happens when the node says no.
//
// S1 made it the call's own business: a busy lane sent the call, once, to an idle node that renders the same recipe. S2
// moved the decision into the ticket queue (ticket.go, scan.go), which asks the same questions on every scan and holds
// the call until a lane frees; S5 put the whole-node graph on that queue too (ADR 0083 Amendment 1), so this file keeps
// what the queue rests on, with one filter, one ranking and one bounce rule for both kinds of call.
//
// WHERE. A node is a candidate when it serves image-gen, holds a family whose recipe digests to this machine's (the
// strict match of identity.go; the family is sent under the node's OWN name, with the digest, and the node refuses a
// mismatch with 412), carries refine when the caller sent refine=false, runs this release, has the family's route
// configured when it reports its routes, and holds no lease of any class (the delegator cannot know which card a
// node's lease sits on or which card the job would take, so it is conservative and the node's own grant is the
// authority). Candidates rank by the shorter queue, then config order. A ticket (an image's or a graph's) then also leaves out
// the nodes a job of this machine has a claim on (any ticket's, of any process of the machine), and the ones an older waiting
// ticket matches.
//
// HOW AN ATTEMPT ENDS. A node that refuses the POST (503, 429) or accepts the job and answers that another job holds its
// card (gpu_busy, gpu_queued) or that its lease cannot be taken (gpu_lease_unavailable: in all three nothing ran) is
// passed over and is not offered another call by this process for a while (a node that read idle in health but whose own
// grant refuses the job refuses every call alike, and each one would park for the node's whole gpu_wait_ms before the
// bounce came back). A 412 (the node refuses the recipe) is read by the cause it names: a node that has only not finished reading
// its weight files is left alone for the first bounce step and no longer, and does not count as a bounce; any other cause is
// bounced (refusedRecipe). A node's own answer after it accepted the job is final: a render that failed is the call's result
// and is never placed elsewhere, and a job a node holds is never also run here (a media job cannot be recalled, ADR
// 0064). The ticket waits for the next lane; it does not give up the way the S1 call did, and it keeps the arrival time
// the S1 call lost on every bounce. (A node that refuses a graph at its door because it lacks a file is the exception below: it
// is not passed over for a while, because it is not busy.)
//
// A NODE'S "NOT THIS REQUEST". A graph is identified by the files a node holds, and the node decides that at its door: a 503 that
// carries the refusal word (refusalNotCapable) says the graph names a file the node does not hold. That is the node's answer about
// THIS call, not about its load, so it is not a pause on the node (a full node, and a bounce, are): the ticket remembers it for
// itself (scan.go, notCapableAgain), the row says not-capable, and every other call is offered the node as before.

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
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

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
// forgets them. Per process, like the rest of the placer's memory. Chosen transport constants, not measured: this release
// has no node-published verdict to read instead (the place-now header of the plan, section 7, is the lasting fix).
var bounceSteps = []time.Duration{time.Minute, maxCooldown}

// refusalNotCapable is the node's word (fleetnode.RefusalNotCapable; a test holds the two equal, because this package does not
// import the node) for a 503 that says "not this request": the node is not full, it lacks something the request names.
const refusalNotCapable = "not_capable"

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
	// State is one of free (idle, but another ticket is first in line for it), busy (it holds a lease), draining (its
	// card is being drained for one), not-capable (it cannot render this recipe), unreachable, in-use (a job of this
	// machine is running there), bounced (it had the call and passed it back), refused (it answered the POST with a
	// refusal) or skipped.
	State   string   `json:"state"`
	Why     string   `json:"why"`
	Differs []string `json:"differs,omitempty"`
	// Holder is what holds a busy node, in the node's own terms.
	Holder *RowHolder `json:"holder,omitempty"`
	// Since is when the node was first found in this state by the ticket (RFC 3339, UTC).
	Since string `json:"since,omitempty"`
}

// RowHolder is the lease that holds a node or this machine's lane.
type RowHolder struct {
	Epoch        uint64 `json:"epoch,omitempty"`
	Class        string `json:"class"`
	Reason       string `json:"reason,omitempty"`
	RemainingSec int    `json:"remaining_s,omitempty"`
}

// placerState is this PROCESS's memory of the nodes it is leaving alone: nodes that did not answer (a call after a call
// must not pay a probe bound for a box that is down), nodes that took a call and passed it back, and nodes that asked to
// be left alone (Retry-After). It is the package variable placer, so it holds inside a long-lived process (the MCP server
// answers every media call of its session through it) and is empty when a one-shot CLI call starts (main.go runs
// Run once per process): that call pays a bounced node's whole gpu_wait_ms again.
type placerState struct {
	mu    sync.Mutex
	nodes map[string]*nodeBackoff
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

var placer = &placerState{nodes: map[string]*nodeBackoff{}}

func resetPlacer() {
	placer.mu.Lock()
	defer placer.mu.Unlock()
	placer.nodes = map[string]*nodeBackoff{}
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

// failed records one more consecutive failure to reach base and sets its pause, which it returns.
func (p *placerState) failed(base string) time.Duration {
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
	return backoffSteps[step]
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
	// base is the node's URL, and matched says its recipe matched the call (it is busy or paused for another reason): the
	// nodes an older ticket is first in line for.
	base    string
	matched bool
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
			rows = append(rows, rowAt{idx: i, base: b, row: ClusterRow{Node: name, State: pz.state, Why: pz.why()}})
			continue
		}
		live = append(live, b)
	}
	self := selfNodeID(cfg)
	for _, r := range probeRoster(ctx, live, cfg.FleetAuthToken, healthTimeout) {
		i := idxOf[r.Base]
		if r.Err != nil {
			why := r.Miss()
			if !rosterprobe.IsRefusal(r.Err) && r.Source == rosterprobe.SourceProbe {
				// One line per backoff step, not per scan: a node in its pause is not probed, so it is not logged.
				logQueue("%s; not probing it again for %s", why, placer.failed(r.Base))
			}
			state := "unreachable"
			if rosterprobe.IsRefusal(r.Err) {
				state = "refused"
			}
			rows = append(rows, rowAt{idx: i, base: r.Base, row: ClusterRow{Node: r.Shown(), State: state, Why: strings.TrimPrefix(why, r.Shown()+": ")}})
			continue
		}
		placer.ok(r.Base)
		v := r.View
		who := nodeShown(r.Base, v.NodeID)
		if self != "" && strings.EqualFold(strings.TrimSpace(v.NodeID), self) {
			rows = append(rows, rowAt{idx: i, base: r.Base, row: ClusterRow{Node: who, State: "skipped", Why: "this machine's own node: the lane that is not free is the one it would use"}})
			continue
		}
		m, ok := matchFor(local, v, n)
		if !ok {
			rows = append(rows, rowAt{idx: i, base: r.Base, row: ClusterRow{Node: who, State: m.state, Why: m.why, Differs: m.differs}})
			continue
		}
		if v.LeaseHeld || len(v.Leases) > 0 || v.LeaseDraining {
			row := ClusterRow{Node: who, State: "busy", Why: leaseWords(v), Holder: holderOf(v)}
			if v.LeaseDraining && !v.LeaseHeld && len(v.Leases) == 0 {
				row.State, row.Why = "draining", "its card is being drained for a lease that is about to be granted"
			}
			rows = append(rows, rowAt{idx: i, base: r.Base, matched: true, row: row})
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

// holderOf is the lease that holds a node, as its health states it (the worst of the live leases when several are held).
func holderOf(v delegate.NodeView) *RowHolder {
	if !v.LeaseHeld && len(v.Leases) == 0 {
		return nil
	}
	h := &RowHolder{Class: v.LeaseClass, Reason: v.LeaseReason, RemainingSec: v.LeaseRemainingSec}
	if len(v.Leases) > 0 {
		h.Epoch = v.Leases[0].Epoch
		if h.Class == "" {
			h.Class = v.Leases[0].Class
		}
	}
	return h
}

type attemptKind int

const (
	attemptDone       attemptKind = iota // the node ran it (its result, ok or its own deferral, is the call's)
	attemptBounce                        // nothing ran there: another job holds its card, or it was full
	attemptOut                           // this node is out for this call and nothing ran there (it refused the recipe, or was not reached)
	attemptNotCapable                    // the node's door refused THIS request (a graph that names a file it lacks): not full, no pause on the node
	attemptFailed                        // the node had the job and the call failed: final
)

type attemptResult struct {
	kind attemptKind
	res  core.Result
	err  error
	row  ClusterRow
}

// passedBack says a node's answer is a job it accepted and handed back: another job holds its card, or it could not take its
// lease. Nothing ran there, so the call goes on (to the next lane that frees) and the node is left alone for a while.
func passedBack(res core.Result) bool {
	return !res.OK && (core.CardHeld(res.Meta.ErrClass) || res.Meta.ErrClass == core.ErrClassGPULeaseUnavailable)
}

// passBack is how a pause for a passed-back job reads: a lease the node could not take is a refusal, anything else a
// bounce, and the cause carries the node's own reason. A host short of RAM answers with the same class as a held card, so
// a fixed "another job held its card" told that host as a card someone else held.
func passBack(base string, res core.Result) (state, cause string) {
	state, cause = "bounced", "it took the last call and passed it back"
	if res.Meta.ErrClass == core.ErrClassGPULeaseUnavailable {
		state, cause = "refused", "it took the last call and could not take its lease"
	}
	if why := nodeReason(base, res); why != "" {
		return state, cause + ": " + why
	}
	return state, cause
}

// nodeReason is a node's answer as a row may quote it: on one line, its address redacted, and capped like any other node
// text. A pause keeps it for minutes and every ticket's row repeats it.
func nodeReason(base string, res core.Result) string {
	why := strings.Join(strings.Fields(res.Reason), " ")
	if why == "" {
		return ""
	}
	return cutText(rosterprobe.Scrub(base, errors.New(why)), detailMax)
}

// nodeShown names a node in a cluster row: its redacted address and its node id, the same on every path that pauses it, so
// a row's since and the attempt it remembers match by name.
func nodeShown(base, nodeID string) string {
	return fmt.Sprintf("%s (%s)", rosterprobe.Member{Base: base}.Shown(), nodeID)
}

// attempt sends the call to c under jobID and brings the result back. The caller mints the id BEFORE it records the claim, so
// the ticket on disk names the job from the instant it may be on the node (a process that ends in between leaves an id an
// adopter can ask the node about).
func attempt(ctx context.Context, cfg config.Config, req core.Request, pl planned, c candidate, local localID, start time.Time, jobID string, h core.RemoteAttribution) attemptResult {
	payload := make(map[string]any, len(pl.payload)+2)
	for k, v := range pl.payload {
		payload[k] = v
	}
	// The node renders the family under ITS name for this recipe, never the caller's (a name binds different files on
	// different nodes), and the digest lets it refuse a recipe it no longer holds. A graph has neither: it carries its own
	// files' names, and the node refuses at its door when it lacks one.
	if pl.fleetTask == taskImage {
		delete(payload, "family")
		if c.family != "" {
			payload["family"] = c.family
		}
		payload["recipe_digest"] = local.recipe.Digest()
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return attemptResult{kind: attemptFailed, err: &contractError{fmt.Sprintf("the %s request cannot be encoded for the wire: %v", pl.fleetTask, err)}}
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
			state, cause := passBack(c.base, res)
			left := placer.bounced(c.base, c.shown, state, cause)
			return attemptResult{kind: attemptBounce, row: ClusterRow{Node: c.shown, State: state,
				Why: fmt.Sprintf("had the call and passed it back: %s; not offered another call for %s", nodeReason(c.base, res), left.Round(time.Second))}}
		case !res.OK && res.Meta.ErrClass == core.ErrClassGPULeaseUnavailable:
			// The node accepted the job and could not take its lease (a lease location it cannot use, a host-RAM need no
			// state of it admits): nothing ran there either, and a call the fleet could still serve is not ended by one
			// node's fault.
			h.Bounce(res)
			state, cause := passBack(c.base, res)
			left := placer.bounced(c.base, c.shown, state, cause)
			return attemptResult{kind: attemptBounce, row: ClusterRow{Node: c.shown, State: state,
				Why: fmt.Sprintf("took the call and could not take its lease, so nothing ran: %s; not offered another call for %s", nodeReason(c.base, res), left.Round(time.Second))}}
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
			logQueue("%s did not take the connection; not offering it a job for %s", c.shown, placer.failed(c.base))
			return attemptResult{kind: attemptOut, row: ClusterRow{Node: c.shown, State: "unreachable", Why: "did not take the connection: " + pe.Error()}}
		case pe.status == 503 && pe.refusal == refusalNotCapable:
			// The node is neither full nor out of route: it lacks something THIS request names. No pause is put on the node
			// (a cooldown would hide it from the next image ticket and graph that it can serve), and the ticket moves on.
			why := pe.detail
			if why == "" {
				why = pe.Error()
			}
			return attemptResult{kind: attemptNotCapable, row: ClusterRow{Node: c.shown, State: "not-capable", Why: "refused at its door: " + why}}
		case pe.status == 503 || pe.status == 429:
			// A node that names no Retry-After is still left alone for the first backoff step: the ticket tries the
			// roster again at its next scan, and a full node must not be the first one every scan sends the job to.
			cause := fmt.Sprintf("it answered %d to the last dispatch and asked to be left alone", pe.status)
			if pe.retryAfter <= 0 {
				cause = fmt.Sprintf("it answered %d to the last dispatch (it is full)", pe.status)
				if pl.fleetTask != taskImage {
					// A graph's 503 that carries no refusal word is the node's own state (a route that is not configured, a queue that
					// is full), not "full" in particular: a file it lacks is refused WITH the word, in the case above.
					cause = fmt.Sprintf("it answered %d to the last dispatch at its door", pe.status)
				}
			}
			placer.cooldown(c.base, maxDuration(pe.retryAfter, backoffSteps[0]), c.shown, cause)
			return attemptResult{kind: attemptBounce, row: ClusterRow{Node: c.shown, State: "refused", Why: fmt.Sprintf("answered %d to the dispatch: %s", pe.status, pe.Error())}}
		case pe.status == 412:
			return refusedRecipe(c, pe)
		case pe.status != 0:
			placer.bounced(c.base, c.shown, "refused", fmt.Sprintf("it answered %d to the last dispatch", pe.status))
			return attemptResult{kind: attemptOut, row: ClusterRow{Node: c.shown, State: "refused", Why: fmt.Sprintf("answered %d to the dispatch: %s", pe.status, pe.Error())}}
		}
	}
	return attemptResult{kind: attemptFailed, err: serr}
}

// refusalCause reads the cause a node named in the JSON body of its refusal (mediacap.RecipeRefusalCauseKey), "" when it named
// none: the body is not an object, has no such member, or the word is not one of the three of the wire. What a node writes
// chooses nothing beyond those words, and a word is a member of the body and never a part of its sentence.
func refusalCause(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	cause, _ := m[mediacap.RecipeRefusalCauseKey].(string)
	switch cause {
	case mediacap.RecipeRefusalPending, mediacap.RecipeRefusalUnreadable, mediacap.RecipeRefusalChanged:
		return cause
	}
	return ""
}

// refusedRecipe is a node's 412: it refused the recipe_digest this machine sent, and nothing ran there. The cause it names (pe.cause)
// tells a wait from a difference, and the two are not left alone alike.
//
// A node that has not finished reading its weight files (pending) cannot say yet whether anything changed: it has made no finding,
// and it is expected to have read them soon. It is left alone for the first bounce step and no longer, however often it answers
// so, and that is a cooldown and not a bounce: the count that carries a node from a minute to the cap is not touched, so a real
// change after a run of waits still starts at a minute. The pause is there because the rows the node published can be up to a
// minute old (its health memo) and would offer it the job again at the next scan, each dispatch waiting up to 20 s for the same
// answer; what the node IS is decided by the health row the next scan reads, not by this pause.
//
// Any other cause is bounced, a minute and then the cap: a node that cannot read one of its files, or whose files or binding are
// not what it published, refuses every call alike until someone looks. A refusal that names no cause this build knows (a proxy, a
// node of another build) is read that way too, and says it does not know.
func refusedRecipe(c candidate, pe *postError) attemptResult {
	says, bounce := "it refused the recipe (412) without saying why", true
	switch pe.cause {
	case mediacap.RecipeRefusalPending:
		says, bounce = "it refused the recipe (412): it had not finished reading its weight files, so it could not say yet whether they changed", false
	case mediacap.RecipeRefusalUnreadable:
		says = "it refused the recipe (412): it could not read one of its weight files, so it cannot vouch for the family"
	case mediacap.RecipeRefusalChanged:
		says = "it refused the recipe (412): its files or its binding changed since it published them"
	}
	if bounce {
		placer.bounced(c.base, c.shown, "refused", says)
	} else {
		placer.cooldown(c.base, bounceSteps[0], c.shown, says)
	}
	return attemptResult{kind: attemptOut, row: ClusterRow{Node: c.shown, State: "refused", Why: strings.TrimPrefix(says, "it ") + "; " + pe.Error()}}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
