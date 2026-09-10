package placement

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func rowByName(t *testing.T, rows []LayerRow, name string) LayerRow {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row %q in %+v", name, rows)
	return LayerRow{}
}

func TestRowsFromConfigFillsOccupancyAndVerdicts(t *testing.T) {
	cfg := config.CompositeFixture()
	f := admitting()
	f.seats["pair/agent"] = SeatState{Known: true, Loaded: true, Inflight: 7}
	f.seats["single/ocr"] = SeatState{Known: true, Loaded: false}
	rows := RowsFromConfig(cfg, f.live())
	if len(rows) != len(cfg.Layers) {
		t.Fatalf("one row per layer: got %d want %d", len(rows), len(cfg.Layers))
	}
	for i, r := range rows {
		if r.Name != cfg.Layers[i].Name {
			t.Fatalf("rows keep config order: %d = %q want %q", i, r.Name, cfg.Layers[i].Name)
		}
	}
	pair := rowByName(t, rows, "pair")
	if !pair.Admissible || !strings.Contains(pair.Reason, "no guards") {
		t.Fatalf("an unguarded layer is admissible: %+v", pair)
	}
	var agent SeatRow
	for _, s := range pair.Seats {
		if s.Role == "agent" {
			agent = s
		}
	}
	if !agent.Known || !agent.Loaded || agent.Inflight != 7 || agent.Model != "agent-pool" || agent.Device != "0,2" || agent.CtxTokens != 163840 || agent.MaxInflight != 32 {
		t.Fatalf("seat rows carry the declared numbers and the live occupancy: %+v", agent)
	}
	single := rowByName(t, rows, "single")
	if single.Tier != "blackwell-16" || !reflect.DeepEqual(single.Devices, []string{"0", "2"}) || !single.Admissible {
		t.Fatalf("single row: %+v", single)
	}
	for _, s := range single.Seats {
		if s.Role == "ocr" && (!s.Known || s.Loaded) {
			t.Fatalf("ocr seat read cold: %+v", s)
		}
		if s.Role == "router" && s.Known {
			t.Fatalf("the router role has no seat to read: Known must be false, got %+v", s)
		}
	}
	triple := rowByName(t, rows, "triple")
	if !triple.OptIn || triple.DisplayDevice != "1" || triple.DisplayFloorGiB != 4 || !reflect.DeepEqual(triple.Guards, []string{"display_floor", "host_ram", "presence"}) {
		t.Fatalf("triple row keeps the spec: %+v", triple)
	}
	if !triple.Admissible || !strings.Contains(triple.Reason, "display_floor") || !strings.Contains(triple.Reason, "locked") {
		t.Fatalf("the triple's verdict is its long seat's guard run with readings: %+v", triple)
	}
	if triple.Seats[0].DisplayFootprintGiB != 10.5 || triple.Seats[0].HostRAMGiB != 70 || triple.Seats[0].PrefillTPS != 69 {
		t.Fatalf("measured numbers travel on the row: %+v", triple.Seats[0])
	}
	display := rowByName(t, rows, "display")
	if display.Admissible || display.Reason != "dormant (operator decision)" || !display.Dormant {
		t.Fatalf("a dormant layer reports itself so: %+v", display)
	}
	if display.Seats[0].ModelMap["workhorse"] != "gemma-4-e4b-display" {
		t.Fatalf("model_map travels on the router seat row: %+v", display.Seats[0])
	}

	// Guards refuse → the row says which and why.
	atDesk := admitting()
	atDesk.pres = &Presence{Mode: "present", Known: true}
	rows = RowsFromConfig(cfg, atDesk.live())
	triple = rowByName(t, rows, "triple")
	if triple.Admissible || !strings.Contains(triple.Reason, "presence") {
		t.Fatalf("refused triple row names the guard: %+v", triple)
	}
	// No layers → no rows (a plain box publishes nothing).
	if rows := RowsFromConfig(config.Default(), f.live()); rows != nil {
		t.Fatalf("a non-composite box has no rows, got %+v", rows)
	}
}

