package main

import (
	"strings"
	"testing"
)

// TestFallbackProfileKnowsTheRK3588Backend: the backend is known to the off-matrix path, a
// render of it without a tier is refused by name (there is no seat to serve), and the refusal
// for an unknown backend must list it among the ones that exist.
func TestFallbackProfileKnowsTheRK3588Backend(t *testing.T) {
	// The rk3588 template serves no model of its own (llama.cpp faults this board's GPU, so every
	// model is a tier seat). An off-matrix render has no tier and so no seat: it must be refused
	// by name rather than write a config that serves nothing.
	_, err := deriveRender(embeddedProfiles, renderRequest{
		Fallback: "rk3588", GOOS: "linux", LlamaBin: "/opt/offload/build/llama.cpp/build/bin", ModelsDir: "/opt/offload/models",
		Listen: "127.0.0.1:11436", Home: "/opt/offload", Threads: 4,
	})
	if err == nil || !strings.Contains(err.Error(), "serves no model") {
		t.Fatalf("an off-matrix rk3588 render must be refused (no seat, no model), got %v", err)
	}
	if _, err := fallbackProfile("no-such-backend"); err == nil || !strings.Contains(err.Error(), "rk3588") {
		t.Errorf("an unknown backend's refusal must list rk3588, got %v", err)
	}
}
