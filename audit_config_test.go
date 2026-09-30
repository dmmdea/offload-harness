package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// TestClassifyConfigDriftNamesTheHandWiredWin pins the four classes on the exact shape that hid
// the binxarn agent seat: the node carried a hand-set agent_model the seed did not write, while
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

// TestClassifyConfigDriftSeesABindingNoTierSeeds pins the blind spot the first cut had: the Qube's
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
