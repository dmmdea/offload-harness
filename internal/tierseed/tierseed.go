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

// ParseDoc reads the whole document from bytes.
func ParseDoc(raw []byte) (Doc, error) {
	var d Doc
	if err := json.Unmarshal(raw, &d); err != nil {
		return Doc{}, fmt.Errorf("profiles.json: %w", err)
	}
	if len(d.Profiles) == 0 {
		return Doc{}, fmt.Errorf("profiles.json has no profiles — the schema moved")
	}
	return d, nil
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
	if _, explicit := merged["agent_model"]; !explicit && p.ResidentTier != "" {
		workhorse := config.Default().Model
		if m, ok := merged["model"].(string); ok && m != "" {
			workhorse = m
		}
		if p.ResidentTier != workhorse {
			merged["agent_model"] = p.ResidentTier
		}
	}
	if len(merged) == 0 && len(p.MediaSeats) == 0 {
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
// every key against config.Config, and expands __HAILO_HOME__ (plus the usual
// __OFFLOAD_HOME__/__EXE__). An id with no entry is an authoring error — an
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
	out := map[string]any{}
	for k, v := range merged {
		ev := expand(v, home, exe)
		if s, ok := ev.(string); ok {
			ev = strings.ReplaceAll(s, "__HAILO_HOME__", hailoHome)
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
