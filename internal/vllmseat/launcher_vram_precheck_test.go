package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLauncherVRAMPrecheckRunsBetweenMPStopAndMPStart pins the 2026-09-22 launcher fix. The VRAM
// precheck used to run AFTER the new LMCache MP server started, so it measured that server's own
// CUDA contexts and burned the whole SEAT_VRAM_WAIT_SEC on every start. It must run after the old
// MP unit is stopped and before the new one is started, and honour a per-device floor
// (SEAT_VRAM_FLOOR_MIB_<index>) so a display card carrying the desktop can clear at all.
func TestLauncherVRAMPrecheckRunsBetweenMPStopAndMPStart(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	stop := strings.Index(s, `systemctl stop "$MP_UNIT"`)
	check := strings.Index(s, "# VRAM precheck")
	start := strings.Index(s, `systemd-run --unit="$MP_UNIT"`)
	if stop < 0 || check < 0 || start < 0 {
		t.Fatalf("launcher lost a landmark: stop=%d precheck=%d start=%d", stop, check, start)
	}
	if !(stop < check && check < start) {
		t.Fatalf("the VRAM precheck must sit between the old MP unit's stop (%d) and the new MP server's start (%d); it is at %d", stop, start, check)
	}
	if strings.Count(s, "# VRAM precheck") != 1 {
		t.Fatal("the launcher carries more than one VRAM precheck")
	}
	if !strings.Contains(s, `vf_var="SEAT_VRAM_FLOOR_MIB_$d"; vf="${!vf_var:-$VFLOOR}"`) {
		t.Fatal("the precheck no longer honours the per-device floor SEAT_VRAM_FLOOR_MIB_<index>")
	}

	stopRaw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_stop.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stopRaw), `vf_var="SEAT_VRAM_FLOOR_MIB_$d"; vf="${!vf_var:-$VFLOOR}"`) {
		t.Fatal("seat_stop.sh's VRAM readback no longer honours the per-device floor")
	}
}

// TestRenderedEnvCarriesPerSeatStatusFileAndHangWatchdog: every rendered seat env names its OWN
// cache-server status file (two seats on one box must not overwrite each other's verdict) and
// EXPORTS the engine-hang watchdog (seat_fg.sh sources the env without set -a).
func TestRenderedEnvCarriesPerSeatStatusFileAndHangWatchdog(t *testing.T) {
	files, err := pipelineFlagship().Artifacts(wslTemplatesDir(), wslRT())
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may carry the template with CRLF; the lines are what is asserted.
	env := strings.ReplaceAll(files["qwen3.8-27b-vllm-3card.env"], "\r\n", "\n")
	for _, want := range []string{
		"\nSEAT_L2_STATUS_FILE=/root/g7/seat-l2-qwen3.8-27b-vllm-3card.status\n",
		"\nexport VLLM_EXECUTE_MODEL_TIMEOUT_SECONDS=120\n",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("rendered env is missing %q", strings.TrimSpace(want))
		}
	}
}
