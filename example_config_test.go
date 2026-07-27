package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestConfigExampleRoundTrips (LO-17): the committed config.example.json must
// load back into exactly config.Default() — proving it is regenerated from the
// code (go generate .) and has not drifted (the old file still said
// the current escalation_model default after LO-17-class drift).
func TestConfigExampleRoundTrips(t *testing.T) {
	cfg, err := config.Load("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	def := config.Default()
	// comfy_dir is the ONE platform-dependent default (config.DefaultComfyDir): the
	// committed example is the Windows reference, while Default() is unbound off Windows
	// so a non-Windows box reports NOT CONFIGURED instead of a path that cannot exist.
	// Pin the example's value explicitly, then normalize so the whole-struct drift guard
	// still covers every other field on every OS.
	if cfg.ComfyDir != "C:/ComfyUI" {
		t.Fatalf("config.example.json comfy_dir = %q, want the Windows reference \"C:/ComfyUI\" — regenerate with `go generate .` on Windows", cfg.ComfyDir)
	}
	def.ComfyDir = cfg.ComfyDir
	if !reflect.DeepEqual(cfg, def) {
		t.Fatalf("config.example.json does not round-trip to config.Default() — regenerate with `go generate .`\n got: %+v\nwant: %+v", cfg, def)
	}
	if cfg.EscalationModel != "gemma4-26b-a4b" {
		t.Fatalf("escalation_model = %q, want gemma4-26b-a4b (the drift LO-17 class of bug)", cfg.EscalationModel)
	}
}

// TestConfigExampleCarriesEveryKey (LO-17): every json-tagged Config field
// appears in the example (~20 newer keys were missing before), so the file is
// a complete key inventory.
func TestConfigExampleCarriesEveryKey(t *testing.T) {
	raw, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("example is not valid JSON: %v", err)
	}
	tp := reflect.TypeOf(config.Config{})
	for i := 0; i < tp.NumField(); i++ {
		tag := strings.SplitN(tp.Field(i).Tag.Get("json"), ",", 2)[0]
		if tag == "" || tag == "-" {
			continue
		}
		if _, ok := m[tag]; !ok {
			t.Errorf("config.example.json missing key %q — regenerate with `go generate .`", tag)
		}
	}
}
