package delegate

import (
	"context"
	"fmt"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// PlannedPlacement is where a call's joint deal WOULD put one subtask, read without dispatching it
// (agent_delegate lint_only, register D-109).
type PlannedPlacement struct {
	// Route is the route the deal would apply (a pin hint's route is already resolved to auto).
	Route string `json:"route"`
	// Where is one of: "local", "node" (a fleet node, named by Node), "capacity_wait" (every eligible node is
	// full or too far from starting the job, so a real call would wait for room), "no_eligible_node", or
	// "not_computed".
	Where string `json:"where"`
	Node  string `json:"node,omitempty"`
	// Reason is the deal's own sentence, or why nothing was computed.
	Reason string `json:"reason"`
}

// PlanPlacement is the placement the route=auto / route=remote deal would give each of contracts, computed from
// the same readings RunWith takes before it deals, and nothing else: the local lease file (read), the local seat's
// in-flight count (one GET), the local run registry (read), and each fleet node's health document (one GET each).
// It takes no lease, mints no job, writes no ledger or intent row, opens no queue and starts no seat; the job ids
// the deal mints are throwaway ranking seeds and are discarded.
//
// The answer is the DEAL's pick. A real call can still land elsewhere: a lease taken in between, a node that
// refuses, a retry. The reason says so where it matters. A route this does not model (local is trivial, spread and
// queue are not computed) answers Where "local" or "not_computed" with the reason, never a guess.
func PlanPlacement(ctx context.Context, cfg config.Config, contracts []core.AgentContract, route string, remotes []string, opts *RunOptions) []PlannedPlacement {
	out := make([]PlannedPlacement, len(contracts))
	notComputed := func(rt, why string) []PlannedPlacement {
		for i := range out {
			out[i] = PlannedPlacement{Route: rt, Where: "not_computed", Reason: why}
		}
		return out
	}
	if route == "" {
		route = "auto"
	}
	pin, route, err := resolvePin(route, opts)
	if err != nil {
		return notComputed(route, err.Error())
	}
	switch route {
	case "local":
		for i := range out {
			out[i] = PlannedPlacement{Route: route, Where: "local", Reason: "route=local runs on this box's seat; the fleet is never read"}
		}
		return out
	case "auto", "remote":
	default:
		return notComputed(route, "placement: not computed for route="+route+" without dispatching (only auto, remote and local are read ahead)")
	}
	if !cfg.AgentDelegationEnabled {
		return notComputed(route, "agent_delegation_enabled is false in this server's config")
	}
	if len(remotes) == 0 {
		remotes = cfg.DelegateRemotes
	}
	r := &runner{cfg: cfg, route: route, pin: pin, remotes: remotes}
	if opts != nil {
		r.quarantine = opts.Quarantine
		r.tenant = opts.Tenant
	}
	// The same sequence RunWith runs for route=auto/remote before any goroutine starts.
	leaseInfo := LocalLease(cfg.GPULockPath, cfg.StateDir)
	busy := route == "remote"
	room, roomNote := 0, ""
	if route == "auto" {
		busy = r.readAutoLocalSlot(ctx, leaseInfo, contracts)
		room, roomNote = r.localRunCapRoom()
		if r.remoteHint() {
			r.hintSeatFree = !busy
			busy = true
		}
	}
	localView := r.localView()
	readFleet := busy || len(contracts) > room
	for _, c := range contracts {
		if !r.localServesLayer(localView, c) {
			readFleet = true
			break
		}
	}
	var views []NodeView
	var bases []string
	var failed map[string]string
	if readFleet {
		views, bases, _, failed = r.fetchViewsDetailed(ctx)
	}
	deal := r.dealAutoRemoteRoom(contracts, localView, views, bases, busy, failed, room, roomNote)
	for i, slot := range deal {
		p := PlannedPlacement{Route: route, Reason: slot.reason}
		switch {
		case slot.capacityWait:
			p.Where = "capacity_wait"
		case slot.noRemote:
			p.Where = "no_eligible_node"
			if slot.reason == "" {
				p.Reason = "no node in the fleet snapshot can run this contract"
			}
		case slot.base == "":
			p.Where = "local"
			p.Node = slot.view.NodeID
		default:
			p.Where = "node"
			p.Node = slot.view.NodeID
		}
		if p.Where == "local" && leaseInfo.Held && LeaseForContract(cfg, leaseInfo, contracts[i]).Held {
			p.Reason += fmt.Sprintf("; a GPU lease is held on this box now (%s), so a real call waits for it or defers", HolderLine(leaseInfo))
		}
		out[i] = p
	}
	return out
}
