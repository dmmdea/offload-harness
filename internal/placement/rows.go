package placement

import (
	"github.com/dmmdea/offload-harness/internal/config"
)

// SeatRow is one seat of a layer as a node publishes it in health and
// offload_status: the declared numbers the table needs plus the live
// occupancy. It is the ONE shape shared by fleetnode, delegate.NodeView and
// offload_status so a delegator can rebuild a remote's layers (FromRows) and
// run the same Decide over them. Every measured number is omitempty; role is
// always present because a seat without a role cannot be resolved.
type SeatRow struct {
	Role                string            `json:"role"`
	Model               string            `json:"model,omitempty"`
	Device              string            `json:"device,omitempty"`
	CtxTokens           int               `json:"ctx_tokens,omitempty"`
	MaxInflight         int               `json:"max_inflight,omitempty"`
	Loaded              bool              `json:"loaded,omitempty"`
	Known               bool              `json:"known,omitempty"`
	Inflight            int               `json:"inflight,omitempty"`
	FootprintGiB        float64           `json:"footprint_gib,omitempty"`
	DisplayFootprintGiB float64           `json:"display_footprint_gib,omitempty"`
	HostRAMGiB          float64           `json:"host_ram_gib,omitempty"`
	PrefillTPS          float64           `json:"prefill_tps,omitempty"`
	ModelMap            map[string]string `json:"model_map,omitempty"`
}

// LayerRow is one layer as a node publishes it: the spec (so a delegator can
// rebuild config.LayerSpec exactly) plus the node's OWN verdict on the layer
// (Admissible/Reason from LayerAdmissible with the node's live readers). The
// verdict travels because the display-card guards can only be read where the
// card is; the delegator feeds it to Decide as the row verdict and the node
// re-checks at admission. name, admissible and reason are always present:
// a row that names no layer, or reports no verdict, is not a row.
type LayerRow struct {
	Name            string    `json:"name"`
	Tier            string    `json:"tier,omitempty"`
	Devices         []string  `json:"devices,omitempty"`
	OptIn           bool      `json:"opt_in,omitempty"`
	Dormant         bool      `json:"dormant,omitempty"`
	DisplayDevice   string    `json:"display_device,omitempty"`
	DisplayFloorGiB float64   `json:"display_floor_gib,omitempty"`
	Guards          []string  `json:"guards,omitempty"`
	Seats           []SeatRow `json:"seats,omitempty"`
	Admissible      bool      `json:"admissible"`
	Reason          string    `json:"reason"`
}

// dormantReason is the verdict a dormant layer publishes: the operator, not a
// guard, holds it closed.
const dormantReason = "dormant (operator decision)"

// RowsFromConfig renders what a node publishes for its layers: every seat's
// occupancy from live.Seat, every layer's verdict from LayerAdmissible over
// its placement seat — the agent seat where one exists, else the long or
// router seat an opt-in layer places on — in config order. A non-composite
// box returns nil so its health and status stay byte-identical.
func RowsFromConfig(cfg config.Config, live Live) []LayerRow {
	if !cfg.Composite() {
		return nil
	}
	rows := make([]LayerRow, 0, len(cfg.Layers))
	for _, l := range cfg.Layers {
		row := LayerRow{
			Name: l.Name, Tier: l.Tier, Devices: l.Devices, OptIn: l.OptIn, Dormant: l.Dormant,
			DisplayDevice: l.DisplayDevice, DisplayFloorGiB: l.DisplayFloorGiB, Guards: l.Guards,
		}
		for _, s := range l.Seats {
			sr := SeatRow{
				Role: s.Role, Model: s.Model, Device: s.Device, CtxTokens: s.CtxTokens, MaxInflight: s.MaxInflight,
				FootprintGiB: s.FootprintGiB, DisplayFootprintGiB: s.DisplayFootprintGiB, HostRAMGiB: s.HostRAMGiB,
				PrefillTPS: s.PrefillTPS, ModelMap: s.ModelMap,
			}
			if live.Seat != nil && s.Model != "" {
				st := live.Seat(l.Name, s.Role)
				sr.Known, sr.Loaded, sr.Inflight = st.Known, st.Loaded, st.Inflight
			}
			row.Seats = append(row.Seats, sr)
		}
		switch {
		case l.Dormant:
			row.Admissible, row.Reason = false, dormantReason
		default:
			seat, ok := placementSeat(l)
			if !ok {
				row.Admissible, row.Reason = false, "no seat to place on"
				break
			}
			ok, reason, _ := LayerAdmissible(l, seat, live, nil)
			row.Admissible, row.Reason = ok, reason
		}
		rows = append(rows, row)
	}
	return rows
}

// placementSeat is the seat a layer's verdict is computed for: agent, then
// long, then router — the roles the table places on, in the order a layer is
// most likely to be entered.
func placementSeat(l config.LayerSpec) (config.LayerSeat, bool) {
	for _, role := range []string{RoleAgent, RoleLong, RoleRouter} {
		if s, ok := findSeat(l, role); ok {
			return s, true
		}
	}
	if len(l.Seats) > 0 {
		return l.Seats[0], true
	}
	return config.LayerSeat{}, false
}

// FromRows rebuilds a remote node's []config.LayerSpec and a Live whose Seat
// answers occupancy from the rows and whose Verdict answers each layer's own
// admissibility; DeviceFree, DeviceIndex, HostFree and Presence stay nil (the
// delegator cannot read the remote's cards — the verdict stands in, and the
// node re-checks at admission). nil rows → nil layers and an empty Live, so a
// plain node decodes to nothing composite.
func FromRows(rows []LayerRow) ([]config.LayerSpec, Live) {
	if len(rows) == 0 {
		return nil, Live{}
	}
	layers := make([]config.LayerSpec, 0, len(rows))
	seats := make(map[string]SeatState, len(rows)*3)
	verdicts := make(map[string]LayerRow, len(rows))
	for _, r := range rows {
		l := config.LayerSpec{
			Name: r.Name, Tier: r.Tier, Devices: r.Devices, OptIn: r.OptIn, Dormant: r.Dormant,
			DisplayDevice: r.DisplayDevice, DisplayFloorGiB: r.DisplayFloorGiB, Guards: r.Guards,
		}
		for _, s := range r.Seats {
			l.Seats = append(l.Seats, config.LayerSeat{
				Role: s.Role, Model: s.Model, Device: s.Device, CtxTokens: s.CtxTokens, MaxInflight: s.MaxInflight,
				FootprintGiB: s.FootprintGiB, DisplayFootprintGiB: s.DisplayFootprintGiB, HostRAMGiB: s.HostRAMGiB,
				PrefillTPS: s.PrefillTPS, ModelMap: s.ModelMap,
			})
			if s.Known {
				seats[r.Name+"/"+s.Role] = SeatState{Known: true, Loaded: s.Loaded, Inflight: s.Inflight}
			}
		}
		layers = append(layers, l)
		verdicts[r.Name] = r
	}
	live := Live{
		Seat: func(layer, role string) SeatState { return seats[layer+"/"+role] },
		Verdict: func(layer string) (*bool, string) {
			r, ok := verdicts[layer]
			if !ok {
				return nil, ""
			}
			v := r.Admissible
			return &v, r.Reason
		},
	}
	return layers, live
}
