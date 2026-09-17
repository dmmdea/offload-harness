package main

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestDoctorPrintsConfigFindings (registers S-38, S-42): doctor is where an
// operator goes to find out why the box behaves the way it does, so every
// non-fatal configuration finding has to be VISIBLE there — as a FAIL row and a
// non-zero exit, exactly like a missing model alias or an unbound vLLM seat.
//
// Until now these were invisible: the retired-key note was one stderr line at
// startup that scrolls past every command, and nothing at all reported a
// media-lane GPU wait sitting at ten minutes against a 90 s design (C-33) or a
// fleet remote pointed at a serving port retired weeks earlier.
func TestDoctorPrintsConfigFindings(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.GPUWaitMs = 600000    // the live value (C-33)
	cfg.VisionGPUWaitSec = 90 // against the vision lane's default
	cfg.DelegateRemotes = []string{"http://node-a:18798"}
	cfg.RetiredKeys = []string{"videogen_wait_ms"}

	var out strings.Builder
	err := doctorRun(cfg, nil, &out)
	got := out.String()
	if err == nil {
		t.Fatalf("doctor must exit non-zero on a config finding:\n%s", got)
	}
	for _, want := range []string{
		"gpu_wait_ms", "600000", "vision_gpu_wait_sec", // S-42, the timeout-chain finding
		"http://node-a:18798", "18811", // S-38, the fleet-remote shape
		"videogen_wait_ms", "retired", // S-42, the retired key still in the file
	} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor output must name %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "FAIL") {
		t.Errorf("config findings must print as FAIL rows:\n%s", got)
	}
	if !strings.Contains(err.Error(), "3 ") {
		t.Errorf("the exit error must count the findings, got %v", err)
	}
}

// TestDoctorSilentOnACleanConfig: the section must be entirely absent — not an
// empty header — on a config with nothing to say, so a green doctor stays as
// short as it is today and the rows above keep their meaning.
func TestDoctorSilentOnACleanConfig(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	var out strings.Builder
	if err := doctorRun(cfg, nil, &out); err != nil {
		t.Fatalf("a default config must pass doctor: %v\n%s", err, out.String())
	}
	if got := out.String(); strings.Contains(got, "config findings") {
		t.Fatalf("no findings section expected on a clean config:\n%s", got)
	}
}
