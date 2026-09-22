// lanes.go — busy-aware cascade remote lanes (roast delta 7 of multi-node
// delegation): the DAILY lane's invisible upgrade. Where seat_endpoints
// (endpoints.go) is a STATIC pin — that seat is always remote —
// cascade_remote_lanes is a per-call failover: a cascade text call rides its
// configured lane only while the local box would make it WAIT — the
// machine-wide GPU lease is held, or another model holds the local llama-swap
// and a swap would queue behind its in-flight requests (register C-41) — AND
// the lane's llama-swap roster verifiably serves the model. Quality-first by
// construction: routing never changes WHICH model answers, only WHERE — the
// lane must serve the SAME model id/alias the call names, and any doubt
// (idle GPU, idle seat, probe failure, roster miss) resolves to the local
// base. Lane
// requests ride the same netguard.SafeTransport dial gate as seat overrides,
// so the never-cloud boundary holds at dial time here too.
package llamaclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// laneResidencyTTL is how long one roster answer backs lane residency for a
// base. Same order as fleetnode's agent-seat residency cache: fresh enough
// that a lane that dropped the model stops receiving calls within a window,
// coarse enough that a burst of cascade calls costs one probe, not one each.
const laneResidencyTTL = 30 * time.Second

// laneProbeTimeout bounds one roster GET — swapclient's probe discipline: a
// probe that outlives the call it informs is a hang, not a check. Shorter
// than any generation budget, so a dead lane costs at most this once per TTL.
const laneProbeTimeout = 5 * time.Second

// WithRemoteLanes installs the busy-aware lane table and its gates, returning
// the client (chainable at construction, like WithSeatEndpoints).
// lanes maps the exact model id or llama-swap alias a call names to the
// remote base URL that also serves it; busy reports whether the local
// machine-wide GPU lease is held (delegate.LocalBusy in production); busyFor
// answers the same question PER MODEL for the case no lease covers — another
// session's seat loaded on this box's llama-swap, so a swap to the requested
// model would wait behind its in-flight requests (LocalSwapBusy in
// production, register C-41) — and returns the sentence the serve log prints;
// resident reports whether base verifiably serves model (RosterResident in
// production — a seam so tests fake any gate). EITHER busy gate firing takes
// the lane; residency is required in both cases.
//
// An empty/nil map — or no busy gate at all, or a missing residency gate,
// which could never fire safely — installs NOTHING: no lane table, no gates,
// no second HTTP client. A config without cascade_remote_lanes yields a
// client byte-identical to a pre-lanes build (pinned by test). Lane bases get
// the same tailnet-guarded client the seat overrides ride, built once and
// shared.
func (c *Client) WithRemoteLanes(lanes map[string]string, busy func() bool, busyFor func(model string) (bool, string), resident func(base, model string) bool) *Client {
	if len(lanes) == 0 || (busy == nil && busyFor == nil) || resident == nil {
		return c
	}
	m := make(map[string]string, len(lanes))
	for model, base := range lanes {
		m[model] = strings.TrimRight(base, "/") // same normalization New applies to base
	}
	c.remoteLanes = m
	c.laneBusy = busy
	c.laneBusyFor = busyFor
	c.laneResident = resident
	if c.safeHTTP == nil {
		c.safeHTTP = &http.Client{Timeout: c.http.Timeout, Transport: netguard.SafeTransport(newTransport())}
	}
	return c
}

// WithLaneRoute installs the lane ROUTER beside the gates: for a lane base it
// recognizes as a fleet NODE it returns that node's chat path and the bearer
// token the call must carry; for a plain llama-swap base it returns ("", ""),
// and the call keeps the client's own generation path with no credential.
//
// It exists because the two fleet nodes' llama-swap binds 127.0.0.1:11436 and
// nothing else (read from the port directory, 2026-09-15): a lane base of the
// form http://<node>:11436 cannot be reached from this box at all, and binding
// llama-swap to the tailnet would be a new unauthenticated listener. The vision
// lane (0.116.0, ADR 0040) already solved this shape — the node's own
// bearer-gated :18811 proxies to its loopback llama-swap — and C-41b gives the
// cascade the same door: POST /fleet/chat. Without this router a node-shaped
// lane is dead config, which is exactly how C-41's first half could pass every
// test and still never fire in production.
//
// Chainable and OPTIONAL: nil, or a client with no lanes installed, is
// unchanged. FleetLaneGates builds the production pair (residency + route)
// over ONE shared probe cache.
func (c *Client) WithLaneRoute(route func(base string) (path, token string)) *Client {
	if route == nil || c.remoteLanes == nil {
		return c
	}
	c.laneRoute = route
	return c
}

