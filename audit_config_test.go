package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/hwdetect"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// TestClassifyConfigDriftNamesTheHandWiredWin pins the four classes on the exact shape that hid
// the <node-f> agent seat: the node carried a hand-set agent_model the seed did not write, while
// the seed wrote a DIFFERENT value, and a node-local endpoint that no tier owns must stay silent.
func TestClassifyConfigDriftNamesTheHandWiredWin(t *testing.T) {
	seed := map[string]any{
		"agent_model":   "gemma4-e2b", // what the fallback derived
		"ctx_size_hint": 32768,        // an int in the seed ...
		"stt_model":     "turbo.bin",  // the node never got it
	}
	live := map[string]any{
		"agent_model":      "qwen3.5-4b-agent",       // the measured winner, hand-set
		"ctx_size_hint":    float64(32768),           // ... and the float64 JSON decodes to: the SAME number
		"agent_seat_tok_s": float64(10),              // owned by some tier's seed, absent from this one
		"endpoint":         "http://127.0.0.1:11436", // node-local, owned by no seed
	}
	owned := map[string]bool{"agent_model": true, "ctx_size_hint": true, "stt_model": true, "agent_seat_tok_s": true}

	got := map[string]configDriftClass{}
	for _, f := range classifyConfigDrift(seed, live, owned, isBindingKey) {
		got[f.Key] = f.Class
	}
	want := map[string]configDriftClass{
		"agent_model":      driftDifferent,
		"ctx_size_hint":    driftMatch,
		"stt_model":        driftSeedOnly,
		"agent_seat_tok_s": driftLiveOnly,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s classified %q, want %q", k, got[k], w)
		}
	}
	if c, present := got["endpoint"]; present {
		t.Errorf("endpoint is owned by no seed and must not be reported, got %q — a report that flags node-local keys buries the real drift", c)
	}
}

// TestClassifyConfigDriftPutsDriftFirst: the report exists to be read, so what differs leads and
// what matches trails.
func TestClassifyConfigDriftPutsDriftFirst(t *testing.T) {
	seed := map[string]any{"a": 1, "b": 2}
	live := map[string]any{"a": float64(1), "b": float64(3), "c": "x"}
	owned := map[string]bool{"a": true, "b": true, "c": true}
	out := classifyConfigDrift(seed, live, owned, isBindingKey)
	order := []configDriftClass{}
	for _, f := range out {
		order = append(order, f.Class)
	}
	want := []configDriftClass{driftDifferent, driftLiveOnly, driftMatch}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("got %v, want %v", order, want)
		}
	}
}

// TestClassifyConfigDriftSeesABindingNoTierSeeds pins the blind spot the first cut had: <node-b>'s
// image-edit and animate routes were wired by hand and are seeded by NO tier, so a comparison over
// seed-owned keys alone could not see them. They must surface as UNSEEDED; a node-local endpoint or
// path must not.
func TestClassifyConfigDriftSeesABindingNoTierSeeds(t *testing.T) {
	live := map[string]any{
		"gen_edit_script":   "D:/render/edit.py",
		"animategen_script": "D:/render/animate.py",
		"comfy_dir":         "D:/ComfyUI",
		"endpoint":          "http://127.0.0.1:11436",
		"tts_endpoint":      "http://127.0.0.1:3900", // this box's own service URL, not a seat
	}
	got := map[string]configDriftClass{}
	for _, f := range classifyConfigDrift(map[string]any{}, live, map[string]bool{}, isBindingKey) {
		got[f.Key] = f.Class
	}
	for _, k := range []string{"gen_edit_script", "animategen_script"} {
		if got[k] != driftUnseeded {
			t.Errorf("%s classified %q, want UNSEEDED — a hand-wired route no tier carries is exactly the win a fresh install loses", k, got[k])
		}
	}
	for _, k := range []string{"comfy_dir", "endpoint", "tts_endpoint"} {
		if c, ok := got[k]; ok {
			t.Errorf("%s is node-local and must not be reported, got %q", k, c)
		}
	}
}

