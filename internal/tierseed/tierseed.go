// Package tierseed resolves a hardware tier's config_seed into the harness config
// fragment an install should start from.
//
// The seeds existed only inside install.ps1, which meant a tier's media binding was
// both Windows-shaped (`sd-cli.exe` baked into the table) and PowerShell-only. A tier
// is a HARDWARE class: the same tier on Linux must produce the same capabilities, so
// the resolution lives here — one implementation, cross-compiled, testable — and the
// wrappers become consumers.
//
// It also VALIDATES, because a seed is config that ships to every machine of that
// class and a typo in it is a capability that silently never works:
//   - every key must be a real Config field (a misspelled seed key would be dropped
//     by the loader with only a warning, on every install of that tier);
//   - no literal ".exe" — binaries carry the __EXE__ token so one seed renders on
//     both platforms;
//   - vae_mode is rejected as "cpu" on a CUDA backend, where it was MEASURED at 7.8x
//     slower (58.2s vs 7.5s) — correct on an AMD/UMA part, a trap everywhere else.
package tierseed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediaseat"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// Tokens a seed value may carry. They exist so ONE table row renders correctly on
// every machine and OS: the install root differs per machine (see the `home` key),
// and the executable suffix differs per platform.
const (
	TokenHome = "__OFFLOAD_HOME__"
	TokenExe  = "__EXE__"
)

// Options describe the machine the seed is being resolved FOR — never the machine
// doing the resolving, so a Windows box can render a Linux node's config.
type Options struct {
	// Home is the install root substituted for __OFFLOAD_HOME__. Forward slashes are
	// used verbatim: the harness accepts them on Windows and they survive JSON.
	Home string
	// GOOS selects the executable suffix. "" = the running platform.
	GOOS string
	// RAMTier selects the optional config_seed_ram_mid_high overlay ("mid"/"high").
	RAMTier string
	// HailoHome is the Hailo repo checkout __HAILO_HOME__ expands to.
	HailoHome string
	// CoralHome is the Coral sidecar home __CORAL_HOME__ expands to (its venv,
	// models/ and the accelerators/coral/ scripts live under it; Coral D3).
	CoralHome string
	// RknpuHome is the RKNPU sidecar home __RKNPU_HOME__ expands to (its venv, models/
	// and the accelerators/rknpu/ scripts live under it).
	RknpuHome string
	// VLLMSeatActive says whether THIS box renders the tier's vLLM agent seat. The
	// caller decides it with vllmseat.Spec.Detect — the engine is a hand-built venv
	// the installer does not create, so a box without it binds the fallback seat. The
	// binding and the rendered llama-swap entry therefore agree by construction: both
	// are driven by the same detection.
	VLLMSeatActive bool
	// ExtraVLLMSeatsActive names, by seat id, which of the tier's EXTRA vLLM seats
	// (Profile.ExtraVLLMSeats) this box can run. The caller decides each one with the same
	// vllmseat.Spec.Detect it uses for the lane seat: the seats share the venv and each
	// needs its own weights. An extra seat that is absent here is neither rostered nor
	// bound, and the layer it backs is left out of the seeded layers — the config never
	// advertises a seat the box cannot serve.
	ExtraVLLMSeatsActive map[string]bool
}

// vaeArgs maps the declared vae_mode to the sd.cpp flag it stands for. Free-text
// extra args were how "--vae-on-cpu" spread to a tier it was wrong for.
var vaeArgs = map[string]string{
	"tiling": "--vae-tiling",
	"cpu":    "--vae-on-cpu",
	"none":   "",
}

