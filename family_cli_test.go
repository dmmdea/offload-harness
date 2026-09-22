package main

import (
	"os"
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
