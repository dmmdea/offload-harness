package pipeline

// Opt-in image-prompt refiner (refiner.go) coverage. The load-bearing
// properties under test, in order: the quoted-span guard in BOTH directions
// (dropped/altered spans AND added quotes, with the whole-output wrap strip),
// curly-quote normalization, odd-quote pairing, fail-safe fallback on every
// refiner problem (transport, timeout, truncation, budget), the batch circuit
// breaker, byte-compatibility of the OFF path, output-path stability with the
// refine knob present, timeout wiring, and batch/sdcpp parity of the shared
// decision point. Render scripts are GPU-free node stubs that record their
// argv (the pipeline test convention); the refiner call is faked through the
// p.refineGen seam (nil = the real llamaclient path, covered by the httptest
// wiring test).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// refinerTestPipeline mirrors footprintTestPipeline: an isolated ledger dir and
// a fake VRAM sampler, so the real render paths run end to end with a stub.
func refinerTestPipeline(t *testing.T, cfg config.Config) *Pipeline {
	t.Helper()
	cfg.LedgerPath = filepath.Join(t.TempDir(), "ledger.jsonl")
	p := &Pipeline{cfg: cfg}
	p.fleetSample = func(int) (float64, error) { return 1.0, nil }
	return p
}

// neverGen is a refineGen that fails the test if the refiner is ever called.
func neverGen(t *testing.T) func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
	t.Helper()
	return func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
		t.Error("refiner was called; it must not be on this path")
		return llamaclient.GenResult{}, errors.New("unexpected refiner call")
	}
}

// writeArgvStub writes a node render stub that creates its out file (argv[0]
// after the script path) and records the full arg list as JSON, so tests can
// assert exactly which prompt the render call received.
func writeArgvStub(t *testing.T, dir string) (script, argvPath string) {
	t.Helper()
	argvPath = filepath.Join(dir, "argv.json")
	script = filepath.Join(dir, "argv-stub.mjs")
	content := `import {writeFileSync} from "node:fs";
const args = process.argv.slice(2);
writeFileSync(args[0], "stub-output");
writeFileSync("` + filepath.ToSlash(argvPath) + `", JSON.stringify(args));
`
	if err := os.WriteFile(script, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return script, argvPath
}

func readArgv(t *testing.T, argvPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("stub recorded no argv: %v", err)
	}
	var args []string
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	return args
}

// --- span extraction: pairing, odd-quote drop, curly normalization ---

