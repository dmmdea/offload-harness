package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/servingcfg"
)

// The load fence places a model by the serving config when layers do not declare it (register C-86a).
// The unload side has done so since F15 (gpualloc.LeaseScope.Locate); the fence is armed from
// Config.ModelPins (Load), so ModelPins is where the two have to agree.
//
// Synthetic card ids only: the repeated-nibble heads the leak gate treats as placeholders.
const (
	fenceCard0 = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"
	fenceCard1 = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff"
	fenceCard2 = "GPU-cccc3333-dddd-eeee-ffff-000000000000"
)

// fenceServingYAML is shaped like the llama-swap config of a three-card box that declares no layers:
// nearly every llama.cpp entry pins its cards with an env CUDA_VISIBLE_DEVICES entry (card ids), the
// two vLLM seats are proxy: entries (their cards are pinned in their own unit), and a few entries pin
// nothing the harness can read.
const fenceServingYAML = `
models:
  gemma-4-26b:
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard0 + `"]
    cmd: llama-server --port ${PORT}
  gemma-4-12b:
    aliases: [workhorse-12b]
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard0 + `"]
  gemma-4-e4b:
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard0 + `"]
  gemma-4-e2b:
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard0 + `"]
  embeddinggemma:
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard0 + `"]
  bge-reranker:
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard0 + `"]
  whisper-stt:
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard2 + `"]
  qwen3-vl-8b:
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard2 + `"]
  embeddinggemma2:
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard2 + `"]
  qwen3.8-27b-262k:
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard0 + `,` + fenceCard2 + `"]
  agent-pool-2card:
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard0 + `,` + fenceCard2 + `"]
  qwen3.8-27b-vllm:
    proxy: http://127.0.0.1:1234
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard0 + `"]
  qwen3.8-27b-vllm-3card:
    proxy: http://127.0.0.1:1235
  unpinned-dual:
    env: ["FOO=bar"]
    cmd: llama-server --device CUDA0 -sm layer
  cmd-only-pin:
    cmd: CUDA_VISIBLE_DEVICES=` + fenceCard2 + ` llama-server
  macro-pin:
    env: ["CUDA_VISIBLE_DEVICES=${gpu_pin}"]
  index-no-order:
    env: ["CUDA_VISIBLE_DEVICES=0"]
  index-ordered:
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=2"]
  claimed-a:
    aliases: [claimed]
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard0 + `"]
  claimed-b:
    aliases: [claimed]
    env: ["CUDA_VISIBLE_DEVICES=` + fenceCard2 + `"]
`

func writeFenceServing(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A box that declares no layers (the installer's tier seeds write none) has no declared card for any
// seat, so ModelPins placed nothing and the fence read every model as every card. The serving config
// is where those seats really are pinned: the env CUDA_VISIBLE_DEVICES of the entry, with F15's
// strictness (a bare index only beside CUDA_DEVICE_ORDER=PCI_BUS_ID; a proxy: seat, an entry that pins
// nothing in env, a macro and an alias two entries claim differently stay unknown).
func TestModelPinsFallBackToTheServingConfigOnABoxWithNoLayers(t *testing.T) {
	c := Default()
	c.ServingConfigPath = writeFenceServing(t, fenceServingYAML)
	placed := []struct {
		model string
		want  []string
	}{
		{"gemma-4-26b", []string{fenceCard0}},
		{"GEMMA-4-12B", []string{fenceCard0}},   // names compare case-insensitively
		{"workhorse-12b", []string{fenceCard0}}, // an alias is placed where its entry is
		{"whisper-stt", []string{fenceCard2}},
		{"qwen3-vl-8b", []string{fenceCard2}},
		{"embeddinggemma2", []string{fenceCard2}},
		{"qwen3.8-27b-262k", []string{fenceCard0, fenceCard2}}, // a pair spans both cards
		{"index-ordered", []string{"2"}},
	}
	for _, tc := range placed {
		got, ok := c.ModelPins(tc.model)
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ModelPins(%q) = %v, %v; want %v from the serving config", tc.model, got, ok, tc.want)
		}
	}
	unknown := []string{
		"qwen3.8-27b-vllm",       // a proxy: seat, even with an env pin beside it
		"qwen3.8-27b-vllm-3card", // a proxy: seat
		"unpinned-dual",          // pins no CUDA_VISIBLE_DEVICES in env
		"cmd-only-pin",           // device flags and cmd pins are never read
		"macro-pin",              // not a literal device list
		"index-no-order",         // a CUDA index is not an nvidia-smi index without PCI_BUS_ID
		"claimed",                // an alias two entries claim with different pins
		"not-in-the-yaml",
		"", "  ",
	}
	for _, m := range unknown {
		if pins, ok := c.ModelPins(m); ok || len(pins) != 0 {
			t.Errorf("ModelPins(%q) = %v, %v; a model the serving config cannot place is UNKNOWN, which the fence reads as every card", m, pins, ok)
		}
	}
}

// The direction of every doubt is "fence": a serving config that is not set, cannot be read or is
// not YAML places no model.
func TestModelPinsServingConfigThatCannotBeReadPlacesNothing(t *testing.T) {
	for name, path := range map[string]string{
		"unset":    "",
		"blank":    "   ",
		"missing":  filepath.Join(t.TempDir(), "gone.yaml"),
		"not yaml": writeFenceServing(t, "models: [unclosed"),
	} {
		c := Default()
		c.ServingConfigPath = path
		if pins, ok := c.ModelPins("gemma-4-26b"); ok || len(pins) != 0 {
			t.Errorf("%s serving config: ModelPins = %v, %v; want unknown", name, pins, ok)
		}
	}
}

// Failing closed is invisible: the refusal a blocked load prints names only the lease holder, so a
// serving config that moved, is mid-edit or has a typo would read as the fence's old behaviour. The
// fence's reader says so once per cause, through ModelPins, the one door every caller asks. A layer
// that places a model never reads the file, and an unset path is the default: neither says anything.
func TestModelPinsSaysOnceThatTheServingConfigCannotBeRead(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	t.Cleanup(servingcfg.SetNoteLogger(func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}))
	heard := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}

	c := Default()
	c.ServingConfigPath = filepath.Join(t.TempDir(), "gone.yaml")
	for i := 0; i < 3; i++ {
		if pins, ok := c.ModelPins("gemma-4-26b"); ok || len(pins) != 0 {
			t.Fatalf("ModelPins with an unreadable serving config = %v, %v; want unknown", pins, ok)
		}
	}
	got := heard()
	if len(got) != 1 || !strings.Contains(got[0], c.ServingConfigPath) || !strings.Contains(got[0], "fences every seat") {
		t.Fatalf("an unreadable serving config must be said once, naming the file and what the fence does instead; heard %q", got)
	}

	layered := CompositeFixture()
	layered.ServingConfigPath = filepath.Join(t.TempDir(), "never-read.yaml")
	if pins, ok := layered.ModelPins("qwen3-vl-8b"); !ok || !reflect.DeepEqual(pins, []string{"2"}) {
		t.Fatalf("ModelPins(qwen3-vl-8b) = %v, %v; the layers place it", pins, ok)
	}
	unset := Default()
	unset.ModelPins("gemma-4-26b")
	if again := heard(); len(again) != 1 {
		t.Fatalf("a model the layers place, and an unset path, must say nothing; heard %q", again)
	}
}

// Layers outrank the serving config for a model both declare (as in LeaseScope.Locate), and a model
// no layer declares is placed by the serving config even on a box that does declare layers.
func TestLayersOutrankTheServingConfigForTheFence(t *testing.T) {
	c := CompositeFixture()
	c.ServingConfigPath = writeFenceServing(t, `
