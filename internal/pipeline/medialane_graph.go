package pipeline

// MediaLaneFree for a whole-node run_graph (P0 plan, S4; ADR 0082 amendment), and the place such a graph keeps in the line while it
// is a ticket (leaveGraphPlace; P0 plan S5, ADR 0083 amendment).
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
// mediaremote refuses to carry devices), so the question is not asked for it, and neither is it for a process under a lease it
// inherited (the lane is its parent's): both read as free and judged, and the call takes the path it always took. A lease
// directory that cannot be opened and a card table that does not read in laneProbeBound are the lane in question and not read:
// free, with the reason, and Unjudged, exactly as for an image (resolveLane), so a ticket queue does not take them for a free
// lane (the card table is slowest to read exactly when a heavy render holds the card): the graph becomes a ticket whose first
// scan looks at the fleet. TestMediaLaneFreeGraphIsFreeWhenTheCardTableCannotBeRead holds it.

import (
	"context"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// graphLaneFree answers the lane question for a run_graph request.
func (p *Pipeline) graphLaneFree(ctx context.Context, req core.Request) core.LaneVerdict {
	// notJudged answers "free, and here is why this box did not look". unread is true only for a READ that failed: there the lane
	// is in question and the box could not look at it, which the router must not take for a free lane (core.LaneVerdict.Unjudged).
	notJudged := func(why string, unread bool) core.LaneVerdict {
		return core.LaneVerdict{Free: true, Why: "not judged: " + why, Unjudged: unread}
	}
	if devs := declaredDevices(req.Params); len(devs) > 0 {
		return notJudged("the graph names devices ("+strings.Join(devs, ", ")+"), so it is card-scoped and stays on this machine", false)
	}
	token := paramStr(req.Params, "waiter_token")
	need := declaredNeed(nil, token).declaresRAM(classDefaultRAM(p.cfg))

	// A call that runs under a lease its parent holds has a lane chosen for it.
	if inherited, ierr := ambientLeaseEnv(); ierr != nil || inherited != nil {
		return notJudged("this process runs under a GPU lease it inherited, so the lane is its parent's", false)
	}
	m, err := p.scopedManager()
	if err != nil {
		return notJudged("the lease directory cannot be opened ("+err.Error()+"); the call reports it itself", true)
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
		return notJudged("the card table could not be read ("+cause+"); the call reads it itself", true)
	}
	// Every read below goes through a view that prunes nothing: the question must not change the state it asks about.
	r := &laneReading{p: p, m: m.ReadOnly(), cards: table, token: token}
	// What the call declares on its largest card: if the host refuses that, it refuses the call.
	r.askRAM = p.declaredRAM(ctx, need, table, nil)
	r.wholeNode()
	return r.verdict()
}

// leaveGraphPlace keeps a whole-node graph's place in this box's line (core.PlaceKeeper; ADR 0083 Amendment 1). A graph ticket
// waits for a lane anywhere in the cluster, and the place is what keeps it senior to every caller that arrives after it. It is the
// whole-node token the real admission leaves for the graph when it waits its window (wholeNodeBusy: no card named, the run-graph
// reason), made at the ticket's arrival time; a graph that already holds a place brings it along, and the final local run spends
// it through the waiter_token path every resumed call takes (acquireMediaLease). Nothing here admits anything. A graph that names
// devices is card-scoped, stays here and is never a ticket, so it keeps no place; a host that does not lease cards, and a process
// under a lease it inherited, keep none either (core.ErrNoMediaPlace).
func (p *Pipeline) leaveGraphPlace(req core.Request, since time.Time) (core.MediaPlace, error) {
	if devs := declaredDevices(req.Params); len(devs) > 0 {
		return core.MediaPlace{}, noPlace("the graph names devices (" + strings.Join(devs, ", ") + "), so it is card-scoped and stays on this machine")
	}
	if inherited, ierr := ambientLeaseEnv(); ierr != nil || inherited != nil {
		return core.MediaPlace{}, noPlace("this process runs under a GPU lease it inherited, so the lane is its parent's")
	}
	m, err := p.scopedManager()
	if err != nil {
		return core.MediaPlace{}, noPlace("the lease directory cannot be opened (" + err.Error() + ")")
	}
	if !m.CardScoped() {
		return core.MediaPlace{}, noPlace("this host does not lease cards, so it keeps no places in line")
	}
	resume := ""
	if tok, ok := m.ResumeToken(paramStr(req.Params, "waiter_token")); ok {
		// A caller that already holds a place brings it along: its id and the arrival it left with.
		resume, since = tok.ID, tok.Since()
	}
	if since.IsZero() {
		since = time.Now()
	}
	tok, err := m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "run-graph", Origin: "pipeline", ResumeToken: resume}, since)
	if err != nil {
		return core.MediaPlace{}, noPlace(err.Error())
	}
	return core.MediaPlace{Token: tok.ID, Since: tok.Since(), Devices: tok.Devices}, nil
}
