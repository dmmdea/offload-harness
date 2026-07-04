package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/local-offload/internal/config"
)

// TestConfigExampleRoundTrips (LO-17): the committed config.example.json must
// load back into exactly config.Default() — proving it is regenerated from the
// code (go generate .) and has not drifted (the old file still said
// gemma4-26b-a4b for escalation_model after the default moved to qwythos).
func TestConfigExampleRoundTrips(t *testing.T) {
	cfg, err := config.Load("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	def := config.Default()
	if !reflect.DeepEqual(cfg, def) {
		t.Fatalf("config.example.json does not round-trip to config.Default() — regenerate with `go generate .`\n got: %+v\nwant: %+v", cfg, def)
	}
	if cfg.EscalationModel != "qwythos" {
		t.Fatalf("escalation_model = %q, want qwythos (the drift LO-17 fixed)", cfg.EscalationModel)
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
