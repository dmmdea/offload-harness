// run_localseat.go - Local seat inspection: lease, fence, run cap and busy probing of this box.

package delegate

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/hostneed"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	// Aliased: this file's `placement` struct (a resolved "run it HERE") predates
	// the package and is used at every placement site; the package is the
	// composite tier's decision TABLE (ADR 0052), so the alias names what it is.
	placetable "github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/seatguard"
	"github.com/dmmdea/offload-harness/internal/seatload"
)

// localSlotAhead reports whether the local seat's run-cap line has a free slot
// ahead of a NEWCOMER - the question the C-60 FIFO (modelaffinity.AwaitSeatSlot)
// answers for every run that reaches the seat. A newcomer's own record is not in
// the registry yet, so every registered run on the seat is ahead of it (waiters
// included: the FIFO counts a waiter that arrived first), and a slot is free while
// fewer than fleet_max_concurrent_jobs are. An unreadable registry, an unset seat
// and an unlimited cap all read as a free slot: fail toward the behaviour every
// earlier version had, exactly as the busy probe does.
//
// Memoised for fetchViewsMemoTTL like the fleet probe, so a dozen subtasks waiting
// on the capacity wait read the registry once per tick between them.
func (r *runner) localSlotAhead() (free bool, note string) {
	if fetchViewsMemoTTL > 0 {
		r.localSlotMu.Lock()
		if !r.localSlotAt.IsZero() && time.Since(r.localSlotAt) < fetchViewsMemoTTL {
			free, note = r.localSlotFree, r.localSlotNote
			r.localSlotMu.Unlock()
			return free, note
		}
		r.localSlotMu.Unlock()
	}
	free, note = r.readLocalSlot()
	if fetchViewsMemoTTL > 0 {
		r.localSlotMu.Lock()
		r.localSlotAt, r.localSlotFree, r.localSlotNote = time.Now(), free, note
		r.localSlotMu.Unlock()
	}
	return free, note
}

// localLease is the machine-wide lease as it stands between THIS contract and a local seat
// (LeaseForContract): the leases on the cards of the seats the contract could run on, and
// nothing else. Every read of the local lease that asks "is the local seat reserved, fenced
// or busy for this contract" goes through it, so a lease on one card is not a reason to
// route, wait or defer work a free card can take.
func (r *runner) localLease(c core.AgentContract) gpulease.Info {
	return LeaseForContract(r.cfg, LocalLease(r.cfg.GPULockPath, r.cfg.StateDir), c)
}

// fencedLocal reports whether a lease this process does not hold FENCES every local seat c
// could run on, so that a local run would be turned away at the seat's own pre-check
// (pipeline/agenttask.go, S-26) after the delegator had spent an attempt, a ledger row and an
// intent record on it: the wasted local leg (GPU routing P7, register C-86). It is the
// pre-check's own question (ForeignFence, which exempts the holder's own child) asked of the
// contract's chain of seats (placement.LeasesAgainstContract), so the two cannot disagree: a
// lease that fences the seat of a card-2 render does not fence a contract whose card-0 seat is
// free.
//
// It answers true only when there is somewhere else to wait for: a delegator with no remotes
// has no other node to spare the leg for, and route=local is the caller's explicit choice and
// is never gated. In those cases the local run's own pre-check is the fast, honest answer.
func (r *runner) fencedLocal(c core.AgentContract) (info gpulease.Info, why string, fenced bool) {
	if r.route == "local" || len(r.remotes) == 0 {
		return gpulease.Info{}, "", false
	}
	return fencedLocalIn(r.cfg, LocalLease(r.cfg.GPULockPath, r.cfg.StateDir), c)
}

// fencedLocalIn is fencedLocal over a lease reading the caller already holds (the spread run's
// ONE snapshot), without the route and roster conditions.
func fencedLocalIn(cfg config.Config, info gpulease.Info, c core.AgentContract) (gpulease.Info, string, bool) {
	held := placetable.LeasesAgainstContract(cfg, info, c, func(l gpulease.Info) bool {
		fenced, _ := ForeignFence(l)
		return fenced
	})
	if !held.Held {
		return gpulease.Info{}, "", false
	}
	_, why := ForeignFence(held)
	return held, why, true
}