func TestRowJSONTagsMatchTheHealthContract(t *testing.T) {
	// A minimal row: only the always-present keys appear.
	b, err := json.Marshal(LayerRow{Name: "single", Seats: []SeatRow{{Role: "router"}}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	keys := sortedKeys(m)
	if !reflect.DeepEqual(keys, []string{"admissible", "name", "reason", "seats"}) {
		t.Fatalf("minimal layer row keys: %v", keys)
	}
	var seats []map[string]json.RawMessage
	if err := json.Unmarshal(m["seats"], &seats); err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(seats[0]); !reflect.DeepEqual(got, []string{"role"}) {
		t.Fatalf("minimal seat row keys: %v", got)
	}
	// The full set, spelled exactly as the plan's contract.
	full := LayerRow{Name: "triple", Tier: "t", Devices: []string{"0,1,2"}, OptIn: true, Dormant: true, DisplayDevice: "1", DisplayFloorGiB: 4, Guards: []string{"presence"},
		Seats:      []SeatRow{{Role: "long", Model: "m", Device: "0,1,2", CtxTokens: 1, MaxInflight: 2, Loaded: true, Known: true, Inflight: 3, FootprintGiB: 4, DisplayFootprintGiB: 5, HostRAMGiB: 6, PrefillTPS: 7, ModelMap: map[string]string{"a": "b"}}},
		Admissible: true, Reason: "r"}
	b, _ = json.Marshal(full)
	m = nil
	_ = json.Unmarshal(b, &m)
	wantLayer := []string{"admissible", "devices", "display_device", "display_floor_gib", "dormant", "guards", "name", "opt_in", "reason", "seats", "tier"}
	if got := sortedKeys(m); !reflect.DeepEqual(got, wantLayer) {
		t.Fatalf("layer row keys:\n got %v\nwant %v", got, wantLayer)
	}
	seats = nil
	_ = json.Unmarshal(m["seats"], &seats)
	wantSeat := []string{"ctx_tokens", "device", "display_footprint_gib", "footprint_gib", "host_ram_gib", "inflight", "known", "loaded", "max_inflight", "model", "model_map", "prefill_tps", "role"}
	if got := sortedKeys(seats[0]); !reflect.DeepEqual(got, wantSeat) {
		t.Fatalf("seat row keys:\n got %v\nwant %v", got, wantSeat)
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestFromRowsRebuildsTheSpecAndAnswersOccupancyFromRows(t *testing.T) {
	cfg := config.CompositeFixture()
	f := admitting()
	f.seats["pair/agent"] = SeatState{Known: true, Loaded: true, Inflight: 9}
	rows := RowsFromConfig(cfg, f.live())
	layers, live := FromRows(rows)
	if !reflect.DeepEqual(layers, cfg.Layers) {
		t.Fatalf("spec round trip:\n got %+v\nwant %+v", layers, cfg.Layers)
	}
	if live.DeviceFree != nil || live.HostFree != nil || live.Presence != nil || live.DeviceIndex != nil {
		t.Fatal("a remote Live has no machine readers: the node's verdict stands in for them")
	}
	if live.Seat == nil || live.Verdict == nil {
		t.Fatal("a remote Live answers occupancy and verdicts from the rows")
	}
	if st := live.Seat("pair", "agent"); !st.Known || !st.Loaded || st.Inflight != 9 {
		t.Fatalf("occupancy from the row: %+v", st)
	}
	if st := live.Seat("pair", "nope"); st.Known {
		t.Fatalf("an unknown seat is unknown: %+v", st)
	}
	if st := live.Seat("single", "router"); st.Known {
		t.Fatalf("the router seat stays unknown through the rows: %+v", st)
	}
	if v, reason := live.Verdict("triple"); v == nil || !*v || !strings.Contains(reason, "display_floor") {
		t.Fatalf("triple verdict and reason from the row: %v %q", v, reason)
	}
	if v, reason := live.Verdict("display"); v == nil || *v || reason != dormantReason {
		t.Fatalf("dormant display verdict is false with its reason: %v %q", v, reason)
	}
	if v, _ := live.Verdict("nope"); v != nil {
		t.Fatalf("an unknown layer has no verdict: %v", v)
	}
	// Empty rows → nil layers, empty Live (a plain node decodes to nothing composite).
	if layers, live := FromRows(nil); layers != nil || live.Seat != nil || live.Verdict != nil {
		t.Fatalf("no rows → nothing, got %+v %+v", layers, live)
	}
	// A JSON round trip of the rows (what health actually carries) survives.
	b, _ := json.Marshal(rows)
	var back []LayerRow
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	layers2, _ := FromRows(back)
	if !reflect.DeepEqual(layers2, cfg.Layers) {
		t.Fatalf("spec survives the wire:\n got %+v\nwant %+v", layers2, cfg.Layers)
	}
}
