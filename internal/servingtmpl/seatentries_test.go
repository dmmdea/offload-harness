package servingtmpl

import (
	"reflect"
	"strings"
	"testing"
)

const seatEntriesYAML = `
macros:
  common: "--port ${PORT} --host 127.0.0.1"
models:
  zeta-llama:
    aliases: [z, " zed "]
    cmd: >-
      /bin/llama-server -m z.gguf ${common}
    checkEndpoint: /health
  alpha-wrapped:
    cmd: /seats/alpha-cmd.sh
    proxy: http://127.0.0.1:18797
    checkEndpoint: none
  bare: {}
`

// TestSeatEntriesReadsEverySeatAsLlamaSwapRunsIt: macros are expanded in the command, entries come
// back sorted by id, and what a seat does not state stays empty (never guessed).
func TestSeatEntriesReadsEverySeatAsLlamaSwapRunsIt(t *testing.T) {
	got, err := SeatEntries(seatEntriesYAML)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if want := []string{"alpha-wrapped", "bare", "zeta-llama"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	alpha, bare, zeta := got[0], got[1], got[2]
	if alpha.Cmd != "/seats/alpha-cmd.sh" || alpha.Proxy != "http://127.0.0.1:18797" || alpha.CheckEndpoint != "none" {
		t.Errorf("alpha = %+v", alpha)
	}
	if bare.Cmd != "" || bare.Proxy != "" || bare.CheckEndpoint != "" || len(bare.Aliases) != 0 {
		t.Errorf("a seat that states nothing must read as nothing, got %+v", bare)
	}
	if !strings.Contains(zeta.Cmd, "llama-server") || strings.Contains(zeta.Cmd, "${common}") || !strings.Contains(zeta.Cmd, "--host 127.0.0.1") {
		t.Errorf("the macro was not expanded in %q", zeta.Cmd)
	}
	if want := []string{"z", "zed"}; !reflect.DeepEqual(zeta.Aliases, want) {
		t.Errorf("aliases = %v, want %v", zeta.Aliases, want)
	}
	if !zeta.Answers("ZED") || !zeta.Answers(" zeta-llama ") || zeta.Answers("alpha-wrapped") || zeta.Answers("") {
		t.Errorf("Answers must match id or alias, ignoring case and padding, and nothing else")
	}
}

// TestSeatEntriesRefusesNonYAML: an unreadable file is an error, not an empty seat list.
func TestSeatEntriesRefusesNonYAML(t *testing.T) {
	if _, err := SeatEntries("models: [unclosed"); err == nil {
		t.Fatal("a text that is not YAML must be an error")
	}
	got, err := SeatEntries("healthCheckTimeout: 300\n")
	if err != nil || len(got) != 0 {
		t.Fatalf("a config with no models is an empty list, not an error: %v %v", got, err)
	}
}