// Profile is the part of a profiles.json entry this package reads.
type Profile struct {
	Backend           string         `json:"backend"`
	ConfigSeed        map[string]any `json:"config_seed"`
	ConfigSeedMidHigh map[string]any `json:"config_seed_ram_mid_high"`
	// ResidentTier is the tier's preferred hot model. It SEEDS the agent planner
	// seat (agent_model) when it differs from the workhorse — see Resolve.
	ResidentTier string `json:"resident_tier"`
	// AgentCtxTokens is the tier's agent context window. Resolve seeds it as
	// agent_ctx_tokens unless config_seed or a vLLM seat binding already set it. It used
	// to reach a config only through install.ps1, which wrote the tier field directly;
	// install.sh never did, so every fresh LINUX node (binxarn, the Lenovo) got the code
	// default instead of the window its tier was measured at. Seeding it here gives both
	// installers the same value from one place.
	AgentCtxTokens int `json:"agent_ctx_tokens"`
	// MediaSeats are the tier's alias-backed media capabilities. They are the SOLE
	// writer of the config keys they bind — see mediaseat.Bindings.
	MediaSeats []mediaseat.Seat `json:"media_seats"`
	// VLLMSeat is the tier's persistent vLLM agent seat (ADR 0035). When the box can
	// actually run it (Options.VLLMSeatActive), it is the SOLE writer of agent_model
	// and agent_ctx_tokens; otherwise its declared fallback is.
	VLLMSeat *vllmseat.Spec `json:"vllm_seat,omitempty"`
	// ExtraVLLMSeats are the tier's further vLLM seats: served on demand beside VLLMSeat
	// on the same card, and never the agent lane — they never bind agent_model, so each
	// is validated as a non-lane seat (vllmseat.Spec.ValidateExtra) and carries no
	// fallback. They exist because a card can serve more than one heavy model in turn
	// (ampere-16: the 27B lane seat and the 35B fast digest seat), and the second seat
	// and the layer it backs lived only in a reference box's hand-edited config, so a
	// fresh install lost both — the capability-loss class ADR 0048 was written against.
	// Each active extra seat joins the box's `vllm_seats` roster with its own storeless
	// or store binding; the layers name it by id or alias.
	ExtraVLLMSeats []vllmseat.Spec `json:"extra_vllm_seats,omitempty"`
	// Composes lists the tiers this tier is a COMPLETE instance of at the same
	// time (ADR 0039: blackwell-3x16 composes blackwell-16 and blackwell-2x16).
	// It is seeded as `tiers` = Composes + the tier's own id and `tier_profile`
	// = the id, so health and status can advertise every tier the box is while
	// installed.json keeps ONE id and the matrix keeps one row per tier.
	Composes []string `json:"composes,omitempty"`
	// Layers are the device layers a composite box places work onto, seeded
	// VERBATIM into config.layers so at runtime everything reads config and a
	// box that seeds none is byte-identical on every surface. The pair layer's
	// agent seat may be declared bare and is derived from VLLMSeat at resolve
	// time (fillPairAgent), so the delegation seat's model, window, concurrency
	// and device pin live in exactly one place.
	Layers []config.LayerSpec `json:"layers,omitempty"`
}

// Doc is the whole profiles.json document this package reads: the GPU tier table
// plus the additive accelerators table (ADR 0024).
type Doc struct {
	Profiles     map[string]Profile     `json:"profiles"`
	Accelerators map[string]Accelerator `json:"accelerators"`
}

// Load reads the profile table from a repo root.
func Load(root string) (map[string]Profile, error) {
	d, err := LoadDoc(root)
	if err != nil {
		return nil, err
	}
	return d.Profiles, nil
}

// LoadDoc reads the whole document (profiles + accelerators) from a repo root.
func LoadDoc(root string) (Doc, error) {
	raw, err := os.ReadFile(filepath.Join(root, "setup", "templates", "profiles.json"))
	if err != nil {
		return Doc{}, err
	}
	return ParseDoc(raw)
}

// Parse reads the profile table from bytes — the install case, where the table is
// embedded in the binary because the machine has no checkout.
func Parse(raw []byte) (map[string]Profile, error) {
	d, err := ParseDoc(raw)
	if err != nil {
		return nil, err
	}
	return d.Profiles, nil
}

// ParseDoc reads the whole document from bytes. Every profile's `layers` block
// is held to config.ValidateLayerKeys BEFORE Validate sees the decoded value:
// json.Unmarshal drops a key it cannot match without a word, so `host_ram_gb`
// on the triple layer's seat decoded as host_ram_gib 0, Validate/ValidateLayers
// saw a well-formed seat, and the installer seeded a host_ram guard that reads
// `free ≥ 0` — fail-open from one dropped character. The strict pass needs the
// raw bytes, which is why it lives here and not in Validate; Parse, Load and
// LoadDoc all route through this function, so the embedded copy the installer
// ships cannot carry a misspelt layer key either.
func ParseDoc(raw []byte) (Doc, error) {
	var d Doc
	if err := json.Unmarshal(raw, &d); err != nil {
		return Doc{}, fmt.Errorf("profiles.json: %w", err)
	}
	if len(d.Profiles) == 0 {
		return Doc{}, fmt.Errorf("profiles.json has no profiles — the schema moved")
	}
	if err := validateLayerKeys(raw); err != nil {
		return Doc{}, err
	}
	if err := d.Validate(); err != nil {
		return Doc{}, err
	}
	return d, nil
}

// validateLayerKeys re-reads each profile's `layers` block as raw JSON and
// refuses any layers[] / layers[].seats[] key that is not a config field, by
// tier and JSON path. Profiles are visited in id order so a table with two
// mistakes fails deterministically.
func validateLayerKeys(raw []byte) error {
	var top struct {
		Profiles map[string]map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("profiles.json: %w", err)
	}
	ids := make([]string, 0, len(top.Profiles))
	for id := range top.Profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := config.ValidateLayerKeys(top.Profiles[id]["layers"]); err != nil {
			return fmt.Errorf("profiles.json: tier %q %w", id, err)
		}
	}
	return nil
}

