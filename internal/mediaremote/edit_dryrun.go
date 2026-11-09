package mediaremote

// DryRunEdit: where a generative edit WOULD go, answered without sending or rendering anything. It resolves the same things the real
// call resolves before it renders: whether this machine has the edit lane, this machine's edit identity (the digest a node must
// publish), and for every roster node the verdict of the strict recipe match. It reads each node's /fleet/health (the roster probe
// the real call makes) and this machine's weight-file fingerprints; it takes no lease, probes no card and queues nothing.

import (
	"context"
	"sort"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// EditNodeVerdict is one roster node's answer to the match.
type EditNodeVerdict struct {
	Node    string   `json:"node"`
	Matched bool     `json:"matched"`
	Family  string   `json:"family,omitempty"`
	State   string   `json:"state,omitempty"`
	Why     string   `json:"why,omitempty"`
	Differs []string `json:"differs,omitempty"`
}

// EditDryRun is the answer of DryRunEdit.
type EditDryRun struct {
	// Route is the route as normalised (local, auto or remote).
	Route string `json:"route"`
	// Lane says where the call would start: "local" (this machine has the edit lane and the route allows it), "fleet" (it goes to a
	// matching node), or "none" (neither is possible).
	Lane string `json:"lane"`
	// LocalLane is whether this machine's own edit route is configured (the files behind the binding are there).
	LocalLane bool `json:"local_lane_configured"`
	// Digest is this machine's edit recipe digest; IdentityWhy says why there is none.
	Digest      string `json:"digest,omitempty"`
	IdentityWhy string `json:"identity_why,omitempty"`
	// Matched names the node the call would be offered first (a matching node that is idle), "" when none.
	Matched string            `json:"matched_node,omitempty"`
	Nodes   []EditNodeVerdict `json:"nodes,omitempty"`
	Note    string            `json:"note,omitempty"`
}

// DryRunEdit resolves req (a core.TaskEditImageGenerative request) against this machine and the roster, sending nothing.
func DryRunEdit(ctx context.Context, cfg config.Config, req core.Request, route string, remotes []string) EditDryRun {
	r, ok := NormalizeRoute(route)
	if !ok {
		return EditDryRun{Route: route, Lane: "none", Note: "unrecognized route; use local, auto or remote"}
	}
	d := EditDryRun{Route: r, LocalLane: LocalConfigured(cfg, req)}
	if r == RouteAuto {
		cfg = effectiveCfg(cfg, "")
	}
	if _, err := plan(req); err != nil {
		d.Lane, d.Note = "none", "the request cannot be placed: "+err.Error()
		return d
	}
	local, okID, why := localEditIdentity(ctx, cfg, str(req.Params, "family"))
	if okID {
		d.Digest = local.recipe.Digest()
	} else {
		d.IdentityWhy = why
	}
	if r == RouteLocal {
		d.Lane = "none"
		if d.LocalLane {
			d.Lane = "local"
		}
		return d
	}
	bases, err := candidateBases(cfg, remotes)
	switch {
	case err != nil:
		d.Note = err.Error()
	case !okID:
		d.Note = "no fleet node can be matched without this machine's own edit identity"
	default:
		cands, rows := candidatesFor(ctx, cfg, bases, local, needOf(req))
		sort.SliceStable(rows, func(a, b int) bool { return rows[a].idx < rows[b].idx })
		for _, c := range cands {
			d.Nodes = append(d.Nodes, EditNodeVerdict{Node: c.shown, Matched: true, Family: c.family})
		}
		for _, row := range rows {
			d.Nodes = append(d.Nodes, EditNodeVerdict{Node: row.row.Node, Matched: row.matched, State: row.row.State, Why: row.row.Why, Differs: row.row.Differs})
		}
		if len(cands) > 0 {
			d.Matched = cands[0].shown
		}
	}
	switch {
	case r == RouteAuto && d.LocalLane:
		d.Lane = "local"
		if d.Matched != "" {
			d.Note = "this machine has the lane: a free lane runs the edit here, a busy one makes it a ticket that goes to a matching node"
		}
	case d.Matched != "":
		d.Lane = "fleet"
	default:
		d.Lane = "none"
	}
	return d
}
