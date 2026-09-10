package config

// CompositeFixture is the reference composite box every placement test reasons
// about: the Qube's four layers with the measured footprints and windows. Shared
// across packages so the table, the delegate, the pipeline and the status tests
// all see one box; never consulted at runtime.
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