// fencedLocalSpread is fencedLocal over the spread run's ONE lease snapshot.
func (r *runner) fencedLocalSpread(c core.AgentContract) (gpulease.Info, string, bool) {
	if r.route == "local" || len(r.remotes) == 0 {
		return gpulease.Info{}, "", false
	}
	return fencedLocalIn(r.cfg, r.spreadLease, c)
}

// spreadLeaseFor narrows the spread run's ONE lease read (r.spreadLease: every subtask deals
// against one snapshot) to a contract's own local seats.
func (r *runner) spreadLeaseFor(c core.AgentContract) gpulease.Info {
	return LeaseForContract(r.cfg, r.spreadLease, c)
}

// anyLeaseHeld is the deal's busy reading of the lease: true when ANY subtask has no local
// seat free of every lease. One contract that cannot run locally is enough to consider the
// fleet; a free seat for the others is not lost, because each subtask's own placement reads
// its seats again.
func (r *runner) anyLeaseHeld(info gpulease.Info, subtasks []core.AgentContract) bool {
	if !info.Held {
		return false
	}
	for _, c := range subtasks {
		if LeaseForContract(r.cfg, info, c).Held {
			return true
		}
	}
	return len(subtasks) == 0 // nothing to narrow by: the whole-node reading
}

// localStillBusy is the deal's own reading of the local seat, taken again: the
// capacity wait of a subtask a DEAL kept off the seat because it read busy
// (PlacedResult.overflow) may take the seat only once the deal would have. route=spread
// reads busy on any request in flight (agent_spread_local_slot: always never reads
// busy); route=auto on a held lease, an in-flight count at the fleet's own cap, a
// load in progress (W-01), or a seat a load cannot take now: another vLLM seat it would evict (occupiedBy) or a host short of the
// memory for the cold load (hostShort). An UNREADABLE seat keeps the deal's busy answer: the deal
// fails open because it has no evidence, but here it read the seat busy, and a probe
// that fails afterwards (a box too loaded to answer in time) is no evidence of idle.
// Failing open here ran the overflow on the busy seat (register C-88, CI run
// 36937589311); a seat that never reads again leaves the subtask to the remotes or
// to the wait's own end.
func (r *runner) localStillBusy(ctx context.Context, lease gpulease.Info) (busy bool, note string) {
	rd := r.busyReadingNow(ctx)
	if r.route == "spread" {
		return (rd.busy || rd.unknown) && r.cfg.SpreadLocalSlot() != config.SpreadLocalAlways, rd.note
	}
	return lease.Held || r.atRunCap(rd.inflight) || rd.loading || rd.cannotLoad() || rd.unknown, rd.note
}

// atRunCap reports whether `inflight` requests on the local seat have reached the fleet's own
// concurrency cap (fleet_max_concurrent_jobs): the cap above which route=auto reads the seat as busy.
// An UNLIMITED cap (a negative setting, which FleetConcurrencyLimit resolves to 0) is never reached.
// The comparison used to be written out at four sites and guarded at one: `inflight >= 0` is always
// true, so with fleet_max_concurrent_jobs < 0 an IDLE local seat read as permanently busy and the work
// left the box although the setting says unlimited (the diagnosis' F11). One helper, so the four
// cannot drift again.
func (r *runner) atRunCap(inflight int) bool {
	limit := r.cfg.FleetConcurrencyLimit()
	return limit > 0 && inflight >= limit
}

// readAutoLocalSlot is route=auto's one joint reading of the local seat for the whole run (W-01): it
// records the reading (autoDealBusy) and whether the seat's LOAD read busy (autoDealReadBusy), logs the
// one line per run, and returns the deal's busy flag, which also counts a lease. It is a method, and not
// a block in RunWith, so the reading can be pinned without a live run (unlimited_cap_test.go).
func (r *runner) readAutoLocalSlot(ctx context.Context, leaseInfo gpulease.Info, subtasks []core.AgentContract) (busy bool) {
	localBusy := r.probeLocalBusy(ctx)
	// cannotLoad: another vLLM seat holds the cards and loading this one would evict it (occupiedBy), or the host cannot take the
	// seat's cold load now (hostShort).
	busy = r.anyLeaseHeld(leaseInfo, subtasks) || r.atRunCap(localBusy.inflight) || localBusy.loading || localBusy.cannotLoad()
	// A live driver-fault repeat on the seat's cards (ADR 0096): the deal sends the work to a remote first. Not part
	// of autoDealReadBusy (the seat's LOAD), so the capacity wait can still give an overflow subtask the seat.
	if faulted, why := r.localFaulted(subtasks); faulted {
		busy = true
		localBusy.note += "; " + why
	}
	r.autoDealBusy = localBusy
	// The seat's LOAD only: a lease is the wait's own business (Reserved, fencedLocal),
	// and a lease this process holds must not keep its own subtasks off the seat.
	r.autoDealReadBusy.Store(r.atRunCap(localBusy.inflight) || localBusy.loading || localBusy.cannotLoad())
	// One line per run, mirroring route=spread's own local-slot log
	// (review round 1 item 4): before this the identical W-01 read had
	// no trace at all, so an operator could not tell "busy" from
	// "idle" without re-deriving it from the placement_reason.
	log.Printf("delegate: auto local slot: busy=%v inflight=%d loading=%v occupied_by=%q host_short=%v (%s)", busy, localBusy.inflight, localBusy.loading, localBusy.occupiedBy, localBusy.hostShort != "", localBusy.note)
	return busy
}

