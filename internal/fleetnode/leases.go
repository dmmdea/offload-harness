package fleetnode

// Fleet per-card lease truth, the node's half (plan P7, register C-86).
//
// WHAT WAS WRONG. With card-scoped leases several leases are live at once, each on its own
// cards, and /fleet/health described only the lowest epoch in one singular block. A delegator
// could not tell which cards a long render held, so it read the whole node as spoken for: a
// render on one card of three made the node a non-target for work that would have run on the
// other two, and the node's own saturation reading said "a new dispatch is refused right now"
// while two cards sat idle.
//
// WHAT THIS ADDS, all additive and omitted when there is no lease:
//
//   - leases[]: one entry per live lease with the cards it sits on (the lower-cased GPU UUIDs a
//     lease records; none = the whole node), where those cards came from (declared, inferred,
//     whole-node), its own term end, busy, overdue, exclusive, draining, its own standing and
//     one verdict word. A delegator that reads it fences this node only for the contracts whose
//     seats sit on those cards.
//   - The singular lease block, lease_exclusive and lease_draining stay for every reader one
//     release behind (and every current reader that reads only the singular fields), and are
//     the WORST across the live leases: class text if any lease is text, busy, orphaned or
//     stalled if any lease is, exclusive or draining if any is, the longest remaining time.
//     Overdue folds the other way, because those readers compute busy-and-not-overdue: it is
//     set only when no live lease is a long hold of its own, so an abandoned lease on one card
//     cannot hide a live long render on another. A reader that sees only that block is
//     therefore never told less than is true; with one lease it is byte for byte what it
//     always was.
//   - saturation and dispatch follow the same cards: the node says it is closed only when the
//     leases that refuse new work cover every card it has, and a text reservation refuses a
//     contract only when it holds every seat that contract could run on (textLeaseAgainst).

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/placement"
)

// LeaseEntry is the wire shape of ONE live lease in /fleet/health leases[].
type LeaseEntry struct {
	Epoch uint64 `json:"epoch"`
	Class string `json:"class"`
	// Devices are the cards the lease sits on, as lease ids (lower-cased GPU UUIDs): the
	// record's declared cards, else the cards the evidence rule inferred for a legacy lease.
	// ABSENT means the whole node, and a reader must take absence as every card.
	Devices []string `json:"devices,omitempty"`
	// Scope says where Devices came from: declared, inferred or whole-node.
	Scope string `json:"scope"`
	// Until is the lease's declared term end (RFC3339); absent when it declares none.
	Until        string `json:"until,omitempty"`
	RemainingSec int    `json:"remaining_sec,omitempty"`
	Busy         bool   `json:"busy,omitempty"`
	Overdue      bool   `json:"overdue,omitempty"`
	Exclusive    bool   `json:"exclusive,omitempty"`
	Draining     bool   `json:"draining,omitempty"`
	// LeaseStanding is embedded so orphaned and stalled sit flat on the entry.
	LeaseStanding
	// Verdict is one word for what the lease is doing, in the precedence `gpu status` uses:
	// held-stalled, held-orphaned, held-overdue, else held. "held" is the absence of news and
	// does not claim the holder is working.
	Verdict string `json:"verdict"`
}

// refusesNewWork is the predicate the node has always applied to its one lease, now per lease:
// a text reservation, or a lease long enough (and not overdue) to be called busy.
func (e LeaseEntry) refusesNewWork() bool {
	return e.Class == string(gpulease.ClassText) || (e.Busy && !e.Overdue)
}

// leaseReading is what one inspection of the lease directory becomes on the wire.
type leaseReading struct {
	block               *LeaseHealth
	entries             []LeaseEntry
	exclusive, draining bool
}