// Validate refuses the composite shapes the table cannot seed truthfully, at
// PARSE time so the embedded copy the installer ships cannot carry them: a
// `composes` id or a layer tier that is not in the table would seed a `tiers`
// list naming a tier no install can resolve, and health would advertise it to
// the fleet; composes without layers would seed nothing, silently; a tier
// composing itself or another composite has no defined layer set. The seeded
// layers are then run through config's own validator in BOTH bindings (vLLM
// seat active and fallback), because either is what an install writes. It runs
// from ParseDoc, which Parse and Load both route through, so every reader of
// the table gets the same refusal.
func (d Doc) Validate() error {
	ids := make([]string, 0, len(d.Profiles))
	for id := range d.Profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := d.Profiles[id]
		if err := validateVLLMSeats(id, p); err != nil {
			return err
		}
		if len(p.Composes) == 0 && len(p.Layers) == 0 {
			continue
		}
		if len(p.Layers) == 0 {
			return fmt.Errorf("profiles.json: tier %q composes %v but declares no layers — nothing would be seeded", id, p.Composes)
		}
		seen := map[string]bool{}
		for _, c := range p.Composes {
			if c == id {
				return fmt.Errorf("profiles.json: tier %q composes itself", id)
			}
			cp, ok := d.Profiles[c]
			if !ok {
				return fmt.Errorf("profiles.json: tier %q composes unknown tier %q", id, c)
			}
			if len(cp.Composes) > 0 || len(cp.Layers) > 0 {
				return fmt.Errorf("profiles.json: tier %q composes %q, which is itself composite — nesting has no defined layer set", id, c)
			}
			if seen[c] {
				return fmt.Errorf("profiles.json: tier %q composes %q twice", id, c)
			}
			seen[c] = true
		}
		tiers := append(append([]string{}, p.Composes...), id)
		for i, l := range p.Layers {
			if _, ok := d.Profiles[l.Tier]; !ok {
				return fmt.Errorf("profiles.json: tier %q layers[%d] %q stands for unknown tier %q", id, i, l.Name, l.Tier)
			}
			if !containsID(tiers, l.Tier) {
				return fmt.Errorf("profiles.json: tier %q layers[%d] %q stands for tier %q, which this tier does not compose (%v)", id, i, l.Name, l.Tier, tiers)
			}
		}
		if err := validateLayerSeatClosure(id, p); err != nil {
			return err
		}
		// Either binding is what an install writes, and with extra seats each of them
		// may be present or absent on its own: every combination must seed layers that
		// config's own validator accepts.
		for _, set := range activityCombos(p) {
			c := config.Config{TierProfile: id, Tiers: tiers, Layers: ResolveLayers(p.Layers, set)}
			if err := c.ValidateLayers(); err != nil {
				return fmt.Errorf("profiles.json: tier %q layers (%s): %w", id, set.describe(), err)
			}
		}
	}
	return nil
}

// validateVLLMSeats refuses the extra-seat shapes the renderer cannot express
// truthfully. It runs for EVERY tier, at parse, so the copy embedded in the installer
// cannot carry one — and it validates every declared extra seat, not only an active
// one: a half-specified seat must die here, never on a box that has fetched its weights.
func validateVLLMSeats(id string, p Profile) error {
	if len(p.ExtraVLLMSeats) == 0 {
		return nil
	}
	if p.VLLMSeat == nil {
		return fmt.Errorf("profiles.json: tier %q declares extra_vllm_seats but no vllm_seat — an extra seat alternates with "+
			"the tier's lane seat, and it is the lane seat a box without the venv falls back from", id)
	}
	names := map[string]string{} // seat id or alias -> the seat that owns it
	units := map[string]string{} // systemd unit -> the seat that owns it
	claim := func(s vllmseat.Spec) error {
		for _, n := range append([]string{s.ID}, s.Aliases...) {
			if prev, dup := names[n]; dup {
				return fmt.Errorf("profiles.json: tier %q: seat name %q is declared twice (%s and %s) — llama-swap resolves "+
					"ids and aliases in one namespace, and a repeat would route one seat's requests to the other", id, n, prev, s.ID)
			}
			names[n] = s.ID
		}
		if prev, dup := units[s.Unit]; dup {
			return fmt.Errorf("profiles.json: tier %q: unit %q is used by both %s and %s — two seats cannot be one systemd unit", id, s.Unit, prev, s.ID)
		}
		units[s.Unit] = s.ID
		return nil
	}
	if err := claim(*p.VLLMSeat); err != nil {
		return err
	}
	for i := range p.ExtraVLLMSeats {
		e := p.ExtraVLLMSeats[i]
		if err := e.ValidateExtra(id); err != nil {
			return fmt.Errorf("profiles.json: %w", err)
		}
		if err := claim(e); err != nil {
			return err
		}
		// The renderer serialises every vLLM seat of a tier as an alternative of the
		// others (one heavy seat per card set at a time — two seats at util 0.90 cannot
		// share a card). A seat on other cards could legitimately be co-resident, and
		// that topology is not rendered: refuse it rather than serialise it silently.
		if normDevices(e.Device) != normDevices(p.VLLMSeat.Device) {
			return fmt.Errorf("profiles.json: tier %q extra seat %s pins device %q but the lane seat pins %q — extra seats are "+
				"rendered as ALTERNATIVES of the lane seat (one heavy seat per card set), and a seat on other cards would need "+
				"a co-resident topology the renderer does not emit", id, e.ID, dashDevice(e.Device), dashDevice(p.VLLMSeat.Device))
		}
	}
	return nil
}

