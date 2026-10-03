package placement

// Which leases stand between a contract and its seats (plan P7, register C-86).
//
// A lease on card 2 does not stand between a contract and a seat on card 0, and the layer
// fallback (AgentChain) means a contract has more than one seat it can run on. So "is this
// contract held by a lease" is a question about the CHAIN: it is held only when every seat in
// the chain sits on a card some lease holds. Three readers ask it and must not disagree:
//
//   - the delegator, about the local box (delegate.LeaseForContract);
//   - the fleet node, at dispatch, about a text reservation (fleetnode);
//   - the delegator, about a remote node, from the layer rows that node publishes
//     (RemoteSeatCards), because it cannot read another box's leases or cards itself.
//
// The direction of every doubt is "held": a box with no layers, a long-context contract, a
// layer nobody declared and a pin the card table cannot place are all read as every card.

import (
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// LeasesAgainstContract narrows info to what stands between c and the local seats it could
// run on. pick, when non-nil, first limits the question to the leases it accepts (the ones
// that refuse new work, the ones that fence); nil asks about every live lease. The result is
// the zero Info as soon as one seat of the chain sits on no accepted lease (the placement
// table falls back to it), and the first seat's accepted leases (the holder a defer names)
// when every seat is held.
//
// Unchanged, whole-node: a box that declares no layers, a long-context contract (it runs on
// a long seat the agent chain does not describe), and a contract no agent seat's window can
// hold, or one that names a layer nobody declares. Nothing narrows on a guess.
func LeasesAgainstContract(cfg config.Config, info gpulease.Info, c core.AgentContract, pick func(gpulease.Info) bool) gpulease.Info {
	if pick != nil {
		info = info.Where(pick)
	}
	if !info.Held || !cfg.Composite() || c.ContextClass == core.ContextClassLong {
		return info
	}
	need := RequestForContract(c, EstimateTokens(c), cfg.AgentMaxTokens).Need()
	chain := AgentChain(cfg.Layers, c.Layer, need)
	if len(chain) == 0 {
		return info
	}
	var first gpulease.Info
	for n, seat := range chain {
		scoped := modelaffinity.ScopeToPins(info, seat.Seat.DeviceList())
		if n == 0 {
			first = scoped
		}
		if !scoped.Held {
			return scoped
		}
	}
	return first
}

// WithDeviceIDs stamps each seat row with the lease ids of the cards its pin names, resolved
// against cards (the card table of the box that owns the rows). A node publishes them so a
// delegator never has to guess which index space a bare pin is in. A pin the table cannot
// place, and a box with no card table, leave the row as it was: unknown is every card. The
// rows are modified in place and returned.
func WithDeviceIDs(rows []LayerRow, cards []gpuprobe.Card) []LayerRow {
	for i := range rows {
		for j := range rows[i].Seats {
			s := &rows[i].Seats[j]
			if ids, ok := gpulease.ResolvePins(pinsOf(s.Device), cards); ok {
				s.DeviceIDs = ids
			}
		}
	}
	return rows
}

// pinsOf splits a seat's pin ("0,2") into its entries.
func pinsOf(device string) []string {
	var out []string
	for _, p := range strings.Split(device, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// RemoteSeatCards is the chain of local agent seats a contract could run on, on a node that
// published rows, as the lease ids of each seat's cards: the node's own ids when it
// published them, else its pin resolved against the node's card table, else nil (the seat's
// cards are unknown, which is every card). One element per seat, in the order the table
// walks them. ok is false when there is no chain to read: a node with no rows, a
// long-context contract, a layer the node does not declare.
func RemoteSeatCards(rows []LayerRow, cards []gpuprobe.Card, c core.AgentContract, est int) (seats [][]string, ok bool) {
	layers, _ := FromRows(rows)
	if len(layers) == 0 || c.ContextClass == core.ContextClassLong {
		return nil, false
	}
	chain := AgentChain(layers, c.Layer, RequestForContract(c, est, 0).Need())
	if len(chain) == 0 {
		return nil, false
	}
	for _, link := range chain {
		var ids []string
		for _, r := range rows {
			if r.Name != link.Layer.Name {
				continue
			}
			for _, s := range r.Seats {
				if s.Role != link.Seat.Role {
					continue
				}
				if len(s.DeviceIDs) > 0 {
					ids = make([]string, 0, len(s.DeviceIDs))
					for _, id := range s.DeviceIDs {
						ids = append(ids, strings.ToLower(strings.TrimSpace(id)))
					}
				} else if got, ok := gpulease.ResolvePins(pinsOf(s.Device), cards); ok {
					ids = got
				}
			}
		}
		seats = append(seats, ids)
	}
	return seats, true
}