func TestQuotedSpans(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"no quotes", "a red bike at dawn", nil},
		{"one span", `a poster saying "BUY NOW"`, []string{`"BUY NOW"`}},
		{"two spans", `mug with "CAFE" and "EST. 1999"`, []string{`"CAFE"`, `"EST. 1999"`}},
		{"curly quotes normalize", "a poster saying “BUY NOW” here", []string{`"BUY NOW"`}},
		// Odd count: the TRAILING quote (an inch mark) is dropped before
		// pairing, so the real span stays protected.
		{"trailing inch mark ignored", `see the "SALE" sign on a 5" pipe`, []string{`"SALE"`}},
		// KNOWN LIMITATION (documented on quotedSpans): even-count inch marks
		// still pair into a bogus span; the guard then falls back safely.
		{"even inch marks mis-pair", `a 5" pipe and an 8" flange`, []string{`" pipe and an 8"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := quotedSpans(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("quotedSpans(%q) = %q, want %q", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("span %d = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestSpanGuard(t *testing.T) {
	cases := []struct {
		name         string
		raw, refined string
		want         string // "" = pass, else a substring of the reason
	}{
		{"no quotes", "a red bike at dawn", "a red bike at golden-hour dawn, 35mm", ""},
		{"span preserved", `a poster saying "BUY NOW" on a wall`, `an editorial poster saying "BUY NOW" on a weathered brick wall, soft light`, ""},
		{"span dropped entirely", `a poster saying "BUY NOW" on a wall`, `an editorial poster on a wall with a bold red headline, morning light`, "dropped quoted span"},
		{"span content changed", `a sign reading "OPEN"`, `a neon sign reading "open" at night`, "dropped quoted span"},
		{"quotes stripped from span", `a poster saying "BUY NOW" on a wall`, `an editorial poster saying BUY NOW on a weathered wall`, "quoted span altered (glyphs/whitespace)"},
		{"whitespace changed inside span", `poster of "BUY NOW"`, `an elegant poster of "BUY  NOW" in neon on a dusk street`, "quoted span altered (glyphs/whitespace)"},
		{"multiple spans preserved", `mug with "CAFE" logo and "EST. 1999" below`, `ceramic mug with "CAFE" logo and "EST. 1999" below, studio light`, ""},
		{"second of two dropped", `mug with "CAFE" logo and "EST. 1999" below`, `ceramic mug with "CAFE" logo, studio light`, "dropped quoted span"},
		{"apostrophes are not quotes", "a dog's breakfast, rock 'n' roll style", "a chaotic still life in a rocking style, 50mm, morning light on a long table", ""},
		{"curly raw straight refined", "a poster saying “BUY NOW” tonight", `a large poster saying "BUY NOW" under sodium streetlight`, ""},
		{"curly raw curly refined", "a poster saying “BUY NOW” tonight", "a large poster saying “BUY NOW” under sodium streetlight", ""},
		{"curly raw span dropped", "a poster saying “BUY NOW” tonight", "a large blank poster under sodium streetlight tonight", "dropped quoted span \"BUY NOW\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := spanGuard(c.raw, c.refined)
			if c.want == "" && got != "" {
				t.Errorf("spanGuard(%q, %q) = %q, want pass", c.raw, c.refined, got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Errorf("spanGuard(%q, %q) = %q, want reason containing %q", c.raw, c.refined, got, c.want)
			}
		})
	}
}

func TestStripWrappingQuotes(t *testing.T) {
	// A full wrap around plain text is stripped.
	raw := "a quiet library"
	wrapped := `"a quiet library at dawn, shafts of dust-lit sun between the stacks"`
	if got := stripWrappingQuotes(wrapped, raw); strings.HasPrefix(got, `"`) {
		t.Errorf("wrap not stripped: %q", got)
	}
	// A wrap around text CONTAINING a preserved span is stripped too.
	rawSpan := `sign "OPEN"`
	wrappedSpan := `"a weathered tin sign "OPEN" hanging over a diner door at night"`
	got := stripWrappingQuotes(wrappedSpan, rawSpan)
	if strings.HasPrefix(got, `"a weathered`) || !strings.Contains(got, `"OPEN"`) {
		t.Errorf("wrap-with-span strip = %q", got)
	}
	// A refined prompt that legitimately STARTS and ENDS with two DISTINCT raw
	// spans is NOT split.
	rawTwo := `"A" and "B"`
	refinedTwo := `"A" beautifully lit beside "B"`
	if got := stripWrappingQuotes(refinedTwo, rawTwo); got != refinedTwo {
		t.Errorf("edge spans must not be split: %q", got)
	}
	// Curly wrap strips as well.
	if got := stripWrappingQuotes("“a quiet library at dawn, warm tungsten pools of light”", raw); strings.ContainsAny(got, "“”") {
		t.Errorf("curly wrap not stripped: %q", got)
	}
}

func TestRefineExplicitlyOff(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		want   bool
	}{
		{"absent", map[string]any{}, false},
		{"nil params", nil, false},
		{"explicit true", map[string]any{"refine": true}, false},
		{"explicit false", map[string]any{"refine": false}, true},
		{"string false", map[string]any{"refine": "false"}, true},
		{"string FALSE", map[string]any{"refine": "FALSE"}, true},
		{"garbage value", map[string]any{"refine": 7}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := refineExplicitlyOff(c.params); got != c.want {
				t.Errorf("refineExplicitlyOff(%v) = %v, want %v", c.params, got, c.want)
			}
		})
	}
}

func TestStripRefineParam(t *testing.T) {
	in := map[string]any{"seed": 7, "refine": false}
	out := stripRefineParam(in)
	if _, ok := out["refine"]; ok || out["seed"] != 7 {
		t.Errorf("stripRefineParam = %v", out)
	}
	if _, ok := in["refine"]; !ok {
		t.Error("stripRefineParam must not mutate its input")
	}
	// No knob present: the SAME map comes back (no copy, no key churn).
	same := map[string]any{"seed": 7}
	if got := stripRefineParam(same); len(got) != 1 || got["seed"] != 7 {
		t.Errorf("no-knob passthrough = %v", got)
	}
}

// --- decision point: success, fallbacks, off states ---

