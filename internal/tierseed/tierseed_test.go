package tierseed

import (
	"strings"
	"testing"
)

func seedOf(kv map[string]any, backend string) Profile {
	return Profile{Backend: backend, ConfigSeed: kv}
}

// TestOneSeedRendersOnBothPlatforms is the whole point: a tier is a HARDWARE class,
// so the same row must produce a working binding on Windows and on Linux. The table
// used to carry `sd-cli.exe`, which cannot exist on a Linux box of the same tier.
func TestOneSeedRendersOnBothPlatforms(t *testing.T) {
	p := seedOf(map[string]any{
		"imagegen_engine": "sdcpp",
		"sdcpp_bin":       "__OFFLOAD_HOME__/sdcpp/sd-cli__EXE__",
	}, "vulkan")

	win, err := Resolve(p, "t", Options{Home: "D:/offload-stack", GOOS: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	if got := win["sdcpp_bin"]; got != "D:/offload-stack/sdcpp/sd-cli.exe" {
		t.Errorf("windows sdcpp_bin = %v", got)
	}
	lin, err := Resolve(p, "t", Options{Home: "/opt/offload", GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if got := lin["sdcpp_bin"]; got != "/opt/offload/sdcpp/sd-cli" {
		t.Errorf("linux sdcpp_bin = %v", got)
	}
}

// TestVaeModeCPUIsRejectedOnCUDA: --vae-on-cpu is CORRECT on an AMD/UMA part and was
// MEASURED at 7.8x slower on CUDA (58.2s vs 7.5s). Free-text extra args are how that
// flag would spread to a tier it is wrong for; a declared mode can be refused.
func TestVaeModeCPUIsRejectedOnCUDA(t *testing.T) {
	_, err := Resolve(seedOf(map[string]any{"vae_mode": "cpu"}, "cuda"), "ampere-6", Options{})
	if err == nil {
		t.Fatal("vae_mode cpu on a CUDA backend must be refused")
	}
	if !strings.Contains(err.Error(), "7.8x") || !strings.Contains(err.Error(), "tiling") {
		t.Errorf("the refusal must carry the measurement and the fix: %v", err)
	}
	// The same mode on the part it was measured for is correct, not an error.
	got, err := Resolve(seedOf(map[string]any{"vae_mode": "cpu"}, "vulkan"), "amd-rdna3", Options{})
	if err != nil {
		t.Fatalf("vulkan/UMA: %v", err)
	}
	args, _ := got["sdcpp_extra_args"].([]any)
	if len(args) != 1 || args[0] != "--vae-on-cpu" {
		t.Errorf("sdcpp_extra_args = %v, want the translated flag", got["sdcpp_extra_args"])
	}
	if _, leaked := got["vae_mode"]; leaked {
		t.Error("vae_mode is a seed directive and must not reach the harness config")
	}
}

// TestUnknownSeedKeyIsRejected: a seed key that is not a Config field is dropped by
// the loader with a warning — on EVERY install of that tier. Catch it at authoring.
func TestUnknownSeedKeyIsRejected(t *testing.T) {
	_, err := Resolve(seedOf(map[string]any{"imagegen_engin": "sdcpp"}, "cuda"), "t", Options{})
	if err == nil || !strings.Contains(err.Error(), "imagegen_engin") {
		t.Fatalf("a typo'd seed key must be refused, got %v", err)
	}
}

// TestLiteralExeIsRejected: the token exists so nobody re-bakes a platform into the
// hardware table.
func TestLiteralExeIsRejected(t *testing.T) {
	_, err := Resolve(seedOf(map[string]any{"sdcpp_bin": "__OFFLOAD_HOME__/sdcpp/sd-cli.exe"}, "vulkan"), "t", Options{})
	if err == nil || !strings.Contains(err.Error(), "__EXE__") {
		t.Fatalf("a literal .exe must be refused with the token named, got %v", err)
	}
}

// TestTextOnlyTierIsNotAnError: six tiers ship no media seat. That is a legitimate
// machine, and the caller must be able to tell it apart from a failure.
func TestTextOnlyTierIsNotAnError(t *testing.T) {
	got, err := Resolve(Profile{Backend: "cuda"}, "cpu", Options{})
	if err != nil || got != nil {
		t.Fatalf("got %v, %v — want (nil, nil)", got, err)
	}
}

func TestResolveAcceleratorsExpandsAndValidates(t *testing.T) {
	accs := map[string]Accelerator{
		"hailo-8l": {Kind: "npu", ConfigSeed: map[string]any{
			"accelerators": []any{"hailo-8l"}, "hailo_endpoint": "http://127.0.0.1:18813",
			"hailo_sidecar_cmd": "__HAILO_HOME__/hailo-http.cmd", "hailo_timeout_sec": 60,
		}},
	}
	out, err := ResolveAccelerators(accs, []string{"hailo-8l"}, Options{Home: `C:\stack`, HailoHome: `D:\Dev\Hailo`, GOOS: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	if out["hailo_sidecar_cmd"] != "D:/Dev/Hailo/hailo-http.cmd" {
		t.Fatalf("token not expanded: %v", out["hailo_sidecar_cmd"])
	}
	if _, err := ResolveAccelerators(accs, []string{"tpu"}, Options{}); err == nil {
		t.Fatal("unknown accelerator id must be an error, not a silent skip")
	}
	bad := map[string]Accelerator{"x": {ConfigSeed: map[string]any{"no_such_key": 1}}}
	if _, err := ResolveAccelerators(bad, []string{"x"}, Options{}); err == nil {
		t.Fatal("a seed key that is not a config.Config json tag must fail at authoring time")
	}
	if out, _ := ResolveAccelerators(accs, nil, Options{}); out != nil {
		t.Fatalf("no ids -> nil seed, got %v", out)
	}
}

// TestEveryShippedAcceleratorSeedIsValid guards the real accelerators table the same
// way TestEveryShippedSeedIsValid guards the tiers: a typo'd key in a shipped
// config_seed would otherwise be dropped on every box that has the device.
func TestEveryShippedAcceleratorSeedIsValid(t *testing.T) {
	d, err := LoadDoc("../..")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Accelerators) == 0 {
		t.Fatal("profiles.json declares no accelerators — hailo-8l should be there")
	}
	for id := range d.Accelerators {
		for _, goos := range []string{"windows", "linux"} {
			out, err := ResolveAccelerators(d.Accelerators, []string{id}, Options{Home: "/tmp/x", HailoHome: "/tmp/hailo", GOOS: goos})
			if err != nil {
				t.Errorf("accelerator %s does not resolve for %s: %v", id, goos, err)
				continue
			}
			for k, v := range out {
				if s, ok := v.(string); ok && strings.Contains(s, "__HAILO_HOME__") {
					t.Errorf("accelerator %s/%s: %s still carries __HAILO_HOME__ after expansion: %v", id, goos, k, s)
				}
			}
		}
	}
}

// TestEveryShippedSeedIsValid guards the real table: every tier in profiles.json must
// resolve for BOTH platforms. This is the gate that would have caught `sd-cli.exe`.
func TestEveryShippedSeedIsValid(t *testing.T) {
	profiles, err := Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	for id, p := range profiles {
		for _, goos := range []string{"windows", "linux"} {
			if _, err := Resolve(p, id, Options{Home: "/tmp/x", GOOS: goos, RAMTier: "high"}); err != nil {
				t.Errorf("tier %s does not resolve for %s: %v", id, goos, err)
			}
		}
	}
}
