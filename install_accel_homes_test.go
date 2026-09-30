package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Every accelerator's seed carries its own home token, and the resolver refuses a token whose home is
// empty. So every verb that resolves accelerator seeds has to supply EVERY device's home, not just the
// device it was written for: `install plan` supplied only the Hailo's and so failed on any board that
// also lists a Coral or an RKNPU.

// allDevicesSysfs is a box that answers both sysfs probes: a Coral apex and the reference board's RKNPU.
func allDevicesSysfs(path string) (string, error) {
	if path == "/sys/class/apex/apex_0/status" {
		return "ALIVE\n", nil
	}
	return rknpuSysfs(path)
}

func TestInstallPlanResolvesEveryAcceleratorHome(t *testing.T) {
	orig := hailortcliRun
	defer func() { hailortcliRun = orig }()
	hailortcliRun = func(args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "scan":
			return "Hailo Devices:\n[-] Device: 0000:03:00.0\n", nil
		case "fw-control identify":
			return "Device Architecture: HAILO8L\n", nil
		}
		return "", errors.New("unexpected args")
	}
	stubSysfs(t, allDevicesSysfs)
	for _, env := range []string{"HAILO_HOME", "CORAL_HOME", "RKNPU_HOME"} {
		t.Setenv(env, "") // pin the <home>/<device> default, not this box's environment
	}

	out := captureStdout(t, func() {
		if err := runInstallPlan([]string{"-json", "-root", ".", "-home", t.TempDir()}); err != nil {
			t.Fatalf("install plan -json on a box with all three devices: %v", err)
		}
	})
	var got struct {
		Verdict struct {
			Accelerators []string `json:"accelerators"`
		} `json:"verdict"`
		ConfigSeed map[string]any `json:"config_seed"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("emitted JSON did not parse: %v\n%s", err, out)
	}
	if strings.Join(got.Verdict.Accelerators, ",") != "hailo-8l,coral-edgetpu,rknpu" {
		t.Fatalf("verdict.accelerators = %v, want [hailo-8l coral-edgetpu rknpu]", got.Verdict.Accelerators)
	}
	// The gate the seeded config carries lists every detected device, in detection order.
	var gate []string
	ids, _ := got.ConfigSeed["accelerators"].([]any)
	for _, id := range ids {
		gate = append(gate, fmt.Sprint(id))
	}
	if strings.Join(gate, ",") != "hailo-8l,coral-edgetpu,rknpu" {
		t.Errorf("config_seed accelerators = %v, want every detected device: [hailo-8l coral-edgetpu rknpu]", gate)
	}
	for key, suffix := range map[string]string{
		"hailo_sidecar_cmd": "/hailo/hailo-http.cmd",
		"coral_sidecar_cmd": "/coral/coral-http.sh",
		"rknpu_sidecar_cmd": "/rknpu/rknpu-http.sh",
	} {
		cmd, _ := got.ConfigSeed[key].(string)
		if !strings.HasSuffix(cmd, suffix) || strings.Contains(cmd, "_HOME__") {
			t.Errorf("%s = %q, want the default <home> expansion ending in %s", key, cmd, suffix)
		}
	}
}

// `install seed` resolves each device's home the same way: the --<device>-home flag, else $<DEVICE>_HOME,
// else <home>/<device>. The RKNPU's is the third.
func TestInstallSeedResolvesTheRknpuHome(t *testing.T) {
	home := t.TempDir()
	seed := func(t *testing.T, args ...string) map[string]any {
		t.Helper()
		var out string
		full := append([]string{"-profile", "cpu", "-os", "linux", "-root", ".", "-home", home}, args...)
		out = captureStdout(t, func() {
			if err := runInstallSeed(full); err != nil {
				t.Fatalf("install seed %v: %v", args, err)
			}
		})
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("install seed %v: output did not parse: %v\n%s", args, err, out)
		}
		return got
	}

	t.Setenv("RKNPU_HOME", "/srv/npu-from-env")
	if got := seed(t, "-accelerators", "rknpu", "-rknpu-home", "/opt/x/rknpu"); got["rknpu_sidecar_cmd"] != "/opt/x/rknpu/rknpu-http.sh" {
		t.Errorf("the flag must win over $RKNPU_HOME: rknpu_sidecar_cmd = %v", got["rknpu_sidecar_cmd"])
	}
	if got := seed(t, "-accelerators", "rknpu"); got["rknpu_sidecar_cmd"] != "/srv/npu-from-env/rknpu-http.sh" {
		t.Errorf("$RKNPU_HOME must apply without the flag: rknpu_sidecar_cmd = %v", got["rknpu_sidecar_cmd"])
	}
	t.Setenv("RKNPU_HOME", "")
	got := seed(t, "-accelerators", "rknpu")
	cmd, _ := got["rknpu_sidecar_cmd"].(string)
	if !strings.HasSuffix(cmd, "/rknpu/rknpu-http.sh") || strings.Contains(cmd, "__") {
		t.Errorf("the default must be <home>/rknpu: rknpu_sidecar_cmd = %q", cmd)
	}
	if got["rknpu_endpoint"] != "http://127.0.0.1:18815" {
		t.Errorf("rknpu_endpoint = %v", got["rknpu_endpoint"])
	}
	if accs, _ := got["accelerators"].([]any); len(accs) != 1 || accs[0] != "rknpu" {
		t.Errorf("accelerators = %v, want [rknpu]", got["accelerators"])
	}
}
