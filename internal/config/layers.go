package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// LayerSeat is one served seat inside a layer (ADR 0039): the model, the device
// PIN it is launched on and the measured numbers a placement decision needs.
// Seats carry their own device pin (not the layer) because a layer's "devices"
// is a documentary list of the sets it MAY occupy while a seat is launched with
// exactly one CUDA_VISIBLE_DEVICES value — `placed.devices` on a result is the
// seat's pin, never the layer's alternatives, so the two must not be conflated.
// The footprints are declared, not probed: a status or health path must never
// load a model to learn its size.
type LayerSeat struct {
	// Role is the closed vocabulary the placement table keys on:
	// router | agent | long | ocr | vision | stt. A layer holds at most one seat
	// per role so `LayerSeat(layer, role)` is unambiguous.
	Role string `json:"role"`
	// Model is the llama-swap id or alias served for this role; "" for the
	// router role, whose models come from the cascade (ModelMap) — a router with
	// a model is a contradiction and is refused.
	Model string `json:"model,omitempty"`
	// Device is this seat's CUDA_VISIBLE_DEVICES pin: "0" | "2" | "0,2" |
	// "0,1,2" | "1". Required on every seat — a seat without a pin cannot be
	// placed truthfully and cannot be checked against the display-card rule.
	Device string `json:"device"`
	// CtxTokens is the seat's SERVED window. It sizes the contract context cap
	// (AgentContextCapBytes) and the window-overflow decision; read from config,
	// never probed before admission.
	CtxTokens int `json:"ctx_tokens,omitempty"`
	// MaxInflight is vLLM's max_num_seqs for the seat; 0 = the seat is never
	// saturated by count (llama.cpp seats). A saturated pair is RECORDED in
	// placed.reason, never acted on (council R2).
	MaxInflight int `json:"max_inflight,omitempty"`
	// FootprintGiB is the VRAM this seat puts on EACH device of Device, measured
	// alone. Used to name the eviction a placement would cause on a shared set.
	FootprintGiB float64 `json:"footprint_gib,omitempty"`
	// DisplayFootprintGiB is the VRAM this seat puts on the layer's display
	// device. The display-floor guard is `free(display) − this ≥ floor`: the
	// old check compared free VRAM alone and admitted a 10.5 GB load onto a
	// card with 9 GB free (council R6). REQUIRED (> 0) on a display_floor-guarded
	// layer for every seat whose pin includes the display device — a 0 here
	// degrades the guard back to that rejected free-VRAM check. Config refuses
	// the omission at load when display_device is a plain CUDA index; a UUID pin
	// is resolved at admission, where a 0 footprint refuses as undeclared.
	DisplayFootprintGiB float64 `json:"display_footprint_gib,omitempty"`
	// HostRAMGiB is the host RAM the seat holds while loaded (experts on CPU);
	// the host_ram guard refuses when free host RAM is below it.
	HostRAMGiB float64 `json:"host_ram_gib,omitempty"`
	// PrefillTPS is the measured prompt-processing rate. The long seats are
	// entered only when tokens ÷ this ≤ the contract's budget: flash-next-262k
	// prefills at 69 t/s, so a 200k prompt is ~48 min against a 900 s cap.
	PrefillTPS float64 `json:"prefill_tps,omitempty"`
	// ModelMap (router role only) maps a route key (workhorse | triage) to the
	// twin model served on THIS layer, so the dormant display layer can
	// substitute its device-1 twins for the cascade's rungs.
	ModelMap map[string]string `json:"model_map,omitempty"`
}