// endpointChoice is ONE request's whole target: where it goes, on what path,
// with what credential, over which HTTP client. It is a struct and not four
// return values because the four must never be resolved separately — a lane
// base paired with the default client would leave the tailnet dial guard, and
// a node base paired with /v1/chat/completions (or with no bearer) is a 404 or
// a 401 against a lane the caller was told was resident.
type endpointChoice struct {
	base   string
	path   string       // the request path; always set (c.path unless a node lane)
	token  string       // fleet bearer, node lanes only; "" = no Authorization header
	client *http.Client // guarded for every remote target, default for local
	// offBox (register C-41c) says the request loads nothing into THIS box's
	// VRAM — a cascade lane or a seat pinned to another node — so the send
	// admits through modelaffinity.AdmitOffBox and never waits on this box's
	// GPU lease. Measured 2026-09-15: the lane fired and the call then sat the
	// whole lease bound at home, and the node never saw it.
	offBox bool
}

// resolveEndpoint decides, ONCE per request, both the base URL and the HTTP
// client the request rides. One decision on purpose: the busy gate can flip
// between two separate lookups, and a lane base paired with the unguarded
// default client would put a remote request outside the dial-time tailnet
// guard. Resolution order:
//
//  1. seat_endpoints static override — wins unconditionally (an operator who
//     pinned a seat remote meant ALWAYS, not busy-hours).
//  2. a configured lane, only when the local box is busy for this model —
//     the GPU lease is held, or a swap to it would wait behind another
//     session's loaded seat (laneBusyFor) — AND the lane roster-serves the
//     model. Logged per rerouted call with the reason, because an invisible
//     reroute the operator cannot see in the serve log is a debugging trap.
//  3. the default base on the default client — including every failure of
//     the gates above (fail-closed to local).
func (c *Client) resolveEndpoint(model string) endpointChoice {
	if model == "" {
		model = c.model
	}
	if base, ok := c.remoteLanes[model]; ok {
		if _, pinned := c.seatEndpoints[model]; !pinned {
			if why := c.laneWhyBusy(model); why != "" && c.laneResident(base, model) {
				path, token, suffix := c.path, "", ""
				if c.laneRoute != nil {
					// A node lane speaks /fleet/chat with a bearer; a plain
					// llama-swap lane answers ("", "") and keeps c.path. The
					// node path is also printed, because "my call went to the
					// node" and "my call went to its llama-swap" fail
					// differently and the serve log is where that is read.
					if p, t := c.laneRoute(base); p != "" {
						path, token, suffix = p, t, p
					}
				}
				log.Printf("cascade remote lane: %s -> %s%s (%s)", model, base, suffix, why)
				return endpointChoice{base: base, path: path, token: token, client: c.safeHTTP, offBox: true}
			}
		}
	}
	// The static seat/default resolution, unchanged.
	// The static seat/default resolution, unchanged — except that a seat pinned to
	// another node (seat_endpoints) is off-box too (register C-41c).
	base := c.BaseFor(model)
	return endpointChoice{base: base, path: c.path, client: c.httpFor(model), offBox: base != c.base}
}

// laneWhyBusy asks both busy gates and returns the reason the lane fires, or
// "" when the local box can answer this model itself. The LEASE gate is asked
// first and costs no network (delegate.LocalBusy reads a lock file); the
// per-model gate is a bounded, cached probe of this box's llama-swap and is
// only paid when no lease is held. Either firing is enough: a held lease and
// a busy seat are two independent ways for a local call to sit in a queue.
func (c *Client) laneWhyBusy(model string) string {
	if c.laneBusy != nil && c.laneBusy() {
		return "local GPU lease held"
	}
	if c.laneBusyFor != nil {
		if busy, why := c.laneBusyFor(model); busy {
			if why == "" {
				why = "local seat busy"
			}
			return why
		}
	}
	return ""
}

