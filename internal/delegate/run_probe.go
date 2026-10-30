// run_probe.go - Fleet probing: memoised node views, dead-node memory and no-eligible-remote reasons.

package delegate

import (
	"context"
	"fmt"
	"log"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// probeSnapshot is ONE fleet probe, memoised on the runner (= on the Run) so
// the sibling subtasks of a fan-out share it instead of each paying their own.
type probeSnapshot struct {
	at        time.Time
	views     []NodeView
	bases     []string
	probeErrs []string
	// failed is probeErrs keyed by the base it belongs to. probeErrs is a flat
	// list because that is what every placement reason renders; a caller that
	// has to ACCUMULATE failures across calls (the capacity wait) needs to know
	// which base each one came from, and matching on substrings would be a
	// guess.
	failed map[string]string
}

// deadProbe is one base's last TRANSPORT failure — the negative cache's entry.
type deadProbe struct {
	at  time.Time
	why string
}

// fetchViewsMemoTTL is how long one fleet probe is reused by the rest of a Run;
// probeNegativeTTL is how long a base that failed at the transport is skipped
// rather than re-dialled. Vars so tests can compress them, exactly like
// pollEvery and placementPollInterval; production never mutates them.
//
// 2 s is chosen SHORTER than the SHORTEST GAP BETWEEN TWO TICKS on purpose: the
// capacity wait's whole job is to notice a node that just freed, so the memo
// must never be able to serve one of its ticks.
//
// The comparison is not against placementPollInterval itself. The tick is
// jittered, so the shortest gap is (1 - jitterFrac) x placementPollInterval =
// 2.4 s, and a memo of 2.5 s would read as "safely under the 3 s interval"
// while quietly answering ticks from cache. TestProbeMemoCannotServeACapacity-
// WaitTick pins the real relation over the PRODUCTION values, because every
// capacity-wait test zeroes the memo in order to compress the tick and so none
// of them would ever notice.
//
// 30 s for the negative cache is the other side of the same trade: long enough
// that a fan-out stops paying fetchNodeViewTimeout per subtask for a box that is
// down, short enough that a node rebooting inside one long Run is picked up
// again while the Run still has budget to use it.
var (
	fetchViewsMemoTTL = 2 * time.Second
	probeNegativeTTL  = 30 * time.Second
)

func (r *runner) fetchViews(ctx context.Context) (views []NodeView, bases []string, probeErrs []string) {
	views, bases, probeErrs, _ = r.fetchViewsDetailed(ctx)
	return views, bases, probeErrs
}

// fetchViewsDetailed is fetchViews plus the failures keyed by BASE, for the one
// caller that accumulates them across calls rather than rendering them once.
func (r *runner) fetchViewsDetailed(ctx context.Context) ([]NodeView, []string, []string, map[string]string) {
	return r.fetchViewsDetailedSince(ctx, time.Time{})
}

// fetchViewsSince is fetchViews for a caller that has just learned its picture of
// the fleet was wrong - a re-placement after a refusal: the memo serves it only
// when the snapshot's probe began strictly AFTER notBefore. A snapshot taken after the
// refusal is shared; one that predates it is the stale picture and is never reused, and
// so is one whose probe began at the very reading of notBefore: on a clock that ticks
// coarsely (Windows: a millisecond or so) a probe that ended before the refusal reads
// the same as the refusal when both fall in one tick, and which came first is unknown.
// Siblings that refuse together are not deduped by this: each one's refusal is newer
// than the probe another began, so each reads the fleet once.
func (r *runner) fetchViewsSince(ctx context.Context, notBefore time.Time) ([]NodeView, []string, []string) {
	views, bases, probeErrs, _ := r.fetchViewsDetailedSince(ctx, notBefore)
	return views, bases, probeErrs
}

func (r *runner) fetchViewsDetailedSince(ctx context.Context, notBefore time.Time) ([]NodeView, []string, []string, map[string]string) {
	if s := r.memoisedProbe(notBefore); s != nil {
		v, b, e := s.copyOut()
		return v, b, e, maps.Clone(s.failed)
	}
	s := &probeSnapshot{at: time.Now()}
	s.views, s.bases, s.probeErrs, s.failed = r.probeRemotes(ctx)
	r.probeMu.Lock()
	r.probeMemo = s
	r.probeMu.Unlock()
	v, b, e := s.copyOut()
	return v, b, e, maps.Clone(s.failed)
}

// copyOut hands every caller its OWN slices. The snapshot is shared by the
// run's subtask goroutines, and callers index views/bases and build derived
// lists from them (untried, Place) — a shared backing array is how one
// subtask's filtering would silently rewrite a sibling's roster.
func (s *probeSnapshot) copyOut() ([]NodeView, []string, []string) {
	return append([]NodeView(nil), s.views...),
		append([]string(nil), s.bases...),
		append([]string(nil), s.probeErrs...)
}

// memoisedProbe returns the run's snapshot while it is inside the memo window,
// else nil. A non-positive TTL switches the memo off — what a test compressing
// the capacity wait's clocks does, because at a 20 ms tick a 2 s memo would
// swallow every re-read the wait exists to make.
func (r *runner) memoisedProbe(notBefore time.Time) *probeSnapshot {
	if fetchViewsMemoTTL <= 0 {
		return nil
	}
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	if r.probeMemo == nil || time.Since(r.probeMemo.at) >= fetchViewsMemoTTL {
		return nil
	}
	if !notBefore.IsZero() && !r.probeMemo.at.After(notBefore) {
		return nil
	}
	return r.probeMemo
}

// recentlyDead answers the negative cache: a base that failed at the transport
// inside probeNegativeTTL is skipped, and the reason it failed is REPLAYED so
// the placement note still names the node and says what happened to it. A
// skipped base that vanished from probeErrs would read as a node nobody ever
// configured — the exact ambiguity fetchViews' error-carrying exists to remove.
func (r *runner) recentlyDead(base string) (string, bool) {
	if probeNegativeTTL <= 0 {
		return "", false
	}
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	d, ok := r.probeDead[base]
	if !ok {
		return "", false
	}
	elapsed := time.Since(d.at)
	if elapsed >= probeNegativeTTL {
		return "", false
	}
	// Elapsed and remaining, not the configured TTL: an operator reading this
	// wants to know how stale the verdict is and when the base is dialled
	// again. Printing the fixed window said neither.
	return fmt.Sprintf("%s (cached %s ago, re-dial in %s)",
		d.why, elapsed.Round(time.Millisecond), (probeNegativeTTL - elapsed).Round(time.Millisecond)), true
}

// noteDeadProbe records a TRANSPORT failure against the base.
//
// Two guards, and both are load-bearing. Only probeUnreachable errors are
// cached: a 503 or a 401 is a node that ANSWERED, and skipping it for 30 s would
// turn a momentary refusal into half a minute of ineligibility. And nothing is
// cached once the CALLER's context is done — the capacity wait's per-tick bound
// fails every base at the same instant, and that is a fact about the tick, never
// about any of the nodes.
func (r *runner) noteDeadProbe(ctx context.Context, base string, err error) {
	if ctx.Err() != nil || !probeUnreachable(err) {
		return
	}
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	if r.probeDead == nil {
		r.probeDead = map[string]deadProbe{}
	}
	r.probeDead[base] = deadProbe{at: time.Now(), why: err.Error()}
}

// probeRemotes is the fan-out itself: every configured remote is probed
// CONCURRENTLY, each bounded by fetchNodeViewTimeout, and the answers are
// reassembled in the CONFIGURED order so views/bases/probeErrs keep the
// positional contract fetchViews has always published. Completion order is not
// an ordering any caller can use — a NodeView carries no base.
func (r *runner) probeRemotes(ctx context.Context) ([]NodeView, []string, []string, map[string]string) {
	type outcome struct {
		view NodeView
		ok   bool
		err  string
	}
	out := make([]outcome, len(r.remotes))
	var wg sync.WaitGroup
	for i, base := range r.remotes {
		if r.quarantine.Blocked(base) {
			// Two fingerprint failures inside the TTL: the node is producing
			// answers about the wrong document. Not a candidate until it expires.
			out[i].err = fmt.Sprintf("%s: quarantined until %s (repeated off-document answers)", base, r.quarantine.Until(base).Format(time.Kitchen))
			continue
		}
		if why, dead := r.recentlyDead(base); dead {
			out[i].err = why
			continue
		}
		wg.Add(1)
		go func(i int, base string) {
			defer wg.Done()
			// The per-base bound is the whole point of the fan-out: ONE
			// unreachable remote costs fetchNodeViewTimeout, never N of them in
			// series. A shorter caller ctx (the capacity wait's per-tick bound,
			// a re-placement's remaining budget) still wins.
			cctx, cancel := context.WithTimeout(ctx, fetchNodeViewTimeout)
			defer cancel()
			v, err := FetchNodeView(cctx, base, r.cfg.FleetAuthToken)
			if err != nil {
				// Logged once per BASE per RUN, not once per (base, subtask):
				// fetchViews is called inside runOne, so an 8-subtask fan-out at
				// two dead remotes printed sixteen identical lines. The error
				// still rides probeErrs on every subtask — the defer REASON must
				// name the cause for each result even when the log line is spent.
				if _, warned := r.probeWarned.LoadOrStore(base, struct{}{}); !warned {
					log.Printf("delegate: health probe of %s failed (not a placement candidate): %v", base, err)
				}
				out[i].err = err.Error()
				r.noteDeadProbe(ctx, base, err)
				return
			}
			// SF-40: the class is the operator's declaration about this base, never the node's own claim.
			v.PublicOnly = r.cfg.RemoteIsPublic(base)
			out[i].view, out[i].ok = v, true
		}(i, base)
	}
	wg.Wait()
	views := make([]NodeView, 0, len(out))
	bases := make([]string, 0, len(out))
	var probeErrs []string
	failed := map[string]string{}
	for i, o := range out {
		switch {
		case o.ok:
			views = append(views, o.view)
			bases = append(bases, r.remotes[i])
		case o.err != "":
			probeErrs = append(probeErrs, o.err)
			failed[r.remotes[i]] = o.err
		}
	}
	return views, bases, probeErrs, failed
}

func (r *runner) noEligibleRemote(st Subtask, views []NodeView, probeErrs []string) (reason, class string) {
	if len(r.remotes) == 0 {
		return "no remote fleet nodes are configured (pass --remote / the remotes argument)", core.DeferClassConfig
	}
	// SF-40: nodes the data-class rule keeps this contract off are named first, by the rule. They are taken out
	// of the roster the sentences below are written about: left in, a public node would count as a lane that
	// "fits", and a fleet whose only answering node is public would fall through to the "placement and gate
	// disagree" line, which blames this package for the operator's own declaration.
	if held, rest := publicHeld(st, views); len(held) > 0 {
		ruled := publicHeldSentence(st, held)
		if len(rest) == 0 && len(probeErrs) == 0 {
			// Nothing else answered and nothing failed: the rule is the whole story. The class is the caller's
			// contract (it cannot be placed on any node this box may send it to, however healthy the fleet), not a
			// broken stack: the fix is a local route or a private-ok node, not a box.
			return ruled, core.DeferClassContract
		}
		restWhy, restClass := r.noEligibleRemoteAmong(st, rest, probeErrs)
		return ruled + "; the other remote(s) could not take it either: " + restWhy, restClass
	}
	return r.noEligibleRemoteAmong(st, views, probeErrs)
}

// noEligibleRemoteAmong is noEligibleRemote's body: the cause of "nothing eligible" over the roster it is given.
func (r *runner) noEligibleRemoteAmong(st Subtask, views []NodeView, probeErrs []string) (reason, class string) {
	lanes, advertisedTooSmall, roomiest, unadvertised := laneStats(st, views)
	contractWhy := contractIneligible(st, lanes, advertisedTooSmall, roomiest, len(unadvertised))
	nodeWhy, nodeClass := nodeSideVerdict(lanes, unadvertised, views, probeErrs)
	if nodeClass == "" {
		// A remote that answered with a fitting lane but holds a TEXT GPU lease
		// is ineligible by design (0.113.16), not by a bug: name the holder so the
		// caller can wait or route elsewhere. Before 0.113.17 this fell through to
		// the "placement and gate disagree — please report" line (seen live on the
		// first lease, 2026-09-06 17:22).
		if leased := leasedLanes(views); len(leased) > 0 {
			why := fmt.Sprintf("%d remote(s) hold a text GPU lease (their cards are reserved for a measurement): %s", len(leased), strings.Join(leased, "; "))
			if contractWhy != "" {
				why += "; and the contract could not be placed on the others as written: " + contractWhy
			}
			return why, core.DeferClassInfrastructure
		}
		// Composite nodes (ADR 0052): for a remote-shaped contract (schema,
		// origin depth) the table's own verdict per layered node is the
		// reason — a window no layer holds (contract), a guard the node's
		// rows refused or a busy pair the long seat would evict (capacity) —
		// because the single-lane ceiling sentence would name agent_ctx_tokens,
		// which on a composite box is one layer's window, not the box's. The
		// single-lane sentence rides along for the remotes that have no rows.
		// A plain fleet has no layered node and keeps every sentence it had.
		if len(st.Contract.OutputSchema) > 0 && st.Contract.Depth == 0 {
			if why, class := layerVerdicts(st, views); why != "" {
				if contractWhy != "" && anySingleLane(views) {
					why += "; on the remote(s) without layers: " + contractWhy
				}
				return why, class
			}
		}
		// The ONE positively-established quiet case: everything answered, the
		// lane is offered and sized, so the contract is the whole story.
		if contractWhy != "" {
			return contractWhy, core.DeferClassContract
		}
		// A contract that NAMES a layer (register A-100) that no answering remote declares: the name
		// is the story. A remote that publishes no layer rows declares none, and layerVerdicts above
		// speaks only for the ones that publish rows, so without this the line below would report a
		// disagreement between the gate and the placement - a bug in this package - where the cause
		// is the layer the caller chose.
		if layer := st.Contract.Layer; layer != "" && !anyDeclaresLayer(views, layer) {
			return fmt.Sprintf("layer %s requested and no answering remote declares it (%d answered with an agent lane; a node that publishes no layers declares none)", layer, lanes), core.DeferClassContract
		}
		// Defensive: every remote answered, at least one advertises a lane this
		// contract fits, and Place still found nothing. That is a bug in this
		// package, not a caller mistake — say so loudly rather than inventing a
		// contract-side excuse for it.
		return fmt.Sprintf("no remote passed the capability gate although %d answered with an agent lane this contract fits (delegate: placement and gate disagree — please report)", lanes), core.DeferClassConfig
	}
	if contractWhy != "" {
		// Both are true. Name both, class the LOUD one: the box is what needs a
		// human, and a rewritten contract would still have nowhere to go.
		return nodeWhy + "; the contract could not be placed as written either: " + contractWhy, nodeClass
	}
	return nodeWhy, nodeClass
}

// layerVerdicts names, per composite node, why the placement table would not
// place st on its advertised layers, and the class the refusals add up to:
// contract when every layered node deferred as a contract problem (no window
// holds it), capacity when any of them refused on a guard or would have to
// wait for a busy seat. "" when no answering node advertises layers or every
// layered node was eligible.
func layerVerdicts(st Subtask, views []NodeView) (reason, class string) {
	var parts []string
	class = core.DeferClassContract
	for _, v := range views {
		if !v.AgentEnabled || len(v.Layers) == 0 {
			continue
		}
		dec, _ := remoteDecision(st, v)
		switch {
		case dec.Defer:
			parts = append(parts, laneID(v)+": "+dec.Reason)
			if dec.DeferClass != core.DeferClassContract {
				class = core.DeferClassCapacity
			}
		case dec.Wait:
			parts = append(parts, laneID(v)+": "+dec.Reason)
			class = core.DeferClassCapacity
		}
	}
	if len(parts) == 0 {
		return "", ""
	}
	return fmt.Sprintf("no composite remote can place the contract on its layers — %s", strings.Join(parts, "; ")), class
}

// anySingleLane reports whether some agent-lane remote advertises no layer
// rows — the single-lane ceiling sentence is only spoken about those.
func anySingleLane(views []NodeView) bool {
	for _, v := range views {
		if v.AgentEnabled && v.AgentResident && len(v.Layers) == 0 {
			return true
		}
	}
	return false
}

// nodeSideVerdict reports what the NODE SIDE contributes to "nothing eligible".
// An EMPTY class is the one positive statement it can make: every configured
// remote answered, at least one offers the agent lane, and EVERY such lane
// advertises a real context ceiling — the node side is demonstrably fine, which
// is the only state in which a quiet contract-side class is honest.
func nodeSideVerdict(lanes int, unadvertised []string, views []NodeView, probeErrs []string) (reason, class string) {
	switch {
	case len(views) == 0 && len(probeErrs) > 0:
		return fmt.Sprintf("all %d configured remote(s) failed the health probe: %s", len(probeErrs), strings.Join(probeErrs, "; ")), core.DeferClassInfrastructure
	case len(probeErrs) > 0:
		return fmt.Sprintf("no remote passed the capability gate, and %d other remote(s) failed the health probe: %s",
			len(probeErrs), strings.Join(probeErrs, "; ")), core.DeferClassInfrastructure
	case lanes == 0:
		return "no remote passed the capability gate (none is both agent-enabled and roster-resident)", core.DeferClassConfig
	case len(unadvertised) > 0:
		// agent_ctx_tokens is omitempty on the wire and config documents 0 as
		// "not advertised — set it when opting a node in": an OPERATOR fix on a
		// box. A ceiling NOBODY advertised cannot make the caller's contract too
		// big, but an unset value counted into lanes and then into the too-small
		// tally, so a 30-token goal came back as "needs ~3102 tokens, the roomiest
		// remote advertises 0" — quiet, contract-classed, exit 0.
		//
		// WHO ACTUALLY PRODUCES A SILENT LANE (R6): a node running the agent lane
		// with agent_ctx_tokens unset. fleetnode.AgentLaneAdmissible gates the lane
		// on fleet_agent_enabled + a resolvable planner seat + a safely reachable
		// listener — never on a ceiling — and health advertises whatever is
		// configured, 0 included. It is NOT a peer predating the lane: such a peer
		// sends no agent_enabled either, decodes as AgentEnabled:false, and is
		// filtered out of `lanes` before this branch can see it. Getting that
		// wrong matters here because it is the whole justification for the class:
		// the reachable state is a fleet where one operator set the field and
		// another did not, and the fix is on the box that did not.
		//
		// PER-LANE, not fleet-wide (R5): this fires on ANY silent lane, not only
		// on a fleet where every lane is silent. The predecessor keyed off
		// `roomiest == 0`, a fleet-wide MAX, so one node with a real ceiling
		// supplied one for every peer that had published none, and that mixed
		// fleet fell straight through to the quiet class. UNKNOWN is not SMALL: a
		// silent node may be a 128k box, and the fix for it is on that box, so the
		// class is loud and the reason names it. The ctx-fit sentence still rides
		// along when every ADVERTISED lane is too small (contractIneligible) —
		// both causes are true and the operator is owed both.
		return fmt.Sprintf("%d of %d agent-enabled remote(s) advertise no context ceiling (agent_ctx_tokens is unset or 0 on %s — an operator sets it on the node), and an unadvertised ceiling can never satisfy the placement gate",
			len(unadvertised), lanes, strings.Join(unadvertised, ", ")), core.DeferClassConfig
	}
	return "", ""
}

// laneStats summarizes the ANSWERING remotes' agent lane for this contract: how
// many offer it at all (agent-enabled AND roster-resident), how many of the ones
// that ADVERTISED a ceiling are too small for it, the roomiest ceiling any of
// them advertises, and the ids of the ones that advertise NO ceiling at all.
//
// advertisedTooSmall deliberately excludes the silent lanes rather than counting
// them (`est+reserve > 0` is trivially true, so they used to inflate it): UNKNOWN
// is not SMALL, and the only honest ceiling arithmetic is over numbers the nodes
// themselves sent. It is what lets the ctx-fit sentence be spoken about the
// advertised half of a mixed fleet without claiming anything about the other.
//
// unadvertised is a list rather than a count because it is the operator's
// worklist: "some node did not publish agent_ctx_tokens" is unactionable across
// a fleet, "node-b2 did not" is one ssh away.
func laneStats(st Subtask, views []NodeView) (lanes, advertisedTooSmall, roomiest int, unadvertised []string) {
	for _, v := range views {
		if !v.AgentEnabled || !v.AgentResident {
			continue
		}
		lanes++
		if v.AgentCtxTokens == 0 {
			unadvertised = append(unadvertised, laneID(v))
			continue
		}
		if v.AgentCtxTokens > roomiest {
			roomiest = v.AgentCtxTokens
		}
		if st.EstTokens+specReserve > v.AgentCtxTokens {
			advertisedTooSmall++
		}
	}
	return lanes, advertisedTooSmall, roomiest, unadvertised
}

// leasedLanes names the agent-lane remotes whose health advertised a held TEXT
// lease (NodeView.LeasedText): eligible by every other measure, reserved by
// choice. The lease holder's details live on the node (its /fleet/health
// "lease"), so the name is what a caller needs to go and look.
func leasedLanes(views []NodeView) []string {
	var out []string
	for _, v := range views {
		switch {
		case !v.AgentEnabled || !v.AgentResident:
		case v.LeasedText:
			out = append(out, laneID(v)+" (text lease held)")
		case v.LeaseBusy:
			// 0.113.27: a long lease of any class refuses too, and an
			// operator reading "why did nothing land there" needs the same
			// sentence for it as for a text lease.
			out = append(out, laneID(v)+" (long GPU lease held)")
		case v.LeaseOverdue:
			// GPU routing P1: the node is ranked last, not excluded, so it is not
			// why a placement failed; it is named so an operator who sees work
			// land on the worse node knows the lease behind the order.
			out = append(out, laneID(v)+" (GPU lease overdue, ranked last)")
		}
	}
	return out
}

// laneID names a node for an operator-facing message. A node that answered
// health without a node_id gets a shape that reads as MISSING rather than a
// fabricated name — the delegator reports what a node said, never more.
func laneID(v NodeView) string {
	if v.NodeID == "" {
		return "(a remote that reported no node_id)"
	}
	return v.NodeID
}

// contractIneligible names the CALLER'S CONTRACT property that makes every
// remote ineligible, or "" when none does. It reports WHICH property; whether
// the contract is the CLASS is noEligibleRemote's decision, and only ever when
// the node side is positively established as fine.
//
// Three of remoteEligible's five conditions are properties of the contract, not
// of any node: a missing OutputSchema (legal per core.AgentContract.Validate,
// and legal for a LOCAL run), a Depth past the origin hop, and a token estimate
// no advertised ceiling can hold. The test that separates them from a node
// problem is WHO CAN FIX IT: these three are fixed by rewriting the contract,
// without touching a box.
func contractIneligible(st Subtask, lanes, advertisedTooSmall, roomiest, unadvertised int) string {
	if len(st.Contract.OutputSchema) == 0 {
		return "the contract carries no output_schema, which REMOTE placement requires — the delegator must hold a mechanical check before it merges a weak node's output (a schemaless contract is still legal locally)"
	}
	if st.Contract.Depth != 0 {
		return fmt.Sprintf("the contract is already at depth %d; only an ORIGIN contract (depth 0) may travel — hop limit 1", st.Contract.Depth)
	}
	// Ctx-fit needs a real number to be too big for, so it is spoken only over the
	// lanes that ADVERTISED one, and every one of those must be too small. Silent
	// lanes are excluded from both sides of that test (laneStats keeps them out of
	// advertisedTooSmall): their ceiling is UNKNOWN, and no verdict may be built
	// on an unknown.
	//
	// Whether this makes the CLASS `contract` is still noEligibleRemote's call and
	// still requires a positively-fine node side — with a silent lane present the
	// class stays LOUD and this sentence rides along as the second cause. That
	// composition is the R6 fix: the predecessor keyed the whole sentence off
	// `unadvertised == 0`, so on a mixed fleet it was SUPPRESSED entirely and the
	// operator was told to set agent_ctx_tokens on one box, fixed it, and only
	// then discovered on the next run that the contract does not fit the other
	// box's advertised ceiling either. Two true causes, one reported.
	//
	// The WORDING carries the scope, because the suppression was not gratuitous:
	// "the roomiest agent-enabled remote advertises 4096" is a fleet-wide MAX
	// claim, and over a fleet with a silent lane it implies that lane is SMALLER —
	// authoring a ceiling for a node that published none (docs: unknown, not
	// small; it may be a 128k box), the same defect class as the invented 404
	// denial a previous round removed. "every remote that DID advertise a ceiling
	// tops out at N" says exactly what was measured and nothing about the rest.
	advertised := lanes - unadvertised
	if advertised > 0 && advertisedTooSmall == advertised {
		ceiling := fmt.Sprintf("the roomiest agent-enabled remote advertises %d", roomiest)
		if unadvertised > 0 {
			ceiling = fmt.Sprintf("every remote that DID advertise a ceiling tops out at %d", roomiest)
		}
		return fmt.Sprintf("the contract needs ~%d context tokens (%d estimated + %d reserved for the loop) and %s",
			st.EstTokens+specReserve, st.EstTokens, specReserve, ceiling)
	}
	return ""
}
