package pipeline

// The batch's family seam (P0 plan S3; ADR 0058 for families, ADR 0083 for the batch verb): RunImageBatchAs renders a batch
// under the binding a named family resolves to, exactly as the single render does, and RunImageBatch (no family) is the
// default binding byte for byte as it was. The runner is a node stub that records its argv and the launch environment and
// writes each job's out, so what the batch asks the renderer for is read off the wire and not off a struct.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// writeBatchProbeStub is a --batch runner that renders every job and records argv and the launch environment.
func writeBatchProbeStub(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "batch-probe-stub.mjs")
	content := `import {readFileSync, writeFileSync} from "node:fs";
const args = process.argv.slice(2);
const env = {};
for (const k of ["COMFY_CUDA_DEVICE", "COMFY_DYNAMIC_VRAM", "COMFY_DIR"]) if (k in process.env) env[k] = process.env[k];
if (process.env.RUNNER_PROBE) writeFileSync(process.env.RUNNER_PROBE, JSON.stringify({argv: args, env}));
const lines = readFileSync(args[args.indexOf("--batch") + 1], "utf8").split("\n").filter(l => l.trim());
const results = lines.map((l, i) => {
  const j = JSON.parse(l);
  writeFileSync(j.out, "stub-output");
  return JSON.stringify({i, out: j.out, seed: j.seed, ok: true, ms: 1});
});
writeFileSync(args[args.indexOf("--results") + 1], results.join("\n") + "\n");
`
	if err := os.WriteFile(script, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return script
}

// familySeamPipeline is a pipeline whose default binding is krea2 (pinned to card 1, with a default license) and that binds
// two named families: a ComfyUI one on card 2 and one on stable-diffusion.cpp.
func familySeamPipeline(t *testing.T, dir, stub string) *Pipeline {
	t.Helper()
	cfg := config.Default()
	cfg.MediaDir = dir
	cfg.ImageGenScript = stub
	cfg.ImageGenFamily = "krea2"
	cfg.ImageGenCkpt = "krea2_turbo_bf16.safetensors"
	cfg.ImageGenSteps, cfg.ImageGenCFG = 8, 1
	cfg.ImageGenLicense = "Default License"
	no := false
	cfg.ImageGenCommercialUse = &no
	cfg.ComfyCudaDevice = "1"
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{
		"qwen-image-2.1": {
			"license": []byte(`"Qwen Research License"`), "commercial_use": []byte(`false`),
			"imagegen_family":    []byte(`"qwen-image-2.1"`),
			"imagegen_ckpt":      []byte(`"qwen_image_2.1_bf16.safetensors"`),
			"imagegen_clip":      []byte(`"qwen3vl_8b_bf16.safetensors"`),
			"imagegen_vae":       []byte(`"qwen_image_2.1_vae_bf16.safetensors"`),
			"imagegen_steps":     []byte(`40`),
			"imagegen_cfg":       []byte(`1`),
			"comfy_cuda_device":  []byte(`"2"`),
			"comfy_dynamic_vram": []byte(`"on"`),
		},
		"cpp-family": {
			"license": []byte(`"Test License"`), "commercial_use": []byte(`true`),
			"imagegen_family": []byte(`"cpp-family"`),
			"imagegen_engine": []byte(`"sdcpp"`),
		},
	}
	return refinerTestPipeline(t, cfg)
}

