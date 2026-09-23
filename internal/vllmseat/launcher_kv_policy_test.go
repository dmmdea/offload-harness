package vllmseat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readLauncher(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// TestLauncherCarriesTheKVLoadFailurePolicy (2026-09-23). The shipped LMCache overlay's patch 05 (LMCache #4709) hands
// the blocks of a failed L2 retrieve back to vLLM, which recomputes them only under kv_load_failure_policy "recompute"
// and fails the request under its default "fail". The policy is a per-seat env knob (SEAT_KV_LOAD_FAILURE_POLICY),
// validated before anything starts and spliced into --kv-transfer-config. The splice is a shell string of escaped
// JSON, so the test renders it the way bash does for every accepted value and parses the result: a quoting slip
// that vLLM would reject at start fails here instead.
func TestLauncherCarriesTheKVLoadFailurePolicy(t *testing.T) {
	s := readLauncher(t)
	for _, must := range []string{
		`KV_POLICY="${SEAT_KV_LOAD_FAILURE_POLICY:-}"`,
		`""|recompute|fail) ;;`,
		`KV_POLICY_JSON=""; [ -n "$KV_POLICY" ] && KV_POLICY_JSON=",\"kv_load_failure_policy\":\"$KV_POLICY\""`,
	} {
		if !strings.Contains(s, must) {
			t.Fatalf("seat_fg.sh lost part of the kv_load_failure_policy knob: %q missing", must)
		}
	}
	// The validation must run after the log redirect (so the refusal reaches the seat log) and before the MP server.
	execAt := strings.Index(s, `exec > >(tee -a "$LOG") 2>&1`)
	validAt := strings.Index(s, `""|recompute|fail) ;;`)
	mpAt := strings.Index(s, `systemd-run --unit="$MP_UNIT"`)
	if !(execAt >= 0 && execAt < validAt && validAt < mpAt) {
		t.Fatalf("policy validation is not between the log redirect and the MP server start (exec %d, validate %d, mp %d)", execAt, validAt, mpAt)
	}

	m := regexp.MustCompile(`--kv-transfer-config "((?:[^"\\]|\\.)*)"`).FindStringSubmatch(s)
	if m == nil {
		t.Fatal("seat_fg.sh no longer passes --kv-transfer-config as one double-quoted shell word")
	}
	if !strings.Contains(m[1], `$KV_POLICY_JSON`) {
		t.Fatalf("--kv-transfer-config does not splice $KV_POLICY_JSON: %s", m[1])
	}
	for _, policy := range []string{"", "recompute", "fail"} {
		splice := ""
		if policy != "" {
			splice = `,"kv_load_failure_policy":"` + policy + `"`
		}
		word := strings.NewReplacer(`\"`, `"`, `$KV_POLICY_JSON`, splice, `$MP_PORT`, "18796").Replace(m[1])
		var cfg map[string]any
		if err := json.Unmarshal([]byte(word), &cfg); err != nil {
			t.Fatalf("policy %q renders --kv-transfer-config that is not JSON (%v): %s", policy, err, word)
		}
		got, has := cfg["kv_load_failure_policy"]
		if policy == "" && has {
			t.Fatalf("an empty policy must leave vLLM's default in force, got %v", got)
		}
		if policy != "" && got != policy {
			t.Fatalf("policy %q rendered kv_load_failure_policy=%v", policy, got)
		}
		if cfg["kv_connector"] != "LMCacheMPConnector" {
			t.Fatalf("policy %q lost the connector: %s", policy, word)
		}
	}
}

// TestLauncherAppendsMPServerExtraArgs pins SEAT_MP_EXTRA_ARGS: extra `lmcache server` arguments (an MP-server arm such
// as --max-gpu-workers) come from the seat env and land AFTER the launcher's own, so an arm never edits the launcher.
func TestLauncherAppendsMPServerExtraArgs(t *testing.T) {
	s := readLauncher(t)
	if !strings.Contains(s, `MPX=(); [ -n "${SEAT_MP_EXTRA_ARGS:-}" ] && read -r -a MPX <<< "$SEAT_MP_EXTRA_ARGS"`) {
		t.Fatal("seat_fg.sh no longer reads SEAT_MP_EXTRA_ARGS into an array")
	}
	if !strings.Contains(s, `--supported-transfer-mode auto "${L2ARG[@]}" "${MPX[@]}"; then`) {
		t.Fatal("SEAT_MP_EXTRA_ARGS is not appended after the launcher's own lmcache server arguments")
	}
}

// TestLauncherReportsDxgFailuresSinceThePreviousStart (2026-09-23). The dxg residency failures a failed WSL2 start leaves
// in the kernel log stay there until the distro restarts, so a count "since the distro started" repeats the same
// warning at every later start — including starts that are perfectly healthy (measured: the 0.28 start after three
// failed 0.30 starts served normally while the old count read 56). The launcher keeps the last count in a file and
// warns only about NEW failures, resetting when the log shrinks (the distro restarted).
func TestLauncherReportsDxgFailuresSinceThePreviousStart(t *testing.T) {
	s := readLauncher(t)
	for _, must := range []string{
		`DXG_FILE="$WORK/.dxg-failures"`,
		`dxg_new=$(( ${dxg_all:-0} - 10#$dxg_prev ))`,
		`[ "${dxg_all:-0}" -lt "$((10#$dxg_prev))" ] && dxg_prev=0`,
		`echo "${dxg_all:-0}" > "$DXG_FILE"`,
	} {
		if !strings.Contains(s, must) {
			t.Fatalf("seat_fg.sh no longer reports dxg failures as a delta since the previous start: %q missing", must)
		}
	}
}

// TestShippedLMCacheOverlayIsComplete (2026-09-23). The overlay a pipeline-parallel seat needs is built from the patch
// set shipped beside the rebuild script (lmcache-patches/, named apart from the overlay it builds — the default
// output is <seat dir>/lmcache-overlay, and a kit of the same name would be swapped away by its own build). Every
// patch the script's verify step demands a marker for must be there, the script must default to its own directory,
// and every file must stay LF: `patch` on the Linux box misses every context line of a CRLF diff, and the checkout
// is on Windows.
func TestShippedLMCacheOverlayIsComplete(t *testing.T) {
	dir := filepath.Join("..", "..", "setup", "templates", "vllm-seat", "lmcache-patches")
	for _, f := range []string{
		"01-pr4253.diff", "02-house-pp-layout.diff", "04-house-pp-register-bind.diff",
		"05-backport-pr4709.diff", "06-backport-pr5249.diff", "smoke-overlay.py",
		"repatch-lmcache-overlay.sh", "README.md",
	} {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f)))
		if err != nil {
			t.Fatalf("shipped overlay is missing %s: %v", f, err)
		}
		if strings.Contains(string(raw), "\r") {
			t.Fatalf("%s carries CR bytes; patch(1) on the Linux box would miss every context line (.gitattributes must keep lmcache-patches/ LF)", f)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "repatch-lmcache-overlay.sh"))
	script := string(raw)
	for _, must := range []string{
		`PATCHES="${PATCHES:-$HERE}"`,
		`grep -E '^SEAT_KV_LOAD_FAILURE_POLICY=' "$env"`,
		`patch -p1 -R --dry-run`,
	} {
		if !strings.Contains(script, must) {
			t.Fatalf("repatch-lmcache-overlay.sh lost %q", must)
		}
	}
}
