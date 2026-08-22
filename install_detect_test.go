package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// TestInstallDetectEmitsAccelerators proves the detection entry point actually
// carries hwdetect.DetectAccelerators' answer into the emitted verdict — the
// wiring, not just the classifier. The runner is faked (this box has no Hailo
// device); real hardware facts are probed as normal and ignored here.
func TestInstallDetectEmitsAccelerators(t *testing.T) {
	orig := hailortcliRun
	defer func() { hailortcliRun = orig }()
	hailortcliRun = func(args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "scan":
			return "Hailo Devices:\n[-] Device: 0000:03:00.0\n", nil
		case "fw-control identify":
			return "Device Architecture: HAILO8L\nPart Number: HM21LB1C2KAE\n", nil
		}
		return "", errors.New("unexpected args")
	}

	out := captureStdout(t, func() {
		if err := runInstallDetect([]string{"-json"}); err != nil {
			t.Fatalf("install detect -json: %v", err)
		}
	})

	var got struct {
		Verdict struct {
			Accelerators []string `json:"accelerators"`
		} `json:"verdict"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("emitted JSON did not parse: %v\n%s", err, out)
	}
	if len(got.Verdict.Accelerators) != 1 || got.Verdict.Accelerators[0] != "hailo-8l" {
		t.Fatalf("verdict.accelerators = %v, want [hailo-8l]", got.Verdict.Accelerators)
	}
}

// TestInstallPlanEmitsAccelerators covers the SECOND verdict-emitting entry
// point (ruling R1): `install plan -json` must carry the probe's answer too,
// through the full plan resolution against the repo's own
// setup/templates/profiles.json (root "." — tests run from the repo root).
// Facts are probed from real hardware, exactly as a real `install plan` would.
// The plan must also predict the SEED the install would write (final review
// finding): the accelerator's config_seed merges over the tier seed with
// __HAILO_HOME__ expanded, exactly as install.ps1 does.
func TestInstallPlanEmitsAccelerators(t *testing.T) {
	orig := hailortcliRun
	defer func() { hailortcliRun = orig }()
	hailortcliRun = func(args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "scan":
			return "Hailo Devices:\n[-] Device: 0000:03:00.0\n", nil
		case "fw-control identify":
			return "Device Architecture: HAILO8L\nPart Number: HM21LB1C2KAE\n", nil
		}
		return "", errors.New("unexpected args")
	}
	t.Setenv("HAILO_HOME", "") // pin the default <home>/hailo resolution, not this box's env

	out := captureStdout(t, func() {
		if err := runInstallPlan([]string{"-json", "-root", ".", "-home", t.TempDir()}); err != nil {
			t.Fatalf("install plan -json: %v", err)
		}
	})

	var got struct {
		Verdict struct {
			Profile      string   `json:"profile"`
			Accelerators []string `json:"accelerators"`
		} `json:"verdict"`
		ConfigSeed map[string]any `json:"config_seed"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("emitted JSON did not parse: %v\n%s", err, out)
	}
	if got.Verdict.Profile == "" {
		t.Fatal("plan emitted no profile — the verdict half of the plan is missing")
	}
	if len(got.Verdict.Accelerators) != 1 || got.Verdict.Accelerators[0] != "hailo-8l" {
		t.Fatalf("verdict.accelerators = %v, want [hailo-8l]", got.Verdict.Accelerators)
	}
	// The seed half: all five accelerator keys, with the token expanded.
	for _, k := range []string{"accelerators", "hailo_endpoint", "hailo_sidecar_cmd", "hailo_timeout_sec", "hailo_idle_sec"} {
		if _, ok := got.ConfigSeed[k]; !ok {
			t.Errorf("config_seed missing accelerator key %q — the plan does not predict the install's seed", k)
		}
	}
	cmd, _ := got.ConfigSeed["hailo_sidecar_cmd"].(string)
	if strings.Contains(cmd, "__HAILO_HOME__") {
		t.Errorf("hailo_sidecar_cmd still carries the __HAILO_HOME__ token: %q", cmd)
	}
	if !strings.HasSuffix(cmd, "/hailo/hailo-http.cmd") {
		t.Errorf("hailo_sidecar_cmd = %q, want the default <home>/hailo expansion ending in /hailo/hailo-http.cmd", cmd)
	}
}

// TestInstallDetectOmitsAcceleratorsWithoutDevice: with no hailortcli (the
// normal case) the verdict must serialise exactly as before — no accelerators
// key at all, so nothing downstream changes on a box with no NPU.
func TestInstallDetectOmitsAcceleratorsWithoutDevice(t *testing.T) {
	orig := hailortcliRun
	defer func() { hailortcliRun = orig }()
	hailortcliRun = func(args ...string) (string, error) {
		return "", errors.New("exec: hailortcli: not found")
	}

	out := captureStdout(t, func() {
		if err := runInstallDetect([]string{"-json"}); err != nil {
			t.Fatalf("install detect -json: %v", err)
		}
	})
	if strings.Contains(out, `"accelerators"`) {
		t.Fatalf("no-NPU verdict must omit accelerators entirely, got:\n%s", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}
