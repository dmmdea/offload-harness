package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// propsFixture mirrors the live payload shape captured from a resident seat
// (lenovo-ampere6 qwen3.5-4b-agent, llama-server b322-4df29be, 2026-08-21):
// build_info, model_path, model_ftype, n_ctx, total_slots, sampler params
// with reasoning_format, chat_template as a string, modalities as a bool map.
func propsFixture() map[string]any {
	return map[string]any{
		"default_generation_settings": map[string]any{
			"n_ctx": 65536,
			"params": map[string]any{
				"seed":                 4294967295, // volatile per-request default: must NOT enter the hash
				"temperature":          0.8,
				"top_k":                40,
				"top_p":                0.95,
				"min_p":                0.05,
				"reasoning_format":     "none",
				"reasoning_in_content": false,
				"chat_format":          "Content-only",
				"samplers":             []string{"top_k", "top_p", "min_p", "temperature"},
			},
		},
		"total_slots":   1,
		"model_path":    "/models/Qwen3.5-4B-UD-Q4_K_XL.gguf",
		"model_ftype":   "Q4_K - Medium",
		"build_info":    "b322-4df29be",
		"chat_template": "{% for m in messages %}...{% endfor %}",
		"modalities":    map[string]bool{"vision": false, "video": false, "audio": false},
	}
}

func propsServer(t *testing.T, model string, payload any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upstream/"+model+"/props" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
}

func TestProbeSeatPinStableAndReadable(t *testing.T) {
	srv := propsServer(t, "seat-a", propsFixture())
	defer srv.Close()

	pin1, ok := ProbeSeatPin(context.Background(), srv.URL, "seat-a")
	if !ok {
		t.Fatal("probe failed against a healthy props endpoint")
	}
	pin2, ok := ProbeSeatPin(context.Background(), srv.URL, "seat-a")
	if !ok || pin2.SHA256 != pin1.SHA256 {
		t.Fatalf("hash not stable across identical answers: %q vs %q", pin1.SHA256, pin2.SHA256)
	}
	if len(pin1.SHA256) != 64 {
		t.Fatalf("sha256 hex length = %d", len(pin1.SHA256))
	}
	for _, want := range []string{"b322-4df29be", "Qwen3.5-4B-UD-Q4_K_XL.gguf", "n_ctx=65536", "rf=none"} {
		if !strings.Contains(pin1.Basis, want) {
			t.Errorf("basis %q missing %q", pin1.Basis, want)
		}
	}
}

// TestProbeSeatPinSensitivity: every field the pin exists to catch drift in
// must actually move the hash — an insensitive pin is decoration. The seed,
// a per-request quantity the server merely defaults, must NOT move it.
func TestProbeSeatPinSensitivity(t *testing.T) {
	base, ok := probeFixturePin(t, propsFixture())
	if !ok {
		t.Fatal("baseline probe failed")
	}
	mutate := func(name string, f func(m map[string]any)) (SeatPin, bool) {
		t.Helper()
		m := propsFixture()
		f(m)
		pin, ok := probeFixturePin(t, m)
		if !ok {
			t.Fatalf("%s: probe failed", name)
		}
		return pin, pin.SHA256 != base.SHA256
	}

	cases := []struct {
		name string
		f    func(m map[string]any)
	}{
		{"reasoning_format", func(m map[string]any) {
			m["default_generation_settings"].(map[string]any)["params"].(map[string]any)["reasoning_format"] = "auto"
		}},
		{"chat_template", func(m map[string]any) { m["chat_template"] = "{% something else %}" }},
		{"build_info", func(m map[string]any) { m["build_info"] = "b999-deadbee" }},
		{"n_ctx", func(m map[string]any) { m["default_generation_settings"].(map[string]any)["n_ctx"] = 131072 }},
		{"model_path", func(m map[string]any) { m["model_path"] = "/models/other.gguf" }},
		{"temperature", func(m map[string]any) {
			m["default_generation_settings"].(map[string]any)["params"].(map[string]any)["temperature"] = 0.0
		}},
	}
	for _, c := range cases {
		if _, moved := mutate(c.name, c.f); !moved {
			t.Errorf("changing %s did not change the pin — the hash cannot catch the drift it exists for", c.name)
		}
	}

	if _, moved := mutate("seed", func(m map[string]any) {
		m["default_generation_settings"].(map[string]any)["params"].(map[string]any)["seed"] = 7
	}); moved {
		t.Error("changing the per-request seed default moved the pin — identical configs would refuse to pair")
	}
}

func probeFixturePin(t *testing.T, payload any) (SeatPin, bool) {
	t.Helper()
	srv := propsServer(t, "seat-a", payload)
	defer srv.Close()
	return ProbeSeatPin(context.Background(), srv.URL, "seat-a")
}

// TestProbeSeatPinRefusals: every non-answer shape must yield ok=false — an
// invented pin is worse than an absent one.
func TestProbeSeatPinRefusals(t *testing.T) {
	// llama-swap error envelope: decodes as JSON, carries no n_ctx.
	srv := propsServer(t, "seat-a", map[string]any{"error": "unspecific error: matrix: model unloaded", "src": "llama-swap"})
	if _, ok := ProbeSeatPin(context.Background(), srv.URL, "seat-a"); ok {
		t.Error("llama-swap error envelope produced a pin")
	}
	srv.Close()

	// 404 (model unknown to the proxy).
	srv404 := httptest.NewServer(http.HandlerFunc(http.NotFound))
	if _, ok := ProbeSeatPin(context.Background(), srv404.URL, "seat-a"); ok {
		t.Error("404 produced a pin")
	}
	srv404.Close()

	// Dead endpoint (connection refused — the server is already closed).
	if _, ok := ProbeSeatPin(context.Background(), srv404.URL, "seat-a"); ok {
		t.Error("dead endpoint produced a pin")
	}

	// An answer MISSING a required discriminator (older llama.cpp build, a
	// proxy mangling the payload): two different builds both missing
	// build_info would hash IDENTICALLY — a pin that can falsely say "same
	// config" — so the probe must refuse rather than pin.
	noBuild := propsFixture()
	noBuild["build_info"] = ""
	srvNB := propsServer(t, "seat-a", noBuild)
	if _, ok := ProbeSeatPin(context.Background(), srvNB.URL, "seat-a"); ok {
		t.Error("empty build_info produced a pin — two different builds could pair")
	}
	srvNB.Close()
	noTmpl := propsFixture()
	noTmpl["chat_template"] = ""
	srvNT := propsServer(t, "seat-a", noTmpl)
	if _, ok := ProbeSeatPin(context.Background(), srvNT.URL, "seat-a"); ok {
		t.Error("empty chat_template produced a pin — two different templates could pair")
	}
	srvNT.Close()

	// Non-JSON body.
	srvHTML := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>proxy error</html>"))
	}))
	defer srvHTML.Close()
	if _, ok := ProbeSeatPin(context.Background(), srvHTML.URL, "seat-a"); ok {
		t.Error("non-JSON body produced a pin")
	}
}