// busyReadingNow is probeLocalBusy memoised for fetchViewsMemoTTL, so a dozen
// waiting subtasks read the engine once per tick between them. A non-positive TTL
// switches the memo off, as it does for the fleet probe.
func (r *runner) busyReadingNow(ctx context.Context) busyReading {
	if fetchViewsMemoTTL > 0 {
		r.localBusyMu.Lock()
		if !r.localBusyAt.IsZero() && time.Since(r.localBusyAt) < fetchViewsMemoTTL {
			rd := r.localBusyRd
			r.localBusyMu.Unlock()
			return rd
		}
		r.localBusyMu.Unlock()
	}
	rd := r.probeLocalBusy(ctx)
	if fetchViewsMemoTTL > 0 {
		r.localBusyMu.Lock()
		r.localBusyAt, r.localBusyRd = time.Now(), rd
		r.localBusyMu.Unlock()
	}
	return rd
}

// readLocalSlot is localSlotAhead's reading, unmemoised: a slot is free while the seat's
// line has room (localRunCapRoom).
func (r *runner) readLocalSlot() (bool, string) {
	room, note := r.localRunCapRoom()
	return room > 0, note
}

// localRunCapRoom is how many MORE runs the local seat's run-cap line takes: the cap
// (fleet_max_concurrent_jobs, default 4) minus the runs registered on the seat, floored
// at 0, or unlimitedHeadroom when no cap is configured. Its fail-open is the real
// gate's: pipeline/agenttask.go skips the wait in line when the registry will not open,
// so an unreadable registry counts nothing ahead of a newcomer - but the reading says
// so in its note, and the process logs it once, because a wrong "free" is what silently
// re-creates the livelock this count exists to prevent.
//
// It reads the planner seat, the seat every run on a plain box takes (agenttask.go
// resolves it first, and only a composite decision moves a run to a layer seat).
func (r *runner) localRunCapRoom() (room int, note string) {
	limit := r.cfg.FleetConcurrencyLimit()
	if limit <= 0 {
		return unlimitedHeadroom, ""
	}
	ahead, note := r.localRunLine()
	return max(limit-ahead, 0), note
}

// localRunLine is how many runs are registered on the local seat's run-cap line right now (running and
// waiting), with the note localRunCapRoom publishes. An unset seat or an unreadable registry counts none.
func (r *runner) localRunLine() (ahead int, note string) {
	limit := r.cfg.FleetConcurrencyLimit()
	seat := strings.TrimSpace(r.cfg.AgentPlannerModel(""))
	if seat == "" {
		return 0, "no agent seat configured, so no run could be counted"
	}
	reg, err := gpuactivity.Open(r.cfg.GPULockPath, r.cfg.StateDir)
	if err != nil {
		r.localSlotWarn.Do(func() {
			log.Printf("delegate: the local run registry could not be opened (%v); the local seat's run-cap line is read as empty", err)
		})
		return 0, fmt.Sprintf("run registry unreadable (%v), read as empty", err)
	}
	// A seat pinned to ONE card shares it with every other seat on it, so its line is the
	// card's (plan P5): the same reading the node's own gate makes (OnSeatPinned). A seat that
	// spans cards, or whose pin is unknown, keeps the per-seat line.
	pins, _ := r.cfg.ModelPins(seat)
	runs := reg.OnSeatPinned(time.Now(), pins, seat)
	ahead = len(runs)
	where := "on seat " + seat
	if len(pins) == 1 {
		where = "on card " + pins[0] + " (seat " + seat + ")"
	}
	return ahead, fmt.Sprintf("%d run(s) registered %s, cap %d", ahead, where, limit)
}