func TestMaybeRefinePrompt_Success(t *testing.T) {
	cfg := config.Default()
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	raw := `a mug with "CAFE" printed on it`
	refined := `a hand-thrown ceramic mug with "CAFE" printed on it, backlit by soft window light, 85mm, shallow depth of field`
	var gotModel, gotSystem, gotUser string
	var gotMax int
	var gotTemp float64
	p.refineGen = func(_ context.Context, model, system, user string, maxTokens int, temperature float64) (llamaclient.GenResult, error) {
		gotModel, gotSystem, gotUser, gotMax, gotTemp = model, system, user, maxTokens, temperature
		return llamaclient.GenResult{Content: refined + "\n"}, nil
	}
	out, ok, note, transient := p.maybeRefinePrompt(context.Background(), raw, false)
	if !ok || out != refined || note != "" || transient {
		t.Fatalf("maybeRefinePrompt = (%q, %v, %q, %v), want the trimmed refined prompt", out, ok, note, transient)
	}
	if gotModel != "refiner-x" || gotSystem != refinerSystem || gotUser != raw {
		t.Errorf("refiner call routing: model=%q system-match=%v user=%q", gotModel, gotSystem == refinerSystem, gotUser)
	}
	if gotMax != refinerMaxTokens || gotTemp != refinerTemperature {
		t.Errorf("refiner sampling: maxTokens=%d temp=%v, want %d / %v", gotMax, gotTemp, refinerMaxTokens, refinerTemperature)
	}
}

func TestMaybeRefinePrompt_FallbackPaths(t *testing.T) {
	raw := `product shot of a bottle labeled "PURE-9"`
	longClean := `an immaculate studio product shot of a frosted glass bottle labeled "PURE-9", macro lens, diffuse key light, subtle rim highlight`
	cases := []struct {
		name          string
		gen           func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error)
		wantNote      string
		wantTransient bool
	}{
		{"transport error", func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
			return llamaclient.GenResult{}, errors.New("connection refused")
		}, "refiner call failed", true},
		{"empty output", func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
			return llamaclient.GenResult{Content: "   \n"}, nil
		}, "empty refiner output", false},
		{"truncated output", func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
			return llamaclient.GenResult{Content: longClean, Truncated: true}, nil
		}, "refiner output truncated (budget)", false},
		{"shorter than input", func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
			return llamaclient.GenResult{Content: `a bottle "PURE-9"`}, nil
		}, "shorter than the input", false},
		{"dropped quoted span", func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
			return llamaclient.GenResult{Content: "an immaculate studio product shot of a frosted glass bottle, macro lens, diffuse key light"}, nil
		}, `dropped quoted span "PURE-9"`, false},
		{"added quote characters", func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
			return llamaclient.GenResult{Content: longClean + `, with a "LIMITED EDITION" ribbon`}, nil
		}, "added quote characters", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.ImageGenRefinerModel = "refiner-x"
			p := refinerTestPipeline(t, cfg)
			p.refineGen = c.gen
			out, ok, note, transient := p.maybeRefinePrompt(context.Background(), raw, false)
			if ok || out != raw {
				t.Fatalf("fallback must return the raw prompt: got (%q, %v)", out, ok)
			}
			if !strings.Contains(note, c.wantNote) {
				t.Errorf("note = %q, want it to contain %q", note, c.wantNote)
			}
			if transient != c.wantTransient {
				t.Errorf("transient = %v, want %v (breaker classification)", transient, c.wantTransient)
			}
		})
	}
}

func TestRefinePrompt_WrapStrippedThenAccepted(t *testing.T) {
	cfg := config.Default()
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	raw := "a quiet library"
	inner := "a quiet library at dawn, shafts of dust-lit sun between the stacks, 35mm, warm tungsten pools of light"
	p.refineGen = func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
		return llamaclient.GenResult{Content: `"` + inner + `"`}, nil // whole answer wrapped in quotes
	}
	out, ok, note, _ := p.maybeRefinePrompt(context.Background(), raw, false)
	if !ok || out != inner {
		t.Fatalf("wrapped output must be stripped and accepted, got (%q, %v, %q)", out, ok, note)
	}
}

func TestRefinePrompt_BudgetSkip(t *testing.T) {
	cfg := config.Default()
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	p.refineGen = neverGen(t)                        // over-budget prompts must never reach the model
	raw := strings.Repeat("lighthouse at dusk ", 60) // ~1140 chars, past the ~200-token budget
	out, ok, note, transient := p.maybeRefinePrompt(context.Background(), raw, false)
	if ok || out != raw || transient {
		t.Fatalf("budget skip must return the raw prompt untransformed, got (%v, transient %v)", ok, transient)
	}
	if !strings.Contains(note, "too long to refine") {
		t.Errorf("note = %q, want the budget reason", note)
	}
}

