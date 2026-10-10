package fleetnode

// The image recipe on the wire (P0 plan, S1; ADR 0082). A node publishes what each of its ComfyUI image families IS
// (the weight files with their sizes and the resolved sampling, digested), says it carries `refine`, and refuses a
// dispatch whose recipe_digest is not the one of the family it names. The delegator's strict match and the node's own
// check read the same digest (mediacap.Recipe.Digest), so they cannot disagree.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// recipeNode is a ComfyUI node whose default binding is krea2 and which serves two named Qwen-Image-2.1 families,
// with every weight file on disk at a size of its own.
func recipeNode(t *testing.T) config.Config {
	t.Helper()
	comfy := t.TempDir()
	for rel, n := range map[string]int{
		"models/diffusion_models/krea2_turbo_bf16.safetensors":               111,
		"models/diffusion_models/qwen_image_2.1_uc_bf16.safetensors":         222,
		"models/diffusion_models/qwen-image-2.1-UC-int8_convrot.safetensors": 333,
		"models/text_encoders/qwen3vl_4b_bf16.safetensors":                   44,
		"models/text_encoders/qwen3vl_8b_bf16.safetensors":                   88,
		"models/vae/qwen_image_vae.safetensors":                              5,
		"models/vae/qwen_image_2.1_vae_bf16.safetensors":                     6,
	} {
		p := filepath.Join(comfy, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := fullCfg()
	cfg.ComfyDir = comfy
	cfg.ImageGenFamily, cfg.ImageGenCkpt, cfg.ImageGenVAE = "krea2", "krea2_turbo_bf16.safetensors", "qwen_image_vae.safetensors"
	cfg.ImageGenSteps, cfg.ImageGenCFG = 8, 1
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{
		"qwen-image-2.1": {
			"license": json.RawMessage(`"Qwen Research License"`), "commercial_use": json.RawMessage(`false`),
			"imagegen_family": json.RawMessage(`"qwen-image-2.1"`), "imagegen_ckpt": json.RawMessage(`"qwen_image_2.1_uc_bf16.safetensors"`),
			"imagegen_clip": json.RawMessage(`"qwen3vl_8b_bf16.safetensors"`), "imagegen_vae": json.RawMessage(`"qwen_image_2.1_vae_bf16.safetensors"`),
		},
		"qwen-image-2.1-fast": {
			"license": json.RawMessage(`"Qwen Research License"`), "commercial_use": json.RawMessage(`false`),
			"imagegen_family": json.RawMessage(`"qwen-image-2.1"`), "imagegen_ckpt": json.RawMessage(`"qwen-image-2.1-UC-int8_convrot.safetensors"`),
			"imagegen_clip": json.RawMessage(`"qwen3vl_8b_bf16.safetensors"`), "imagegen_vae": json.RawMessage(`"qwen_image_2.1_vae_bf16.safetensors"`),
		},
	}
	return cfg
}

// recipeOfFamily is the recipe the test computes on its own for a family of cfg, the way the delegator does.
func recipeOfFamily(t *testing.T, cfg config.Config, name string) mediacap.Recipe {
	t.Helper()
	eff, fi, err := cfg.ResolveImageFamily(name)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := mediacap.ImageRecipe(eff, fi, mediacap.ModelRoots(eff.ComfyDir), nil)
	if !ok {
		t.Fatalf("no recipe for %q", name)
	}
	return r
}

type healthRecipes struct {
	Refine  bool `json:"refine_honoured"`
	Recipes []struct {
		Name    string `json:"name"`
		Default bool   `json:"default"`
		Digest  string `json:"digest"`
		Engine  string `json:"engine"`
		Graph   string `json:"graph"`
		Steps   int    `json:"steps"`
		Files   []struct {
			Role  string `json:"role"`
			Name  string `json:"name"`
			Bytes int64  `json:"bytes"`
		} `json:"files"`
		Explicit []string `json:"explicit"`
	} `json:"image_recipes"`
}

func readHealthRecipes(t *testing.T, s *Server) (healthRecipes, string) {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d: %s", rec.Code, rec.Body.String())
	}
	var h healthRecipes
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	return h, rec.Body.String()
}

// /fleet/health publishes one recipe row per ComfyUI image binding, the default first, each with the digest the
// delegator will compare, the files with the sizes on this node's disk, and the sampling it resolved; and it says the
// node carries `refine`.
func TestHealthPublishesImageRecipes(t *testing.T) {
	cfg := recipeNode(t)
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	h, body := readHealthRecipes(t, s)
	if !h.Refine {
		t.Errorf("a node that builds image-gen with refine must say so (refine_honoured): %s", body)
	}
	if len(h.Recipes) != 3 {
		t.Fatalf("want the default and the two named families, got %d: %s", len(h.Recipes), body)
	}
	def, std, fast := h.Recipes[0], h.Recipes[1], h.Recipes[2]
	if def.Name != "krea2" || !def.Default || std.Name != "qwen-image-2.1" || std.Default || fast.Name != "qwen-image-2.1-fast" {
		t.Errorf("rows = %s / %s / %s (default first, then the named ones in name order)", def.Name, std.Name, fast.Name)
	}
	for _, row := range h.Recipes {
		name := row.Name
		if row.Default {
			name = ""
		}
		if want := recipeOfFamily(t, cfg, name).Digest(); row.Digest != want {
			t.Errorf("%s: published digest %s, the family's recipe digests to %s", row.Name, row.Digest, want)
		}
		if row.Engine != "comfyui" {
			t.Errorf("%s: engine = %q", row.Name, row.Engine)
		}
	}
	if std.Graph != "qwen-image-2.1" || std.Steps != 40 {
		t.Errorf("the named family publishes its resolved sampling (the builder's 40 steps), got graph %q steps %d", std.Graph, std.Steps)
	}
	var got []string
	for _, f := range std.Files {
		got = append(got, fmt.Sprintf("%s=%s:%d", f.Role, f.Name, f.Bytes))
	}
	if want := "ckpt=qwen_image_2.1_uc_bf16.safetensors:222 clip=qwen3vl_8b_bf16.safetensors:88 vae=qwen_image_2.1_vae_bf16.safetensors:6"; strings.Join(got, " ") != want {
		t.Errorf("files = %v, want %s", got, want)
	}
	if strings.Contains(body, `"license":"Qwen Research License"`) == false {
		t.Errorf("the license a result is tagged with is part of the recipe row: %s", body)
	}
}

// A node with no image lane publishes neither key (an older reader and a video-only node see the shape they always
// did), and a binding that names no checkpoint has nothing to identify and publishes no row.
func TestHealthOmitsImageRecipesWithoutAnImageLane(t *testing.T) {
	s, _ := newTestServer(t, config.Config{VideoGenScript: "render/comfy-video.mjs"}, &fakeRunner{}, nil)
	_, body := readHealthRecipes(t, s)
	for _, k := range []string{"image_recipes", "refine_honoured"} {
		if strings.Contains(body, k) {
			t.Errorf("a node without image-gen must not publish %s: %s", k, body)
		}
	}
	unpinned, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	h, body := readHealthRecipes(t, unpinned)
	if len(h.Recipes) != 0 || strings.Contains(body, "image_recipes") {
		t.Errorf("a default binding that names no checkpoint is not an identity: %s", body)
	}
	if !h.Refine {
		t.Errorf("it still builds image-gen with refine: %s", body)
	}
}

// An sd.cpp family has no recipe: it never matches a ComfyUI one, and the node does not pretend.
func TestImageRecipesSkipAnSdcppFamily(t *testing.T) {
	cfg := recipeNode(t)
	cfg.ImageGenFamilies["z-image"] = config.FamilyOverlay{
		"license": json.RawMessage(`"Apache-2.0"`), "commercial_use": json.RawMessage(`true`),
		"imagegen_engine": json.RawMessage(`"sdcpp"`), "sdcpp_bin": json.RawMessage(`"/opt/sd-cli"`),
		"sdcpp_model": json.RawMessage(`"/models/z.gguf"`),
	}
	for _, r := range ImageRecipes(cfg) {
		if r.Name == "z-image" {
			t.Fatalf("an sdcpp family published a recipe: %+v", r)
		}
	}
	if n := len(ImageRecipes(cfg)); n != 3 {
		t.Errorf("the three ComfyUI bindings keep theirs, got %d", n)
	}
}

// Health stays cheap: the rows come from a memo of mediaRoutesTTL, so polling it every few seconds does not stat the
// weight files every time, and a file that changes shows within a minute.
func TestImageRecipesAreMemoisedForAMinute(t *testing.T) {
	cfg := recipeNode(t)
	now := time.Now()
	prev := mediaClock
	mediaClock = func() time.Time { return now }
	t.Cleanup(func() { mediaClock = prev })
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	first, _ := readHealthRecipes(t, s)

	if err := os.WriteFile(filepath.Join(cfg.ComfyDir, "models", "diffusion_models", "krea2_turbo_bf16.safetensors"), make([]byte, 999), 0o644); err != nil {
		t.Fatal(err)
	}
	again, _ := readHealthRecipes(t, s)
	if again.Recipes[0].Digest != first.Recipes[0].Digest {
		t.Fatal("inside the memo window health must not re-read the weight files")
	}
	now = now.Add(mediaRoutesTTL + time.Second)
	later, _ := readHealthRecipes(t, s)
	if later.Recipes[0].Digest == first.Recipes[0].Digest {
		t.Fatal("a weight file that changed must show once the memo is a minute old")
	}
}

// The image-gen payload carries `refine` as the MCP handler does: only an EXPLICIT false reaches the pipeline.
func TestBuildImageGenCarriesRefineFalse(t *testing.T) {
	for payload, want := range map[string]any{
		`{"prompt":"p","refine":false}`: false,
		`{"prompt":"p","refine":true}`:  nil,
		`{"prompt":"p"}`:                nil,
	} {
		req, cleanup := mustBuild(t, fullCfg(), "image-gen", payload)
		got, has := req.Params["refine"]
		cleanup()
		if want == nil && has {
			t.Errorf("%s: refine reached the pipeline as %v; absent and true leave the node's own refiner setting alone", payload, got)
		}
		if want != nil && (!has || got != want) {
			t.Errorf("%s: refine = %v (present %v), want %v", payload, got, has, want)
		}
	}
}

// A seed above 2^53 arrives exactly: the delegator sends a JSON integer and the node's int field reads it as one.
func TestSeedIsExactAboveFloat53(t *testing.T) {
	req, cleanup := mustBuild(t, fullCfg(), "image-gen", `{"prompt":"p","seed":9007199254740993}`)
	defer cleanup()
	if got, ok := req.Params["seed"].(int); !ok || got != 9007199254740993 {
		t.Fatalf("seed = %#v, want the exact 9007199254740993", req.Params["seed"])
	}
}

func dispatchBody(t *testing.T, id string, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"job_id": id, "task_type": "image-gen", "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// THE NODE'S SIDE OF STRICT IDENTITY. A dispatch that names a recipe_digest is admitted only when the family it
// names, on THIS node, digests to it: a node whose files changed since the delegator read its health refuses with
// 412 (not 409, which already means "this job failed here before" and is re-placeable), and creates no job.
func TestRecipeDigestMismatchIs412(t *testing.T) {
	cfg := recipeNode(t)
	runner := &fakeRunner{}
	s, jobs := newTestServer(t, cfg, runner, nil)
	good := recipeOfFamily(t, cfg, "qwen-image-2.1-fast").Digest()
	other := recipeOfFamily(t, cfg, "qwen-image-2.1").Digest()

	rec := do(t, s, http.MethodPost, "/fleet/dispatch", dispatchBody(t, "job-412", map[string]any{
		"prompt": "p", "family": "qwen-image-2.1-fast", "recipe_digest": other}), nil)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("a digest of another recipe must be 412, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "recipe") || !strings.Contains(rec.Body.String(), "qwen-image-2.1-fast") {
		t.Errorf("the refusal must say what it refuses: %s", rec.Body.String())
	}
	if queued, running := jobs.Counts(); queued+running != 0 || len(runner.requests()) != 0 {
		t.Errorf("a refused dispatch creates no job and runs nothing")
	}
	if rec := do(t, s, http.MethodGet, "/fleet/jobs/job-412", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("the refused job must not exist, got %d", rec.Code)
	}

	// The same digest of the family it names is admitted, and the digest is not a pipeline parameter.
	rec = do(t, s, http.MethodPost, "/fleet/dispatch", dispatchBody(t, "job-ok", map[string]any{
		"prompt": "p", "family": "qwen-image-2.1-fast", "recipe_digest": good, "refine": false, "seed": 9007199254740993}), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a matching digest must be accepted, got %d: %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(runner.requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	reqs := runner.requests()
	if len(reqs) != 1 {
		t.Fatalf("the accepted job must run, got %d requests", len(reqs))
	}
	p := reqs[0].Params
	if _, leaked := p["recipe_digest"]; leaked {
		t.Errorf("recipe_digest is the door's, never a pipeline param: %v", p)
	}
	if p["family"] != "qwen-image-2.1-fast" || p["refine"] != false || p["seed"] != 9007199254740993 {
		t.Errorf("params = %v", p)
	}
}

// A digest names a family: an unknown family with a digest is refused too (the node's config changed since the
// delegator read it), and the default binding is named by its own name or by none.
func TestRecipeDigestChecksTheDefaultAndUnknownFamilies(t *testing.T) {
	cfg := recipeNode(t)
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	def := recipeOfFamily(t, cfg, "").Digest()
	for name, fam := range map[string]string{"empty name": "", "its own name": "krea2"} {
		rec := do(t, s, http.MethodPost, "/fleet/dispatch", dispatchBody(t, "job-def-"+strings.ReplaceAll(name, " ", ""), map[string]any{
			"prompt": "p", "family": fam, "recipe_digest": def}), nil)
		if rec.Code != http.StatusAccepted {
			t.Errorf("the default binding (%s) with its own digest must be accepted, got %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", dispatchBody(t, "job-unknown", map[string]any{
		"prompt": "p", "family": "no-such-family", "recipe_digest": def}), nil)
	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("a family this node does not serve, with a digest, is a 412, got %d: %s", rec.Code, rec.Body.String())
	}
	// No digest: the dispatch is what it always was (an unknown family is the pipeline's own deferral, not a refusal).
	rec = do(t, s, http.MethodPost, "/fleet/dispatch", dispatchBody(t, "job-nodigest", map[string]any{"prompt": "p", "family": "no-such-family"}), nil)
	if rec.Code != http.StatusAccepted {
		t.Errorf("no digest, no check: %d %s", rec.Code, rec.Body.String())
	}
}

// BuildRequest with a digest builds the same request as without one (the check is the door's, the request is not changed).
func TestBuildImageGenWithADigestBuildsTheSameRequest(t *testing.T) {
	cfg := recipeNode(t)
	d := recipeOfFamily(t, cfg, "qwen-image-2.1").Digest()
	a, ca := mustBuild(t, cfg, "image-gen", fmt.Sprintf(`{"prompt":"p","family":"qwen-image-2.1","recipe_digest":%q}`, d))
	defer ca()
	b, cb := mustBuild(t, cfg, "image-gen", `{"prompt":"p","family":"qwen-image-2.1"}`)
	defer cb()
	if fmt.Sprint(a.Params) != fmt.Sprint(b.Params) || a.Input != b.Input {
		t.Errorf("a digest changes nothing about the request: %v vs %v", a.Params, b.Params)
	}
	_, _, err := BuildRequest(context.Background(), cfg, true, "image-gen", json.RawMessage(`{"prompt":"p","family":"qwen-image-2.1","recipe_digest":"deadbeef"}`))
	var mm *recipeMismatchError
	if err == nil || !errors.As(err, &mm) {
		t.Fatalf("a wrong digest must be a *recipeMismatchError, got %v", err)
	}
}