// readLeases turns an inspection into the singular block and one entry per live lease.
// standing, when non-nil, is read ONCE per lease (it stamps the orphan marker, and health is
// polled every few seconds).
func readLeases(info gpulease.Info, now time.Time, cfgSec int, standing func(gpulease.Info) gpulease.Standing) leaseReading {
	// The singular block starts as it always was: the lowest epoch's reading.
	rd := leaseReading{block: leaseHealthOf(info, now, cfgSec)}
	all := info.Each()
	var latest time.Time
	for _, l := range all {
		h := leaseHealthOf(l, now, cfgSec)
		var st gpulease.Standing
		if standing != nil {
			st = standing(l)
		}
		e := LeaseEntry{
			Epoch: l.Epoch, Class: string(l.Class), Devices: leaseIDs(l.EffectiveDevices()),
			Scope: string(l.ScopeKind()), RemainingSec: h.RemainingSec,
			Busy: h.Busy, Overdue: h.Overdue || st.Overdue, Exclusive: l.Exclusive, Draining: l.Draining,
			LeaseStanding: LeaseStanding{Orphaned: st.Orphaned, Stalled: st.Stalled},
		}
		if !l.ExpiresAt.IsZero() && l.ExpiresAt.UnixMilli() > 0 {
			e.Until = h.Until
			if l.ExpiresAt.After(latest) {
				latest = l.ExpiresAt
			}
		}
		e.Verdict = "held"
		switch {
		case e.Stalled:
			e.Verdict = "held-stalled"
		case e.Orphaned:
			e.Verdict = "held-orphaned"
		case e.Overdue:
			e.Verdict = "held-overdue"
		}
		rd.entries = append(rd.entries, e)
		rd.exclusive = rd.exclusive || e.Exclusive
		rd.draining = rd.draining || e.Draining
	}
	// The standing the singular block has always carried is the OR across the leases. It is set
	// for one lease as well: the reading that goes with one lease is that lease's.
	for _, e := range rd.entries {
		rd.block.Orphaned = rd.block.Orphaned || e.Orphaned
		rd.block.Stalled = rd.block.Stalled || e.Stalled
	}
	if len(rd.entries) < 2 {
		return rd
	}
	// Several live leases: fold the block to the worst of them. A reader that sees only the
	// block (a delegator one release behind) would otherwise be told the lowest epoch's story,
	// which can be "a short media render" over an exclusive reservation on another card.
	//
	// Overdue folds the OTHER way from the rest. Every reader of the singular fields computes
	// LeaseBusy = Busy && !Overdue (an overdue lease is ranked last, never fenced), so a block
	// that said busy AND overdue because ANY lease is overdue would let an abandoned lease on
	// one card hide a live long render on another: the node would read free to every one of them.
	// The block is overdue only when no live lease is a long hold of its own (busy and not
	// overdue); beside one, it is busy and not overdue, which is what that lease alone says.
	anyOverdue, liveBusy := false, false
	for _, e := range rd.entries {
		if e.Class == string(gpulease.ClassText) {
			rd.block.Class = string(gpulease.ClassText)
		}
		rd.block.Busy = rd.block.Busy || e.Busy
		anyOverdue = anyOverdue || e.Overdue
		liveBusy = liveBusy || (e.Busy && !e.Overdue)
		if e.RemainingSec > rd.block.RemainingSec {
			rd.block.RemainingSec = e.RemainingSec
		}
	}
	rd.block.Overdue = anyOverdue && !liveBusy
	if !latest.IsZero() {
		rd.block.Until = latest.UTC().Format(time.RFC3339)
	}
	return rd
}

// leaseIDs lower-cases a list of card ids for the wire, dropping blanks; nil stays nil so an
// empty set is never published as if it named cards.
func leaseIDs(in []string) []string {
	var out []string
	for _, id := range in {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// closesNode reports whether the leases that refuse new work leave this node nothing to run
// on: one of them is whole-node, or together they hold every card the node has. A node that
// cannot enumerate its cards cannot show a card is left, so the doubt closes it.
func (r leaseReading) closesNode(devices []gpuprobe.Device) bool {
	held := map[string]bool{}
	any := false
	for _, e := range r.entries {
		if !e.refusesNewWork() {
			continue
		}
		any = true
		if len(e.Devices) == 0 {
			return true
		}
		for _, id := range e.Devices {
			held[id] = true
		}
	}
	if !any {
		return false
	}
	if len(devices) == 0 {
		return true
	}
	for _, d := range devices {
		if !held[strings.ToLower(strings.TrimSpace(d.UUID))] {
			return false
		}
	}
	return true
}

// textLeaseAgainst is the dispatch gate's reading of a TEXT reservation: the lease a new job
// of this task type would run under, and whether it refuses the job. A text lease reserves the
// cards it names for a measurement, so it refuses work that would load a model onto them.
//
// An agent contract is judged by the seats it could run on (placement.LeasesAgainstContract,
// the rule the delegator reads for the local box and for this node's rows): it is refused
// only when every seat in its chain sits on a card some text lease holds, because the
// placement table falls back to a seat whose cards are free. Every other task type, and an
// agent payload this node cannot read, keeps the whole-node reading: the cards it will use
// are chosen later, and unknown is every card. Every live text lease counts, not only the
// lowest epoch.
func (s *Server) textLeaseAgainst(taskType string, payload json.RawMessage) (gpulease.Info, bool) {
	if s.opts.Lease == nil {
		return gpulease.Info{}, false
	}
	info := s.opts.Lease()
	if !info.Held {
		return gpulease.Info{}, false
	}
	isText := func(l gpulease.Info) bool { return l.Held && l.Class == gpulease.ClassText }
	if taskType == string(core.TaskAgentRun) {
		var c core.AgentContract
		if json.Unmarshal(payload, &c) == nil {
			got := placement.LeasesAgainstContract(s.opts.Cfg, info, c, isText)
			return got, got.Held
		}
	}
	text := info.Where(isText)
	return text, text.Held
}