func TestMaybeRefinePrompt_OffStates(t *testing.T) {
	// Unconfigured: never calls the model, note empty.
	p := refinerTestPipeline(t, config.Default())
	p.refineGen = neverGen(t)
	if out, ok, note, _ := p.maybeRefinePrompt(context.Background(), "a raw prompt", false); ok || out != "a raw prompt" || note != "" {
		t.Fatalf("unconfigured refiner must be a byte-level no-op, got (%q, %v, %q)", out, ok, note)
	}
	// Configured but explicitly off: same no-op, still no model call.
	cfg := config.Default()
	cfg.ImageGenRefinerModel = "refiner-x"
	p2 := refinerTestPipeline(t, cfg)
	p2.refineGen = neverGen(t)
	if out, ok, note, _ := p2.maybeRefinePrompt(context.Background(), "a raw prompt", true); ok || out != "a raw prompt" || note != "" {
		t.Fatalf("explicit refine:false must be a no-op, got (%q, %v, %q)", out, ok, note)
	}
}

// --- timeout wiring ---

func TestRefinerTimeout_DeadlineWired(t *testing.T) {
	cfg := config.Default()
	cfg.ImageGenRefinerModel = "refiner-x"
	cfg.ImageGenRefinerTimeoutSec = 7
	p := refinerTestPipeline(t, cfg)
	var remaining time.Duration
	p.refineGen = func(ctx context.Context, _, _, user string, _ int, _ float64) (llamaclient.GenResult, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("refiner ctx has no deadline")
		}
		remaining = time.Until(d)
		return llamaclient.GenResult{Content: user + " with cinematic volumetric lighting and a 35mm lens"}, nil
	}
	if _, ok, _, _ := p.maybeRefinePrompt(context.Background(), "a lighthouse at dusk", false); !ok {
		t.Fatal("expected refinement to succeed")
	}
	if remaining <= 5*time.Second || remaining > 7*time.Second {
		t.Errorf("deadline %v from now, want ~7s (imagegen_refiner_timeout_sec)", remaining)
	}
}

func TestRefinerTimeout_ExpiryFallsBackWithColdSwapHint(t *testing.T) {
	cfg := config.Default()
	cfg.ImageGenRefinerModel = "refiner-x"
	cfg.ImageGenRefinerTimeoutSec = 1
	p := refinerTestPipeline(t, cfg)
	p.refineGen = func(ctx context.Context, _, _, _ string, _ int, _ float64) (llamaclient.GenResult, error) {
		<-ctx.Done() // a hung refiner: only the wired timeout can end this
		return llamaclient.GenResult{}, ctx.Err()
	}
	start := time.Now()
	out, ok, note, transient := p.maybeRefinePrompt(context.Background(), "a lighthouse at dusk", false)
	if ok || out != "a lighthouse at dusk" || !strings.Contains(note, "refiner call failed") {
		t.Fatalf("timeout must fall back to the raw prompt, got (%q, %v, %q)", out, ok, note)
	}
	if !strings.Contains(note, "cold model swap?") {
		t.Errorf("note = %q, want the cold-swap hint on a deadline hit", note)
	}
	if !transient {
		t.Error("a timeout is transport-class and must count toward the batch breaker")
	}
	if el := time.Since(start); el < 900*time.Millisecond || el > 5*time.Second {
		t.Errorf("fell back after %v, want ~1s (the configured timeout)", el)
	}
}

func TestRefinerTimeout_DefaultsTo30s(t *testing.T) {
	cfg := config.Default()
	if cfg.ImageGenRefinerTimeoutSec != 30 {
		t.Fatalf("Default() imagegen_refiner_timeout_sec = %d, want 30", cfg.ImageGenRefinerTimeoutSec)
	}
	// And a zeroed value still gets the 30s guard rather than an instant expiry.
	cfg.ImageGenRefinerModel = "refiner-x"
	cfg.ImageGenRefinerTimeoutSec = 0
	p := refinerTestPipeline(t, cfg)
	var remaining time.Duration
	p.refineGen = func(ctx context.Context, _, _, user string, _ int, _ float64) (llamaclient.GenResult, error) {
		d, _ := ctx.Deadline()
		remaining = time.Until(d)
		return llamaclient.GenResult{Content: user + " under a dramatic stormfront, wide-angle, backlit"}, nil
	}
	if _, ok, _, _ := p.maybeRefinePrompt(context.Background(), "a lighthouse at dusk", false); !ok {
		t.Fatal("expected refinement to succeed")
	}
	if remaining <= 25*time.Second || remaining > 30*time.Second {
		t.Errorf("deadline %v from now, want the 30s default", remaining)
	}
}