// busyReading is what the deal knows about the local seat's load at deal time
// (0.113.20). note says how the reading was obtained, or why there is none.
type busyReading struct {
	busy     bool
	inflight int
	note     string
	// loading is true when a load is IN PROGRESS (probeLocalBusy's Starting
	// branch): the in-flight count is unknown, not zero, and W-01's auto-busy
	// rule reads it as its own signal — a seat mid-load is not idle, whatever
	// inflight (0, by construction) says.
	loading bool
	// unknown is true when the seat's load could not be read (the probe failed,
	// or the read was ambiguous): busy is false only because the deal fails open.
	// The capacity wait must not read that as "now idle" (register C-88).
	unknown bool
	// occupiedBy names the declared vLLM seat that holds this box's cards while
	// the agent seat is NOT loaded, when loading the agent seat would make
	// llama-swap unload it (seatguard's verdict for the agent seat). busy is
	// true with it: the seat has nothing in flight, but taking the local slot
	// costs the occupant a cold load and its prefix cache. The reference box's
	// three-card seat is opencode's model, so a harness contract dealt to an idle
	// agent seat there evicted the operator's session (2026-10-06).
	occupiedBy string
	// hostShort is the sentence for a host that cannot take the agent seat's cold load right now: the seat is NOT loaded, nothing
	// occupies it, and the host-RAM gate the node would put the contract to (pipeline.SeatColdLoadAdmission) refuses, or a lane
	// that declared host RAM is live. busy is true with it, as with occupiedBy: nothing is in flight, but taking the seat now would
	// be refused at the door after a look, so the subtask keeps its place in line and the capacity wait takes the seat the instant
	// the host admits. "" when the host can take it, when the reading could not be taken, and for every seat but this box's own
	// agent seat. The memory comes back: on the reference 3-card Windows box the host had 66.5 GiB available four minutes after a
	// contract was refused at 41.1 GiB for a 36.0 GiB seat with 8.0 GiB of headroom.
	hostShort string
}

// cannotLoad is true when the local seat cannot be loaded for a contract right now, for a reason that ends by itself: another
// vLLM seat holds the cards (occupiedBy) or the host is short of memory for the cold load (hostShort). It is the question every
// "may the local seat take this now" site asks; a site that only names the occupant keeps occupiedBy.
func (b busyReading) cannotLoad() bool { return b.occupiedBy != "" || b.hostShort != "" }

// why renders the reading for a placement reason: the seat a load would evict
// when another vLLM seat holds the cards, the host's shortage when it cannot take the cold load, else the in-flight count.
func (b busyReading) why() string {
	if b.occupiedBy != "" {
		return "local seat occupied: loading it would evict the loaded vLLM seat " + b.occupiedBy
	}
	if b.hostShort != "" {
		return "local seat not loaded, and the host cannot take its cold load now: " + b.hostShort
	}
	return fmt.Sprintf("local seat busy: %d in flight", b.inflight)
}

// probeLocalBusy reads the local agent seat's in-flight count through
// llama-swap (seatload.Inflight). Any failure deals as idle: the busy rule is
// an optimisation of the deal, never a gate, so "could not read" must fall
// toward the behaviour every earlier version had.
func (r *runner) probeLocalBusy(ctx context.Context) busyReading {
	if r.localBusyProbe != nil {
		return r.localBusyProbe(ctx)
	}
	seat := strings.TrimSpace(r.cfg.AgentPlannerModel(""))
	if seat == "" {
		// Nothing to probe and nothing to deal: a box with no agent seat is never given a slot
		// (localServesLayer), so "idle" would misreport it.
		return busyReading{note: noLocalSeat}
	}
	rd := probeSeatBusy(ctx, r.cfg, seat, "agent seat", r.checkSeatGuard)
	if rd.hostShort != "" && !r.hasLine() {
		// This run has no line to wait in, so the seat's door is the line (hostRAMWait): the seat is dealt as it reads without
		// the host, and the door waits for the memory. Read as busy here, the subtask would go to a capacity wait that ends
		// in two minutes, be shed, or have its retry skipped, and never reach the wait the door holds for exactly this caller.
		rd.hostShort, rd.busy, rd.note = "", false, "local seat not loaded"
	}
	return rd
}