// FleetChatPath is the fleet node's cascade door — fleetnode.ChatLanePath on
// the other side of the wire (their equality is pinned by a test in
// internal/fleetnode, which may import this package; the reverse would be an
// import cycle, which is why the constant is spelled twice rather than
// shared).
const FleetChatPath = "/fleet/chat"

// fleetHealthPath is the node's health route, the source of a node lane's
// residency answer.
const fleetHealthPath = "/fleet/health"

// fleetHealthCap bounds the health body this lane will read: the payload is a
// few KB of numbers and model names, and an endpoint that streams megabytes at
// a probe must not be able to hold up the cascade call waiting on it.
const fleetHealthCap = 1 << 20

// fleetHealthWire is the SLICE of GET /fleet/health a cascade lane reads. A
// tolerant decode on purpose (no DisallowUnknownFields): health grows fields
// every release, and a lane that refused to parse a newer node would fail
// closed for the wrong reason.
type fleetHealthWire struct {
	NodeID       string   `json:"node_id"`
	ChatLane     bool     `json:"chat_lane"`
	ServedModels []string `json:"served_models"`
}

// laneProbe is ONE cached reading of what a lane base IS and what it serves.
// Two shapes share it because a lane base is one of exactly two things and the
// operator must never have to declare which in config:
//
//   - a plain llama-swap (http://box:11436): residency is its /v1/models
//     roster, and the call rides the client's own generation path;
//   - a fleet NODE (http://node-b:18811): residency is health's
//     served_models, and the call rides POST /fleet/chat with the fleet
//     bearer, because the node's OWN llama-swap binds 127.0.0.1 and nothing on
//     this box can reach it directly (register C-41b).
type laneProbe struct {
	at     time.Time
	ok     bool // the probe itself succeeded
	node   bool // the base answered /fleet/health as a fleet node
	chat   bool // node lanes: the node advertises the cascade door
	served []string
	roster swapclient.Roster // swap lanes
}

// serves answers residency for either shape, alias-aware in both: a node's
// served_models is already ids PLUS every alias (fleetnode publishes
// swapclient.Roster.Names for exactly this reason), and a swap roster answers
// through Roster.Serves.
func (e laneProbe) serves(model string) bool {
	if !e.ok {
		return false
	}
	if !e.node {
		return e.roster.Serves(model)
	}
	if !e.chat {
		return false
	}
	for _, n := range e.served {
		if strings.EqualFold(strings.TrimSpace(n), model) {
			return true
		}
	}
	return false
}

// FleetLaneGates returns the production lane RESIDENCY gate and the lane
// ROUTER over ONE shared probe cache — a pair rather than two constructors
// because they must never disagree: a base declared resident as a node and
// then routed as a llama-swap would POST a chat completion at a route the node
// does not serve.
//
// What one probe decides, per base, once per laneResidencyTTL:
//
//  1. GET <base>/fleet/health. A fleet node answers it with its node_id, and
//     the lane then reads residency from served_models (ids AND aliases — the
//     alias-blind gate lesson) and routes through FleetChatPath with
//     fleetToken. A node that does NOT advertise chat_lane — an older build,
//     one whose own llama-swap endpoint is unset, or a tokenless listener past
//     loopback — is NOT resident: the door it would 403/404 is never used, the
//     calls stay local, and the reason is logged once per window.
//  2. Anything else: the base is a plain llama-swap and residency is its
//     roster, exactly as before node lanes existed (GET /v1/models through
//     FetchRosterGuarded).
//
// fleetToken is this box's fleet_auth_token. A node lane beyond loopback with
// NO token is treated as not resident rather than called: the node would
// answer 401 and the cascade tier would fail outright, where staying local
// merely queues. Fail-closed is the rule on both halves of this file.
//
// Every probe runs through the tailnet dial gate (netguard.SafeTransport /
// FetchRosterGuarded): a lane base is operator config, and only a per-dial
// check can prove the name still lands inside the tailnet.
func FleetLaneGates(fleetToken string) (resident func(base, model string) bool, route func(base string) (path, token string)) {
	var mu sync.Mutex
	cache := make(map[string]laneProbe)
	hc := &http.Client{Timeout: laneProbeTimeout, Transport: netguard.SafeTransport(nil)}
	// get runs at most one probe per base per TTL; the caller holds mu. The
	// probe runs inline under the lock for the same reason RosterResident's
	// always has: it is bounded by laneProbeTimeout, happens once per window,
	// and only ever on the busy path, where the alternative is queueing behind
	// a held card anyway.
	get := func(base string) laneProbe {
		if e, cached := cache[base]; cached && time.Since(e.at) <= laneResidencyTTL {
			return e
		}
		e := probeLane(base, hc, fleetToken)
		cache[base] = e
		return e
	}
	resident = func(base, model string) bool {
		mu.Lock()
		defer mu.Unlock()
		e := get(base)
		if e.node && !fleetTokenUsable(base, fleetToken) {
			return false
		}
		return e.serves(model)
	}
	route = func(base string) (string, string) {
		mu.Lock()
		defer mu.Unlock()
		e := get(base)
		if e.ok && e.node && e.chat && fleetTokenUsable(base, fleetToken) {
			return FleetChatPath, fleetToken
		}
		return "", ""
	}
	return resident, route
}