// --- real client wiring (the gen == nil branch) ---

func TestRefinePrompt_RealClientWiring(t *testing.T) {
	refined := "a lighthouse at dusk under a violet sky, long exposure, 24mm wide-angle, foam blurred on the rocks"
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write(fakeChat{content: refined, finishReason: "stop", promptTokens: 40}.marshal())
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	p.client = llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	out, ok, note, _ := p.maybeRefinePrompt(context.Background(), "a lighthouse at dusk", false)
	if !ok || out != refined {
		t.Fatalf("real-client refine = (%q, %v, %q)", out, ok, note)
	}
	if gotBody["model"] != "refiner-x" {
		t.Errorf("request model = %v, want refiner-x", gotBody["model"])
	}
	if gotBody["temperature"] != refinerTemperature || gotBody["max_tokens"] != float64(refinerMaxTokens) {
		t.Errorf("request sampling = temp %v / max_tokens %v", gotBody["temperature"], gotBody["max_tokens"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want [system, user]", gotBody["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != refinerSystem {
		t.Errorf("system message not the refiner prompt: %v", sys)
	}
}

// --- E2E: the render call receives the right prompt (single ComfyUI path) ---

func TestRunGenerateImage_RefinerOffByteCompat(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	stub, argvPath := writeArgvStub(t, dir)
	cfg.ImageGenScript = stub
	cfg.MediaDir = dir
	p := refinerTestPipeline(t, cfg) // refiner UNSET
	p.refineGen = neverGen(t)

	raw := "a gray sphere on a plinth"
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: raw})
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}
	if args := readArgv(t, argvPath); args[1] != raw {
		t.Errorf("render received prompt %q, want the raw prompt %q", args[1], raw)
	}
	var data map[string]any
	if err := json.Unmarshal(res.Data, &data); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"refined", "refined_prompt", "refine_fallback"} {
		if _, present := data[k]; present {
			t.Errorf("refiner-off result data must omit %q (byte-compat), got %v", k, data)
		}
	}
	if len(data) != 4 { // image_path, width, height, seed — exactly today's shape
		t.Errorf("result data keys = %v, want exactly the pre-refiner four", data)
	}
}

func TestRunGenerateImage_RefinedPromptReachesRender(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	stub, argvPath := writeArgvStub(t, dir)
	cfg.ImageGenScript = stub
	cfg.MediaDir = dir
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	raw := `a storefront with a sign reading "OPEN LATE"`
	refined := `a rain-slicked storefront at blue hour with a neon sign reading "OPEN LATE", reflections on wet asphalt, 35mm, moody`
	p.refineGen = func(_ context.Context, _, _, _ string, _ int, _ float64) (llamaclient.GenResult, error) {
		return llamaclient.GenResult{Content: refined}, nil
	}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: raw})
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}
	if args := readArgv(t, argvPath); args[1] != refined {
		t.Errorf("render received %q, want the refined prompt", args[1])
	}
	var data map[string]any
	_ = json.Unmarshal(res.Data, &data)
	if data["refined"] != true || data["refined_prompt"] != refined {
		t.Errorf("result data refiner keys = refined:%v refined_prompt:%v", data["refined"], data["refined_prompt"])
	}
}

func TestRunGenerateImage_FallbackRendersRaw(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	stub, argvPath := writeArgvStub(t, dir)
	cfg.ImageGenScript = stub
	cfg.MediaDir = dir
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	p.refineGen = func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
		return llamaclient.GenResult{}, errors.New("llama-swap unreachable")
	}
	raw := "a gray sphere on a plinth"
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: raw})
	if !res.OK {
		t.Fatalf("a refiner failure must never fail the render, got defer: %s", res.Reason)
	}
	if args := readArgv(t, argvPath); args[1] != raw {
		t.Errorf("render received %q, want the raw prompt after fallback", args[1])
	}
	var data map[string]any
	_ = json.Unmarshal(res.Data, &data)
	if data["refined"] != false {
		t.Errorf("data.refined = %v, want false", data["refined"])
	}
	if note, _ := data["refine_fallback"].(string); !strings.Contains(note, "refiner call failed") {
		t.Errorf("data.refine_fallback = %v, want the recorded failure", data["refine_fallback"])
	}
}

