package servingtmpl

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// A box that pins every seat by GPU UUID (the board reorders CUDA indices on a power loss, and the
// template's own header tells an operator to substitute UUIDs if the cards moved) renders
// `CUDA_VISIBLE_DEVICES=GPU-...`. The checked union read only digits, so such a pin read as "pins no
// CUDA_VISIBLE_DEVICES" and a render that was right was refused. A UUID pin is a pin: compared with
// another UUID pin as the same set of cards, compared with an index declaration only through a card
// table (nothing in the rendered text can turn one into the other, and a pin that cannot be shown to
// be the declared card is REFUSED with the way out, never passed), and never reported as missing.

const (
	uuidA = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"
	uuidB = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff"
	uuidC = "GPU-cccc3333-dddd-eeee-ffff-000000000000"
)

// pinDecl declares one router seat on `device` whose twin is the model `twin`.
func pinDecl(device string) CompositeDecl {
	return CompositeDecl{Tier: "blackwell-3x16", Layers: []config.LayerSpec{{
		Name: "display", Seats: []config.LayerSeat{{Role: "router", Device: device,
			ModelMap: map[string]string{"workhorse": "gemma-4-e4b-display"}}},
	}}}
}

// cardTable is a box's card table as a resolver: the UUID prefixes it lists, and the index each is.
func cardTable(m map[string]string) func(string) (string, bool) {
	return func(pin string) (string, bool) {
		for uuid, idx := range m {
			if strings.HasPrefix(strings.ToLower(uuid), strings.ToLower(pin)) {
				return idx, true
			}
		}
		return "", false
	}
}

const cannotCompare = "Declare the seat's device as the GPU UUID in layers"

// renderedPin is a rendered config whose one twin pins `env` (an env list item) or, when env is "",
// carries `cmd` alone.
func renderedPin(env, cmd string) string {
	var b strings.Builder
	b.WriteString("models:\n  gemma-4-e4b-display:\n")
	if env != "" {
		b.WriteString("    env: [\"" + env + "\"]\n")
	}
	b.WriteString("    cmd: " + cmd + "\n")
	return b.String()
}

