package rungraph

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildArgs(t *testing.T) {
	got := buildArgs(Params{
		GraphPath: "g.json", ManifestPath: "m.json", OutDir: "out", ResultPath: "r.json", ReserveVram: "1.5",
	})
	want := []string{"--graph", "g.json", "--manifest", "m.json", "--out-dir", "out", "--result", "r.json", "--reserve-vram", "1.5"}
	if len(got) != len(want) {
		t.Fatalf("len: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestBuildArgsOmitsEmptyOptional(t *testing.T) {
	got := buildArgs(Params{GraphPath: "g.json", OutDir: "out", ResultPath: "r.json"})
	for _, a := range got {
		if a == "--manifest" || a == "--reserve-vram" {
			t.Fatalf("expected no empty optional flag, got %v", got)
		}
	}
}

func TestBuildArgsForATemplateNamesTheTemplateAndNoGraph(t *testing.T) {
	got := buildArgs(Params{
		Template: "tt_open", ManifestPath: "m.json", OverridesPath: "o.json", Seed: "7", OutDir: "out", ResultPath: "r.json", ReserveVram: "1.5",
	})
	want := []string{"--template", "tt_open", "--manifest", "m.json", "--overrides", "o.json", "--seed", "7", "--out-dir", "out", "--result", "r.json", "--reserve-vram", "1.5"}
	if len(got) != len(want) {
		t.Fatalf("len: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestBuildArgsOfAGraphRunCarryNoTemplateFlag(t *testing.T) {
	for _, a := range buildArgs(Params{GraphPath: "g.json", OutDir: "out", ResultPath: "r.json"}) {
		if a == "--template" || a == "--overrides" || a == "--seed" {
			t.Fatalf("a graph run must not carry a template flag: %v", a)
		}
	}
}

func TestEnvelopeKeepsTheFilesTrustedOnFirstUse(t *testing.T) {
	var e Envelope
	if err := json.Unmarshal([]byte(`{"outputs":{},"image_path":"","unverified_models":[],"tofu_first_seen":["models/vae/v.safetensors"]}`), &e); err != nil {
		t.Fatal(err)
	}
	if len(e.TofuFirstSeen) != 1 || e.TofuFirstSeen[0] != "models/vae/v.safetensors" {
		t.Fatalf("TofuFirstSeen = %v", e.TofuFirstSeen)
	}
	out, _ := json.Marshal(Envelope{})
	if strings.Contains(string(out), "tofu_first_seen") {
		t.Errorf("a graph run's envelope has no such key: %s", out)
	}
}