func TestRunGenerateImage_ExplicitRefineFalse(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	stub, argvPath := writeArgvStub(t, dir)
	cfg.ImageGenScript = stub
	cfg.MediaDir = dir
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	p.refineGen = neverGen(t)
	raw := "a gray sphere on a plinth"
	res := p.Run(context.Background(), core.Request{
		Task: core.TaskGenerateImage, Input: raw,
		Params: map[string]any{"refine": false},
	})
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}
	if args := readArgv(t, argvPath); args[1] != raw {
		t.Errorf("render received %q, want the raw prompt", args[1])
	}
	var data map[string]any
	_ = json.Unmarshal(res.Data, &data)
	if data["refined"] != false {
		t.Errorf("data.refined = %v, want false (configured refiner, explicit opt-out)", data["refined"])
	}
	if _, present := data["refine_fallback"]; present {
		t.Errorf("explicit opt-out is not a fallback; data = %v", data)
	}
}

// TestRunGenerateImage_OutPathIgnoresRefineParam: the default output path must
// be IDENTICAL for the same prompt+seed whether the request carries the refine
// knob or not, and whether a refiner is configured or not — the knob selects
// preprocessing, and out always derives from the RAW prompt.
func TestRunGenerateImage_OutPathIgnoresRefineParam(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	stub, argvPath := writeArgvStub(t, dir)
	raw := "a gray sphere on a plinth"
	run := func(refinerModel string, params map[string]any, gen func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error)) string {
		cfg := config.Default()
		cfg.ImageGenScript = stub
		cfg.MediaDir = dir
		cfg.ImageGenRefinerModel = refinerModel
		p := refinerTestPipeline(t, cfg)
		p.refineGen = gen
		res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: raw, Params: params})
		if !res.OK {
			t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
		}
		return readArgv(t, argvPath)[0] // the out path
	}
	okGen := func(_ context.Context, _, _, user string, _ int, _ float64) (llamaclient.GenResult, error) {
		return llamaclient.GenResult{Content: user + " in soft golden-hour light with long shadows, 85mm"}, nil
	}
	outPlain := run("", map[string]any{"seed": 7}, neverGen(t))
	outKnob := run("refiner-x", map[string]any{"seed": 7, "refine": false}, neverGen(t))
	outRefined := run("refiner-x", map[string]any{"seed": 7}, okGen)
	if outPlain != outKnob || outPlain != outRefined {
		t.Errorf("out paths fork on the refine knob: plain %q / knob %q / refined %q", outPlain, outKnob, outRefined)
	}
}

// --- E2E: the sdcpp engine rides the same decision point ---

func TestRunGenerateImageSdcpp_RefinedPromptReachesRender(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	stub, argvPath := writeArgvStub(t, dir)
	cfg.ImageGenEngine = "sdcpp"
	cfg.SdcppBin = filepath.Join(dir, "sd-stub")
	cfg.SdcppModel = filepath.Join(dir, "model.gguf")
	cfg.SdcppScript = stub
	cfg.MediaDir = dir
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	raw := "a lighthouse at dusk"
	refined := "a lighthouse at dusk under a violet sky, long exposure, sea mist catching the beam, 24mm"
	p.refineGen = func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
		return llamaclient.GenResult{Content: refined}, nil
	}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: raw})
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}
	if args := readArgv(t, argvPath); args[1] != refined {
		t.Errorf("sdcpp render received %q, want the refined prompt", args[1])
	}
	var data map[string]any
	_ = json.Unmarshal(res.Data, &data)
	if data["refined"] != true || data["refined_prompt"] != refined {
		t.Errorf("sdcpp result data refiner keys = %v", data)
	}
}

// --- E2E: warm batch — per-job refine/opt-out/fallback + breaker + stamp ---

