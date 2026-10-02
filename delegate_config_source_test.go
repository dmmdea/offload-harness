package main

// C-95: an explicit $LOCAL_OFFLOAD_CONFIG wins for every key, and when that file's
// own `layers` shadow its agent_model the delegate verb says so. The reported run
// (a config copy repointed at a scratch engine) deferred every subtask on a seat
// the copy never named, with nothing telling the operator that the copy's layers -
// not a home config, not the node - had chosen it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func writeConfigJSON(t *testing.T, path string, cfg config.Config) {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDelegateVerbNamesTheConfigWhoseLayersShadowAgentModel(t *testing.T) {
	dir := t.TempDir()
	// The decoy: a home config the env must beat for EVERY key, with its own agent_model.
	home := filepath.Join(dir, "home")
	decoy := config.Default()
	decoy.AgentDelegationEnabled = true
	decoy.AgentModel = "home-seat"
	writeConfigJSON(t, filepath.Join(home, ".local-offload", "config.json"), decoy)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// The copy: layers kept from the node, agent_model repointed at a scratch seat.
	cp := config.CompositeFixture()
	cp.AgentDelegationEnabled = true
	cp.AgentModel = "copy-seat"
	cpPath := filepath.Join(dir, "copy.json")
	writeConfigJSON(t, cpPath, cp)
	t.Setenv("LOCAL_OFFLOAD_CONFIG", cpPath)

	var err error
	stderr := captureStderr(t, func() { err = runDelegate([]string{}) })
	if err == nil || !strings.Contains(err.Error(), "--contract required") {
		t.Fatalf("err = %v, want the verb to reach its own argument check", err)
	}
	for _, want := range []string{cpPath, `agent_model "copy-seat"`, "layers", "placement"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the delegate verb must tell the operator which config's layers pick the seat; missing %q in %q", want, stderr)
		}
	}
	if strings.Contains(stderr, "home-seat") {
		t.Fatalf("the home config must not leak into an explicit $LOCAL_OFFLOAD_CONFIG run: %q", stderr)
	}

	// An agreeing config is silent.
	cp.AgentModel = "agent-pool"
	writeConfigJSON(t, cpPath, cp)
	stderr = captureStderr(t, func() { err = runDelegate([]string{}) })
	if strings.Contains(stderr, "agent_model") {
		t.Fatalf("a config whose agent_model is a layer seat must not be warned about, got %q", stderr)
	}
}
