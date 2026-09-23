package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// generate-image --family / --transparent reach the pipeline exactly as the MCP
// params do (ADR 0058). Each is observed as the pipeline's own refusal, which runs
// before any lease or render, so the test needs no ComfyUI. HOME/USERPROFILE point at
// a temp dir so every "~/.local-offload/..." path the pipeline opens stays in it.
func TestGenerateImageCLIFamilyFlagsReachThePipeline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgPath := filepath.Join(home, "config.json")
	cfgJSON := `{"endpoint":"http://127.0.0.1:1","imagegen_script":"render/comfy-generate.mjs","imagegen_family":"krea2",
		"imagegen_ckpt":"krea2_turbo_bf16.safetensors",
		"imagegen_families":{"qwen-image-2.1":{"license":"Qwen Research License","commercial_use":false,
			"imagegen_family":"qwen-image-2.1","imagegen_ckpt":"qwen_image_2.1_bf16.safetensors",
			"imagegen_clip":"qwen3vl_8b_bf16.safetensors","imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors"}}}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := runGenerateImage([]string{"a fox", "--config", cfgPath, "--family", "nope", "--json"}); err != nil {
			t.Errorf("runGenerateImage: %v", err)
		}
	})
	if !strings.Contains(out, `unknown image family \"nope\"`) || !strings.Contains(out, "qwen-image-2.1 [non-commercial: Qwen Research License]") {
		t.Errorf("--family must reach the pipeline's resolver, got:\n%s", out)
	}
	out = captureStdout(t, func() {
		if err := runGenerateImage([]string{"a fox", "--config", cfgPath, "--transparent", "--json"}); err != nil {
			t.Errorf("runGenerateImage: %v", err)
		}
	})
	if !strings.Contains(out, "transparent output needs") {
		t.Errorf("--transparent on the krea2 default must reach the pipeline and defer, got:\n%s", out)
	}
	// A batch has no family seam: the flag would be silently ignored, so it is refused.
	jobs := filepath.Join(home, "jobs.jsonl")
	if err := os.WriteFile(jobs, []byte(`{"prompt":"p"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runGenerateImage([]string{"--batch", jobs, "--config", cfgPath, "--family", "qwen-image-2.1"})
	if err == nil || !strings.Contains(err.Error(), "--batch renders this machine's default binding") {
		t.Errorf("--family with --batch must be refused, got %v", err)
	}
}

// generate-image --batch renders the default binding, so its JSON carries that
// binding's license at the top level and on every item — the same keys a single
// render returns (ADR 0058). The runner is a node stub that writes each job's out.
func TestGenerateImageCLIBatchCarriesTheDefaultLicense(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	stub := filepath.Join(home, "batch-stub.mjs")
	if err := os.WriteFile(stub, []byte(`import {readFileSync, writeFileSync} from "node:fs";
const a = process.argv.slice(2);
const jobs = readFileSync(a[a.indexOf("--batch") + 1], "utf8").split("\n").filter(l => l.trim());
writeFileSync(a[a.indexOf("--results") + 1], jobs.map((l, i) => { const j = JSON.parse(l); writeFileSync(j.out, "x");
  return JSON.stringify({i, out: j.out, seed: j.seed, ok: true, ms: 1}); }).join("\n") + "\n");
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(home, "config.json")
	// Hermetic lease (the leaseFixture rule): a temp state_dir, never the machine's
	// real lease root, and a short wait so a stray holder fails the test fast.
	cfgJSON := `{"endpoint":"http://127.0.0.1:1","state_dir":` + strconvQuote(filepath.ToSlash(home)) + `,"gpu_wait_ms":5000,"media_dir":` + strconvQuote(filepath.ToSlash(filepath.Join(home, "media"))) +
		`,"imagegen_script":` + strconvQuote(filepath.ToSlash(stub)) + `,"imagegen_family":"krea2",
		"imagegen_license":"Research-Only Test License","imagegen_commercial_use":false}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs := filepath.Join(home, "jobs.jsonl")
	if err := os.WriteFile(jobs, []byte(`{"prompt":"a red bike"}`+"\n"+`{"prompt":"a blue car"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := runGenerateImage([]string{"--batch", jobs, "--config", cfgPath, "--json"}); err != nil {
			t.Errorf("runGenerateImage --batch: %v", err)
		}
	})
	var res struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || !res.OK {
		t.Fatalf("batch result: %v\n%s", err, out)
	}
	var data struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(res.Data, &data)
	var top map[string]any
	_ = json.Unmarshal(res.Data, &top)
	if top["family"] != "krea2" || top["license"] != "Research-Only Test License" || top["commercial_use"] != false ||
		!strings.Contains(fmt.Sprint(top["license_note"]), "research/evaluation use only") {
		t.Errorf("batch payload lacks the default binding's license: %s", res.Data)
	}
	if len(data.Items) != 2 {
		t.Fatalf("items = %v", data.Items)
	}
	for i, it := range data.Items {
		if it["license"] != "Research-Only Test License" || it["commercial_use"] != false || it["license_note"] == nil {
			t.Errorf("item %d lacks the license tag: %v", i, it)
		}
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