// writeBatchArgvStub writes a node stub for the --batch protocol: it reads the
// jobs JSONL, creates each job's out file, and writes one ok result line per
// job — a GPU-free stand-in for batch-jobs.mjs.
func writeBatchArgvStub(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "batch-stub.mjs")
	content := `import {readFileSync, writeFileSync} from "node:fs";
const args = process.argv.slice(2);
const jobsPath = args[args.indexOf("--batch") + 1];
const resultsPath = args[args.indexOf("--results") + 1];
const lines = readFileSync(jobsPath, "utf8").split("\n").filter(l => l.trim());
const results = lines.map((l, i) => {
  const j = JSON.parse(l);
  writeFileSync(j.out, "stub-output");
  return JSON.stringify({i, out: j.out, seed: j.seed, ok: true, ms: 1});
});
writeFileSync(resultsPath, results.join("\n") + "\n");
`
	if err := os.WriteFile(script, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestRunImageBatch_RefinerPerJob(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ImageGenScript = writeBatchArgvStub(t, dir)
	cfg.MediaDir = dir
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	calls := 0
	p.refineGen = func(_ context.Context, _, _, user string, _ int, _ float64) (llamaclient.GenResult, error) {
		calls++
		if strings.HasPrefix(user, "c ") {
			return llamaclient.GenResult{}, errors.New("boom")
		}
		return llamaclient.GenResult{Content: "REFINED " + user + " with dramatic rim lighting, 85mm, shallow focus"}, nil
	}
	off := false
	jobs := []ImageBatchJob{
		{Prompt: "a red bike leaning on a wall"},             // refined
		{Prompt: "b blue car in the rain", Refine: &off},     // per-job opt-out
		{Prompt: "c green boat on a canal in early morning"}, // refiner fails -> raw
	}
	items, err := p.RunImageBatch(context.Background(), jobs)
	if err != nil {
		t.Fatalf("batch failed: %v", err)
	}
	if calls != 2 { // job 1 opted out; jobs 0 and 2 called
		t.Errorf("refiner calls = %d, want 2", calls)
	}
	wantRefined := "REFINED " + jobs[0].Prompt + " with dramatic rim lighting, 85mm, shallow focus"
	if items[0].Refined == nil || !*items[0].Refined || items[0].RefinedPrompt != wantRefined || items[0].RefineFallback != "" {
		t.Errorf("item0 = %+v, want refined with the refined prompt", items[0])
	}
	// Shape parity with the single path: a configured refiner means EVERY item
	// carries refined true/false — including the opt-out.
	if items[1].Refined == nil || *items[1].Refined || items[1].RefinedPrompt != "" || items[1].RefineFallback != "" {
		t.Errorf("item1 (opt-out) = %+v, want refined:false with no fallback", items[1])
	}
	if b, _ := json.Marshal(items[1]); !strings.Contains(string(b), `"refined":false`) {
		t.Errorf("opt-out item JSON must carry refined:false, got %s", b)
	}
	if items[2].Refined == nil || *items[2].Refined || !strings.Contains(items[2].RefineFallback, "refiner call failed") {
		t.Errorf("item2 (fallback) = %+v, want the recorded failure", items[2])
	}
	// The jobs file the render script consumed must carry the refined prompt for
	// job 0 and the RAW prompts for jobs 1 and 2.
	matches, _ := filepath.Glob(filepath.Join(dir, "batch-*.jobs.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("jobs files = %v, want exactly one", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("jobs lines = %d, want 3", len(lines))
	}
	for i, wantPrompt := range []string{wantRefined, jobs[1].Prompt, jobs[2].Prompt} {
		var j ImageBatchJob
		if err := json.Unmarshal([]byte(lines[i]), &j); err != nil {
			t.Fatal(err)
		}
		if j.Prompt != wantPrompt {
			t.Errorf("jobs line %d prompt = %q, want %q", i, j.Prompt, wantPrompt)
		}
	}
}

// TestRunImageBatch_BreakerTripsOnTransportFailures: a refiner that stops
// answering must not stall the batch timeout-by-timeout — after
// refinerBreakerLimit consecutive transport-class failures the remaining jobs
// skip the refiner and say so.
func TestRunImageBatch_BreakerTripsOnTransportFailures(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ImageGenScript = writeBatchArgvStub(t, dir)
	cfg.MediaDir = dir
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	calls := 0
	p.refineGen = func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
		calls++
		return llamaclient.GenResult{}, errors.New("dial tcp: connection refused")
	}
	jobs := make([]ImageBatchJob, 6)
	for i := range jobs {
		jobs[i] = ImageBatchJob{Prompt: strings.Repeat("x", i+1) + " scene in morning light"}
	}
	items, err := p.RunImageBatch(context.Background(), jobs)
	if err != nil {
		t.Fatalf("batch failed: %v", err)
	}
	if calls != refinerBreakerLimit {
		t.Errorf("refiner calls = %d, want %d (breaker must stop further calls)", calls, refinerBreakerLimit)
	}
	for i := 0; i < refinerBreakerLimit; i++ {
		if !strings.Contains(items[i].RefineFallback, "refiner call failed") {
			t.Errorf("item%d fallback = %q, want the transport failure", i, items[i].RefineFallback)
		}
	}
	for i := refinerBreakerLimit; i < len(items); i++ {
		if items[i].RefineFallback != "refiner disabled after 3 consecutive failures" {
			t.Errorf("item%d fallback = %q, want the breaker note", i, items[i].RefineFallback)
		}
		if items[i].Refined == nil || *items[i].Refined {
			t.Errorf("item%d refined = %v, want false", i, items[i].Refined)
		}
		if !items[i].OK {
			t.Errorf("item%d must still render OK — the breaker only skips refinement", i)
		}
	}
}

// TestRunImageBatch_GuardFailuresDoNotTripBreaker: guard-class rejections mean
// the refiner tier is ALIVE — every job must still get its refinement attempt.
func TestRunImageBatch_GuardFailuresDoNotTripBreaker(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ImageGenScript = writeBatchArgvStub(t, dir)
	cfg.MediaDir = dir
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	calls := 0
	p.refineGen = func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
		calls++
		return llamaclient.GenResult{Content: "x"}, nil // shorter than every prompt: guard-class
	}
	jobs := make([]ImageBatchJob, 5)
	for i := range jobs {
		jobs[i] = ImageBatchJob{Prompt: strings.Repeat("y", i+1) + " scene in evening light"}
	}
	items, err := p.RunImageBatch(context.Background(), jobs)
	if err != nil {
		t.Fatalf("batch failed: %v", err)
	}
	if calls != len(jobs) {
		t.Errorf("refiner calls = %d, want %d (guard failures must not trip the breaker)", calls, len(jobs))
	}
	for i := range items {
		if !strings.Contains(items[i].RefineFallback, "shorter than the input") {
			t.Errorf("item%d fallback = %q, want the guard reason", i, items[i].RefineFallback)
		}
	}
}

