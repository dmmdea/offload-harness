package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/servingtmpl"
)

// TestFallbackProfileKnowsTheRK3588Backend: an off-matrix render for the backend must work
// (its template has no 26B, so no MoE placement is asked for), and the refusal for an unknown
// backend must list it among the ones that exist.
func TestFallbackProfileKnowsTheRK3588Backend(t *testing.T) {
	res, err := deriveRender(embeddedProfiles, renderRequest{
		Fallback: "rk3588", GOOS: "linux", LlamaBin: "/opt/offload/build/llama.cpp/build/bin", ModelsDir: "/opt/offload/models",
		Listen: "127.0.0.1:11436", Home: "/opt/offload", Threads: 4,
	})
	if err != nil {
		t.Fatalf("an off-matrix rk3588 render: %v", err)
	}
	if res.Include26B {
		t.Error("the rk3588 fallback asked for a 26B its template does not carry")
	}
	var doc struct {
		Models map[string]any `yaml:"models"`
	}
	if err := yaml.Unmarshal([]byte(res.Config), &doc); err != nil {
		t.Fatalf("the fallback render is not YAML: %v", err)
	}
	if _, ok := doc.Models["gemma4-e2b"]; !ok || len(doc.Models) != 1 {
		t.Errorf("the fallback did not render the rk3588 template (its models are exactly gemma4-e2b): %v", doc.Models)
	}
	if vs := servingtmpl.Audit(res.Config); len(vs) != 0 {
		t.Errorf("the fallback render breaks the serving-config rules:\n%s", servingtmpl.Violations(vs))
	}
	if _, err := fallbackProfile("no-such-backend"); err == nil || !strings.Contains(err.Error(), "rk3588") {
		t.Errorf("an unknown backend's refusal must list rk3588, got %v", err)
	}
}
