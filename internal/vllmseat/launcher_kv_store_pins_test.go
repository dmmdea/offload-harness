package vllmseat

import (
	"strings"
	"testing"
)

// TestLauncherKVNeedKnobIsWiredAheadOfTheExec pins the text of the B-54 fix on every platform (the run that proves its
// behaviour is Linux-only, launcher_kv_store_run_test.go): the knob exists and is validated, the context length the exec
// passes is a variable the pin block may set to -1 and not a literal, the pin's banner is still printed before the decision,
// and the decision sits between the pin and the exec.
func TestLauncherKVNeedKnobIsWiredAheadOfTheExec(t *testing.T) {
	s := readLauncher(t)
	start := strings.Index(s, "# KV pool PINNED FROM FREE MEMORY")
	banner := strings.Index(s, `echo "seat_fg: KV pool PINNED at ${pool_gib} GiB/worker`)
	execAt := strings.Index(s, `exec "$VENV/bin/vllm" serve`)
	if start < 0 || banner < start || execAt < banner {
		t.Fatalf("launcher lost a landmark: pin block %d, pin banner %d, exec %d", start, banner, execAt)
	}
	pin := s[start:execAt]
	if !strings.Contains(pin[banner-start:], "SEAT_KV_NEED_GIB") {
		t.Fatal("the pin block does not read SEAT_KV_NEED_GIB after the pin banner is printed")
	}
	for _, must := range []string{
		`MAXLEN="${SEAT_MAX_LEN:-131072}"`, // the configured length, with the default the exec used to carry
		`MAXLEN=-1`,                        // the one place the auto-fit is chosen
		`--max-model-len -1`,               // the line that says so
	} {
		if !strings.Contains(pin, must) {
			t.Errorf("the pin block lost %q", must)
		}
	}
	if !strings.Contains(s[execAt:], `--max-model-len "$MAXLEN"`) {
		t.Error(`the exec no longer passes --max-model-len "$MAXLEN"`)
	}
	if strings.Contains(s[execAt:], `--max-model-len "${SEAT_MAX_LEN`) {
		t.Error("the exec passes the configured length directly again: the auto-fit decision cannot reach it")
	}
}

// TestSeatEnvTemplateDocumentsTheKVPinKnobs: the template seat env carries the pin family as a documented, commented-out
// block (they are per-box measured figures, so no renderer emits them), and SEAT_KV_NEED_GIB says where its value comes from.
func TestSeatEnvTemplateDocumentsTheKVPinKnobs(t *testing.T) {
	env := readSeatScript(t, "windows-wsl/seat.env")
	for _, must := range []string{
		"\n#SEAT_KV_HEADROOM_GIB=",
		"\n#SEAT_KV_NEED_GIB=",
		"KV cache is needed",
	} {
		if !strings.Contains(env, must) {
			t.Errorf("windows-wsl/seat.env lost %q", strings.TrimSpace(must))
		}
	}
	// Commented out: a rendered seat must not pin a pool nobody measured.
	for _, l := range strings.Split(env, "\n") {
		if strings.HasPrefix(l, "SEAT_KV_") || strings.HasPrefix(l, "SEAT_NONKV_") {
			t.Errorf("the template seat env enables a KV pin knob: %q", l)
		}
	}
}
