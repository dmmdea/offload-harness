package config

// CompositeFixture is the reference composite box every placement test reasons
// about: four layers with the measured footprints and windows. Shared across
// packages so the table, the delegate, the pipeline and the status tests all
// see one box; never consulted at runtime.
//
// It declares one layer the SHIPPED tier table does not: the three-card
// `triple` layer, whose only seat (qwen3.8-flash-next-262k) parked 28-32
// expert layers in host RAM and was removed by the 2026-09-10 operator rule
// that RAM is overflow only. The fixture keeps it because the TABLE still
// serves that shape — a guarded, host-RAM-bearing, display-card-spanning
// layer is exactly what the display_floor / host_ram / presence guards exist
// for, and the day a three-card seat fits inside VRAM the tier declares one
// again. Tests that assert what the box SHIPS read profiles.json, not this.
func CompositeFixture() Config {
	c := Default()
	c.TierProfile = "blackwell-3x16"
	c.Tiers = []string{"blackwell-16", "blackwell-2x16", "blackwell-3x16"}
	c.Layers = []LayerSpec{
		{Name: "single", Tier: "blackwell-16", Devices: []string{"0", "2"}, Seats: []LayerSeat{
			{Role: "router", Device: "0"},
			{Role: "agent", Model: "gemma-4-26b-agent", Device: "0", CtxTokens: 131072, FootprintGiB: 13.0},
			{Role: "ocr", Model: "qwen3-vl-8b", Device: "2", CtxTokens: 16384, FootprintGiB: 11.5},
		}},
		{Name: "pair", Tier: "blackwell-2x16", Devices: []string{"0,2"}, Seats: []LayerSeat{
			{Role: "agent", Model: "agent-pool", Device: "0,2", CtxTokens: 163840, MaxInflight: 32, FootprintGiB: 15.0},
			{Role: "long", Model: "qwen3.8-27b-262k", Device: "0,2", CtxTokens: 262144, FootprintGiB: 15.5, PrefillTPS: 1197},
			{Role: "vision", Model: "qwen3-vl-32b", Device: "0,2", CtxTokens: 16384},
		}},
		{Name: "triple", Tier: "blackwell-3x16", Devices: []string{"0,1,2"}, OptIn: true,
			DisplayDevice: "1", DisplayFloorGiB: 4, Guards: []string{"display_floor", "host_ram", "presence"},
			Seats: []LayerSeat{{Role: "long", Model: "qwen3.8-flash-next-262k", Device: "0,1,2", CtxTokens: 262144, FootprintGiB: 13.0, DisplayFootprintGiB: 10.5, HostRAMGiB: 70, PrefillTPS: 69}}},
		{Name: "display", Tier: "blackwell-16", Devices: []string{"1"}, OptIn: true, Dormant: true,
			DisplayDevice: "1", DisplayFloorGiB: 4, Guards: []string{"display_floor", "presence"},
			Seats: []LayerSeat{{Role: "router", Device: "1", DisplayFootprintGiB: 6.2, ModelMap: map[string]string{"workhorse": "gemma-4-e4b-display", "triage": "gemma-4-e2b-display"}}}},
	}
	return c
}

// FlagshipFixture is CompositeFixture as the operator ordered the flagship on 2026-09-19:
// "the 3 card tier as the agent seat now and the 2 card tier to be the opt in one". The
// triple layer carries the AGENT seat (the pipeline seat spanning every card) and is not
// opt-in; the pair keeps its seats but is entered by name only; the single layer holds the
// one-card seats (the router and agent on card 0, the vision-OCR seat on card 2); the
// display layer stays dormant. It is the box the consumers of card-scoped leases reason
// about: a lease on card 2 fences the flagship, the pair and the card-2 seat, and leaves
// the card-0 seats alone. Never consulted at runtime.
func FlagshipFixture() Config {
	c := CompositeFixture()
	layers := make([]LayerSpec, 0, len(c.Layers))
	for _, l := range c.Layers {
		l.Seats = append([]LayerSeat(nil), l.Seats...)
		switch l.Name {
		case "pair":
			l.OptIn = true
			l.Seats[0].Model = "agent-pool-2card"
		case "triple":
			l.OptIn = false
			l.DisplayDevice, l.DisplayFloorGiB, l.Guards = "", 0, nil
			l.Seats = []LayerSeat{{Role: "agent", Model: "agent-pool", Device: "0,1,2", CtxTokens: 262144, MaxInflight: 32, FootprintGiB: 13.5}}
		}
		layers = append(layers, l)
	}
	c.Layers = layers
	return c
}
