package gpualloc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The serving config places a model that layers do not declare (register F15): its entry's env pin,
// `CUDA_VISIBLE_DEVICES=<uuid>[,<uuid>]` (UUIDs, sometimes indices), matched against the card table.
// Only the env pin counts. These are synthetic ids (the repeated-nibble heads of the card fixture).

const servingYAML = `
models:
  yaml-card0:
    aliases: [yaml-card0-alias]
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"]
    cmd: llama-server --port 1
  yaml-card2:
    env: ["CUDA_VISIBLE_DEVICES=GPU-cccc3333-dddd-eeee-ffff-000000000000"]
  yaml-idx0:
    env: ["CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0"]
  yaml-idx-noorder:
    env: ["CUDA_VISIBLE_DEVICES=0"]
  yaml-pair:
    env: ["CUDA_VISIBLE_DEVICES=GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee,GPU-cccc3333-dddd-eeee-ffff-000000000000"]
  yaml-unpinned:
    env: ["FOO=bar"]
    cmd: llama-server --device CUDA0 -sm layer
  yaml-cmd-only:
    cmd: CUDA_VISIBLE_DEVICES=GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee llama-server
  yaml-proxy:
    proxy: http://127.0.0.1:1234
    env: ["CUDA_VISIBLE_DEVICES=GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"]
  yaml-macro:
    env: ["CUDA_VISIBLE_DEVICES=${gpu_pin}"]
  on-card-0:
    env: ["CUDA_VISIBLE_DEVICES=GPU-cccc3333-dddd-eeee-ffff-000000000000"]
`

func writeServing(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func servingCfg(t *testing.T) config.Config {
	cfg := scopedCfg()
	cfg.ServingConfigPath = writeServing(t, servingYAML)
	return cfg
}

// The serving config's pins place what layers do not declare: a model pinned to another card stays,
// one pinned to a leased card goes, and every model it cannot place goes with its reason, exactly as
// every model went before F15. Layers win over the serving config for a model both declare.
func TestUnloadModelsPlacesWhatLayersDoNotDeclareFromTheServingConfig(t *testing.T) {
	cards := threeCards()
	roster := []string{
		"yaml-card0", "yaml-card0-alias", "yaml-card2", "yaml-idx0", "yaml-idx-noorder", "yaml-pair",
		"yaml-unpinned", "yaml-cmd-only", "yaml-proxy", "yaml-macro", "not-in-yaml", "on-card-0", "embed",
	}
	var warn bytes.Buffer
	got, ok := UnloadModels(context.Background(), servingCfg(t), roster, []string{cards[2].LeaseID()}, hostDeps(cards, false), &warn)
	if !ok {
		t.Fatal("a card lease has a list")
	}
	sort.Strings(got)
	want := []string{"not-in-yaml", "yaml-card2", "yaml-cmd-only", "yaml-idx-noorder", "yaml-macro", "yaml-pair", "yaml-proxy", "yaml-unpinned"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("list = %v\nwant   %v\n%s", got, want, warn.String())
	}
	out := warn.String()
	for _, line := range []string{
		"left resident, placed elsewhere by the serving config pin: yaml-card0, yaml-card0-alias, yaml-idx0",
		"left resident, placed elsewhere by layers: on-card-0",
		"unloaded, placed on a leased card by the serving config pin: yaml-card2, yaml-pair",
		"proxy: seat",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("the lane must say %q:\n%s", line, out)
		}
	}
}

// A serving config that cannot be read places nothing: every model layers do not declare is
// unknown and goes, and the lane says so once.
func TestUnloadModelsSaysOnceWhenTheServingConfigCannotBeRead(t *testing.T) {
	cards := threeCards()
	cfg := scopedCfg()
	cfg.ServingConfigPath = filepath.Join(t.TempDir(), "gone.yaml")
	var warn bytes.Buffer
	got, ok := UnloadModels(context.Background(), cfg, []string{"on-card-0", "on-card-2", "x", "y"}, []string{cards[2].LeaseID()}, hostDeps(cards, false), &warn)
	if !ok {
		t.Fatal("a card lease has a list")
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "on-card-2,x,y" {
		t.Fatalf("list = %v, want on-card-2 (layers) and the two models nothing places", got)
	}
	if n := strings.Count(warn.String(), "the serving config could not be read"); n != 1 {
		t.Errorf("the problem is said once, not %d times:\n%s", n, warn.String())
	}
	// With layers declaring every model the serving config is never needed, so it is never read.
	warn.Reset()
	if _, ok := UnloadModels(context.Background(), cfg, []string{"on-card-0", "on-card-2"}, []string{cards[2].LeaseID()}, hostDeps(cards, false), &warn); !ok {
		t.Fatal("a card lease has a list")
	}
	if strings.Contains(warn.String(), "serving config") {
		t.Errorf("layers placed both models: no word about the serving config:\n%s", warn.String())
	}
}

// Without the roster the candidates are the models layers declare and the serving config lists.
func TestUnloadModelsWithoutARosterAlsoListsTheServingConfigsModels(t *testing.T) {
	cards := threeCards()
	var warn bytes.Buffer
	got, ok := UnloadModels(context.Background(), servingCfg(t), nil, []string{cards[2].LeaseID()}, hostDeps(cards, false), &warn)
	if !ok {
		t.Fatal("a card lease has a list")
	}
	have := map[string]bool{}
	for _, id := range got {
		have[id] = true
	}
	for _, id := range []string{"on-card-2", "pair", "yaml-card2", "yaml-pair", "yaml-proxy", "yaml-unpinned"} {
		if !have[id] {
			t.Errorf("%s must be on the list: %v", id, got)
		}
	}
	for _, id := range []string{"yaml-card0", "yaml-idx0", "on-card-0", "embed"} {
		if have[id] {
			t.Errorf("%s must not be on the list: %v", id, got)
		}
	}
	if have["yaml-card0-alias"] {
		t.Errorf("an alias is not a model id and is not a candidate: %v", got)
	}
}
