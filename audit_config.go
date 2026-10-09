package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/hwdetect"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// audit-config is the config.json half of the drift check that `audit-yaml --against-render`
// does for the serving YAML. It exists because the YAML half was never the problem.
//
// Measured winners were wired BY HAND into a node's config.json — <node-f>'s qwen3.5-4b agent seat
// and its lane keys, <node-c>'s layers and 35B digest seat, <node-b>'s image-edit / inpaint /
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
	// comparison: <node-b>'s image-edit, inpaint and animate routes were wired by hand, measured, and
	// carried by no tier — so they were invisible to a check that only reads what seeds can write.
	driftUnseeded configDriftClass = "UNSEEDED"
)

// isBindingKey reports whether a config key names a seat, model or media route (as opposed to a
// node-local endpoint, path or port). It is a suffix rule on purpose: every binding the harness has
// grown so far ends in one of these, and a key that does not is left to the seed-owned comparison.
// `_endpoint` is deliberately NOT in the list: tts_endpoint and pair_workloads_endpoint are this
// box's own service URLs (opt-in, empty = the lane is absent), and hailo_/coral_/rknpu_endpoint are
// owned by the accelerator seeds — flagging any of them as a hand-wired seat was a false positive.
func isBindingKey(k string) bool {
	// The remote NIM lane is account configuration (an opt-in cloud escalation), not a tier seat.
	if strings.HasPrefix(k, "nim_") {
		return false
	}
	for _, suf := range []string{"_model", "_script", "_unet", "_ckpt", "_family", "_engine", "_preset",
		"_transformer", "_text_encoder", "_vae"} {
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
			if !sameConfigValue(k, sv, lv) {
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

// sameConfigValue compares one key's seed value with its live value. Every key is compared whole, as sameJSON does,
// except kv_cache_server: the key_prefix of each of its bindings is the node's own (a seeded node keeps the prefix it was
// seeded with, and install does not rewrite config.json), so a prefix-only difference is not drift. The exception is narrow
// on purpose: it applies only when BOTH sides are lists of binding objects. A legacy single object (still accepted by the
// loader), a string, null, a list holding anything that is not an object, or a list against a non-list is compared whole,
// exactly as before, so a difference of shape still reads DIFFERENT.
func sameConfigValue(key string, seedVal, liveVal any) bool {
	if key != "kv_cache_server" {
		return sameJSON(seedVal, liveVal)
	}
	ns, errS := normJSON(seedVal)
	nl, errL := normJSON(liveVal)
	if errS != nil || errL != nil {
		return false
	}
	if bs, ok := withoutKeyPrefix(ns); ok {
		if bl, ok := withoutKeyPrefix(nl); ok {
			return reflect.DeepEqual(bs, bl)
		}
	}
	return reflect.DeepEqual(ns, nl)
}

// withoutKeyPrefix returns a copy of v with key_prefix removed from every element, when v is a list whose every element
// is an object; otherwise it reports false. The value it is given is never modified.
func withoutKeyPrefix(v any) ([]any, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]any, len(list))
	for i, el := range list {
		m, ok := el.(map[string]any)
		if !ok {
			return nil, false
		}
		cp := make(map[string]any, len(m))
		for k, val := range m {
			if k != "key_prefix" {
				cp[k] = val
			}
		}
		out[i] = cp
	}
	return out, true
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
	// Accelerator rows (hailo-8l, coral-edgetpu, rknpu) seed their own keys, so the audit owns
	// them too; withLiveAccelerators puts the seed of the devices a node lists into what the node
	// is compared with. Resolving needs every device's home (accelOptions) — with any one empty
	// the whole resolution failed and this skipped it, so no accelerator key was ever owned.
	ids := make([]string, 0, len(doc.Accelerators))
	for id := range doc.Accelerators {
		ids = append(ids, id)
	}
	if accSeed, err := tierseed.ResolveAccelerators(doc.Accelerators, ids, accelOptions(tierseed.Options{Home: home}, "", "", "")); err == nil {
		for k := range accSeed {
			owned[k] = true
		}
	}
	return owned
}

// withLiveAccelerators returns seed with the seed of every accelerator the live config lists merged
// over it — what an install writes, since accelerators ride beside the tier and their seeds merge
// after it (ADR 0024). Compared against the tier's seed alone, every accelerator key a seeded node
// carries would read LIVE-ONLY. An id this build's profiles.json does not declare has no seed to
// compare with and is skipped, not an error.
func withLiveAccelerators(seed, live map[string]any, doc tierseed.Doc, home, goos string) (map[string]any, error) {
	listed, _ := live["accelerators"].([]any)
	var ids []string
	for _, v := range listed {
		if id, ok := v.(string); ok {
			if _, declared := doc.Accelerators[id]; declared {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return seed, nil
	}
	accSeed, err := tierseed.ResolveAccelerators(doc.Accelerators, ids, accelOptions(tierseed.Options{Home: home, GOOS: goos}, "", "", ""))
	if err != nil {
		return nil, err
	}
	merged := make(map[string]any, len(seed)+len(accSeed))
	for k, v := range seed {
		merged[k] = v
	}
	for k, v := range accSeed {
		merged[k] = v
	}
	return merged, nil
}

var errConfigDrift = errors.New("config drift")

// auditDetectRAMTier is this machine's RAM tier (min|low|mid|high), the same value `detect` stamps.
// It is a variable so a test can speak for a box of another size.
var auditDetectRAMTier = func() (tier string, ramGb int, err error) {
	f := auditDetectFacts()
	// A probe that fails reads 0 GB, which classifies as tier min: the base seed, chosen without a word.
	// That is a guess about the audited node, so refuse it and ask for the tier by name.
	if f.RAMGb <= 0 {
		return "", 0, errors.New("audit-config: the RAM probe read 0 GB on this machine, so there is no RAM tier to detect (0 GB would pass for tier min); pass --ram-tier min, low, mid, high or none explicitly")
	}
	return hwdetect.Classify(f).RAMTier, f.RAMGb, nil
}

// auditDetectFacts is the hardware probe behind auditDetectRAMTier, a variable so a test can make it fail.
var auditDetectFacts = hwdetect.Detect

// auditRAMResolution is which RAM tier an audit compared against and where that came from.
type auditRAMResolution struct {
	Tier       string // min|low|mid|high, or "none" when the overlay was switched off
	Source     string // "detected" or "--ram-tier"
	Overlay    string // the RAM tier handed to tierseed.Options: "low"|"mid"|"high", or "" for the base seed alone
	DetectedGb int    // the RAM this machine's probe read, in GB; 0 when the tier was named, not detected
}

// resolveAuditRAMTier turns --ram-tier into the overlay the audit compares. Empty and "auto" mean the
// tier this box detects: the installer applies the RAM overlays on a low/mid/high box (config_seed_ram_low_up
// from low up, config_seed_ram_mid_high from mid up), so an audit of the BASE seed alone calls every
// overlay-carried key drift (23 of 38 rows on an 8 GB card with 64 GB RAM).
// "none" keeps the base-seed comparison selectable. Low, mid and high each select overlays (tierseed):
// config_seed_ram_low_up applies on all three, config_seed_ram_mid_high on mid and high only.
func resolveAuditRAMTier(flag string, detect func() (string, int, error)) (auditRAMResolution, error) {
	v := strings.ToLower(strings.TrimSpace(flag))
	source, detectedGb := "--ram-tier", 0
	if v == "" || v == "auto" {
		tier, gb, err := detect()
		if err != nil {
			return auditRAMResolution{}, err
		}
		v, source, detectedGb = strings.ToLower(strings.TrimSpace(tier)), "detected", gb
	}
	switch v {
	case "low", "mid", "high":
		return auditRAMResolution{Tier: v, Source: source, Overlay: v, DetectedGb: detectedGb}, nil
	case "min", "none":
		return auditRAMResolution{Tier: v, Source: source, DetectedGb: detectedGb}, nil
	}
	return auditRAMResolution{}, fmt.Errorf("audit-config: --ram-tier must be auto, none, min, low, mid or high, got %q (%s)", flag, source)
}

// ramTierIsThisMachines is the warning an audit owes when the tier it compared came from THIS machine's
// RAM probe but the config, platform or install root it audits can belong to another node. "" when there
// is nothing to say: the tier was named, or the audit points at this machine's own config and platform.
func ramTierIsThisMachines(r auditRAMResolution, cfgFlag, goos, home string) string {
	if r.Source != "detected" {
		return ""
	}
	if cfgFlag == "" && home == "" && (goos == "" || goos == runtime.GOOS) {
		return ""
	}
	return fmt.Sprintf("audit-config: warning: the RAM overlay compared (ram-tier=%s, %d GB) is this machine's RAM tier, not necessarily the audited node's; pass --ram-tier min, low, mid, high or none to compare another node's config",
		r.Tier, r.DetectedGb)
}

// describe says in words which seed the audit compared: the overlay's own name, or the base alone.
func (r auditRAMResolution) describe() string {
	switch r.Overlay {
	case "low":
		return "base seed + config_seed_ram_low_up overlay (where the tier declares one)"
	case "mid", "high":
		return "base seed + config_seed_ram_low_up and config_seed_ram_mid_high overlays (where the tier declares them)"
	}
	return "base seed only, no RAM overlay"
}

// sourceLabel is the source with the RAM the probe read when the tier was detected.
func (r auditRAMResolution) sourceLabel() string {
	if r.Source == "detected" {
		return fmt.Sprintf("detected, %d GB", r.DetectedGb)
	}
	return r.Source
}

// ramOverlayName is the JSON spelling of the overlay compared: its config key, or "none".
func ramOverlayName(r auditRAMResolution) string {
	switch r.Overlay {
	case "low":
		return "config_seed_ram_low_up"
	case "mid", "high":
		return "config_seed_ram_mid_high"
	}
	return "none"
}

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
	ramTier := fs.String("ram-tier", "", "RAM tier whose overlay to compare against: auto (THIS machine's detected RAM tier, as install applies; the default; pass a tier for another node's config) | none (the base seed alone) | min | low | mid | high")
	vllmSeat := fs.String("vllm-seat-active", "auto", "whether this node serves the tier's vLLM agent seat: auto (detect locally, as install does) | true | false")
	vllmVenv := fs.String("vllm-venv", "", "hand-built vLLM virtualenv for auto detection (default: <home>/vllm-env)")
	hfHome := fs.String("hf-home", "", "HF cache root for auto detection (default: $HF_HOME, else <home>/hf)")
	vllmSeatDir := fs.String("vllm-seat-dir", "", "where the vLLM seats' wrapper scripts live, for auto detection of a tier's extra seats (default: <home>/seat)")
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
	// PowerShell 5.1 writes UTF-8 WITH a BOM; encoding/json rejects one. The harness's own
	// loader strips it the same way (config.StripBOM), so this audit reads what it reads.
	rawCfg = config.StripBOM(rawCfg)
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
	// The flag speaks for the tier's vLLM seats as a set: `true` says the node serves them all
	// (the lane seat and every extra seat), `false` none, `auto` detects each one locally.
	vllmActive := false
	extraActive := map[string]bool{}
	switch *vllmSeat {
	case "true":
		vllmActive = true
		for _, e := range p.ExtraVLLMSeats {
			extraActive[e.ID] = true
		}
	case "false":
	case "auto":
		vllmActive, extraActive = detectVLLMSeats(p, vllmRuntimeFlags{venv: *vllmVenv, hfHome: *hfHome, seatDir: *vllmSeatDir}.resolve(installHome), io.Discard)
	default:
		return fmt.Errorf("audit-config: --vllm-seat-active must be auto, true or false, got %q", *vllmSeat)
	}
	ram, err := resolveAuditRAMTier(*ramTier, auditDetectRAMTier)
	if err != nil {
		return err
	}
	if w := ramTierIsThisMachines(ram, *cfgFlag, *goos, *home); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
	seed, err := tierseed.Resolve(p, tier, tierseed.Options{
		Home: installHome, GOOS: *goos, RAMTier: ram.Overlay, VLLMSeatActive: vllmActive, ExtraVLLMSeatsActive: extraActive,
	})
	if err != nil {
		return fmt.Errorf("audit-config: resolve %s: %w", tier, err)
	}
	if seed, err = withLiveAccelerators(seed, live, doc, installHome, *goos); err != nil {
		return fmt.Errorf("audit-config: resolve the accelerator seeds for %s: %w", path, err)
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
			"config": path, "tier": tier, "tier_source": tierSource,
			"ram_tier": ram.Tier, "ram_tier_source": ram.Source, "ram_overlay": ramOverlayName(ram),
			"drifted": drifted, "findings": findings,
		}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	} else {
		fmt.Printf("audit-config: %s against tier %s (from %s; goos=%q ram-tier=%s (%s; %s) vllm-seat-active=%v)\n",
			path, tier, tierSource, *goos, ram.Tier, ram.sourceLabel(), ram.describe(), vllmActive)
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