models:
  qwen3-vl-8b:
    env: ["CUDA_VISIBLE_DEVICES=`+fenceCard0+`"]
  gemma-4-26b-agent:
    env: ["CUDA_VISIBLE_DEVICES=`+fenceCard2+`"]
  only-in-the-yaml:
    env: ["CUDA_VISIBLE_DEVICES=`+fenceCard2+`"]
`)
	// qwen3-vl-8b is the layers' card-2 seat: the yaml's card 0 does not move it.
	if got, ok := c.ModelPins("qwen3-vl-8b"); !ok || !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("ModelPins(qwen3-vl-8b) = %v, %v; the layers' pin [2] outranks the serving config", got, ok)
	}
	if got, ok := c.ModelPins("gemma-4-26b-agent"); !ok || !reflect.DeepEqual(got, []string{"0"}) {
		t.Errorf("ModelPins(gemma-4-26b-agent) = %v, %v; the layers' pin [0] outranks the serving config", got, ok)
	}
	if got, ok := c.ModelPins("only-in-the-yaml"); !ok || !reflect.DeepEqual(got, []string{fenceCard2}) {
		t.Errorf("ModelPins(only-in-the-yaml) = %v, %v; no layer declares it, so the serving config places it", got, ok)
	}
	if pins, ok := c.ModelPins("declared-nowhere"); ok || len(pins) != 0 {
		t.Errorf("ModelPins(declared-nowhere) = %v, %v; neither places it: unknown", pins, ok)
	}
}

// What the fence gets from the serving config is a copy: a caller that edits the slice it was
// handed (the run registry keeps it) must not edit the next answer.
func TestModelPinsFromTheServingConfigAreACopy(t *testing.T) {
	c := Default()
	c.ServingConfigPath = writeFenceServing(t, fenceServingYAML)
	// A file edited moments ago is never memoised, and the copy is the memo's to protect.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(c.ServingConfigPath, old, old); err != nil {
		t.Fatal(err)
	}
	first, ok := c.ModelPins("qwen3.8-27b-262k")
	if !ok || len(first) != 2 {
		t.Fatalf("ModelPins = %v, %v", first, ok)
	}
	first[0] = "GPU-scribbled"
	again, _ := c.ModelPins("qwen3.8-27b-262k")
	if strings.Contains(strings.Join(again, ","), "scribbled") {
		t.Fatalf("a caller's edit reached the next answer: %v", again)
	}
}

// LayerPins is the layers' reading alone. The unload list asks it first so that it can say which of
// the two placed a model (LeaseScope.Locate: "by layers" or "by the serving config pin").
func TestLayerPinsReadTheLayersAloneAndNeverTheServingConfig(t *testing.T) {
	c := CompositeFixture()
	c.ServingConfigPath = writeFenceServing(t, "models:\n  only-in-the-yaml:\n    env: [\"CUDA_VISIBLE_DEVICES="+fenceCard2+"\"]\n")
	if got, ok := c.LayerPins("qwen3-vl-8b"); !ok || !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("LayerPins(qwen3-vl-8b) = %v, %v; want the layers' [2]", got, ok)
	}
	if pins, ok := c.LayerPins("only-in-the-yaml"); ok || len(pins) != 0 {
		t.Errorf("LayerPins(only-in-the-yaml) = %v, %v; the serving config is not the layers", pins, ok)
	}
	plain := Default()
	plain.ServingConfigPath = c.ServingConfigPath
	if pins, ok := plain.LayerPins("only-in-the-yaml"); ok || len(pins) != 0 {
		t.Errorf("a box with no layers has no layer pins, got %v, %v", pins, ok)
	}
}
