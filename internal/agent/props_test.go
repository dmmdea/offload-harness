package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// vllmFixture mirrors the live payloads captured from a vLLM 0.28.0 seat on
// the A2 (2026-09-08): /props 404s, /version and /v1/models answer 200 and
// carry the engine build, the resolved checkpoint path and the served window.
func vllmFixture(version, root, served string, maxLen int) (map[string]any, map[string]any) {
	ver := map[string]any{"version": version}
	models := map[string]any{
		"object": "list",
		"data": []any{map[string]any{
			"id": served, "object": "model", "owned_by": "vllm",
			"root": root, "max_model_len": maxLen,
		}},
	}
	return ver, models
}

// vllmServer serves the vLLM shape: /props 404 (as a real vLLM does), the two
// metadata endpoints 200. Extra data entries let a test drive the
// several-aliases and no-match branches.
func vllmServer(t *testing.T, model string, ver, models map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/upstream/" + model + "/version":
			_ = json.NewEncoder(w).Encode(ver)
		case "/upstream/" + model + "/v1/models":
			_ = json.NewEncoder(w).Encode(models)
		default:
			http.NotFound(w, r) // /props included — this is the branch under test
		}
	}))
}

func vllmPin(t *testing.T, model, version, root, served string, maxLen int) (SeatPin, bool) {
	t.Helper()
	ver, models := vllmFixture(version, root, served, maxLen)
	srv := vllmServer(t, model, ver, models)
	defer srv.Close()
	return ProbeSeatPin(context.Background(), srv.URL, model)
}

// A vLLM seat used to get NO pin at all, because ProbeSeatPin only spoke
// llama.cpp's /props. Unpinned rows cannot enter a paired experiment, which is
// how an A2 4B-vs-9B comparison shipped with the weights, the engine build and
// the served window all unrecorded.
func TestProbeSeatPinVLLMFallbackProducesAPin(t *testing.T) {
	pin, ok := vllmPin(t, "seat-9b", "0.28.0",
		"/hf/hub/models--RedHatAI--Qwen3.5-9B-quantized.w4a16/snapshots/a398088", "seat-9b", 131072)
	if !ok {
		t.Fatal("a healthy vLLM seat produced no pin — the fallback did not fire")
	}
	if len(pin.SHA256) != 64 {
		t.Errorf("SHA256 = %q, want 64 hex chars", pin.SHA256)
	}
	for _, want := range []string{"vllm", "0.28.0", "max_model_len=131072", "served=seat-9b"} {
		if !strings.Contains(pin.Basis, want) {
			t.Errorf("basis %q missing %q — an operator cannot read what changed", pin.Basis, want)
		}
	}
}

// The whole point: two arms that differ must not pin the same. Each field is
// moved ALONE so a test failure names the field that stopped discriminating.
func TestProbeSeatPinVLLMSensitivity(t *testing.T) {
	base, ok := vllmPin(t, "seat", "0.28.0", "/hf/hub/models--A/snapshots/aaa", "seat", 131072)
	if !ok {
		t.Fatal("baseline pin failed")
	}
	for _, c := range []struct {
		name                  string
		version, root, served string
		maxLen                int
	}{
		{"different checkpoint", "0.28.0", "/hf/hub/models--B/snapshots/bbb", "seat", 131072},
		{"different engine version", "0.29.0", "/hf/hub/models--A/snapshots/aaa", "seat", 131072},
		{"different served window", "0.28.0", "/hf/hub/models--A/snapshots/aaa", "seat", 65536},
		{"different snapshot of same repo", "0.28.0", "/hf/hub/models--A/snapshots/ccc", "seat", 131072},
	} {
		got, ok := vllmPin(t, "seat", c.version, c.root, c.served, c.maxLen)
		if !ok {
			t.Fatalf("%s: pin failed", c.name)
		}
		if got.SHA256 == base.SHA256 {
			t.Errorf("%s: pin did NOT move — two different arms would pair as identical", c.name)
		}
	}
}

// A vLLM pin and a llama.cpp pin must never collide: they cover disjoint field
// sets, so a shared hash would claim two unlike seats are the same config.
func TestVLLMPinNeverCollidesWithLlamaCpp(t *testing.T) {
	cpp, ok := probeFixturePin(t, propsFixture())
	if !ok {
		t.Fatal("llama.cpp fixture pin failed")
	}
	vl, ok := vllmPin(t, "seat", "0.28.0", "/models/Qwen3.5-4B-UD-Q4_K_XL.gguf", "seat", 65536)
	if !ok {
		t.Fatal("vllm fixture pin failed")
	}
	if cpp.SHA256 == vl.SHA256 {
		t.Fatal("a vLLM pin collided with a llama.cpp pin")
	}
	if strings.HasPrefix(cpp.Basis, "vllm ") {
		t.Error("llama.cpp basis claims to be vllm")
	}
	if !strings.HasPrefix(vl.Basis, "vllm ") {
		t.Errorf("vllm basis %q does not announce its coverage class", vl.Basis)
	}
}