// RosterResident returns the residency gate ALONE, for a caller with no fleet
// token to offer: FleetLaneGates("")'s first half. A node lane past loopback
// is never resident through it (no credential to present); a llama-swap lane
// behaves exactly as it always has.
func RosterResident() func(base, model string) bool {
	resident, _ := FleetLaneGates("")
	return resident
}

// fleetTokenUsable reports whether a call to THIS node lane could carry a
// credential the node will accept: any token at all, or a loopback base (a
// node on this very box admits the lane tokenless, the same trust boundary the
// node itself applies — fleetnode.AgentLaneSafelyReachable).
func fleetTokenUsable(base, token string) bool {
	if strings.TrimSpace(token) != "" {
		return true
	}
	u, err := url.Parse(base)
	if err != nil {
		return false // unparseable: not provably loopback, so not usable
	}
	return netguard.LoopbackAddr(u.Host)
}

// probeLane runs the one reading behind both gates. Failures are never fatal
// and never silent: each resolves to a lane that is NOT resident (the calls
// stay local — always the safe answer) and logs once per TTL window per base,
// because a lane that never engages used to leave no evidence anywhere at all.
func probeLane(base string, hc *http.Client, fleetToken string) laneProbe {
	if h, ok := probeFleetHealth(base, hc); ok {
		switch {
		case !h.ChatLane:
			log.Printf("cascade remote lane: fleet node %s (%s) does not advertise chat_lane; the lane is treated as NOT resident for %s (calls stay local) — the node needs a bound endpoint and, past loopback, a fleet_auth_token", base, h.NodeID, laneResidencyTTL)
		case !fleetTokenUsable(base, fleetToken):
			log.Printf("cascade remote lane: fleet node %s (%s) advertises chat_lane but this box has no fleet_auth_token; the lane is treated as NOT resident for %s (calls stay local) — the node would answer 401", base, h.NodeID, laneResidencyTTL)
		}
		return laneProbe{at: time.Now(), ok: true, node: true, chat: h.ChatLane, served: h.ServedModels}
	}
	// GUARDED fetch: a lane base is a REMOTE, operator-configured endpoint,
	// and netguard.TailnetURL admits a dotless MagicDNS name on shape alone
	// (it resolves nothing at config time). Only the per-dial check inside
	// FetchRosterGuarded can prove the name still lands inside the tailnet —
	// the plain FetchRoster used here before rode the default transport, proxy
	// and all.
	ctx, cancel := context.WithTimeout(context.Background(), laneProbeTimeout)
	roster, err := swapclient.FetchRosterGuarded(ctx, base, laneProbeTimeout)
	cancel()
	if err != nil {
		// A TAKEN lane logs a line (resolveEndpoint above); a lane that never
		// engages because its probe fails logged nothing at all, so "my cascade
		// calls never left the busy box" had no evidence anywhere. Once per TTL
		// window per base — the probe itself runs at most that often, so this
		// cannot become per-call spam.
		log.Printf("cascade remote lane: roster probe of %s failed; lane treated as NOT resident for %s (calls stay local): %v", base, laneResidencyTTL, err)
	}
	return laneProbe{at: time.Now(), ok: err == nil, roster: roster}
}