// DeviceList splits the seat's pin into its CUDA indices ("0,2" → ["0","2"]),
// trimming spaces and dropping empties, so device arithmetic (the display-card
// rule, eviction naming) never string-matches a comma-joined pin.
func (s LayerSeat) DeviceList() []string {
	parts := strings.Split(s.Device, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LayerSpec is one device layer of a composite box (ADR 0039): the composed
// tier it stands for, the device sets it may occupy, its seats and the guards
// that gate admission. A box declares layers by seeding them into config from
// the tier table (tierseed), so at runtime everything reads config — a box that
// seeds none has one implicit layer and is byte-identical on every surface.
type LayerSpec struct {
	// Name is the layer id placement records and a contract may request
	// (`layer` on the wire): single | pair | triple | display. Unique per box
	// and shaped like a contract layer id so it can travel on the wire unchanged.
	Name string `json:"name"`
	// Tier is the composed tier this layer stands for (blackwell-16 for
	// single, blackwell-2x16 for pair, …) — what health advertises per layer.
	Tier string `json:"tier"`
	// Devices lists the device SETS this layer may occupy (documentary; the
	// seats carry the pin). Required: a layer with no devices cannot be a layer.
	Devices []string `json:"devices"`
	// Seats are the roles served on this layer, at most one per role.
	Seats []LayerSeat `json:"seats"`
	// OptIn marks a layer that is never the default for any task class — the
	// triple layer is entered on an explicit context_class only.
	OptIn bool `json:"opt_in,omitempty"`
	// Dormant declares a layer that is never placed until the operator flips it
	// (the display layer ships dormant until its measurement is read; council R7).
	Dormant bool `json:"dormant,omitempty"`
	// DisplayDevice is the device (CUDA index or GPU-UUID prefix) the display
	// guards read live. The Qube pins it by UUID because the board reorders
	// indices on power loss.
	DisplayDevice string `json:"display_device,omitempty"`
	// DisplayFloorGiB is the VRAM the desktop keeps on DisplayDevice: the
	// display_floor guard admits a seat only while
	// free(DisplayDevice) − seat.DisplayFootprintGiB ≥ this.
	DisplayFloorGiB float64 `json:"display_floor_gib,omitempty"`
	// Guards are the live checks an admission on this layer must pass, from the
	// closed set display_floor | presence | host_ram. Every guard fails CLOSED:
	// a probe that cannot read refuses.
	Guards []string `json:"guards,omitempty"`
}

// layerGuards is the closed vocabulary of Guards. An unknown guard is refused
// at load rather than skipped: a guard silently ignored is a display card that
// gets loaded while the operator is at the desk.
var layerGuards = map[string]bool{"display_floor": true, "presence": true, "host_ram": true}

// layerRoles is the closed vocabulary of LayerSeat.Role — the keys the
// placement table resolves seats by. A misspelt role would never be found by
// `LayerSeat(layer, role)` and the seat would sit declared but unplaceable.
var layerRoles = map[string]bool{"router": true, "agent": true, "long": true, "ocr": true, "vision": true, "stt": true}

// presenceModes is the closed vocabulary of Config.OperatorPresence. "" means
// present (fail closed); the operator opts the display card in with auto/away.
var presenceModes = map[string]bool{"": true, "auto": true, "away": true, "present": true}

// layerNameRe is the shape of a layer id, identical to the contract's `layer`
// field so a name that validates here can always be dispatched on the wire.
var layerNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// defaultOperatorIdle is the last-input threshold behind presence mode "auto"
// when operator_idle_sec is 0: 15 minutes, long enough that a coffee break does
// not look like "away" to a 10 GB load onto the display card.
const defaultOperatorIdle = 15 * time.Minute

// agentContextCapCeilingBytes bounds AgentContextCapBytes on a composite box:
// 2 MiB, so a 262k window (≈768 KiB at chars/3) fits but a contract can never
// grow past what a single JSON-RPC message reasonably carries.
const agentContextCapCeilingBytes = 2 << 20

// Composite reports whether this box declares device layers (ADR 0039). It is
// THE gate every new surface keys on: a false here means every result, health
// row, status view and ledger entry is byte-identical to the pre-layer build.
func (c Config) Composite() bool {
	return len(c.Layers) > 0
}

// Layer returns the declared layer by name. A miss is a miss, not a default:
// placement onto an undeclared layer must be refused, never guessed.
func (c Config) Layer(name string) (LayerSpec, bool) {
	for _, l := range c.Layers {
		if l.Name == name {
			return l, true
		}
	}
	return LayerSpec{}, false
}

// LayerSeat returns the seat serving `role` on `layer`. It exists so the
// placement table, the delegate gate and the pipeline all resolve a seat the
// same way — by declared role, never by model-name pattern matching.
func (c Config) LayerSeat(layer, role string) (LayerSeat, bool) {
	l, ok := c.Layer(layer)
	if !ok {
		return LayerSeat{}, false
	}
	for _, s := range l.Seats {
		if s.Role == role {
			return s, true
		}
	}
	return LayerSeat{}, false
}

// PresenceMode returns operator_presence with the default applied: "" reads as
// "present", so a box that never set the key keeps the display card closed to
// placement until the operator has read the probe's readings and opted in.
func (c Config) PresenceMode() string {
	m := strings.ToLower(strings.TrimSpace(c.OperatorPresence))
	if m == "" {
		return "present"
	}
	return m
}

// OperatorIdle is the last-input threshold for presence mode "auto" with the
// 15-minute default applied (operator_idle_sec 0 = default, never "0 seconds",
// which would read every keystroke gap as away).
func (c Config) OperatorIdle() time.Duration {
	if c.OperatorIdleSec <= 0 {
		return defaultOperatorIdle
	}
	return time.Duration(c.OperatorIdleSec) * time.Second
}

// AgentContextCapBytes is the inline-context cap every agent door validates a
// contract against. On a plain box it is core.AgentContextMaxBytes (256 KiB)
// unchanged; on a composite box it scales with the largest declared seat window
// (ctx_tokens × 3 at chars/3, capped at 2 MiB) — at 256 KiB a contract estimates
// ~87k tokens and a 163,840 window can never overflow, so without this both
// long seats would be dead code. A composite box whose seats declare no window
// (refused by ValidateLayers, but defended here) keeps the plain cap.
func (c Config) AgentContextCapBytes() int {
	if !c.Composite() {
		return core.AgentContextMaxBytes
	}
	maxCtx := 0
	for _, l := range c.Layers {
		for _, s := range l.Seats {
			if s.CtxTokens > maxCtx {
				maxCtx = s.CtxTokens
			}
		}
	}
	if maxCtx <= 0 {
		return core.AgentContextMaxBytes
	}
	capBytes := maxCtx * 3
	if capBytes > agentContextCapCeilingBytes {
		capBytes = agentContextCapCeilingBytes
	}
	return capBytes
}

// ValidateLayers refuses at load the layer shapes that would misplace work at
// runtime — each error names the JSON path so a seeded or hand-edited config
// fails by name, not at the first contract. Presence mode and the tier identity
// are checked on every box (closed vocabularies fail loudly); the per-layer and
// window rules apply only when layers are declared, so a plain box is untouched.
func (c Config) ValidateLayers() error {
	if !presenceModes[strings.ToLower(strings.TrimSpace(c.OperatorPresence))] {
		return fmt.Errorf("operator_presence: %q is not a presence mode (auto, away, present; empty = present)", c.OperatorPresence)
	}
	if c.OperatorIdleSec < 0 {
		return fmt.Errorf("operator_idle_sec: %d must not be negative (0 = 900)", c.OperatorIdleSec)
	}
	if len(c.Tiers) > 0 {
		if c.TierProfile == "" {
			return fmt.Errorf("tiers: declared without tier_profile — the box must name the tier it is installed as")
		}
		if !containsString(c.Tiers, c.TierProfile) {
			return fmt.Errorf("tiers must include tier_profile %q (got %v)", c.TierProfile, c.Tiers)
		}
	}
	if !c.Composite() {
		return nil
	}
	if c.TierProfile == "" {
		return fmt.Errorf("layers: declared without tier_profile — a composite box must name the tier it is installed as")
	}
	seen := map[string]bool{}
	anyWindow := false
	for i, l := range c.Layers {
		at := fmt.Sprintf("layers[%d]", i)
		if !layerNameRe.MatchString(l.Name) {
			return fmt.Errorf("%s: name %q must match %s", at, l.Name, layerNameRe.String())
		}
		at = fmt.Sprintf("layers[%d] %q", i, l.Name)
		if seen[l.Name] {
			return fmt.Errorf("%s: duplicate layer name", at)
		}
		seen[l.Name] = true
		if l.Tier == "" {
			return fmt.Errorf("%s: needs a tier", at)
		}
		if len(c.Tiers) > 0 && !containsString(c.Tiers, l.Tier) {
			return fmt.Errorf("%s: tier %q is not in tiers %v", at, l.Tier, c.Tiers)
		}
		if len(l.Devices) == 0 {
			return fmt.Errorf("%s: no devices", at)
		}
		for j, d := range l.Devices {
			if strings.TrimSpace(d) == "" {
				return fmt.Errorf("%s: devices[%d] is empty", at, j)
			}
		}
		if l.DisplayFloorGiB > 0 && l.DisplayDevice == "" {
			return fmt.Errorf("%s: display_floor_gib needs display_device", at)
		}
		if l.DisplayFloorGiB < 0 {
			return fmt.Errorf("%s: display_floor_gib must not be negative", at)
		}
		for _, g := range l.Guards {
			if !layerGuards[g] {
				return fmt.Errorf("%s: unknown guard %q (display_floor, presence, host_ram)", at, g)
			}
			if g == "display_floor" && (l.DisplayDevice == "" || l.DisplayFloorGiB <= 0) {
				return fmt.Errorf("%s: guard display_floor needs display_device and display_floor_gib", at)
			}
		}
		if len(l.Seats) == 0 {
			return fmt.Errorf("%s: no seats", at)
		}
		// The display_floor guard is `free(display) − seat.DisplayFootprintGiB ≥
		// floor`. With a 0 footprint that arithmetic degrades to the free-VRAM
		// check the council rejected (R6: it admitted a 10.5 GB load onto a card
		// with 9 GB free), so every seat whose pin includes the display device must
		// declare the number. Config can resolve that match only when
		// display_device is a plain CUDA index; a UUID pin is resolved at admission
		// (placement refuses "display footprint undeclared" there, never free − 0).
		floorGuarded := containsString(l.Guards, "display_floor")
		hostRAMGuarded := containsString(l.Guards, "host_ram")
		display := strings.TrimSpace(l.DisplayDevice)
		roles := map[string]bool{}
		for j, s := range l.Seats {
			sat := fmt.Sprintf("%s seats[%d]", at, j)
			if !layerRoles[s.Role] {
				return fmt.Errorf("%s: unknown role %q (router, agent, long, ocr, vision, stt)", sat, s.Role)
			}
			sat = fmt.Sprintf("%s seat %s", at, s.Role)
			if roles[s.Role] {
				return fmt.Errorf("%s: duplicate role", sat)
			}
			roles[s.Role] = true
			if len(s.DeviceList()) == 0 {
				return fmt.Errorf("%s: needs a device (the seat's CUDA_VISIBLE_DEVICES pin, e.g. \"0,2\")", sat)
			}
			if s.Role == "router" {
				if s.Model != "" {
					return fmt.Errorf("%s: router carries no model (its rungs come from model_map / the cascade), got %q", sat, s.Model)
				}
			} else if strings.TrimSpace(s.Model) == "" {
				return fmt.Errorf("%s: role %s needs a model", sat, s.Role)
			}
			if s.CtxTokens < 0 || s.MaxInflight < 0 || s.FootprintGiB < 0 || s.DisplayFootprintGiB < 0 || s.HostRAMGiB < 0 || s.PrefillTPS < 0 {
				return fmt.Errorf("%s: measured numbers must not be negative", sat)
			}
			if floorGuarded && isCUDAIndex(display) && containsString(s.DeviceList(), display) && s.DisplayFootprintGiB <= 0 {
				return fmt.Errorf("%s: display_footprint_gib is undeclared (0) but the pin %q includes display device %q — guard display_floor is free − footprint ≥ floor, and a 0 footprint degrades it to the free-VRAM check", sat, s.Device, display)
			}
			// The host_ram guard is `free host RAM ≥ seat.HostRAMGiB`. With a 0 it
			// is `free ≥ 0` — true on every box — so a seat on a host_ram-guarded
			// layer must declare the number or the guard is fail-OPEN: the ~70 GB
			// flash-next load would be admitted onto a box with 20 GB free. The
			// same shape closes a misspelt key (host_ram_gb) that a lenient decode
			// would drop to 0 without a word.
			if hostRAMGuarded && s.HostRAMGiB <= 0 {
				return fmt.Errorf("%s: host_ram_gib is undeclared (0) on a host_ram-guarded layer — the guard is free host RAM ≥ host_ram_gib, and ≥ 0 admits the load on every box", sat)
			}
			// A long seat is entered only when tokens ÷ prefill_tps fits the
			// contract's budget; a 0 rate makes that a division by zero, and a rate
			// that decodes as 0 from a misspelt key would never be noticed before
			// the first long placement.
			if s.Role == "long" && s.PrefillTPS <= 0 {
				return fmt.Errorf("%s: prefill_tps is undeclared (0) — the feasibility rule is tokens ÷ prefill_tps ≤ budget, and a long seat cannot be entered without its measured rate", sat)
			}
			if s.CtxTokens > 0 {
				anyWindow = true
			}
		}
	}
	if !anyWindow {
		return fmt.Errorf("layers: no layer seat declares ctx_tokens — the contract context cap cannot be sized")
	}
	return nil
}

// containsString is the tiny membership test the tier identity checks share.
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ValidateLayerKeys is the strict-key companion of ValidateLayers for the two
// readers that still hold the raw JSON: tierseed.ParseDoc (the tier table, and
// the copy embedded in the installer) and Load (a hand-edited config.json).
// encoding/json drops a key it cannot match WITHOUT A WORD, and ValidateLayers
// sees only the decoded struct — so `host_ram_gb` on the triple layer's seat
// decoded as host_ram_gib 0, the host_ram guard became `free ≥ 0`, and a ~70 GB
// load was admissible on a box with 20 GB free, from one dropped character with
// no signal at authoring, install or load. Every layers[] and layers[].seats[]
// key must be a json tag on LayerSpec / LayerSeat, spelled exactly (the table is
// lowercase; a case-insensitive match would let `Host_RAM_GiB` through here and
// nowhere else). The known sets are read off the structs by reflection so this
// lint cannot drift from the shape it guards; the error names the JSON path the
// way ValidateLayers does. A nil / null `layers` is not composite and passes.
func ValidateLayerKeys(rawLayers []byte) error {
	if len(rawLayers) == 0 {
		return nil
	}
	var layers []map[string]json.RawMessage
	if err := json.Unmarshal(rawLayers, &layers); err != nil {
		return fmt.Errorf("layers: %w", err)
	}
	layerKnown := jsonKeys(reflect.TypeOf(LayerSpec{}))
	seatKnown := jsonKeys(reflect.TypeOf(LayerSeat{}))
	for i, l := range layers {
		at := fmt.Sprintf("layers[%d] %q", i, rawString(l["name"]))
		for _, k := range sortedRawKeys(l) {
			if !layerKnown[k] {
				return fmt.Errorf("%s: unknown key %q — not a layer field (%s); it would be dropped on decode and whatever it declared would read as unset", at, k, strings.Join(sortedKeysOf(layerKnown), ", "))
			}
		}
		rawSeats, ok := l["seats"]
		if !ok {
			continue
		}
		var seats []map[string]json.RawMessage
		if err := json.Unmarshal(rawSeats, &seats); err != nil {
			return fmt.Errorf("%s seats: %w", at, err)
		}
		for j, s := range seats {
			sat := fmt.Sprintf("%s seats[%d] %q", at, j, rawString(s["role"]))
			for _, k := range sortedRawKeys(s) {
				if !seatKnown[k] {
					return fmt.Errorf("%s: unknown key %q — not a seat field (%s); a measured number under a misspelt key decodes as 0, and a 0 turns a fail-closed guard fail-open", sat, k, strings.Join(sortedKeysOf(seatKnown), ", "))
				}
			}
		}
	}
	return nil
}

// LayerKeysOf lists the json tags of LayerSpec ("layers[].<key>") and LayerSeat
// ("layers[].seats[].<key>") in the spelling the tier table's _fields block
// documents them under, so a table lint can hold the docs and the struct to the
// same set in both directions.
func LayerKeysOf() []string {
	var out []string
	for _, k := range sortedKeysOf(jsonKeys(reflect.TypeOf(LayerSpec{}))) {
		out = append(out, "layers[]."+k)
	}
	for _, k := range sortedKeysOf(jsonKeys(reflect.TypeOf(LayerSeat{}))) {
		out = append(out, "layers[].seats[]."+k)
	}
	return out
}

// jsonKeys is every json tag on a struct type — the set a JSON object of that
// type may carry. The same walk warnUnknownKeys and tierseed do for Config.
func jsonKeys(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		name := strings.SplitN(t.Field(i).Tag.Get("json"), ",", 2)[0]
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// rawString decodes a raw JSON string for an error path; anything else (absent,
// not a string) reads as "" so the path is still printed.
func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func sortedRawKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isCUDAIndex reports whether a display_device is a plain CUDA index ("1")
// rather than a GPU-UUID pin — the only form config can match against a seat's
// device pin without a live probe.
func isCUDAIndex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// stripComposite zeroes the five composite keys (ADR 0039) on a config that
// Load hands back WITH an error. LoadWithSource returns that value to every
// subcommand and records the error only in Source.LoadErr, which main's loadCfg
// discards — so without this `local-offload mcp` would place work onto the exact
// layer shape the validator refused, and a validator that failed BEFORE
// ValidateLayers ran would leave unvalidated layers intact. Cleared, the value
// is the byte-identical non-composite shape no placement can target; every
// pre-existing key keeps its pre-0.116 failed-load behaviour (a separate
// decision — exiting on Source.LoadErr would change every subcommand).
func stripComposite(c *Config) {
	c.Layers = nil
	c.Tiers = nil
	c.TierProfile = ""
	c.OperatorPresence = ""
	c.OperatorIdleSec = 0
}