// probeSeatBusy is the reading behind every "is this local seat busy" question the harness asks of a seat: the
// delegator's own (probeLocalBusy, for the agent seat) and the lane doors' (LocalSeatBusy, for the seat a single call
// runs on, ADR 0078). One function, so the two cannot disagree about what busy is. `what` names the seat's role in the
// log and the note ("agent seat", "vision seat"); guard is the seat guard's verdict for a model.
func probeSeatBusy(ctx context.Context, cfg config.Config, seat, what string, guard func(context.Context, string) seatguard.Verdict) busyReading {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		log.Printf("delegate: local seat busy probe skipped (endpoint %q, %s %q); dealing the local slot as idle", endpoint, what, seat)
		return busyReading{note: "no local endpoint or " + what + " configured"}
	}
	pctx, cancel := context.WithTimeout(ctx, localBusyProbeTimeout)
	defer cancel()
	rd, err := seatload.Inflight(pctx, localBusyClient, endpoint, seat)
	if err != nil {
		log.Printf("delegate: local seat busy probe of %s/%s failed; dealing the local slot as idle: %v", endpoint, seat, err)
		return busyReading{note: "busy probe failed: " + err.Error(), unknown: true}
	}
	if !rd.Loaded {
		// Not loaded is idle only while nothing else would be pushed off the cards to
		// load it. A loaded vLLM seat that the agent seat's load would unload (the
		// reference box's opencode-only three-card seat shares the agent seat's cards
		// and llama-swap set) makes the slot as unavailable as a busy one: the deal
		// prefers a remote with room, waits in line when every remote is full, and
		// falls back here only when no remote can take the contract at all. The guard
		// is asked before the ambiguity below: it resolves names from the serving
		// config, not the roster, so it still answers when the roster read failed (and
		// it answers "nothing evicted" when the agent seat is itself the one running).
		// A /running it cannot read names no seat and changes nothing (the deal fails
		// open, as the probe always has); a serving config it cannot read makes it
		// protect whatever vLLM seat is loaded, which only sends the work to the fleet.
		if v := guard(pctx, seat); v.Protect && v.Seat != "" {
			return busyReading{busy: true, occupiedBy: v.Seat, note: "local seat not loaded; " + v.Reason}
		}
		if rd.Ambiguous {
			// Degraded read: the roster could not resolve the seat and /running
			// holds other entries. Idle is the SAFE reading for a deal (the
			// worst case is the pre-0.113.20 stacking), but it must be visible.
			log.Printf("delegate: local seat busy probe of %s/%s is ambiguous (roster unreadable: %v; /running lists %d other model(s)); dealing the local slot as idle", endpoint, seat, rd.RosterErr, rd.RunningOthers)
			return busyReading{note: "ambiguous: roster unreadable", unknown: true}
		}
		rd := busyReading{note: "local seat not loaded"}
		if why := localHostShort(cfg, seat); why != "" {
			rd.busy, rd.hostShort = true, why
			rd.note += "; the host cannot take its cold load now"
		}
		return rd
	}
	if rd.Starting {
		// A load in progress: the request that triggered it is queued on the
		// engine, and the probe deliberately did not ask the upstream (it would
		// have blocked for the whole load — register D-92). Busy, with the count
		// unknown rather than zero.
		return busyReading{busy: true, loading: true, note: "local seat " + strings.TrimPrefix(rd.Source, "running-state:") + " (a load is in progress)"}
	}
	return busyReading{busy: rd.Inflight > 0, inflight: rd.Inflight, note: rd.Source}
}

// seatNeed is the host need of a cold load of the box's agent seat, as the seat's door reads it. hostneed.SeatNeed (the config's
// figures) until internal/pipeline hands in its own reader, which adds what the harness measured of the seat's loads.
var seatNeed = hostneed.SeatNeed

// SetSeatNeed replaces the reader localHostShort judges the host by. internal/pipeline calls it at init with the reader its
// door uses, so the delegator and the door ask the same question of the same figures. A nil reader is ignored.
func SetSeatNeed(read func(config.Config) hostneed.Need) {
	if read != nil {
		seatNeed = read
	}
}