func TestCheckCompositeReadsUUIDPins(t *testing.T) {
	cases := []struct {
		name     string
		device   string                      // the declared pin
		env, cmd string                      // the rendered pin
		wantErr  string                      // "" = the render must pass
		resolve  func(string) (string, bool) // the box's card table; nil = none available
	}{
		// What worked before and must keep working: index against index.
		{"index pin agrees", "1", "CUDA_VISIBLE_DEVICES=1", "llama-server -ngl 99", "", nil},
		{"a two-card index pin in the other order is the same set", "0,2", "CUDA_VISIBLE_DEVICES=2,0", "llama-server -ngl 99", "", nil},
		{"index pin disagrees", "1", "CUDA_VISIBLE_DEVICES=0", "llama-server -ngl 99", `declared device pin "1", rendered "0"`, nil},
		{"an entry that pins nothing is still a finding", "1", "", "llama-server -ngl 99", "pins no CUDA_VISIBLE_DEVICES", nil},

		// The defect: a UUID pin read as no pin at all. It is a pin, and with no card table to compare it
		// against an index declaration it is REFUSED with the way out, not passed.
		{"a UUID pin against an index with no card table is refused, not passed", "1", "CUDA_VISIBLE_DEVICES=" + uuidB, "llama-server -ngl 99", cannotCompare, nil},
		{"a UUID pin set inline in the command is read the same way", "1", "", "CUDA_VISIBLE_DEVICES=" + uuidB + " llama-server -ngl 99", cannotCompare, nil},
		{"two UUIDs against a two-card index declaration with no table are refused", "0,2", "CUDA_VISIBLE_DEVICES=" + uuidA + "," + uuidC, "llama-server", cannotCompare, nil},

		// With the box's card table the comparison is made, as index against index.
		{"a UUID that is the declared index passes", "1", "CUDA_VISIBLE_DEVICES=" + uuidB, "llama-server -ngl 99", "", cardTable(map[string]string{uuidA: "0", uuidB: "1", uuidC: "2"})},
		{"a UUID that is another index is a plain mismatch", "1", "CUDA_VISIBLE_DEVICES=" + uuidB, "llama-server -ngl 99", `declared device pin "1", rendered`, cardTable(map[string]string{uuidA: "0", uuidB: "2", uuidC: "1"})},
		{"two UUIDs that are the declared pair pass", "0,2", "CUDA_VISIBLE_DEVICES=" + uuidA + "," + uuidC, "llama-server", "", cardTable(map[string]string{uuidA: "0", uuidB: "1", uuidC: "2"})},
		{"a UUID the table does not list is refused", "1", "CUDA_VISIBLE_DEVICES=" + uuidB, "llama-server -ngl 99", cannotCompare, cardTable(map[string]string{uuidA: "0"})},

		// What a UUID pin can still prove without a table: how many cards it names.
		{"two UUIDs against a one-card declaration is a count mismatch", "1", "CUDA_VISIBLE_DEVICES=" + uuidA + "," + uuidB, "llama-server", `declared device pin "1", rendered`, nil},
		{"one UUID against a two-card declaration is a count mismatch", "0,2", "CUDA_VISIBLE_DEVICES=" + uuidA, "llama-server", `declared device pin "0,2", rendered`, nil},

		// UUID against UUID is the same arithmetic as index against index, prefix-tolerant.
		{"a declared UUID prefix matches the rendered full UUID", "GPU-bbbb2222", "CUDA_VISIBLE_DEVICES=" + uuidB, "llama-server", "", nil},
		{"the same UUID in either case is the same card", strings.ToUpper(uuidB), "CUDA_VISIBLE_DEVICES=" + uuidB, "llama-server", "", nil},
		{"a different card is a different pin", "GPU-bbbb2222", "CUDA_VISIBLE_DEVICES=" + uuidC, "llama-server", `declared device pin "GPU-bbbb2222", rendered`, nil},
		{"two UUIDs in the other order are the same set", uuidA + "," + uuidC, "CUDA_VISIBLE_DEVICES=" + uuidC + "," + uuidA, "llama-server", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decl := pinDecl(tc.device)
			decl.ResolvePin = tc.resolve
			err := CheckComposite(renderedPin(tc.env, tc.cmd), decl, nil)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("the render must pass, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("the render must be refused with %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("refusal = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// The twin pinned by UUID beside one pinned by index: the finding names the entry that is wrong and
// only that one.
func TestCheckCompositeNamesOnlyTheTwinThatDisagrees(t *testing.T) {
	rendered := `
models:
  gemma-4-e4b-display:
    env: ["CUDA_VISIBLE_DEVICES=` + uuidB + `"]
    cmd: llama-server -ngl 99
  gemma-4-e2b-display:
    env: ["CUDA_VISIBLE_DEVICES=0"]
    cmd: llama-server -ngl 99
`
	decl := CompositeDecl{Tier: "blackwell-3x16", Layers: []config.LayerSpec{{
		Name: "display", Seats: []config.LayerSeat{{Role: "router", Device: "1",
			ModelMap: map[string]string{"workhorse": "gemma-4-e4b-display", "triage": "gemma-4-e2b-display"}}},
	}}}
	decl.ResolvePin = cardTable(map[string]string{uuidA: "0", uuidB: "1", uuidC: "2"}) // the e4b twin's UUID is card 1
	err := CheckComposite(rendered, decl, nil)
	if err == nil {
		t.Fatal("the index-pinned twin disagrees with its declaration and must be refused")
	}
	if !strings.Contains(err.Error(), "gemma-4-e2b-display") || strings.Contains(err.Error(), "gemma-4-e4b-display") {
		t.Fatalf("only the e2b twin is wrong, got %v", err)
	}
}