// probeFleetHealth asks whether base is a FLEET NODE. ok is false for every
// other answer — a plain llama-swap 404s this route, a dead base refuses the
// dial — and the caller then reads the base as a llama-swap, so a deployment
// with no node lanes behaves exactly as it did before. A node is recognized by
// a node_id in a 200 body, not by the status alone: something else answering
// 200 at that path must not be mistaken for one.
func probeFleetHealth(base string, hc *http.Client) (fleetHealthWire, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), laneProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+fleetHealthPath, nil)
	if err != nil {
		return fleetHealthWire{}, false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fleetHealthWire{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fleetHealthWire{}, false
	}
	var h fleetHealthWire
	if err := json.NewDecoder(io.LimitReader(resp.Body, fleetHealthCap)).Decode(&h); err != nil {
		return fleetHealthWire{}, false
	}
	if strings.TrimSpace(h.NodeID) == "" {
		return fleetHealthWire{}, false
	}
	return h, true
}

// laneBusyTTL is how long ONE reading of this box's llama-swap backs the
// per-model busy gate. Shorter than laneResidencyTTL on purpose: residency is
// a property of a remote roster that changes when an operator edits a config,
// while "is the local seat working right now" changes with every request the
// other session finishes, and a stale yes keeps sending work off a box that
// is free again.
const laneBusyTTL = 5 * time.Second

// laneBusyProbeTimeout bounds the whole local reading (/running plus one
// in-flight read per loaded model). The same 3 s the gpu-status seat read
// uses: this is a loopback probe on the call's critical path, so it must fail
// fast and fail toward local rather than delay the very call it advises.
const laneBusyProbeTimeout = 3 * time.Second

// swapOccupant is one model holding this box's cards right now.
type swapOccupant struct {
	id       string
	starting bool // llama-swap says `starting`/`stopping`: the swap is under way
	inflight int  // requests the seat holds (seatload's gauge), ready seats only
}

// LocalSwapBusy returns the production per-model busy gate: "would a swap to
// `model` have to WAIT?" — the C-41 case the lease gate cannot see.
//
// The measured symptom had no GPU lease at all. Another session's contract
// held `qwen3.8-27b-vllm` through llama-swap; the `interactive` set is
// mutually exclusive, so every cascade tier needed a swap, and llama-swap
// swaps only once the loaded model's in-flight requests finish (300-900 s
// contracts). The cascade call queued there until its own HTTP deadline and
// the tier deferred — with a fleet node idle and serving the same GGUF.
//
// Busy, for a given model, means: this box's llama-swap lists ANOTHER model
// (alias-aware: the requested name is resolved through the roster, because
// /running carries canonical ids while the harness binds aliases) either in
// state `starting`/`stopping` — a swap is already under way — or `ready` with
// the seat gauge reporting at least one request in flight. A loaded but IDLE
// model is not busy: llama-swap swaps it out at once. The requested model
// being the loaded one is never busy: no swap is needed at all.
//
// One reading is cached for laneBusyTTL, so a burst of cascade calls costs
// one probe per window, not one each. Every failure — an unreadable
// /running, an unreadable roster (without it an alias-bound seat reads as
// "another model" and the call would leave the box wrongly), an unreadable
// seat gauge — resolves to NOT busy and is logged once per window: exactly
// RosterResident's fail-closed discipline, from the other side. "Could not
// tell" must never move a call off this machine.
func LocalSwapBusy(endpoint string) func(model string) (bool, string) {
	type snapshot struct {
		at        time.Time
		roster    swapclient.Roster
		occupants []swapOccupant
		ok        bool
	}
	var mu sync.Mutex
	var snap snapshot
	probeClient := &http.Client{Timeout: laneBusyProbeTimeout}
	return func(model string) (bool, string) {
		if strings.TrimSpace(endpoint) == "" || strings.TrimSpace(model) == "" {
			return false, ""
		}
		mu.Lock()
		defer mu.Unlock()
		if snap.at.IsZero() || time.Since(snap.at) > laneBusyTTL {
			roster, occ, ok := probeLocalSwap(endpoint, probeClient)
			snap = snapshot{at: time.Now(), roster: roster, occupants: occ, ok: ok}
		}
		if !snap.ok {
			return false, ""
		}
		names := []string{model}
		if id, ok := snap.roster.Canonical(model); ok && !strings.EqualFold(id, model) {
			names = append(names, id)
		}
		for _, o := range snap.occupants {
			for _, n := range names {
				if strings.EqualFold(o.id, n) {
					return false, "" // the requested model IS the loaded one: no swap, no wait
				}
			}
		}
		for _, o := range snap.occupants {
			switch {
			case o.starting:
				return true, fmt.Sprintf("local seat busy: %s starting", o.id)
			case o.inflight > 0:
				return true, fmt.Sprintf("local seat busy: %s, %d in flight", o.id, o.inflight)
			}
		}
		return false, ""
	}
}