func TestRunImageBatchFamilySeam(t *testing.T) {
	requireNodePipeline(t)
	// gpugen frees the ComfyUI it is told about when a run ends: aim that at a local server, never at one that happens to
	// be running on the machine.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	t.Setenv("COMFY_API", srv.URL)

	dir := t.TempDir()
	stub := writeBatchProbeStub(t, dir)
	probe := filepath.Join(dir, "probe.json")
	t.Setenv("RUNNER_PROBE", probe)
	p := familySeamPipeline(t, dir, stub)
	jobs := func() []ImageBatchJob {
		return []ImageBatchJob{
			{Prompt: "a red bike", Out: filepath.Join(dir, "a.png"), Seed: 11},
			{Prompt: "a blue car", Out: filepath.Join(dir, "b.png"), Seed: 22},
		}
	}
	read := func() runnerProbe {
		t.Helper()
		defer os.Remove(probe)
		return readProbe(t, probe)
	}

	t.Run("a named family renders under its own binding and tags its own license", func(t *testing.T) {
		items, err := p.RunImageBatchAs(context.Background(), "qwen-image-2.1", jobs())
		if err != nil {
			t.Fatalf("batch: %v", err)
		}
		got := read()
		if v, _ := flagVal(got.Argv, "--ckpt"); v != "qwen_image_2.1_bf16.safetensors" {
			t.Errorf("--ckpt = %q, want the family's checkpoint (argv %v)", v, got.Argv)
		}
		if v, _ := flagVal(got.Argv, "--family"); v != "qwen-image-2.1" {
			t.Errorf("--family = %q, want the family's name (argv %v)", v, got.Argv)
		}
		if v, _ := flagVal(got.Argv, "--steps"); v != "40" {
			t.Errorf("--steps = %q, want the family's 40 (argv %v)", v, got.Argv)
		}
		if got.Env["COMFY_CUDA_DEVICE"] != "2" {
			t.Errorf("the card pin must be the family's (2), got %q: a batch pinned to the default binding's card would render beside another job", got.Env["COMFY_CUDA_DEVICE"])
		}
		if len(items) != 2 {
			t.Fatalf("items = %+v", items)
		}
		for i, it := range items {
			if !it.OK || it.Family != "qwen-image-2.1" || it.License != "Qwen Research License" || it.CommercialUse == nil || *it.CommercialUse {
				t.Errorf("item %d = %+v, want the family's name and license, commercial_use false", i, it)
			}
		}
	})

	t.Run("no family is the default binding, as it always was", func(t *testing.T) {
		for name, run := range map[string]func() ([]ImageBatchItem, error){
			"RunImageBatch":         func() ([]ImageBatchItem, error) { return p.RunImageBatch(context.Background(), jobs()) },
			"RunImageBatchAs(\"\")": func() ([]ImageBatchItem, error) { return p.RunImageBatchAs(context.Background(), "", jobs()) },
			"the default's own name": func() ([]ImageBatchItem, error) {
				return p.RunImageBatchAs(context.Background(), "krea2", jobs())
			},
		} {
			items, err := run()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			got := read()
			if v, _ := flagVal(got.Argv, "--ckpt"); v != "krea2_turbo_bf16.safetensors" {
				t.Errorf("%s: --ckpt = %q, want the default binding's", name, v)
			}
			if got.Env["COMFY_CUDA_DEVICE"] != "1" {
				t.Errorf("%s: card pin = %q, want the default's 1", name, got.Env["COMFY_CUDA_DEVICE"])
			}
			if items[0].Family != "krea2" || items[0].License != "Default License" {
				t.Errorf("%s: item = %+v, want the default binding's family and license", name, items[0])
			}
		}
	})

	t.Run("an unknown family stops before anything runs", func(t *testing.T) {
		items, err := p.RunImageBatchAs(context.Background(), "nope", jobs())
		if err == nil || !strings.Contains(err.Error(), `unknown image family "nope"`) {
			t.Fatalf("want the resolver's own error naming the family, got items=%v err=%v", items, err)
		}
		if _, statErr := os.Stat(probe); statErr == nil {
			t.Errorf("the renderer ran for a family that does not exist")
		}
	})

	t.Run("a family on stable-diffusion.cpp has no warm batch and says so", func(t *testing.T) {
		_, err := p.RunImageBatchAs(context.Background(), "cpp-family", jobs())
		if err == nil || !strings.Contains(err.Error(), "stable-diffusion.cpp") {
			t.Fatalf("want a refusal naming the engine, got %v", err)
		}
		if _, statErr := os.Stat(probe); statErr == nil {
			t.Errorf("the ComfyUI renderer ran for a family bound to another engine")
		}
	})
}

// The batch's lease is asked for on the FAMILY's card: a batch of a family pinned to a free card is not held up by a
// holder on the default binding's card, and the default binding's batch still is. Reading the need off the default
// binding for a named family would queue (or refuse) the batch behind the wrong holder, and the RAM guard would size
// the lease from the wrong files.
func TestRunImageBatchAsAsksForTheFamilysCard(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) {
		c.ComfyCudaDevice = "2" // the default binding renders on card C
		c.ImageGenFamily = "krea2"
		c.ImageGenFamilies = map[string]config.FamilyOverlay{
			"on-card-a": {
				"license": []byte(`"Test License"`), "commercial_use": []byte(`false`),
				"imagegen_family":   []byte(`"on-card-a"`),
				"imagegen_ckpt":     []byte(`"other.safetensors"`),
				"comfy_cuda_device": []byte(`"0"`), // this family renders on card A
			},
		}
	}})
	holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "the default card", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDC)}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	f.letRunnersGo()
	job := func(name string) []ImageBatchJob {
		return []ImageBatchJob{{Prompt: "a calm ocean at dawn", Out: filepath.Join(f.dir, name)}}
	}

	if _, err := f.p.RunImageBatchAs(context.Background(), "", job("default.png")); err == nil || !IsGPUBusy(err) {
		t.Fatalf("the default binding's batch must be held by the holder of card C, got %v", err)
	}
	if _, err := f.p.RunImageBatchAs(context.Background(), "on-card-a", job("family.png")); err != nil && IsGPUBusy(err) {
		t.Fatalf("a batch of a family pinned to card A was held by the holder of card C: it asked for the default binding's card (%v)", err)
	}
}

func TestRunImageBatchAsKeepsTheEmptyBatchAndPromptRules(t *testing.T) {
	dir := t.TempDir()
	p := familySeamPipeline(t, dir, filepath.Join(dir, "never-runs.mjs"))
	if _, err := p.RunImageBatchAs(context.Background(), "qwen-image-2.1", nil); err == nil || !strings.Contains(err.Error(), "empty batch") {
		t.Errorf("an empty batch is still refused by name, got %v", err)
	}
	if _, err := p.RunImageBatchAs(context.Background(), "qwen-image-2.1", []ImageBatchJob{{Prompt: "ok"}, {Prompt: "  "}}); err == nil || !strings.Contains(err.Error(), "job 1: empty prompt") {
		t.Errorf("an empty prompt still refuses the whole batch with its index, got %v", err)
	}
	data, _ := json.Marshal(ImageBatchItem{})
	if strings.Contains(string(data), "node") {
		t.Errorf("an item carries no node: that belongs to the CLI's result rows, not the pipeline's item: %s", data)
	}
}
