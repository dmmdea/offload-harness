package tierseed

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
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
	out, err := ResolveAccelerators(accs, []string{"hailo-8l"}, Options{Home: `C:\stack`, HailoHome: `D:\x\hailo`, GOOS: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	if out["hailo_sidecar_cmd"] != "D:/x/hailo/hailo-http.cmd" {
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

// The RKNPU has its own home token, like each device: it expands from RknpuHome (trailing slash and
// backslashes normalised), it is refused when EMPTY (an empty home would render "/rknpu-http.sh", a
// launcher at the filesystem root that nothing installed), and it expands beside the other two homes
// in one merge without either leaking into the other.
func TestResolveAcceleratorsExpandsRknpuHome(t *testing.T) {
	accs := map[string]Accelerator{
		"hailo-8l": {Kind: "npu", ConfigSeed: map[string]any{
			"accelerators": []any{"hailo-8l"}, "hailo_sidecar_cmd": "__HAILO_HOME__/hailo-http.cmd",
		}},
		"coral-edgetpu": {Kind: "tpu", ConfigSeed: map[string]any{
			"coral_sidecar_cmd": "__CORAL_HOME__/coral-http.sh",
		}},
		"rknpu": {Kind: "npu", ConfigSeed: map[string]any{
			"rknpu_endpoint": "http://127.0.0.1:18815", "rknpu_sidecar_cmd": "__RKNPU_HOME__/rknpu-http.sh", "rknpu_timeout_sec": 60,
		}},
	}
	out, err := ResolveAccelerators(accs, []string{"rknpu"}, Options{Home: "/srv/stack", RknpuHome: `/srv/stack/rknpu/`, GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if out["rknpu_sidecar_cmd"] != "/srv/stack/rknpu/rknpu-http.sh" {
		t.Fatalf("__RKNPU_HOME__ not expanded: %v", out["rknpu_sidecar_cmd"])
	}
	out, err = ResolveAccelerators(accs, []string{"rknpu"}, Options{Home: "/srv/stack", RknpuHome: `D:\x\rknpu`, GOOS: "windows"})
	if err != nil || out["rknpu_sidecar_cmd"] != "D:/x/rknpu/rknpu-http.sh" {
		t.Fatalf("a Windows-shaped home must normalise to forward slashes: %v, %v", out["rknpu_sidecar_cmd"], err)
	}

	_, err = ResolveAccelerators(accs, []string{"rknpu"}, Options{Home: "/srv/stack", HailoHome: "/x/hailo", CoralHome: "/x/coral", GOOS: "linux"})
	if err == nil || !strings.Contains(err.Error(), "__RKNPU_HOME__") || !strings.Contains(err.Error(), "RknpuHome") {
		t.Fatalf("an empty RknpuHome must be refused naming the token and the option, got %v", err)
	}

	all, err := ResolveAccelerators(accs, []string{"hailo-8l", "coral-edgetpu", "rknpu"},
		Options{Home: "/srv/stack", HailoHome: "/h", CoralHome: "/c", RknpuHome: "/r", GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"hailo_sidecar_cmd": "/h/hailo-http.cmd", "coral_sidecar_cmd": "/c/coral-http.sh", "rknpu_sidecar_cmd": "/r/rknpu-http.sh",
	} {
		if all[k] != want {
			t.Errorf("%s = %v, want %s", k, all[k], want)
		}
	}
}

// Each device's seed lists its own id under "accelerators" — the gate config.HasAccelerator reads. A box
// carrying two devices must seed BOTH, in the order the ids were given (that order is the shared-name
// rule's, ADR 0037); merging the seeds key by key kept only the last device's list, so the config gated
// on one device while installed.json advertised both.
func TestResolveAcceleratorsSeedsEveryDeviceInTheGate(t *testing.T) {
	accs := map[string]Accelerator{
		"hailo-8l": {ConfigSeed: map[string]any{"accelerators": []any{"hailo-8l"}, "hailo_timeout_sec": 60}},
		"rknpu":    {ConfigSeed: map[string]any{"accelerators": []any{"rknpu"}, "rknpu_timeout_sec": 60}},
	}
	for _, c := range []struct {
		ids  []string
		want []any
	}{
		{[]string{"hailo-8l", "rknpu"}, []any{"hailo-8l", "rknpu"}},
		{[]string{"rknpu", "hailo-8l"}, []any{"rknpu", "hailo-8l"}},
		{[]string{"rknpu", "rknpu"}, []any{"rknpu"}},
		{[]string{"rknpu"}, []any{"rknpu"}},
	} {
		out, err := ResolveAccelerators(accs, c.ids, Options{Home: "/h"})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(out["accelerators"], c.want) {
			t.Errorf("ids %v: accelerators = %v, want %v", c.ids, out["accelerators"], c.want)
		}
	}
	out, _ := ResolveAccelerators(accs, []string{"hailo-8l", "rknpu"}, Options{Home: "/h"})
	if out["hailo_timeout_sec"] != 60 || out["rknpu_timeout_sec"] != 60 {
		t.Errorf("the other keys must still merge: %v", out)
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
	leftover := regexp.MustCompile(`__[A-Z]+_HOME__`)
	for id := range d.Accelerators {
		for _, goos := range []string{"windows", "linux"} {
			out, err := ResolveAccelerators(d.Accelerators, []string{id}, Options{
				Home: "/tmp/x", HailoHome: "/tmp/hailo", CoralHome: "/tmp/coral", RknpuHome: "/tmp/rknpu", GOOS: goos})
			if err != nil {
				t.Errorf("accelerator %s does not resolve for %s: %v", id, goos, err)
				continue
			}
			for k, v := range out {
				if s, ok := v.(string); ok && leftover.MatchString(s) {
					t.Errorf("accelerator %s/%s: %s still carries a home token after expansion: %v", id, goos, k, s)
				}
			}
		}
	}
	if _, ok := d.Accelerators["rknpu"]; !ok {
		t.Error("profiles.json declares no rknpu accelerator")
	}
}

// TestEveryDeclaredAcceleratorHasAFleetVisibilityDecision (register E-08): which accelerators may
// leave their box is decided per device, and the decision lives in config.LocalOnlyAccelerator,
// keyed on the id profiles.json declares. A rename of the id would leave the guard keyed on a
// string no device carries; a new device declared without a decision would be published by
// default. Either fails here, by name. The Hailo-8L stays on the box that carries it; the Coral
// and the RKNPU are published, accepted and advertised over the fleet.
func TestEveryDeclaredAcceleratorHasAFleetVisibilityDecision(t *testing.T) {
	d, err := LoadDoc("../..")
	if err != nil {
		t.Fatal(err)
	}
	localOnly := map[string]bool{"hailo-8l": true, "coral-edgetpu": false, "rknpu": false}
	for id := range d.Accelerators {
		want, decided := localOnly[id]
		if !decided {
			t.Errorf("profiles.json declares accelerator %q with no fleet-visibility decision: add it to this table and to config.LocalOnlyAccelerator (docs/systems/accelerators.md, ADR 0038 amendment)", id)
			continue
		}
		if got := config.LocalOnlyAccelerator(id); got != want {
			t.Errorf("config.LocalOnlyAccelerator(%q) = %v, want %v", id, got, want)
		}
	}
	for id := range localOnly {
		if _, declared := d.Accelerators[id]; !declared {
			t.Errorf("this table decides accelerator %q but profiles.json no longer declares it: the guard would be keyed on an id no device carries", id)
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