// The accelerator rows seed their own keys (ADR 0024), so the audit owns them: it can then say what an
// accelerator node's endpoint or launcher drifted to. Resolving them needs EVERY device's home — before
// that the whole resolution failed on the first empty home token and was skipped without a word, so no
// accelerator key was ever owned.
func TestSeedOwnedKeysIncludeTheAcceleratorKeys(t *testing.T) {
	doc, err := tierseed.LoadDoc(".")
	if err != nil {
		t.Fatal(err)
	}
	owned := seedOwnedKeys(doc, t.TempDir())
	for _, k := range []string{
		"accelerators", "hailo_endpoint", "hailo_sidecar_cmd", "coral_endpoint", "coral_sidecar_cmd",
		"rknpu_endpoint", "rknpu_sidecar_cmd", "rknpu_timeout_sec", "rknpu_idle_sec",
	} {
		if !owned[k] {
			t.Errorf("%s is not seed-owned: the accelerator rows did not resolve", k)
		}
	}
}

// auditNode writes a live config made of the tier's own seed plus the given extra keys, runs
// audit-config against that tier and returns what it printed and whether it reported drift.
func auditNode(t *testing.T, tier, home string, extra map[string]any, adjust func(live map[string]any)) (string, error) {
	t.Helper()
	stubDetectedRAMTier(t, "min") // hermetic: the audit would otherwise read this machine's RAM
	doc, err := tierseed.LoadDoc(".")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := tierseed.Resolve(doc.Profiles[tier], tier, tierseed.Options{Home: home, GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]any{}
	for k, v := range seed {
		live[k] = v
	}
	for k, v := range extra {
		live[k] = v
	}
	if adjust != nil {
		adjust(live)
	}
	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var runErr error
	out := captureStdout(t, func() {
		runErr = runAuditConfig([]string{"--config", cfg, "--tier", tier, "--root", ".", "--home", home,
			"--goos", "linux", "--vllm-seat-active", "false", "--all"})
	})
	return out, runErr
}

// acceleratorSeedFor is what the installer writes for the listed devices: the same resolver, the same homes.
func acceleratorSeedFor(t *testing.T, home string, ids ...string) map[string]any {
	t.Helper()
	doc, err := tierseed.LoadDoc(".")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := tierseed.ResolveAccelerators(doc.Accelerators, ids, accelOptions(tierseed.Options{Home: home, GOOS: "linux"}, "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	return seed
}

// A node the installer seeded — the tier's keys plus the seed of the accelerators it lists — audits clean:
// the accelerator keys it carries are the seed's own, not hand-set ones the seed does not write.
func TestAuditConfigLeavesASeededAcceleratorNodeClean(t *testing.T) {
	t.Setenv("HAILO_HOME", "")
	t.Setenv("CORAL_HOME", "")
	t.Setenv("RKNPU_HOME", "")
	home := t.TempDir()
	for _, ids := range [][]string{{"rknpu"}, {"hailo-8l", "rknpu"}} {
		out, err := auditNode(t, "cpu", home, acceleratorSeedFor(t, home, ids...), nil)
		if err != nil {
			t.Errorf("devices %v: a seeded accelerator node reports drift: %v\n%s", ids, err, out)
		}
	}
}

// The audit now sees the accelerator keys: a value hand-set on the node differs from the seed, and a
// device the node does not list adds no row (its seed is not the node's to carry).
func TestAuditConfigNamesAHandTunedAcceleratorKey(t *testing.T) {
	t.Setenv("HAILO_HOME", "")
	t.Setenv("CORAL_HOME", "")
	t.Setenv("RKNPU_HOME", "")
	home := t.TempDir()
	out, err := auditNode(t, "cpu", home, acceleratorSeedFor(t, home, "rknpu"), func(live map[string]any) {
		live["rknpu_timeout_sec"] = 90
	})
	if err == nil {
		t.Fatalf("a hand-tuned rknpu_timeout_sec must be reported as drift:\n%s", out)
	}
	if !strings.Contains(out, "DIFFERENT") || !strings.Contains(out, "rknpu_timeout_sec") {
		t.Errorf("the drift must name rknpu_timeout_sec as DIFFERENT:\n%s", out)
	}
	if strings.Contains(out, "hailo_") || strings.Contains(out, "coral_") {
		t.Errorf("devices the node does not list must add no rows:\n%s", out)
	}
}

// A device this build does not declare in profiles.json (a newer node's, say) has no seed to compare
// with: it is skipped, not an audit failure.
func TestAuditConfigSkipsAnUndeclaredAccelerator(t *testing.T) {
	t.Setenv("RKNPU_HOME", "")
	home := t.TempDir()
	extra := acceleratorSeedFor(t, home, "rknpu")
	out, err := auditNode(t, "cpu", home, extra, func(live map[string]any) {
		live["accelerators"] = []any{"rknpu", "tpu-from-the-future"}
	})
	if err != nil && !errors.Is(err, errConfigDrift) {
		t.Fatalf("an undeclared device must be skipped, not fail the audit itself: %v\n%s", err, out)
	}
}

// vision_tasks is written by the tier's media seat (like vision_model), so it is a seed-owned key
// the audit compares: the seed carries a []string, the live config decodes a []any, and the
// comparison must read them by value, not by Go type.
func TestClassifyConfigDriftReadsVisionTasksByValue(t *testing.T) {
	owned := map[string]bool{"vision_tasks": true}
	seed := map[string]any{"vision_tasks": []string{"vqa", "ocr"}}
	for _, tc := range []struct {
		name string
		live map[string]any
		want configDriftClass
	}{
		{"same list", map[string]any{"vision_tasks": []any{"vqa", "ocr"}}, driftMatch},
		{"hand-widened to all three", map[string]any{"vision_tasks": []any{"vqa", "ocr", "assess_image"}}, driftDifferent},
		{"never seeded on the node", map[string]any{}, driftSeedOnly},
	} {
		got := classifyConfigDrift(seed, tc.live, owned, isBindingKey)
		if len(got) != 1 || got[0].Key != "vision_tasks" || got[0].Class != tc.want {
			t.Errorf("%s: got %+v, want one %s finding for vision_tasks", tc.name, got, tc.want)
		}
	}
	// A node whose tier carries no seed for it keeps a hand-set list as LIVE-ONLY, never a silent pass.
	got := classifyConfigDrift(map[string]any{}, map[string]any{"vision_tasks": []any{"vqa"}}, owned, isBindingKey)
	if len(got) != 1 || got[0].Class != driftLiveOnly {
		t.Errorf("a hand-set vision_tasks the seed does not carry: got %+v, want LIVE-ONLY", got)
	}
}

// And the real tier: the rk3588 seed, resolved the way audit-config resolves it, carries the
// seat-written vision_tasks, so a node that lost it reads SEED-ONLY.
func TestAuditConfigSeedOwnsTheRK3588VisionTasks(t *testing.T) {
	doc, err := tierseed.ParseDoc(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	if !seedOwnedKeys(doc, "/opt/offload")["vision_tasks"] {
		t.Fatal("vision_tasks is not a seed-owned key: the audit would not compare it")
	}
}

// unconstrained_seats is written by the tier's rkllm seat, so it is a seed-owned key the audit
// compares: a node that lost it reads SEED-ONLY (its pipeline would send the NPU a grammar again),
// and a hand-widened list reads DIFFERENT. The seed carries a []string, the live config a []any.
func TestAuditConfigSeedOwnsTheRK3588UnconstrainedSeats(t *testing.T) {
	doc, err := tierseed.ParseDoc(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	if !seedOwnedKeys(doc, "/opt/offload")["unconstrained_seats"] {
		t.Fatal("unconstrained_seats is not a seed-owned key: the audit would not compare it")
	}
	owned := map[string]bool{"unconstrained_seats": true}
	seed := map[string]any{"unconstrained_seats": []string{"npu"}}
	for _, tc := range []struct {
		name string
		live map[string]any
		want configDriftClass
	}{
		{"same list", map[string]any{"unconstrained_seats": []any{"npu"}}, driftMatch},
		{"hand-widened", map[string]any{"unconstrained_seats": []any{"npu", "other"}}, driftDifferent},
		{"never seeded on the node", map[string]any{}, driftSeedOnly},
	} {
		got := classifyConfigDrift(seed, tc.live, owned, isBindingKey)
		if len(got) != 1 || got[0].Key != "unconstrained_seats" || got[0].Class != tc.want {
			t.Errorf("%s: got %+v, want one %s finding", tc.name, got, tc.want)
		}
	}
}

// TestClassifyConfigDriftTreatsKeyPrefixAsNodeOwned pins the one place audit-config does not compare a
// value whole: the key_prefix of each kv_cache_server binding belongs to the node (a seeded node keeps the
// prefix it was seeded with, and install does not rewrite config.json), so a prefix-only difference
// between two lists of bindings is not drift. Every other difference is, and so is a shape the rule
// does not cover: a legacy single object, or a list holding something that is not an object.
func TestClassifyConfigDriftTreatsKeyPrefixAsNodeOwned(t *testing.T) {
	// The prefixes are built rather than pasted, so no line reads key_prefix next to an opaque
	// string (the shape the tree's secret scanner classifies as a generic API key).
	prefixA, prefixB := "seat-"+"alpha", "seat-"+"beta"
	binding := func(addr, seat, prefix string) map[string]any {
		b := map[string]any{"enabled": true, "store": "fs_native", "address": addr, "seat": seat}
		if prefix != "" {
			b["key_prefix"] = prefix
		}
		return b
	}
	// The seed carries its bindings as a typed list (what tierseed writes); a live config decodes to []any.
	seedList := func(bs ...map[string]any) any { return bs }
	liveList := func(bs ...map[string]any) any {
		out := make([]any, len(bs))
		for i, b := range bs {
			out[i] = b
		}
		return out
	}
	one := binding("/mnt/kv/one", "seat-one", prefixA)
	oneOtherPrefix := binding("/mnt/kv/one", "seat-one", prefixB)
	oneNoPrefix := binding("/mnt/kv/one", "seat-one", "")
	oneOtherAddr := binding("/mnt/kv/elsewhere", "seat-one", prefixA)
	two := binding("/mnt/kv/two", "seat-two", prefixA)

	classify := func(t testing.TB, seedVal, liveVal any) configDrift {
		t.Helper()
		seed := map[string]any{"kv_cache_server": seedVal}
		live := map[string]any{"kv_cache_server": liveVal}
		for _, f := range classifyConfigDrift(seed, live, map[string]bool{"kv_cache_server": true}, isBindingKey) {
			if f.Key == "kv_cache_server" {
				return f
			}
		}
		t.Fatal("kv_cache_server was not reported at all")
		return configDrift{}
	}

	for _, c := range []struct {
		name       string
		seed, live any
		want       configDriftClass
	}{
		{"both lists, prefix-only difference", seedList(one), liveList(oneOtherPrefix), driftMatch},
		{"both lists, identical", seedList(one), liveList(one), driftMatch},
		{"both lists, an address differs", seedList(one), liveList(oneOtherAddr), driftDifferent},
		{"live has an extra binding", seedList(one), liveList(one, two), driftDifferent},
		{"live is missing a binding", seedList(one, two), liveList(one), driftDifferent},
		{"live binding without a prefix against a seed that has one", seedList(one), liveList(oneNoPrefix), driftMatch},
		{"seed binding without a prefix against a live one that has one", seedList(oneNoPrefix), liveList(one), driftMatch},
		{"live legacy single object against the list seed, same prefix", seedList(one), one, driftDifferent},
		{"live legacy single object against the list seed, other prefix", seedList(one), oneOtherPrefix, driftDifferent},
		{"list live against a legacy-object seed", one, liveList(one), driftDifferent},
		{"a list holding a non-object is compared whole", []any{one, "x"}, []any{oneOtherPrefix, "x"}, driftDifferent},
		{"a list against a string", seedList(one), "x", driftDifferent},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(t, c.seed, c.live); got.Class != c.want {
				t.Errorf("classified %q, want %q", got.Class, c.want)
			}
		})
	}

	t.Run("the report keeps both prefixes and the inputs are not mutated", func(t *testing.T) {
		seedVal, liveVal := seedList(one), liveList(oneOtherPrefix)
		before := func() string {
			s, _ := json.Marshal(seedVal)
			l, _ := json.Marshal(liveVal)
			return string(s) + "|" + string(l)
		}
		was := before()
		got := classify(t, seedVal, liveVal)
		if got.Class != driftMatch {
			t.Fatalf("classified %q, want MATCH", got.Class)
		}
		if after := before(); after != was {
			t.Errorf("the inputs were modified: %s -> %s", was, after)
		}
		s, _ := json.Marshal(got.Seed)
		l, _ := json.Marshal(got.Live)
		if !strings.Contains(string(s), prefixA) || !strings.Contains(string(l), prefixB) {
			t.Errorf("the report must still show both prefixes, got seed %s live %s", s, l)
		}
	})
}

// ramGbOfTier is a RAM size that classifies as each tier, for a stub that must speak with a size too.
var ramGbOfTier = map[string]int{"min": 8, "low": 32, "mid": 64, "high": 128}

// stubDetectedRAMTier makes the audit see a box of the given RAM tier, whatever machine runs the suite.
func stubDetectedRAMTier(t *testing.T, tier string) {
	t.Helper()
	prev := auditDetectRAMTier
	auditDetectRAMTier = func() (string, int, error) { return tier, ramGbOfTier[tier], nil }
	t.Cleanup(func() { auditDetectRAMTier = prev })
}

// The audit must compare the seed the installer WOULD write on this box, and that includes the RAM
// overlay of the tier the box detects. `none` stays selectable. Low selects the low-and-up overlay only,
// mid/high select both, and min selects neither.
func TestResolveAuditRAMTier(t *testing.T) {
	cases := []struct {
		flag, detected string
		overlay, tier  string
		source         string
		wantErr        bool
	}{
		{"", "mid", "mid", "mid", "detected", false},
		{"auto", "high", "high", "high", "detected", false},
		{"", "low", "low", "low", "detected", false},
		{"", "min", "", "min", "detected", false},
		{"none", "mid", "", "none", "--ram-tier", false},
		{"mid", "low", "mid", "mid", "--ram-tier", false},
		{"HIGH", "min", "high", "high", "--ram-tier", false},
		{"low", "high", "low", "low", "--ram-tier", false},
		{"huge", "mid", "", "", "", true},
	}
	for _, c := range cases {
		got, err := resolveAuditRAMTier(c.flag, func() (string, int, error) { return c.detected, ramGbOfTier[c.detected], nil })
		if (err != nil) != c.wantErr {
			t.Errorf("flag %q detected %q: err=%v, wantErr=%v", c.flag, c.detected, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if got.Overlay != c.overlay || got.Tier != c.tier || got.Source != c.source {
			t.Errorf("flag %q detected %q: got %+v, want overlay=%q tier=%q source=%q", c.flag, c.detected, got, c.overlay, c.tier, c.source)
		}
	}
}

// auditRAMNode audits a node whose config is blackwell-8's seed WITH the mid RAM overlay applied
// (what an install on a 64 GB box writes), under the given extra flags.
func auditRAMNode(t *testing.T, flags ...string) (string, error) {
	t.Helper()
	doc, err := tierseed.LoadDoc(".")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	seed, err := tierseed.Resolve(doc.Profiles["blackwell-8"], "blackwell-8", tierseed.Options{Home: home, GOOS: "linux", RAMTier: "mid"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var runErr error
	out := captureStdout(t, func() {
		runErr = runAuditConfig(append([]string{"--config", cfg, "--tier", "blackwell-8", "--root", ".", "--home", home,
			"--goos", "linux", "--vllm-seat-active", "false"}, flags...))
	})
	return out, runErr
}

// The false positive this fixes: a 64 GB box carries the overlay, the default audit compared the
// base seed only and called every overlay key drift.
func TestAuditConfigDefaultsToTheDetectedRAMTier(t *testing.T) {
	stubDetectedRAMTier(t, "mid")
	out, err := auditRAMNode(t)
	if err != nil {
		t.Fatalf("a mid-RAM box carrying the mid overlay must audit clean by default, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "ram-tier=mid") || !strings.Contains(out, "detected") || !strings.Contains(out, "config_seed_ram_mid_high") {
		t.Errorf("the text output must say which RAM overlay was compared, got:\n%s", out)
	}
}

// `none` stays selectable: the base seed alone, so the overlay-carried keys read as drift again.
func TestAuditConfigRAMTierNoneComparesTheBaseSeed(t *testing.T) {
	stubDetectedRAMTier(t, "mid")
	out, err := auditRAMNode(t, "--ram-tier", "none")
	if !errors.Is(err, errConfigDrift) {
		t.Fatalf("--ram-tier none compares the base seed, so a node carrying the overlay must drift, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "ram-tier=none") || !strings.Contains(out, "base seed only") {
		t.Errorf("the text output must say the base seed alone was compared, got:\n%s", out)
	}
}

// A detected low tier selects the low-and-up overlay only: a tier with no such overlay (blackwell-8) is
// compared against its base seed, so a node carrying the MID overlay drifts, and the text says which
// overlay layers were compared.
func TestAuditConfigDetectedLowRAMComparesTheLowUpOverlayOnly(t *testing.T) {
	stubDetectedRAMTier(t, "low")
	out, err := auditRAMNode(t)
	if !errors.Is(err, errConfigDrift) {
		t.Fatalf("a low-RAM box does not take the mid/high overlay, so a node carrying it must drift, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "ram-tier=low") || !strings.Contains(out, "config_seed_ram_low_up") || strings.Contains(out, "config_seed_ram_mid_high") {
		t.Errorf("a low-RAM audit must name the low-and-up overlay and not the mid/high one, got:\n%s", out)
	}
}

// ampere-6 is the tier that declares a low-and-up overlay (the Qwen3.6-35B-A3B spill seat's agent
// binding). A node installed on a 32 GB box carries it, so the default audit on a detected low box
// must call it clean, and `--ram-tier min` (the base seed alone, the 4B) must read it as drift:
// without the low-and-up layer in the audit a correctly installed 32 GB node reads as drifted.
func TestAuditConfigDetectedLowRAMKeepsAnInstalledSpillSeatNodeClean(t *testing.T) {
	doc, err := tierseed.LoadDoc(".")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	seed, err := tierseed.Resolve(doc.Profiles["ampere-6"], "ampere-6", tierseed.Options{Home: home, GOOS: "linux", RAMTier: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if seed["agent_model"] != "qwen3.6-35b-a3b-agent" {
		t.Fatalf("the fixture is wrong: ampere-6 at ram low seeds agent_model %v", seed["agent_model"])
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	audit := func(flags ...string) (string, error) {
		var runErr error
		out := captureStdout(t, func() {
			runErr = runAuditConfig(append([]string{"--config", cfg, "--tier", "ampere-6", "--root", ".", "--home", home,
				"--goos", "linux", "--vllm-seat-active", "false"}, flags...))
		})
		return out, runErr
	}
	stubDetectedRAMTier(t, "low")
	if out, err := audit(); err != nil {
		t.Fatalf("a 32 GB (low) ampere-6 node carrying the spill-seat binding must audit clean by default, got %v\n%s", err, out)
	}
	if out, err := audit("--ram-tier", "min"); !errors.Is(err, errConfigDrift) {
		t.Fatalf("against the base seed alone (min) the spill-seat binding is drift, got %v\n%s", err, out)
	}
}

func TestAuditConfigRejectsAnUnknownRAMTier(t *testing.T) {
	stubDetectedRAMTier(t, "mid")
	_, err := auditRAMNode(t, "--ram-tier", "huge")
	if err == nil || errors.Is(err, errConfigDrift) || !strings.Contains(err.Error(), "ram-tier") {
		t.Fatalf("an unknown --ram-tier must be refused by name, got %v", err)
	}
}

func TestAuditConfigJSONNamesTheRAMOverlay(t *testing.T) {
	stubDetectedRAMTier(t, "mid")
	for _, c := range []struct {
		flags            []string
		tier, src, overl string
	}{
		{nil, "mid", "detected", "config_seed_ram_mid_high"},
		{[]string{"--ram-tier", "none"}, "none", "--ram-tier", "none"},
	} {
		out, _ := auditRAMNode(t, append([]string{"--json"}, c.flags...)...)
		var rep map[string]any
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("--json output is not JSON: %v\n%s", err, out)
		}
		if rep["ram_tier"] != c.tier || rep["ram_tier_source"] != c.src || rep["ram_overlay"] != c.overl {
			t.Errorf("flags %v: ram_tier=%v ram_tier_source=%v ram_overlay=%v, want %s / %s / %s",
				c.flags, rep["ram_tier"], rep["ram_tier_source"], rep["ram_overlay"], c.tier, c.src, c.overl)
		}
	}
}

// A failed RAM probe reads as 0 GB, which classifies as tier min and would pick the base seed with no
// word said. The audit must refuse and ask for --ram-tier instead of guessing.
func TestAuditConfigRefusesAFailedRAMProbe(t *testing.T) {
	prev := auditDetectFacts
	auditDetectFacts = func() hwdetect.Facts { return hwdetect.Facts{RAMGb: 0} }
	t.Cleanup(func() { auditDetectFacts = prev })
	for _, flags := range [][]string{nil, {"--ram-tier", "auto"}} {
		out, err := auditRAMNode(t, flags...)
		if err == nil || errors.Is(err, errConfigDrift) || !strings.Contains(err.Error(), "--ram-tier") || !strings.Contains(err.Error(), "0 GB") {
			t.Fatalf("flags %v: a failed RAM probe must be refused with a pointer to --ram-tier, got %v\n%s", flags, err, out)
		}
		if strings.Contains(out, "ram-tier=min") {
			t.Errorf("flags %v: the audit must not quietly fall back to tier min, got:\n%s", flags, out)
		}
	}
	// A named tier never consults the probe, so a box whose probe fails can still be audited.
	if out, err := auditRAMNode(t, "--ram-tier", "mid"); err != nil {
		t.Fatalf("an explicit --ram-tier must not need the probe, got %v\n%s", err, out)
	}
	if out, err := auditRAMNode(t, "--ram-tier", "none"); !errors.Is(err, errConfigDrift) {
		t.Fatalf("--ram-tier none must not need the probe either, got %v\n%s", err, out)
	}
}

// The header names the RAM the probe read, so a wrong detection is visible; a named tier has no probe to report.
func TestAuditConfigHeaderNamesTheDetectedRAM(t *testing.T) {
	stubDetectedRAMTier(t, "mid")
	out, err := auditRAMNode(t)
	if err != nil {
		t.Fatalf("got %v\n%s", err, out)
	}
	if !strings.Contains(out, "ram-tier=mid (detected, 64 GB;") {
		t.Errorf("the header must name the detected RAM in GB, got:\n%s", out)
	}
	out, _ = auditRAMNode(t, "--ram-tier", "mid")
	if strings.Contains(out, " GB") || !strings.Contains(out, "ram-tier=mid (--ram-tier;") {
		t.Errorf("a named tier has no detected GB to show, got:\n%s", out)
	}
}

// Detection reads THIS machine. Pointed at another node's config (--config, --home, a foreign --goos)
// without a named tier, the overlay compared is this machine's RAM tier, and the audit says so on stderr
// (stdout stays the report, JSON included).
func TestAuditConfigWarnsWhenTheRAMTierIsThisMachinesNotTheNodes(t *testing.T) {
	stubDetectedRAMTier(t, "mid")
	for _, flags := range [][]string{nil, {"--ram-tier", "auto"}} {
		var out string
		warn := captureStderr(t, func() { out, _ = auditRAMNode(t, flags...) })
		if !strings.Contains(warn, "warning") || !strings.Contains(warn, "this machine's RAM tier") || !strings.Contains(warn, "--ram-tier") {
			t.Errorf("flags %v: auditing another node's config on a detected RAM tier must warn, got stderr %q", flags, warn)
		}
		if strings.Contains(out, "warning") {
			t.Errorf("flags %v: the warning belongs on stderr, not in the report:\n%s", flags, out)
		}
	}
	for _, flags := range [][]string{{"--ram-tier", "mid"}, {"--ram-tier", "none"}, {"--ram-tier", "low"}} {
		if warn := captureStderr(t, func() { auditRAMNode(t, flags...) }); warn != "" {
			t.Errorf("flags %v: a named tier is the operator's own choice, no warning expected, got %q", flags, warn)
		}
	}
}

// Auditing this very node (config from the environment, this platform) compares the right RAM, so no warning.
func TestAuditConfigDoesNotWarnForThisMachinesOwnConfig(t *testing.T) {
	stubDetectedRAMTier(t, "mid")
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{"tier_profile":"blackwell-8"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCAL_OFFLOAD_CONFIG", cfg)
	for _, flags := range [][]string{nil, {"--goos", runtime.GOOS}} {
		warn := captureStderr(t, func() {
			captureStdout(t, func() {
				_ = runAuditConfig(append([]string{"--root", ".", "--vllm-seat-active", "false"}, flags...))
			})
		})
		if warn != "" {
			t.Errorf("flags %v: this machine's own config needs no warning, got %q", flags, warn)
		}
	}
}