// TestRunImageBatch_StampFromRawJobs: the jobs/results filenames must hash the
// RAW jobs — an identical batch re-run with a (temperature-sampled, different)
// refinement must reuse the same files, not mint new ones.
func TestRunImageBatch_StampFromRawJobs(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ImageGenScript = writeBatchArgvStub(t, dir)
	cfg.MediaDir = dir
	cfg.ImageGenRefinerModel = "refiner-x"
	p := refinerTestPipeline(t, cfg)
	run := 0
	p.refineGen = func(_ context.Context, _, _, user string, _ int, _ float64) (llamaclient.GenResult, error) {
		run++
		return llamaclient.GenResult{Content: strings.Repeat("R", run) + "EFINED " + user + " with painterly depth and warm backlight"}, nil
	}
	jobs := []ImageBatchJob{{Prompt: "a red bike leaning on a wall", Seed: 4242}}
	for i := 0; i < 2; i++ {
		if _, err := p.RunImageBatch(context.Background(), jobs); err != nil {
			t.Fatalf("run %d failed: %v", i, err)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "batch-*.jobs.jsonl"))
	if len(matches) != 1 {
		t.Errorf("jobs files after two identical runs = %v, want exactly one (raw-jobs stamp)", matches)
	}
}

// TestRunImageBatch_NoRefinerByteCompat: with no refiner configured, batch
// items carry NONE of the refiner keys.
func TestRunImageBatch_NoRefinerByteCompat(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ImageGenScript = writeBatchArgvStub(t, dir)
	cfg.MediaDir = dir
	p := refinerTestPipeline(t, cfg)
	p.refineGen = neverGen(t)
	items, err := p.RunImageBatch(context.Background(), []ImageBatchJob{{Prompt: "a red bike leaning on a wall"}})
	if err != nil {
		t.Fatalf("batch failed: %v", err)
	}
	b, _ := json.Marshal(items[0])
	for _, k := range []string{"refined", "refined_prompt", "refine_fallback"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("refiner-less batch item must omit %q, got %s", k, b)
		}
	}
}
