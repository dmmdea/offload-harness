package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/hwdetect"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// audit-config is the config.json half of the drift check that `audit-yaml --against-render`
// does for the serving YAML. It exists because the YAML half was never the problem.
//
// Measured winners were wired BY HAND into a node's config.json — binxarn's qwen3.5-4b agent seat
// and its lane keys, the Lenovo's layers and 35B digest seat, the Qube's image-edit / inpaint /
// animate routes — and never written back to profiles.json. Nothing compared the two, so the node
// kept working, the seed kept the loser, and every fresh install (and every regeneration of the
// tier matrix, which reads the seed) silently erased the win. The 2026-09-21 wiring-debt audit
// found that pattern on every node it read.
//
// It compares only SEED-OWNED keys: every key some tier's resolved seed can write. A live config
// also holds plenty of keys that are legitimately this machine's own (endpoints, paths, ports), and
// a report that flagged those would bury the real drift in noise.

// configDriftClass is one key's relationship between the live config and the tier seed.
type configDriftClass string

const (
	driftMatch     configDriftClass = "MATCH"
	driftDifferent configDriftClass = "DIFFERENT" // both set, values differ
	driftLiveOnly  configDriftClass = "LIVE-ONLY" // set by hand on the node; the seed does not carry it
	driftSeedOnly  configDriftClass = "SEED-ONLY" // the seed writes it; the node does not have it
	// UNSEEDED is a live binding that NO tier seeds at all. It is the blind spot of a seed-owned
	// comparison: the Qube's image-edit, inpaint and animate routes were wired by hand, measured, and
	// carried by no tier — so they were invisible to a check that only reads what seeds can write.
	driftUnseeded configDriftClass = "UNSEEDED"
)

// isBindingKey reports whether a config key names a seat, model or media route (as opposed to a
// node-local endpoint, path or port). It is a suffix rule on purpose: every binding the harness has
// grown so far ends in one of these, and a key that does not is left to the seed-owned comparison.
func isBindingKey(k string) bool {
	// The remote NIM lane is account configuration (an opt-in cloud escalation), not a tier seat.
	if strings.HasPrefix(k, "nim_") {
		return false
	}
	for _, suf := range []string{"_model", "_script", "_unet", "_ckpt", "_family", "_engine", "_preset",
		"_transformer", "_text_encoder", "_vae", "_endpoint"} {
		if strings.HasSuffix(k, suf) {
			return true
		}
	}
	return false
}

type configDrift struct {
	Key   string           `json:"key"`
	Class configDriftClass `json:"class"`
	Live  any              `json:"live,omitempty"`
	Seed  any              `json:"seed,omitempty"`
}

