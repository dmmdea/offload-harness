// lanes.go — busy-aware cascade remote lanes (roast delta 7 of multi-node
// delegation): the DAILY lane's invisible upgrade. Where seat_endpoints
// (endpoints.go) is a STATIC pin — that seat is always remote —
// cascade_remote_lanes is a per-call failover: a cascade text call rides its
// configured lane only while the local machine-wide GPU lease is held AND the
// lane's llama-swap roster verifiably serves the model. Quality-first by
// construction: routing never changes WHICH model answers, only WHERE — the
// lane must serve the SAME model id/alias the call names, and any doubt
// (idle GPU, probe failure, roster miss) resolves to the local base. Lane
// requests ride the same netguard.SafeTransport dial gate as seat overrides,
// so the never-cloud boundary holds at dial time here too.
package llamaclient

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/netguard"
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

// WithRemoteLanes installs the busy-aware lane table and its two gates,
// returning the client (chainable at construction, like WithSeatEndpoints).
// lanes maps the exact model id or llama-swap alias a call names to the
// remote base URL that also serves it; busy reports whether the local
// machine-wide GPU lease is held (delegate.LocalBusy in production);
// resident reports whether base verifiably serves model (RosterResident in
// production — a seam so tests fake either gate).
//
// An empty/nil map — or a missing gate func, which could never fire safely —
// installs NOTHING: no lane table, no gates, no second HTTP client. A config
// without cascade_remote_lanes yields a client byte-identical to a pre-lanes
// build (pinned by test). Lane bases get the same tailnet-guarded client the
// seat overrides ride, built once and shared.
func (c *Client) WithRemoteLanes(lanes map[string]string, busy func() bool, resident func(base, model string) bool) *Client {
	if len(lanes) == 0 || busy == nil || resident == nil {
		return c
	}
	m := make(map[string]string, len(lanes))
	for model, base := range lanes {
		m[model] = strings.TrimRight(base, "/") // same normalization New applies to base
	}
	c.remoteLanes = m
	c.laneBusy = busy
	c.laneResident = resident
	if c.safeHTTP == nil {
		c.safeHTTP = &http.Client{Timeout: c.http.Timeout, Transport: netguard.SafeTransport(newTransport())}
	}
	return c
}

// resolveEndpoint decides, ONCE per request, both the base URL and the HTTP
// client the request rides. One decision on purpose: the busy gate can flip
// between two separate lookups, and a lane base paired with the unguarded
// default client would put a remote request outside the dial-time tailnet
// guard. Resolution order:
//
//  1. seat_endpoints static override — wins unconditionally (an operator who
//     pinned a seat remote meant ALWAYS, not busy-hours).
//  2. a configured lane, only when the local GPU lease is held AND the lane
//     roster-serves the model — logged per rerouted call, because an
//     invisible reroute the operator cannot see in the serve log is a
//     debugging trap.
//  3. the default base on the default client — including every failure of
//     the gates above (fail-closed to local).
func (c *Client) resolveEndpoint(model string) (string, *http.Client) {
	if model == "" {
		model = c.model
	}
	if base, ok := c.remoteLanes[model]; ok {
		if _, pinned := c.seatEndpoints[model]; !pinned && c.laneBusy() && c.laneResident(base, model) {
			log.Printf("cascade remote lane: %s -> %s (local GPU lease held)", model, base)
			return base, c.safeHTTP
		}
	}
	return c.BaseFor(model), c.httpFor(model) // the static seat/default resolution, unchanged
}

// RosterResident returns the production lane residency gate: "does the
// llama-swap behind base currently serve model?", answered alias-aware via
// swapclient.FetchRoster + Roster.Serves (the plannerUnserved idiom — an
// id-only match would declare a correctly-served alias missing).
//
// One roster answer is cached PER BASE for laneResidencyTTL under a mutex, so
// a burst of cascade calls during a render costs one probe per lane per
// window, and two models on one lane share one probe. A probe FAILURE is
// cached as not-resident for the same window — fail-closed (the calls stay
// local, always the safe answer) without re-probing a dead lane on every
// call. The probe runs inline under the lock: it is bounded by
// laneProbeTimeout, happens at most once per TTL per base, and only ever on
// the busy path, where the alternative is queueing behind a held GPU anyway.
func RosterResident() func(base, model string) bool {
	type entry struct {
		at     time.Time
		roster swapclient.Roster
		ok     bool // the probe itself succeeded
	}
	var mu sync.Mutex
	cache := make(map[string]entry)
	return func(base, model string) bool {
		mu.Lock()
		defer mu.Unlock()
		e, cached := cache[base]
		if !cached || time.Since(e.at) > laneResidencyTTL {
			// GUARDED fetch: a lane base is a REMOTE, operator-configured
			// endpoint, and netguard.TailnetURL admits a dotless MagicDNS name
			// on shape alone (it resolves nothing at config time). Only the
			// per-dial check inside FetchRosterGuarded can prove the name still
			// lands inside the tailnet — the plain FetchRoster used here before
			// rode the default transport, proxy and all.
			ctx, cancel := context.WithTimeout(context.Background(), laneProbeTimeout)
			roster, err := swapclient.FetchRosterGuarded(ctx, base, laneProbeTimeout)
			cancel()
			if err != nil {
				// A TAKEN lane logs a line (resolveEndpoint above); a lane that
				// never engages because its probe fails logged nothing at all,
				// so "my cascade calls never left the busy box" had no evidence
				// anywhere. Once per TTL window per base — the probe itself runs
				// at most that often, so this cannot become per-call spam.
				log.Printf("cascade remote lane: roster probe of %s failed; lane treated as NOT resident for %s (calls stay local): %v", base, laneResidencyTTL, err)
			}
			e = entry{at: time.Now(), roster: roster, ok: err == nil}
			cache[base] = e
		}
		return e.ok && e.roster.Serves(model)
	}
}