// hostRAMWait says who is the line for the local seat's host-RAM gate (LocalOptions.HostRAMWait). A delegator that has a line
// answers 0: the door looks once and defers at once, and the capacity wait re-reads the host and the seat every tick and takes the
// seat the instant the host admits. The line exists when the subtask has another node to be re-placed on (isReplaceable's own
// condition: not route=local, remotes configured) and the capacity wait is there to hold it (agent_placement_wait_sec not
// switched off, and the run is not sheddable, which never waits). Without one, route=local pinned or a box with no remotes, the door
// itself is the line and waits for the host for as long as its admission budget lasts (HostRAMWaitAdmission).
func (r *runner) hostRAMWait() *time.Duration {
	w := time.Duration(0)
	if !r.hasLine() {
		w = HostRAMWaitAdmission
	}
	return &w
}

// hasLine says whether this run has a line a subtask can wait in for the local seat's host memory: another node it could be
// re-placed on (not route=local, remotes configured) and a capacity wait to hold it (agent_placement_wait_sec not switched off,
// and the run not sheddable, which never waits). The one answer both sides read: with a line the delegator reads the host
// before it deals the seat (probeLocalBusy) and the door looks once; without one the delegator does not read the host and the
// door waits (hostRAMWait).
func (r *runner) hasLine() bool {
	return r.route != "local" && len(r.remotes) > 0 && r.cfg.AgentPlacementWaitSec >= 0 && r.priority >= core.BandNormal
}

// localHostShort says why the host cannot take the cold load of seat right now, "" when it can or when that cannot be told. The
// node's own door (pipeline.SeatColdLoadAdmission) puts every contract that finds the agent seat unloaded to the host-RAM rule and
// defers it as capacity when the host is short, so the delegator asks the same rule BEFORE it deals the seat a subtask: a subtask
// dealt to a seat the host cannot load is a place in line (the capacity wait re-reads the host every tick and takes the seat the
// instant it admits), not a refused attempt. Only for this box's own agent seat, and only when the node states the seat's footprint
// (agent_seat_host_ram_gib, as the door does): a local reading says nothing about an endpoint on another box, and a seat the
// config never sized is not refused on an invented figure. Fail open, as probeSeatBusy always has: a lease directory that cannot
// be opened, a host that cannot be read and a need no host could ever meet (Impossible: waiting does not help, the door answers) read
// as "not short".
//
// The need is the one the door judges by (seatNeed): the harness's measured need once it holds cold loads of the seat, the
// config's figures as the seed. internal/pipeline imports this package, so it hands its reader in at init (SetSeatNeed) and a
// program that links no pipeline reads the config's figures. The two readings can still part for a moment (the host moved
// between the two looks, a load was recorded in between): the subtask then goes to the door, which answers with the host-RAM
// defer, and the capacity wait takes that as one more tick (placements.holdsLocalForHost).
func localHostShort(cfg config.Config, seat string) string {
	if !strings.EqualFold(strings.TrimSpace(seat), strings.TrimSpace(cfg.AgentPlannerModel(""))) {
		return ""
	}
	if modelaffinity.EndpointHost(cfg.Endpoint, cfg.FleetNodeID) != "" {
		return ""
	}
	if cfg.AgentPlacementWaitSec < 0 {
		// The operator switched the capacity wait off: a subtask kept off the seat would have nowhere to wait and would be
		// published at once, so the door is the place to wait (hostRAMWait says so) and the delegator does not read the host.
		return ""
	}
	need := seatNeed(cfg)
	if (need.Source != hostneed.SourceSeat && need.Source != hostneed.SourceMeasured) || need.GiB <= 0 {
		return ""
	}
	m, err := gpulease.OpenAt(cfg.GPULockPath, cfg.StateDir)
	if err != nil {
		return ""
	}
	// The door's first refusal, in its words: a lane that declared host RAM is live, and a seat is not loaded while a lane streams.
	for _, l := range m.Leases() {
		if l.DeclaresHostRAM() {
			return fmt.Sprintf("a lane that declared %.1f GiB of host RAM is live (%s lease epoch %d), and a seat is not loaded while a lane streams", max(l.HostRAMGiB, l.HostRAMCommitGiB), l.Class, l.Epoch)
		}
	}
	chk := m.HostRAMCheckLoad(need.HostLoad())
	if chk.OK || chk.Impossible || chk.Unreadable {
		return ""
	}
	return need.SeatRefusal(chk.Why)
}

