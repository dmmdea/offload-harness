package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// CODE3: a cap with no usable stride cannot be checked, so it is a refusal (LatentTokens answers 0
// for a zero stride, which used to read "fits" and skip the pre-lease typed defer).
func TestTokenCapRefusalRefusesACapWithoutAUsableStride(t *testing.T) {
	for _, stride := range []int{0, -8, 4, 7, 12, 32} {
		err := TokenCapRefusal("sdcpp_max_tokens", 832, 480, 49, stride, 0, 6000)
		if err == nil {
			t.Errorf("stride %d with a cap: want a refusal, got nil", stride)
			continue
		}
		if !strings.Contains(err.Error(), "TOKEN_CAP_EXCEEDED") || !strings.Contains(err.Error(), "stride") {
			t.Errorf("stride %d: the refusal must be the typed token-cap one and name the stride: %v", stride, err)
		}
	}
	for _, stride := range []int{8, 16} {
		if err := TokenCapRefusal("sdcpp_max_tokens", 64, 64, 5, stride, 0, 6000); err != nil {
			t.Errorf("stride %d inside the cap: %v", stride, err)
		}
	}
	// no cap configured = no check, whatever the stride
	if err := TokenCapRefusal("sdcpp_max_tokens", 832, 480, 49, 0, 0, 0); err != nil {
		t.Errorf("no cap configured must be no check, got %v", err)
	}
}

// TST4: the Node screen splits on every JS \s (a non-breaking space among them), so a cpu hidden
// behind one is refused there; the Go screen must refuse it too.
func TestScreenExtraArgsSeesACpuBehindAUnicodeSpace(t *testing.T) {
	for _, sp := range []string{"\u00a0", "\u2003", "\u3000", "\ufeff", "\u2028", "\u202f"} {
		arg := "--threads=" + sp + "cpu"
		if _, _, _, found := ScreenExtraArgs(ExtraArgsSdcpp, []string{arg}); !found {
			t.Errorf("%q passed the Go screen; the Node screen refuses it", arg)
		}
	}
	if _, _, _, found := ScreenExtraArgs(ExtraArgsSdcpp, []string{"--threads=\u00a04"}); found {
		t.Error("a unicode space around an ordinary value is not a refusal")
	}
}

// CODE2 / TST4: the Go and the Node screens read ONE list (render/testdata/screen-parity-table.json;
// render/igpu-engine.test.mjs reads the same file), so they cannot drift apart again.
func TestGoScreensAgreeWithTheSharedParityTable(t *testing.T) {
	raw, err := os.ReadFile("../../render/testdata/screen-parity-table.json")
	if err != nil {
		t.Fatal(err)
	}
	var tbl struct {
		BackendRefused   []string   `json:"backend_refused"`
		BackendAllowed   []string   `json:"backend_allowed"`
		ExtraArgsRefused [][]string `json:"extra_args_refused"`
		ExtraArgsAllowed [][]string `json:"extra_args_allowed"`
	}
	if err := json.Unmarshal(raw, &tbl); err != nil {
		t.Fatal(err)
	}
	if len(tbl.BackendRefused) < 20 || len(tbl.ExtraArgsRefused) < 10 {
		t.Fatalf("the parity table looks empty: %d backend rows, %d extra-args rows", len(tbl.BackendRefused), len(tbl.ExtraArgsRefused))
	}
	for _, b := range tbl.BackendRefused {
		if CPUBackendRefusal(b) == nil {
			t.Errorf("CPUBackendRefusal(%q) = nil, the shared table (and the Node screen) refuse it", b)
		}
	}
	for _, b := range tbl.BackendAllowed {
		if err := CPUBackendRefusal(b); err != nil {
			t.Errorf("CPUBackendRefusal(%q) = %v, the shared table allows it", b, err)
		}
	}
	for _, args := range tbl.ExtraArgsRefused {
		if _, _, _, found := ScreenExtraArgs(ExtraArgsSdcpp, args); !found {
			t.Errorf("ScreenExtraArgs(%q) passed, the shared table (and the Node screen) refuse it", args)
		}
	}
	for _, args := range tbl.ExtraArgsAllowed {
		if i, a, why, found := ScreenExtraArgs(ExtraArgsSdcpp, args); found {
			t.Errorf("ScreenExtraArgs(%q) refused element %d %q (%s), the shared table allows it", args, i, a, why)
		}
	}
}
