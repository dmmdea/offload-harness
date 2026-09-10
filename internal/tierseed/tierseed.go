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
	// VLLMSeatActive says whether THIS box renders the tier's vLLM agent seat. The
	// caller decides it with vllmseat.Spec.Detect — the engine is a hand-built venv
	// the installer does not create, so a box without it binds the fallback seat. The
	// binding and the rendered llama-swap entry therefore agree by construction: both
	// are driven by the same detection.
	VLLMSeatActive bool
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
	// MediaSeats are the tier's alias-backed media capabilities. They are the SOLE
	// writer of the config keys they bind — see mediaseat.Bindings.
	MediaSeats []mediaseat.Seat `json:"media_seats"`
	// VLLMSeat is the tier's persistent vLLM agent seat (ADR 0035). When the box can
	// actually run it (Options.VLLMSeatActive), it is the SOLE writer of agent_model
	// and agent_ctx_tokens; otherwise its declared fallback is.
	VLLMSeat *vllmseat.Spec `json:"vllm_seat,omitempty"`
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
		for _, active := range []bool{true, false} {
			c := config.Config{TierProfile: id, Tiers: tiers, Layers: fillPairAgent(p.Layers, p.VLLMSeat, active)}
			if err := c.ValidateLayers(); err != nil {
				return fmt.Errorf("profiles.json: tier %q layers (vllm_seat active=%v): %w", id, active, err)
			}
		}
	}
	return nil
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
			b = p.VLLMSeat.Bindings()
		}
		for k, v := range b {
			merged[k] = v
		}
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
		layers := fillPairAgent(p.Layers, p.VLLMSeat, opt.VLLMSeatActive)
		tiers := append(append([]string{}, p.Composes...), id)
		if err := (config.Config{TierProfile: id, Tiers: tiers, Layers: layers}).ValidateLayers(); err != nil {
			return nil, fmt.Errorf("tier %q layers: %w", id, err)
		}
		out["tier_profile"] = id
		out["tiers"] = tiers
		out["layers"] = layers
	}
	return out, nil
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
// every key against config.Config, and expands __HAILO_HOME__ / __CORAL_HOME__
// (plus the usual __OFFLOAD_HOME__/__EXE__). An id with no entry is an authoring error — an
// installer that detected a device the table does not describe must say so.
//
// This function is the single authority on the rule; install.ps1 carries a
// PowerShell parity copy for the no-Go-binary path — change it here first.
func ResolveAccelerators(accs map[string]Accelerator, ids []string, opt Options) (map[string]any, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	merged := map[string]any{}
	for _, id := range ids {
		a, ok := accs[id]
		if !ok {
			return nil, fmt.Errorf("accelerator %q detected but not declared in profiles.json accelerators", id)
		}
		for k, v := range a.ConfigSeed {
			merged[k] = v
		}
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
	hailoHome := strings.TrimRight(strings.ReplaceAll(opt.HailoHome, `\`, "/"), "/")
	coralHome := strings.TrimRight(strings.ReplaceAll(opt.CoralHome, `\`, "/"), "/")
	out := map[string]any{}
	for k, v := range merged {
		ev := expand(v, home, exe)
		if s, ok := ev.(string); ok {
			// A home token left EMPTY would render "/coral-http.sh" — a launcher
			// at the filesystem root that nothing ever installed, and a sidecar
			// that never spawns with no hint why. Refuse the render instead.
			if strings.Contains(s, "__HAILO_HOME__") && hailoHome == "" {
				return nil, fmt.Errorf("accelerator seed key %q uses __HAILO_HOME__ but no HailoHome was given", k)
			}
			if strings.Contains(s, "__CORAL_HOME__") && coralHome == "" {
				return nil, fmt.Errorf("accelerator seed key %q uses __CORAL_HOME__ but no CoralHome was given", k)
			}
			s = strings.ReplaceAll(s, "__HAILO_HOME__", hailoHome)
			ev = strings.ReplaceAll(s, "__CORAL_HOME__", coralHome)
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