// Same discipline as the llama.cpp path: a missing discriminator yields NO pin
// rather than one hashed over empty values, which could falsely say "same".
func TestProbeSeatPinVLLMRefusals(t *testing.T) {
	cases := []struct {
		name                  string
		version, root, served string
		maxLen                int
	}{
		{"no engine version", "", "/hf/hub/models--A/snapshots/aaa", "seat", 131072},
		{"no resolved checkpoint path", "0.28.0", "", "seat", 131072},
		{"no served window", "0.28.0", "/hf/hub/models--A/snapshots/aaa", "seat", 0},
		{"negative served window", "0.28.0", "/hf/hub/models--A/snapshots/aaa", "seat", -1},
	}
	for _, c := range cases {
		if _, ok := vllmPin(t, "seat", c.version, c.root, c.served, c.maxLen); ok {
			t.Errorf("%s: produced a pin anyway", c.name)
		}
	}

	// Several entries and none matching the seat name: pinning an arbitrary one
	// would attribute another model's config to this run.
	ver := map[string]any{"version": "0.28.0"}
	models := map[string]any{"object": "list", "data": []any{
		map[string]any{"id": "other-a", "root": "/hf/A", "max_model_len": 131072},
		map[string]any{"id": "other-b", "root": "/hf/B", "max_model_len": 131072},
	}}
	srv := vllmServer(t, "seat", ver, models)
	defer srv.Close()
	if _, ok := ProbeSeatPin(context.Background(), srv.URL, "seat"); ok {
		t.Error("ambiguous roster with no name match produced a pin")
	}

	// Empty roster.
	srvEmpty := vllmServer(t, "seat", ver, map[string]any{"object": "list", "data": []any{}})
	defer srvEmpty.Close()
	if _, ok := ProbeSeatPin(context.Background(), srvEmpty.URL, "seat"); ok {
		t.Error("empty roster produced a pin")
	}
}

// A single unnamed entry IS pinned: a seat whose llama-swap alias differs from
// its vLLM --served-model-name is the normal deployment on this fleet.
func TestProbeSeatPinVLLMSingleEntryAliasMismatch(t *testing.T) {
	ver, models := vllmFixture("0.28.0", "/hf/hub/models--A/snapshots/aaa", "qwen3.5-9b-vllm", 131072)
	srv := vllmServer(t, "agent-pool", ver, models)
	defer srv.Close()
	pin, ok := ProbeSeatPin(context.Background(), srv.URL, "agent-pool")
	if !ok {
		t.Fatal("a sole-entry roster under an alias produced no pin")
	}
	if !strings.Contains(pin.Basis, "served=qwen3.5-9b-vllm") {
		t.Errorf("basis %q lost the vLLM served name", pin.Basis)
	}
}

// Only a 404 means "this engine does not have /props". A llama.cpp seat
// answering 500/503 is a seat having a bad moment, and chasing it with the
// vLLM fallback would spend the probe's budget on a path that cannot answer —
// on exactly the run where the seat is already struggling. The request COUNT
// is the assertion: an ok=false alone would pass even if the fallback fired.
func TestProbeSeatPinNon404DoesNotTryTheVLLMFallback(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusBadGateway} {
		var hits int32
		ver, models := vllmFixture("0.28.0", "/hf/hub/models--A/snapshots/aaa", "seat", 131072)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/upstream/seat/props":
				w.WriteHeader(status) // the seat is unwell, NOT a different engine
			case "/upstream/seat/version":
				_ = json.NewEncoder(w).Encode(ver)
			case "/upstream/seat/v1/models":
				_ = json.NewEncoder(w).Encode(models)
			default:
				http.NotFound(w, r)
			}
		}))
		if pin, ok := ProbeSeatPin(context.Background(), srv.URL, "seat"); ok {
			t.Errorf("status %d: produced a pin %q — a struggling llama.cpp seat was pinned as if it were vLLM", status, pin.Basis)
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Errorf("status %d: made %d requests, want exactly 1 — the vLLM fallback fired on a non-404", status, got)
		}
		srv.Close()
	}
}

// The whole probe must respect ONE budget, not one per HTTP call: it runs
// inline on the path that returns an agent task's result, and the vLLM route
// issues three requests where the llama.cpp route issued one.
//
// Every endpoint answers CORRECTLY but SLOWLY (delay just over half the
// budget). That is what makes the two designs separable, and an earlier
// version of this test got it wrong: with a single stalled call the probe
// returns after one client timeout either way, so the test passed against a
// deliberately broken build. Here, with three slow-but-valid endpoints:
//
//	shared budget  -> the deadline expires mid-probe: NO pin, ~1 budget
//	per-call budget-> every call succeeds on its own clock: a pin, ~3 delays
//
// so both the verdict and the elapsed time discriminate.
func TestProbeSeatPinTotalBudgetIsSharedAcrossCalls(t *testing.T) {
	delay := seatPinClient.Timeout/2 + 200*time.Millisecond
	ver, models := vllmFixture("0.28.0", "/hf/hub/models--A/snapshots/aaa", "seat", 131072)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/upstream/seat/props":
			http.NotFound(w, r) // slow 404 -> enters the vLLM fallback having already spent budget
		case "/upstream/seat/version":
			_ = json.NewEncoder(w).Encode(ver)
		case "/upstream/seat/v1/models":
			_ = json.NewEncoder(w).Encode(models)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	start := time.Now()
	pin, ok := ProbeSeatPin(context.Background(), srv.URL, "seat")
	elapsed := time.Since(start)

	if ok {
		t.Errorf("probe produced a pin %q after %s — each call got its own budget, so the probe outran the bound this file promises", pin.Basis, elapsed)
	}
	if ceiling := seatPinClient.Timeout + 900*time.Millisecond; elapsed > ceiling {
		t.Errorf("probe took %s, want under %s — the budget is being spent per call, not shared", elapsed, ceiling)
	}
}