// dealReadLocalBusy reports whether this run's placement read the local seat busy and
// dealt around it: spread's busy rule (skipsBusyLocal), or route=auto's busy formula at the
// deal or a per-subtask placement. The capacity wait keeps that reading for every subtask
// it holds, not only the deal's overflow (awaitCapacity's dealBusy).
func (r *runner) dealReadLocalBusy() bool {
	switch r.route {
	case "spread":
		return r.skipsBusyLocal()
	case "auto":
		return r.autoDealReadBusy.Load()
	}
	return false
}

// autoOccupant is route=auto's reading when it found the local seat unable to take a load now, occupied by another vLLM seat or
// short of host memory (cannotLoad): the joint deal's (autoDealBusy) or the per-subtask placement's (autoLocalBusy); zero otherwise.
func (r *runner) autoOccupant() busyReading {
	if r.autoDealBusy.cannotLoad() {
		return r.autoDealBusy
	}
	if r.autoLocalBusy.cannotLoad() {
		return r.autoLocalBusy
	}
	return busyReading{}
}

// checkSeatGuard asks the box's seat guard (internal/seatguard, the same reading
// the cascade uses to keep a rung from evicting a vLLM seat) whether loading model
// would unload a loaded vLLM seat. seatGuardCheck is the test seam; nil = the
// process-wide guard, which is nil (and every verdict zero) on a box that declares
// no vllm_seats, names no endpoint or turns cascade_seat_guard off.
func (r *runner) checkSeatGuard(ctx context.Context, model string) seatguard.Verdict {
	if r.seatGuardCheck != nil {
		return r.seatGuardCheck(ctx, model)
	}
	return seatguard.Shared(r.cfg).Check(ctx, model)
}

// skipsBusyLocal reports whether this run deals its local rotation slots away:
// the seat read busy at deal time AND the config did not pin the slot local.
func (r *runner) skipsBusyLocal() bool {
	return r.spreadLocalBusy.busy && r.cfg.SpreadLocalSlot() != config.SpreadLocalAlways
}

// localView is THE local seat's NodeView. One constructor because the deal and
// the retry's alternativeNode must describe the same seat — two literals here
// is how a node id drifts between a placement and its telemetry.
func (r *runner) localView() NodeView {
	return NodeView{NodeID: r.localNodeID(), AgentSeat: r.cfg.AgentPlannerModel(""), Local: true}
}

// localServesLayer reports whether the local seat may be handed c. A contract that names no layer is
// the local seat's like any other. One that names a layer is the local seat's only if this box DECLARES
// that layer (register A-108): a box that does not has nothing to run it on, and the seat can only defer
// it by name (runner.decide) while a node that declares the layer sits idle. Place applies the same rule
// for its own callers; the two deals do not call Place, so each of them, and every other place that
// chooses the local seat, asks here.
//
// A delegator states its layers in its own config, never in a health document, so localView carries no
// rows and declaresLayer alone would read every composite box as layerless. cfg is the production answer
// and the first test DecideOnLayer applies (an undeclared layer defers by name), so a box that fails it
// can only ever have deferred the contract. A view that does carry rows (Place's own convention, and what
// a white-box deal is handed) is honoured as well.
//
// A view that names no agent seat serves no contract (seatless): a delegation client names no model,
// so every contract it places goes to the fleet or waits in line for it, the same way a contract naming
// an undeclared layer does, instead of landing on a seat that can only defer it.
func (r *runner) localServesLayer(local NodeView, c core.AgentContract) bool {
	if seatless(local) {
		return false
	}
	if c.Layer == "" || declaresLayer(local, c.Layer) {
		return true
	}
	_, declared := r.cfg.Layer(c.Layer)
	return declared
}

// seatless reports whether the local view names no agent seat. localView fills it with the resolution
// the local run makes (agent_model, else model); a delegation client (`install client`) writes both
// empty, so its local run could only defer "no agent seat resolvable".
func seatless(local NodeView) bool {
	return strings.TrimSpace(local.AgentSeat) == ""
}

// localRefusal says why the local seat cannot take c, for a placement reason. It is called only where
// localServesLayer said no.
func localRefusal(local NodeView, c core.AgentContract) string {
	if seatless(local) {
		return noLocalSeat
	}
	return "this box declares no layer " + c.Layer
}

// noLocalSeat is the placement-reason phrase for a box with no agent seat.
const noLocalSeat = "this box has no agent seat (agent_model and model are both empty: a delegation client)"