// validateLayerSeatClosure ties a layer seat that NAMES a vLLM seat to that seat's own
// declaration. Naming the seat explicitly (rather than leaving the agent seat bare, as
// the composite tier's pair layer does) duplicates its window, its concurrency and its
// card pin into the layer, and two copies of a measured number drift: the layer would
// then advertise a window the engine does not serve. One number, one place — so the
// copy must equal the original, and a mismatch is refused at parse.
func validateLayerSeatClosure(id string, p Profile) error {
	set := VLLMSeatSet{Primary: p.VLLMSeat, Extras: p.ExtraVLLMSeats}
	for _, l := range p.Layers {
		for _, s := range l.Seats {
			spec, named := set.specNamed(s.Model)
			if !named {
				continue
			}
			at := fmt.Sprintf("profiles.json: tier %q layers %q seat %s (%s)", id, l.Name, s.Role, spec.ID)
			if s.CtxTokens != spec.MaxModelLen {
				return fmt.Errorf("%s declares ctx_tokens %d but the vllm seat serves max_model_len %d", at, s.CtxTokens, spec.MaxModelLen)
			}
			if s.MaxInflight != 0 && s.MaxInflight != spec.MaxNumSeqs {
				return fmt.Errorf("%s declares max_inflight %d but the vllm seat runs max_num_seqs %d", at, s.MaxInflight, spec.MaxNumSeqs)
			}
			if normDevices(s.Device) != normDevices(spec.Device) {
				return fmt.Errorf("%s pins device %q but the vllm seat pins %q", at, dashDevice(s.Device), dashDevice(spec.Device))
			}
		}
	}
	return nil
}

