package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeatLauncherPinsTheV1ModelRunnerOnWSL2 (ADR 0048 Amendment 1, 2026-09-17).
// vLLM >= 0.29 selects its V2 GPU model runner by default; that runner needs
// UVA, which vLLM reports unavailable under WSL2, and with vLLM's own
// VLLM_WSL2_ENABLE_PIN_MEMORY=1 it starts and then dies in kernel warm-up
// (CUDA error: invalid device ordinal, measured on an RTX 5060 Ti). The
// launcher therefore pins the V1 runner on WSL2 for vLLM >= 0.29 unless the env
// file already chose; 0.28 keeps whichever runner it picks, and its V2 runner runs on WSL2 (the TP2 DFlash
// spec-decode arms logged `gpu_worker.py:396] Using V2 Model Runner` and served). The launcher is shell rendered
// verbatim, so this pins its text.
func TestSeatLauncherPinsTheV1ModelRunnerOnWSL2(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	// Three facts pinned: the WSL2 detector, the >= 0.29 version gate (0.28's
	// V2 runner runs on WSL2 — the DFlash spec-decode arms logged it — so a
	// WSL-only pin would silently change such a seat), and the pin itself.
	const line = `export VLLM_USE_V2_MODEL_RUNNER="${VLLM_USE_V2_MODEL_RUNNER:-0}"`
	for _, must := range []string{
		`if grep -qi microsoft /proc/version 2>/dev/null; then`,
		`if [ "$vllm_major" -gt 0 ] || [ "$vllm_minor" -ge 29 ]; then`,
		line,
	} {
		if !strings.Contains(s, must) {
			t.Fatalf("seat_fg.sh no longer pins the V1 model runner on WSL2 for vLLM >= 0.29 (a 0.29 seat there fails with 'UVA is not available' at start) or lost the version gate that leaves 0.28's runner choice alone: %q missing", must)
		}
	}
	// The pin must come AFTER the venv PATH export (same block the engine reads)
	// and BEFORE the serve line, so it is in force for `exec vllm serve`.
	pathAt := strings.Index(s, `export PATH="$VENV/bin`)
	pinAt := strings.Index(s, line)
	serveAt := strings.Index(s, `exec "$VENV/bin/vllm" serve`)
	if pathAt < 0 || serveAt < 0 || !(pathAt < pinAt && pinAt < serveAt) {
		t.Errorf("the V1-runner pin is not between the venv PATH export and the serve line (path %d, pin %d, serve %d)", pathAt, pinAt, serveAt)
	}
}