// classifyConfigDrift compares live against seed over the seed-owned keys (plus every key the seed
// itself writes). It is pure so the rules can be tested without a machine or a profiles.json.
// Values are compared after a JSON round trip, so an int in the seed and the float64 the live
// config decodes to are the same number.
func classifyConfigDrift(seed, live map[string]any, owned map[string]bool, isBinding func(string) bool) []configDrift {
	keys := map[string]bool{}
	for k := range owned {
		keys[k] = true
	}
	for k := range seed {
		keys[k] = true
	}
	var out []configDrift
	for k := range keys {
		sv, inSeed := seed[k]
		lv, inLive := live[k]
		switch {
		case inSeed && inLive:
			class := driftMatch
			if !sameJSON(sv, lv) {
				class = driftDifferent
			}
			out = append(out, configDrift{Key: k, Class: class, Live: lv, Seed: sv})
		case inLive:
			out = append(out, configDrift{Key: k, Class: driftLiveOnly, Live: lv})
		case inSeed:
			out = append(out, configDrift{Key: k, Class: driftSeedOnly, Seed: sv})
		}
	}
	// Live bindings no tier seeds at all: the other half of "a win that exists only on the node".
	if isBinding != nil {
		for k, lv := range live {
			if str, isStr := lv.(string); isStr && str == "" {
				continue // an empty route is unbound, not a hand-wired binding
			}
			if !keys[k] && isBinding(k) {
				out = append(out, configDrift{Key: k, Class: driftUnseeded, Live: lv})
			}
		}
	}
	rank := map[configDriftClass]int{driftDifferent: 0, driftLiveOnly: 1, driftUnseeded: 2, driftSeedOnly: 3, driftMatch: 4}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Class] != rank[out[j].Class] {
			return rank[out[i].Class] < rank[out[j].Class]
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func sameJSON(a, b any) bool {
	na, errA := normJSON(a)
	nb, errB := normJSON(b)
	if errA != nil || errB != nil {
		return false
	}
	return reflect.DeepEqual(na, nb)
}

func normJSON(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(raw, &out)
	return out, err
}

// seedOwnedKeys is every key that any tier's resolved seed writes — the vocabulary a tier is
// responsible for. A tier whose Resolve fails is skipped, not fatal: the audit must still run on
// the tier the node actually carries.
func seedOwnedKeys(doc tierseed.Doc, home string) map[string]bool {
	owned := map[string]bool{}
	for name, p := range doc.Profiles {
		seed, err := tierseed.Resolve(p, name, tierseed.Options{Home: home})
		if err != nil {
			continue
		}
		for k := range seed {
			owned[k] = true
		}
	}
	return owned
}

var errConfigDrift = errors.New("config drift")

func runAuditConfig(args []string) error {
	fs := flag.NewFlagSet("audit-config", flag.ExitOnError)
	cfgFlag := fs.String("config", "", "config file to audit (default: the harness's own resolution — $LOCAL_OFFLOAD_CONFIG, ./config.json, ~/.local-offload/config.json)")
	tierFlag := fs.String("tier", "", "tier to audit against (default: the config's tier_profile, else this machine's detected tier)")
	root := fs.String("root", ".", "repo root holding setup/templates/profiles.json")
	home := fs.String("home", "", "install root the seed should assume (default: the resolved harness home)")
	asJSON := fs.Bool("json", false, "emit the findings as JSON")
	showMatch := fs.Bool("all", false, "also list keys that MATCH")
	// The same three inputs `install seed` resolves with. Without them the audit compares the node
	// against a seed the installer would never have written - e.g. a vLLM box against its FALLBACK
	// agent - and reports drift that is its own artifact.
	goos := fs.String("goos", "", "target OS whose seed to compare against (default: this platform)")
	ramTier := fs.String("ram-tier", "", "RAM tier overlay the installer would apply (mid | high; default: none)")
	vllmSeat := fs.String("vllm-seat-active", "auto", "whether this node serves the tier's vLLM agent seat: auto (detect locally, as install does) | true | false")
	vllmVenv := fs.String("vllm-venv", "", "hand-built vLLM virtualenv for auto detection (default: <home>/vllm-env)")
	hfHome := fs.String("hf-home", "", "HF cache root for auto detection (default: $HF_HOME, else <home>/hf)")
	_ = fs.Parse(args)

	userHome, _ := os.UserHomeDir()
	exists := func(p string) bool { info, err := os.Stat(p); return err == nil && !info.IsDir() }
	path := config.ResolvePath(*cfgFlag, os.Getenv("LOCAL_OFFLOAD_CONFIG"), userHome, exists)
	if path == "" {
		return fmt.Errorf("audit-config: no config file found (no --config, no $LOCAL_OFFLOAD_CONFIG, no ./config.json, no ~/.local-offload/config.json)")
	}
	rawCfg, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("audit-config: read %s: %w", path, err)
	}
	// PowerShell writes UTF-8 WITH a BOM; encoding/json rejects one. Every Windows node's config
	// would otherwise fail to parse here while the harness itself reads it fine.
	rawCfg = bytes.TrimPrefix(rawCfg, []byte{0xEF, 0xBB, 0xBF})
	var live map[string]any
	if err := json.Unmarshal(rawCfg, &live); err != nil {
		return fmt.Errorf("audit-config: %s is not valid JSON: %w", path, err)
	}

	installHome := *home
	if installHome == "" {
		installHome = config.DefaultBase()
	}
	rawProfiles, err := profilesJSON(*root)
	if err != nil {
		return err
	}
	doc, err := tierseed.ParseDoc(rawProfiles)
	if err != nil {
		return err
	}

	tier, tierSource := *tierFlag, "--tier"
	if tier == "" {
		if t, ok := live["tier_profile"].(string); ok && t != "" {
			tier, tierSource = t, "the config's tier_profile"
		} else {
			tier, tierSource = hwdetect.Classify(hwdetect.Detect()).Profile, "this machine's detected tier"
		}
	}
	p, ok := doc.Profiles[tier]
	if !ok {
		return fmt.Errorf("audit-config: tier %q (from %s) is not in profiles.json", tier, tierSource)
	}
	vllmActive := false
	switch *vllmSeat {
	case "true":
		vllmActive = true
	case "false":
	case "auto":
		if p.VLLMSeat != nil {
			rt := vllmRuntimeFlags{venv: *vllmVenv, hfHome: *hfHome}.resolve(installHome)
			vllmActive, _ = p.VLLMSeat.Detect(rt)
		}
	default:
		return fmt.Errorf("audit-config: --vllm-seat-active must be auto, true or false, got %q", *vllmSeat)
	}
	seed, err := tierseed.Resolve(p, tier, tierseed.Options{
		Home: installHome, GOOS: *goos, RAMTier: *ramTier, VLLMSeatActive: vllmActive,
	})
	if err != nil {
		return fmt.Errorf("audit-config: resolve %s: %w", tier, err)
	}

	findings := classifyConfigDrift(seed, live, seedOwnedKeys(doc, installHome), isBindingKey)
	drifted := 0
	for _, f := range findings {
		if f.Class != driftMatch {
			drifted++
		}
	}

	if *asJSON {
		b, err := json.MarshalIndent(map[string]any{
			"config": path, "tier": tier, "tier_source": tierSource, "drifted": drifted, "findings": findings,
		}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	} else {
		fmt.Printf("audit-config: %s against tier %s (from %s; goos=%q ram-tier=%q vllm-seat-active=%v)\n",
			path, tier, tierSource, *goos, *ramTier, vllmActive)
		for _, f := range findings {
			if f.Class == driftMatch && !*showMatch {
				continue
			}
			fmt.Printf("  %-10s %-28s live=%s  seed=%s\n", f.Class, f.Key, shortJSON(f.Live), shortJSON(f.Seed))
		}
		fmt.Printf("%d seed-owned key(s) drifted; %d match\n", drifted, len(findings)-drifted)
	}
	if drifted > 0 {
		return fmt.Errorf("%w: %d key(s) differ between %s and the %s seed", errConfigDrift, drifted, path, tier)
	}
	return nil
}

func shortJSON(v any) string {
	if v == nil {
		return "-"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	s := string(b)
	if len(s) > 70 {
		s = s[:67] + "..."
	}
	return strings.ReplaceAll(s, "\n", " ")
}