// normDevices canonicalises a CUDA_VISIBLE_DEVICES list as a SET ("2,0" == "0,2"); an
// empty pin is the seat's implicit card 0, matching how the seat itself renders it.
func normDevices(d string) string {
	var out []string
	for _, p := range strings.Split(d, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return "0"
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func dashDevice(d string) string {
	if strings.TrimSpace(d) == "" {
		return "0 (implicit)"
	}
	return d
}

// layerSingle is the planner-default layer: placement row 5b hands every agent
// contract of a box that declares no pair to its agent seat.
const layerSingle = "single"

// VLLMSeatSet is a tier's declared vLLM seats and which of them a box runs — the one
// input a layer resolution needs, shared by the seed (Resolve) and the render check
// (install render), so both reason about the same layers.
type VLLMSeatSet struct {
	// Primary is the tier's lane seat (Profile.VLLMSeat); PrimaryActive says whether
	// this box runs it (Options.VLLMSeatActive).
	Primary       *vllmseat.Spec
	PrimaryActive bool
	// Extras are the tier's extra seats and ExtraActive which of them, by id, the box runs.
	Extras      []vllmseat.Spec
	ExtraActive map[string]bool
}

// specNamed finds the declared vLLM seat a layer seat's model names, by id or alias.
func (s VLLMSeatSet) specNamed(name string) (vllmseat.Spec, bool) {
	if name == "" {
		return vllmseat.Spec{}, false
	}
	all := s.Extras
	if s.Primary != nil {
		all = append([]vllmseat.Spec{*s.Primary}, s.Extras...)
	}
	for _, spec := range all {
		if spec.ID == name || containsID(spec.Aliases, name) {
			return spec, true
		}
	}
	return vllmseat.Spec{}, false
}

// running reports whether this box runs the seat with the given id.
func (s VLLMSeatSet) running(id string) bool {
	if s.Primary != nil && s.Primary.ID == id {
		return s.PrimaryActive
	}
	return s.ExtraActive[id]
}

// describe names the combination for an error: which seats are assumed running.
func (s VLLMSeatSet) describe() string {
	if len(s.Extras) == 0 {
		return fmt.Sprintf("vllm_seat active=%v", s.PrimaryActive)
	}
	var on []string
	for _, e := range s.Extras {
		if s.ExtraActive[e.ID] {
			on = append(on, e.ID)
		}
	}
	return fmt.Sprintf("vllm_seat active=%v, extra seats running %v", s.PrimaryActive, on)
}

// activityCombos enumerates every way a box can run this tier's vLLM seats: the lane
// seat present or absent, and each extra seat present or absent on its own.
func activityCombos(p Profile) []VLLMSeatSet {
	n := len(p.ExtraVLLMSeats)
	var out []VLLMSeatSet
	for _, primary := range []bool{true, false} {
		for mask := 0; mask < 1<<n; mask++ {
			active := map[string]bool{}
			for i, e := range p.ExtraVLLMSeats {
				if mask&(1<<i) != 0 {
					active[e.ID] = true
				}
			}
			out = append(out, VLLMSeatSet{Primary: p.VLLMSeat, PrimaryActive: primary, Extras: p.ExtraVLLMSeats, ExtraActive: active})
		}
	}
	return out
}

// ResolveLayers turns a tier's DECLARED layers into what a box actually serves. It is
// exported because the seed is not its only consumer: `install render` checks the
// rendered serving config against these same layers.
//
//   - A bare agent seat ({"role": "agent"}) derives from the lane seat exactly as
//     FillPairAgent does: the vLLM seat when the box runs it, else its fallback.
//   - A seat whose model NAMES a declared vLLM seat (by id or alias) is served only
//     while that seat runs: otherwise it is dropped, and a layer left with no seat is
//     dropped with it. The seeded config never advertises a layer the box cannot serve.
//   - `single` is the planner-default layer (placement row 5b: a box that declares no
//     pair routes every agent contract to it). A layer set that declared `single` and
//     lost it would make the node ineligible for every contract it ran the day before,
//     so the whole set resolves to none — the box is then a plain, non-composite box.
//
// The input is never mutated: one Profile resolves for many boxes.
func ResolveLayers(layers []config.LayerSpec, s VLLMSeatSet) []config.LayerSpec {
	filled := fillPairAgent(layers, s.Primary, s.PrimaryActive)
	var kept []config.LayerSpec
	for _, l := range filled {
		var seats []config.LayerSeat
		for _, seat := range l.Seats {
			if spec, named := s.specNamed(seat.Model); named && !s.running(spec.ID) {
				continue
			}
			seats = append(seats, seat)
		}
		if len(seats) == 0 {
			continue
		}
		l.Seats = seats
		kept = append(kept, l)
	}
	if declaresLayer(layers, layerSingle) && !declaresLayer(kept, layerSingle) {
		return nil
	}
	return kept
}

func declaresLayer(layers []config.LayerSpec, name string) bool {
	for _, l := range layers {
		if l.Name == name {
			return true
		}
	}
	return false
}

// containsID is the membership test Validate shares between composes and layers.
func containsID(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Resolve renders one tier's seed for a target machine: overlay applied, tokens
// expanded, vae_mode translated, and the whole thing validated. The result is ready
// to merge into a config.json.
func Resolve(p Profile, id string, opt Options) (map[string]any, error) {
	if err := mediaseat.Validate(p.MediaSeats, id); err != nil {
		return nil, err
	}
	merged := map[string]any{}
	for k, v := range p.ConfigSeed {
		merged[k] = v
	}
	if opt.RAMTier == "mid" || opt.RAMTier == "high" {
		for k, v := range p.ConfigSeedMidHigh {
			merged[k] = v
		}
	}
	// The agent planner seat DERIVES from resident_tier: the table has claimed
	// "resident_tier ... is the agent's default" since the field existed, and the
	// installers print it as a manual -model hint — this makes the claim true in
	// config. Rules keep the fallback chain LIVE where it already worked:
	//   - an explicit config_seed.agent_model (or config_seed_ram_mid_high value,
	//     including an explicit "" blank-out) always wins. The overlay exists ONLY
	//     for mid/high RAM — low/min RAM has no overlay at all, so nothing here can
	//     blank a seat on the boxes that drop the big model; the low-RAM guard is
	//     the table lint (TestNo26BAgentSeatOnRAMDroppable26B), which forbids a 26B
	//     seat on any row whose 26B placement is RAM-droppable;
	//   - derive only when resident_tier DIFFERS from the row's effective
	//     workhorse — materializing agent_model=workhorse would silently fork the
	//     live fallback (an operator changing `model` expects the planner to follow).
	// A declared vLLM agent seat OVERRIDES config_seed's agent_model, unlike every
	// other seed key. config_seed carries the llama.cpp FALLBACK — which is what a box
	// without the venv must get — so the seat has to win when the box can run it, or
	// the tier would render the seat into llama-swap and then route the agent lane at
	// a different model. That split is the exact defect this field exists to close.
	if p.VLLMSeat != nil {
		b := p.VLLMSeat.FallbackBindings()
		if opt.VLLMSeatActive {
			// The seat's own validation runs HERE too, not only under
			// `install vllm-seat` (Artifacts): `install seed` is the path that
			// writes a box's config.json, and a bound-lane value the harness
			// config would refuse must fail at render, never land on a box
			// (review finding, ADR 0049 Amendment 3).
			if err := p.VLLMSeat.Validate(id); err != nil {
				return nil, err
			}
			b = p.VLLMSeat.Bindings()
		}
		for k, v := range b {
			merged[k] = v
		}
	}
	// The extra seats a box runs join the roster and the bindings AFTER the lane seat's
	// own (the roster's order is the table's). They never touch agent_model: only the
	// lane seat is the agent lane. A box that runs an extra seat but not the lane seat
	// has a roster of just that seat, and `doctor` still finds every roster seat bound.
	roster, binds, err := extraSeatSeeds(p, id, opt)
	if err != nil {
		return nil, err
	}
	if len(roster) > 0 {
		have, _ := merged["vllm_seats"].([]string)
		merged["vllm_seats"] = append(append([]string{}, have...), roster...)
		bound, _ := merged["kv_cache_server"].([]map[string]any)
		merged["kv_cache_server"] = append(append([]map[string]any{}, bound...), binds...)
	}
	if _, set := merged["agent_ctx_tokens"]; !set && p.AgentCtxTokens > 0 {
		merged["agent_ctx_tokens"] = p.AgentCtxTokens
	}
	if _, explicit := merged["agent_model"]; !explicit && p.ResidentTier != "" {
		workhorse := config.Default().Model
		if m, ok := merged["model"].(string); ok && m != "" {
			workhorse = m
		}
		if p.ResidentTier != workhorse {
			merged["agent_model"] = p.ResidentTier
		}
	}
	if len(merged) == 0 && len(p.MediaSeats) == 0 && len(p.Layers) == 0 {
		return nil, nil // a text-only tier is a legitimate answer, not an error
	}
	if err := validate(merged, p.Backend, id); err != nil {
		return nil, err
	}

	goos := opt.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	exe := ""
	if goos == "windows" {
		exe = ".exe"
	}
	home := strings.TrimRight(strings.ReplaceAll(opt.Home, `\`, "/"), "/")

	out := map[string]any{}
	for k, v := range merged {
		if k == "vae_mode" {
			continue // translated below, never emitted as a config key
		}
		out[k] = expand(v, home, exe)
	}
	if mode, ok := merged["vae_mode"].(string); ok {
		if flag := vaeArgs[mode]; flag != "" {
			out["sdcpp_extra_args"] = appendArg(out["sdcpp_extra_args"], flag)
		}
	}
	// The seat is the sole writer of its binding, so this lands LAST and
	// unconditionally: one declaration produces both the llama-swap seat and the
	// config key that routes to it, and the two cannot disagree.
	for k, v := range mediaseat.Bindings(p.MediaSeats) {
		out[k] = v
	}
	// A composite tier seeds its identity and its layers LAST, like the media
	// bindings: composes/layers are their sole writer (validate refuses the keys
	// in config_seed), so what lands in config is exactly what the table declares
	// plus the one derived seat. Validated here as config.Load would, so a table
	// that seeds a layer set config refuses dies at authoring time, not on the
	// first install of the tier.
	if len(p.Layers) > 0 {
		// A box whose vLLM seats do not run seeds only the layers it can serve — and none
		// at all when the planner-default layer is not among them (ResolveLayers), in
		// which case it is a plain box and carries no composite identity either.
		layers := ResolveLayers(p.Layers, VLLMSeatSet{
			Primary: p.VLLMSeat, PrimaryActive: opt.VLLMSeatActive,
			Extras: p.ExtraVLLMSeats, ExtraActive: opt.ExtraVLLMSeatsActive,
		})
		if len(layers) > 0 {
			tiers := append(append([]string{}, p.Composes...), id)
			if err := (config.Config{TierProfile: id, Tiers: tiers, Layers: layers}).ValidateLayers(); err != nil {
				return nil, fmt.Errorf("tier %q layers: %w", id, err)
			}
			out["tier_profile"] = id
			out["tiers"] = tiers
			out["layers"] = layers
		}
	}
	return out, nil
}

// extraSeatSeeds is the roster and the cache-server bindings the tier's RUNNING extra
// seats contribute, in table order. Each running seat is validated as a non-lane seat
// here too — `install seed` is the path that writes a box's config.json, and a seat the
// box will run must fail at render, never land on it half-specified.
func extraSeatSeeds(p Profile, tier string, opt Options) (roster []string, binds []map[string]any, err error) {
	for i := range p.ExtraVLLMSeats {
		e := p.ExtraVLLMSeats[i]
		if !opt.ExtraVLLMSeatsActive[e.ID] {
			continue
		}
		if err := e.ValidateExtra(tier); err != nil {
			return nil, nil, err
		}
		roster = append(roster, e.ID)
		binds = append(binds, e.ConfigBinding())
	}
	return roster, binds, nil
}

// Accelerator is one profiles.json `accelerators` entry: an additive device
// beside the GPU tier (ADR 0024). Its seed merges AFTER the tier's own seed.
type Accelerator struct {
	Kind       string         `json:"kind"`
	Owns       []string       `json:"owns"`
	ConfigSeed map[string]any `json:"config_seed"`
	Notes      string         `json:"notes"`
}

// ResolveAccelerators merges the seeds of the listed accelerator ids, validates
// every key against config.Config, and expands __HAILO_HOME__ / __CORAL_HOME__ /
// __RKNPU_HOME__ (plus the usual __OFFLOAD_HOME__/__EXE__). An id with no entry is an authoring error — an
// installer that detected a device the table does not describe must say so.
//
// This function is the single authority on the rule; install.ps1 carries a
// PowerShell parity copy for the no-Go-binary path — change it here first.
func ResolveAccelerators(accs map[string]Accelerator, ids []string, opt Options) (map[string]any, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	merged := map[string]any{}
	// Every device's seed lists its own id under "accelerators" — the gate. Merged key by
	// key the last device's list would replace the others', so the gate is the union, in
	// the order the ids were given (that order is the shared-name rule's, ADR 0037).
	var gate []any
	for _, id := range ids {
		a, ok := accs[id]
		if !ok {
			return nil, fmt.Errorf("accelerator %q detected but not declared in profiles.json accelerators", id)
		}
		for k, v := range a.ConfigSeed {
			if list, isList := v.([]any); k == "accelerators" && isList {
				for _, e := range list {
					if !slices.ContainsFunc(gate, func(g any) bool { return reflect.DeepEqual(g, e) }) {
						gate = append(gate, e)
					}
				}
				continue
			}
			merged[k] = v
		}
	}
	if gate != nil {
		merged["accelerators"] = gate
	}
	if err := validate(merged, "", "accelerators:"+strings.Join(ids, "+")); err != nil {
		return nil, err
	}
	goos := opt.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	exe := ""
	if goos == "windows" {
		exe = ".exe"
	}
	home := strings.TrimRight(strings.ReplaceAll(opt.Home, `\`, "/"), "/")
	homeOf := func(h string) string { return strings.TrimRight(strings.ReplaceAll(h, `\`, "/"), "/") }
	// One row per device: the token a seed value carries, the option that fills it.
	homes := []struct{ token, option, dir string }{
		{"__HAILO_HOME__", "HailoHome", homeOf(opt.HailoHome)},
		{"__CORAL_HOME__", "CoralHome", homeOf(opt.CoralHome)},
		{"__RKNPU_HOME__", "RknpuHome", homeOf(opt.RknpuHome)},
	}
	out := map[string]any{}
	for k, v := range merged {
		ev := expand(v, home, exe)
		if s, ok := ev.(string); ok {
			for _, h := range homes {
				// A home token left EMPTY would render "/coral-http.sh" — a launcher
				// at the filesystem root that nothing ever installed, and a sidecar
				// that never spawns with no hint why. Refuse the render instead.
				if strings.Contains(s, h.token) && h.dir == "" {
					return nil, fmt.Errorf("accelerator seed key %q uses %s but no %s was given", k, h.token, h.option)
				}
				s = strings.ReplaceAll(s, h.token, h.dir)
			}
			ev = s
		}
		out[k] = ev
	}
	return out, nil
}

// validate is where a bad seed dies — at authoring time, not on someone's machine.
func validate(seed map[string]any, backend, id string) error {
	known := configKeys()
	bound := map[string]bool{}
	for _, k := range mediaseat.BoundKeys() {
		bound[k] = true
	}
	var problems []string
	for _, k := range sortedKeys(seed) {
		if k == "vae_mode" {
			continue // a seed-only directive, translated to sdcpp_extra_args
		}
		if bound[k] {
			problems = append(problems, fmt.Sprintf("%q is written by a media_seat, not by config_seed — "+
				"declare the seat instead. Two writers is how the binding and the seat it names drifted apart", k))
			continue
		}
		if compositeKeys[k] {
			problems = append(problems, fmt.Sprintf("%q is written by the tier's composes/layers, not by config_seed — "+
				"declare them on the profile instead. Two writers is how a binding and the seat it names drift apart", k))
			continue
		}
		if !known[k] {
			problems = append(problems, fmt.Sprintf("unknown key %q (not a harness config field — it would be dropped on every install of this tier)", k))
		}
		if s, ok := seed[k].(string); ok && strings.Contains(strings.ToLower(s), ".exe") {
			problems = append(problems, fmt.Sprintf("%s carries a literal \".exe\" — use the %s token so the tier renders on every OS", k, TokenExe))
		}
	}
	// A seeded agent_profile is a NAME the agent loop must be able to resolve. The key
	// check above only proves it is a real config field, so "reserch" would ship in a
	// tier template and surface as a per-call defer on every box that installed it.
	// Validate the VALUE here, at authoring time, exactly as vae_mode does below.
	if v, ok := seed["agent_profile"]; ok {
		s, isStr := v.(string)
		switch {
		case !isStr:
			problems = append(problems, "agent_profile must be a string")
		case strings.TrimSpace(s) != s:
			problems = append(problems, fmt.Sprintf("agent_profile %q has surrounding whitespace", s))
		case s != "":
			if _, err := agent.LookupProfile(s); err != nil {
				problems = append(problems, "agent_profile: "+err.Error())
			}
		}
	}
	if mode, ok := seed["vae_mode"]; ok {
		s, isStr := mode.(string)
		if !isStr {
			problems = append(problems, "vae_mode must be a string (tiling|cpu|none)")
		} else if _, valid := vaeArgs[s]; !valid {
			problems = append(problems, fmt.Sprintf("vae_mode %q is not one of tiling|cpu|none", s))
		} else if s == "cpu" && strings.Contains(backend, "cuda") {
			problems = append(problems, "vae_mode \"cpu\" on a CUDA backend: measured 7.8x slower (58.2s vs 7.5s with tiling). "+
				"It is correct on an AMD/UMA part and a trap everywhere else — use \"tiling\"")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("tier %q config_seed:\n  - %s", id, strings.Join(problems, "\n  - "))
	}
	return nil
}

// compositeKeys are the config keys Resolve derives from a profile's
// composes/layers. A config_seed (or accelerator seed) carrying one is a second
// writer of the box's identity, refused by name exactly like a media binding.
var compositeKeys = map[string]bool{"tier_profile": true, "tiers": true, "layers": true}

// FillPairAgent resolves the tier table's BARE agent seat — the one a
// composite tier leaves as `{"role": "agent"}` so the seat's numbers live in
// exactly one place (vllm_seat) — into a full seat: the vLLM seat's first
// alias, window and max_num_seqs when the box actually runs it, the tier's
// declared fallback when it does not. Exported because the SEED is not the
// only consumer: `install render` checks the rendered config against the same
// resolved layers (servingtmpl.CheckComposite), and a check run against the
// bare declaration would report the agent seat as placeable on nothing.
// The input is never mutated.
func FillPairAgent(layers []config.LayerSpec, seat *vllmseat.Spec, active bool) []config.LayerSpec {
	return fillPairAgent(layers, seat, active)
}

// fillPairAgent derives a layer's BARE agent seat ({"role": "agent"} with no
// model) from the tier's vLLM seat, so the delegation seat's model, window,
// concurrency and device pin are declared once — in vllm_seat — and the layer
// cannot drift from the seat it stands for. Active, the seat is the vLLM pool
// (first alias, the name the fleet routes to; max_model_len; max_num_seqs);
// otherwise it is the declared llama.cpp fallback on the same device with no
// concurrency count (a llama.cpp seat is never saturated by count). A field the
// layer already declares is kept, and the table's own slice is never mutated —
// one Profile resolves for many boxes.
func fillPairAgent(layers []config.LayerSpec, seat *vllmseat.Spec, active bool) []config.LayerSpec {
	out := make([]config.LayerSpec, len(layers))
	for i, l := range layers {
		l.Seats = append([]config.LayerSeat(nil), l.Seats...)
		for j, s := range l.Seats {
			if s.Role != "agent" || s.Model != "" || seat == nil {
				continue
			}
			if active {
				s.Model = seat.ID
				if len(seat.Aliases) > 0 {
					s.Model = seat.Aliases[0]
				}
				if s.CtxTokens == 0 {
					s.CtxTokens = seat.MaxModelLen
				}
				if s.MaxInflight == 0 {
					s.MaxInflight = seat.MaxNumSeqs
				}
			} else {
				s.Model = seat.Fallback
				if s.CtxTokens == 0 {
					s.CtxTokens = seat.FallbackCtx
				}
				if s.Device == "" && seat.FallbackDevice != "" {
					s.Device = seat.FallbackDevice
				}
			}
			if s.Device == "" {
				s.Device = seat.Device
			}
			l.Seats[j] = s
		}
		out[i] = l
	}
	return out
}

// configKeys is every json tag on config.Config — the set a seed may write.
func configKeys() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(config.Config{})
	for i := 0; i < t.NumField(); i++ {
		name := strings.SplitN(t.Field(i).Tag.Get("json"), ",", 2)[0]
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

func expand(v any, home, exe string) any {
	switch t := v.(type) {
	case string:
		s := strings.ReplaceAll(t, TokenExe, exe)
		if home != "" {
			s = strings.ReplaceAll(s, TokenHome, home)
		}
		return s
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, expand(e, home, exe))
		}
		return out
	default:
		return v
	}
}

// appendArg adds a flag to sdcpp_extra_args without duplicating it, so a seed that
// still lists the flag explicitly does not get it twice.
func appendArg(existing any, flag string) []any {
	var out []any
	if cur, ok := existing.([]any); ok {
		for _, e := range cur {
			if s, ok := e.(string); ok && s == flag {
				return cur
			}
		}
		out = append(out, cur...)
	}
	return append(out, flag)
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
