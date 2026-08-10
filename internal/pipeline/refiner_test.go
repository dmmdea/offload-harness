package pipeline

// Opt-in image-prompt refiner (refiner.go) coverage. The load-bearing
// properties under test, in order: the quoted-span guard (the text-render
// contract), fail-safe fallback on every refiner problem, byte-compatibility
// of the OFF path (refiner unset => the render receives the raw prompt and the
// result data carries no refiner keys), timeout wiring, and the batch/sdcpp
// parity of the shared decision point. Render scripts are GPU-free node stubs
// that record their argv (the pipeline test convention); the refiner call is
// faked through the p.refineGen seam (nil = the real llamaclient path, covered
// by the httptest wiring test).

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

// --- quoted-span guard ---

func TestMissingQuotedSpan(t *testing.T) {
	cases := []struct {
		name         string
		raw, refined string
		want         string
	}{
		{"no quotes", "a red bike at dawn", "a red bike at golden-hour dawn, 35mm", ""},
		{"span preserved", `a poster saying "BUY NOW" on a wall`, `an editorial poster saying "BUY NOW" on a weathered brick wall, soft light`, ""},
		{"span dropped", `a poster saying "BUY NOW" on a wall`, `an editorial poster saying BUY NOW TODAY on a wall`, `"BUY NOW"`},
		{"span altered", `a sign reading "OPEN"`, `a neon sign reading "open" at night`, `"OPEN"`},
		{"multiple spans preserved", `mug with "CAFE" logo and "EST. 1999" below`, `ceramic mug with "CAFE" logo and "EST. 1999" below, studio light`, ""},
		{"second of two dropped", `mug with "CAFE" logo and "EST. 1999" below`, `ceramic mug with "CAFE" logo, studio light`, `"EST. 1999"`},
		{"apostrophes are not quotes", "a dog's breakfast, rock 'n' roll style", "a chaotic still life in a rocking style, 50mm, morning light on a long table", ""},
		{"unpaired trailing quote ignored", `say "hi" and a stray " mark`, `please say "hi" warmly in soft light and nothing else here`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := missingQuotedSpan(c.raw, c.refined); got != c.want {
				t.Errorf("missingQuotedSpan(%q, %q) = %q, want %q", c.raw, c.refined, got, c.want)
			}
		})
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
	out, ok, note := p.maybeRefinePrompt(context.Background(), raw, false)
	if !ok || out != refined || note != "" {
		t.Fatalf("maybeRefinePrompt = (%q, %v, %q), want the trimmed refined prompt", out, ok, note)
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
	cases := []struct {
		name     string
		gen      func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error)
		wantNote string
	}{
		{"transport error", func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
			return llamaclient.GenResult{}, errors.New("connection refused")
		}, "refiner call failed"},
		{"empty output", func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
			return llamaclient.GenResult{Content: "   \n"}, nil
		}, "empty refiner output"},
		{"shorter than input", func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
			return llamaclient.GenResult{Content: `a bottle "PURE-9"`}, nil
		}, "shorter than the input"},
		{"dropped quoted span", func(context.Context, string, string, string, int, float64) (llamaclient.GenResult, error) {
			return llamaclient.GenResult{Content: "an immaculate studio product shot of a frosted glass bottle, macro lens, diffuse key light"}, nil
		}, `dropped quoted span "PURE-9"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.ImageGenRefinerModel = "refiner-x"
			p := refinerTestPipeline(t, cfg)
			p.refineGen = c.gen
			out, ok, note := p.maybeRefinePrompt(context.Background(), raw, false)
			if ok || out != raw {
				t.Fatalf("fallback must return the raw prompt: got (%q, %v)", out, ok)
			}
			if !strings.Contains(note, c.wantNote) {
				t.Errorf("note = %q, want it to contain %q", note, c.wantNote)
			}
		})
	}
}

func TestMaybeRefinePrompt_OffStates(t *testing.T) {
	// Unconfigured: never calls the model, note empty.
	p := refinerTestPipeline(t, config.Default())
	p.refineGen = neverGen(t)
	if out, ok, note := p.maybeRefinePrompt(context.Background(), "a raw prompt", false); ok || out != "a raw prompt" || note != "" {
		t.Fatalf("unconfigured refiner must be a byte-level no-op, got (%q, %v, %q)", out, ok, note)
	}
	// Configured but explicitly off: same no-op, still no model call.
	cfg := config.Default()
	cfg.ImageGenRefinerModel = "refiner-x"
	p2 := refinerTestPipeline(t, cfg)
	p2.refineGen = neverGen(t)
	if out, ok, note := p2.maybeRefinePrompt(context.Background(), "a raw prompt", true); ok || out != "a raw prompt" || note != "" {
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
	if _, ok, _ := p.maybeRefinePrompt(context.Background(), "a lighthouse at dusk", false); !ok {
		t.Fatal("expected refinement to succeed")
	}
	if remaining <= 5*time.Second || remaining > 7*time.Second {
		t.Errorf("deadline %v from now, want ~7s (imagegen_refiner_timeout_sec)", remaining)
	}
}

func TestRefinerTimeout_ExpiryFallsBack(t *testing.T) {
	cfg := config.Default()
	cfg.ImageGenRefinerModel = "refiner-x"
	cfg.ImageGenRefinerTimeoutSec = 1
	p := refinerTestPipeline(t, cfg)
	p.refineGen = func(ctx context.Context, _, _, _ string, _ int, _ float64) (llamaclient.GenResult, error) {
		<-ctx.Done() // a hung refiner: only the wired timeout can end this
		return llamaclient.GenResult{}, ctx.Err()
	}
	start := time.Now()
	out, ok, note := p.maybeRefinePrompt(context.Background(), "a lighthouse at dusk", false)
	if ok || out != "a lighthouse at dusk" || !strings.Contains(note, "refiner call failed") {
		t.Fatalf("timeout must fall back to the raw prompt, got (%q, %v, %q)", out, ok, note)
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
	if _, ok, _ := p.maybeRefinePrompt(context.Background(), "a lighthouse at dusk", false); !ok {
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
	out, ok, note := p.maybeRefinePrompt(context.Background(), "a lighthouse at dusk", false)
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

// --- E2E: warm batch — per-job refine/opt-out/fallback through one pass ---

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
	if !items[0].Refined || items[0].RefinedPrompt != wantRefined || items[0].RefineFallback != "" {
		t.Errorf("item0 = %+v, want refined with the refined prompt", items[0])
	}
	if items[1].Refined || items[1].RefinedPrompt != "" || items[1].RefineFallback != "" {
		t.Errorf("item1 (opt-out) = %+v, want no refiner keys", items[1])
	}
	if items[2].Refined || !strings.Contains(items[2].RefineFallback, "refiner call failed") {
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
