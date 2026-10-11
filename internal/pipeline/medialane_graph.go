package pipeline

// MediaLaneFree for a whole-node run_graph (P0 plan, S4; ADR 0082 amendment).
//
// A graph the operator did not pin to cards holds the whole node: the caller's graph owns its placement and, in the default
// ComfyUI instance, sees every card (acquireMediaLease with declaredNeed(nil)). So the question is the whole-node one, read by
// the same helpers the image probe reads the whole-node plan with (laneReading.wholeNode: any lease, any in-process slot, any
// place in line, and the host's memory against what the call declares), and it keeps the image probe's contract: a RELAXATION
// of the real wait-0 admission that never calls busy a lane the grant would serve (TestMediaLaneFreeGraphNeverRefusesAGrant
// drives the real admission over a table of states) and creates and removes nothing (TestMediaLaneFreeGraphWritesNothing).
//
// What a graph declares for the host is the media class default: the lease is taken for the graph as a whole, before anything
// reads the weights it names, so the guard sizes it as the largest ComfyUI family the box binds (classDefaultRAM; a box that
// binds none declares nothing). The node's door does resolve the files a dispatched graph names (fleetnode/graph_files.go),
// but their sizes are not what the lease declares. So the declaration is an upper bound for the families the box binds and
// not the graph's own size: a graph that names weights bigger than every bound family can be under-counted. That is a named
// limit of this step (S0's G3 owns the numbers), not a measurement.
//
// A graph with operator-declared devices is a card-scoped graph the delegator never places on another node (request.go in
// mediaremote refuses to carry devices), so the question is not asked for it. An inherited lease and a card table that does not
// read in laneProbeBound read as free, as for an image: the call then takes the path it always took.

import (
	"context"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// graphLaneFree answers the lane question for a run_graph request.
func (p *Pipeline) graphLaneFree(ctx context.Context, req core.Request) core.LaneVerdict {
	notJudged := func(why string) core.LaneVerdict { return core.LaneVerdict{Free: true, Why: "not judged: " + why} }
	if devs := declaredDevices(req.Params); len(devs) > 0 {
		return notJudged("the graph names devices (" + strings.Join(devs, ", ") + "), so it is card-scoped and stays on this machine")
	}
	token := paramStr(req.Params, "waiter_token")
	need := declaredNeed(nil, token).declaresRAM(classDefaultRAM(p.cfg))

	// A call that runs under a lease its parent holds has a lane chosen for it.
	if inherited, ierr := ambientLeaseEnv(); ierr != nil || inherited != nil {
		return notJudged("this process runs under a GPU lease it inherited, so the lane is its parent's")
	}
	m, err := p.scopedManager()
	if err != nil {
		return notJudged("the lease directory cannot be opened (" + err.Error() + "); the call reports it itself")
	}
	// The card table, once, under a bound of its own: it sizes the host-RAM need (the declaration depends on the largest card).
	rctx, cancel := context.WithTimeout(ctx, laneProbeBound)
	table, _, terr := p.alloc.CardTable(rctx, p.cfg)
	cancel()
	if terr != nil || len(table) == 0 {
		cause := "it listed no card"
		if terr != nil {
			cause = terr.Error()
		}
		return notJudged("the card table could not be read (" + cause + "); the call reads it itself")
	}
	// Every read below goes through a view that prunes nothing: the question must not change the state it asks about.
	r := &laneReading{p: p, m: m.ReadOnly(), cards: table, token: token}
	// What the call declares on its largest card: if the host refuses that, it refuses the call.
	r.askRAM = p.declaredRAM(ctx, need, table, nil)
	r.wholeNode()
	return r.verdict()
}