// probeLocalSwap reads this box's llama-swap once: the roster (for alias
// resolution), /running (what holds the cards), and — for every loaded model
// past its load — the in-flight gauge through seatload.Inflight (read at each
// seat's own /running `proxy`, never through /upstream, so this probe does not
// reset any seat's idle unload timer), the same reader `gpu status` prints "N request(s) in flight" from and the drain waits
// on. ok=false means the reading could not be completed and the caller must
// treat the box as free.
func probeLocalSwap(endpoint string, client *http.Client) (swapclient.Roster, []swapOccupant, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), laneBusyProbeTimeout)
	defer cancel()
	// The llama-swap ROOT, once: config `endpoint` may carry the trailing /v1
	// (swapclient.BaseURL is the one rule for that), and seatload reads
	// /running off the root without normalizing itself.
	endpoint = swapclient.BaseURL(endpoint)
	sc, err := swapclient.New(endpoint, laneBusyProbeTimeout)
	if err != nil {
		log.Printf("cascade remote lane: the local llama-swap at %s could not be addressed; the local seat is treated as NOT busy for %s (calls stay local): %v", endpoint, laneBusyTTL, err)
		return swapclient.Roster{}, nil, false
	}
	roster, rerr := swapclient.FetchRoster(ctx, endpoint, laneBusyProbeTimeout)
	if rerr != nil {
		// Without the roster the requested model cannot be resolved to the id
		// /running lists it under, so an alias-bound seat serving THIS VERY
		// model would read as "another model" and the call would leave a box
		// that was about to answer it. Fail closed.
		log.Printf("cascade remote lane: the roster of the local llama-swap at %s could not be read; the local seat is treated as NOT busy for %s (calls stay local): %v", endpoint, laneBusyTTL, rerr)
		return swapclient.Roster{}, nil, false
	}
	rows, err := sc.Running(ctx)
	if err != nil {
		log.Printf("cascade remote lane: /running of the local llama-swap at %s could not be read; the local seat is treated as NOT busy for %s (calls stay local): %v", endpoint, laneBusyTTL, err)
		return swapclient.Roster{}, nil, false
	}
	occ := make([]swapOccupant, 0, len(rows))
	for _, r := range rows {
		switch strings.ToLower(strings.TrimSpace(r.State)) {
		case "stopped", "shutdown":
			continue
		case "starting", "stopping":
			occ = append(occ, swapOccupant{id: r.ID, starting: true})
			continue
		}
		rd, ierr := seatload.Inflight(ctx, client, endpoint, r.ID)
		if ierr != nil {
			log.Printf("cascade remote lane: the in-flight gauge of %s on the local llama-swap at %s could not be read; the local seat is treated as NOT busy for %s (calls stay local): %v", r.ID, endpoint, laneBusyTTL, ierr)
			return swapclient.Roster{}, nil, false
		}
		occ = append(occ, swapOccupant{id: r.ID, starting: rd.Starting, inflight: rd.Inflight})
	}
	return roster, occ, true
}
