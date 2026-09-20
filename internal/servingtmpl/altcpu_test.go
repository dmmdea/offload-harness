package servingtmpl

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func altCPUParams(goos string) Params {
	return Params{
		LlamaBin: "/opt/llama-vulkan", ModelsDir: "/opt/models", Listen: "127.0.0.1:11436",
		Ctx: 8192, KVType: "f16", FlashAttn: "off", Threads: 6, GOOS: goos, Backend: "vulkan",
		AltCPULlamaBin: "/opt/llama-cpu",
	}
}

func readTmpl(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAltCPUSeatsRenderBesideTheVulkanSeats: the dual-route shape binxarn was
// hand-spliced into on 2026-09-20, produced by the renderer — same ids, the CPU
// build's binary and loader path, the cpu template's flags, matrix membership.
func TestAltCPUSeatsRenderBesideTheVulkanSeats(t *testing.T) {
	out, err := Render(readTmpl(t, "llama-swap.linux-vulkan.yaml"), altCPUParams("linux"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\n  offload-e4b-cpu:\n",
		"\n  gemma4-e2b-cpu:\n",
		"aliases: [gemma4-e4b-cpu, offload-cpu]",
		`  ldcpu: "LD_LIBRARY_PATH=/opt/llama-cpu:${LD_LIBRARY_PATH:-}"`,
		`env: ["${ldcpu}"]`,
		"/opt/llama-cpu/llama-server --model /opt/models/gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf",
		"/opt/llama-cpu/llama-server --model /opt/models/gemma-4-E2B-it-qat-UD-Q4_K_XL.gguf",
		"    e4bc: offload-e4b-cpu\n",
		"    e2bc: gemma4-e2b-cpu\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered config missing %q", want)
		}
	}
	// The CPU entries carry the cpu template's flags: threads, no -ngl, no flash-attn.
	for _, m := range regexp.MustCompile(`(?ms)^  (offload-e4b-cpu|gemma4-e2b-cpu):\n(.*?)^  [A-Za-z]`).FindAllStringSubmatch(out, -1) {
		body := m[2]
		if strings.Contains(body, "-ngl") || strings.Contains(body, "--n-gpu-layers") || strings.Contains(body, "--flash-attn") {
			t.Errorf("%s carries GPU flags:\n%s", m[1], body)
		}
		if !strings.Contains(body, "--threads 6") || !strings.Contains(body, "--ctx-size 8192") || !strings.Contains(body, "--cache-type-k f16") {
			t.Errorf("%s is missing the cpu template's flags:\n%s", m[1], body)
		}
		if !strings.Contains(body, "ttl: 300") {
			t.Errorf("%s must idle-unload like every seat", m[1])
		}
	}
	// Matrix: the CPU vars join the interactive set, so they are exclusive with the
	// GPU seats and never with the residents.
	set := regexp.MustCompile(`interactive: "([^"]+)"`).FindStringSubmatch(out)
	if set == nil || !strings.Contains(set[1], "e4bc") || !strings.Contains(set[1], "e2bc") {
		t.Fatalf("interactive set lacks the CPU vars: %v", set)
	}
	if vs := Audit(out); len(vs) != 0 {
		t.Errorf("rendered dual-route config fails the audit:\n%s", Violations(vs))
	}
	if left := uniqueTokens(out); len(left) > 0 {
		t.Errorf("unresolved tokens: %v", left)
	}
}

// TestAltCPUSeatsOnWindowsUseTheExeAndNoLoaderMacro: the Windows builds are
// self-contained (no LD_LIBRARY_PATH), and the binary is llama-server.exe.
func TestAltCPUSeatsOnWindowsUseTheExeAndNoLoaderMacro(t *testing.T) {
	p := altCPUParams("windows")
	p.LlamaBin, p.AltCPULlamaBin = `C:/llama-vulkan`, `C:/llama-cpu`
	out, err := Render(readTmpl(t, "llama-swap.win-vulkan.yaml"), p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "C:/llama-cpu/llama-server.exe --model") {
		t.Error("windows CPU seat must run llama-server.exe from the CPU build dir")
	}
	if strings.Contains(out, "ldcpu") {
		t.Error("windows templates carry no loader macro")
	}
}

// TestAltCPUIsRefusedOnACPUTier: the cpu backend IS the primary route on a cpu
// tier; a second CPU family there is a configuration error, not a no-op.
func TestAltCPUIsRefusedOnACPUTier(t *testing.T) {
	p := altCPUParams("linux")
	p.Backend, p.LlamaBin = "cpu", "/opt/llama-cpu"
	if _, err := Render(readTmpl(t, "llama-swap.linux-cpu.yaml"), p); err == nil || !strings.Contains(err.Error(), "cpu tier") {
		t.Fatalf("want a refusal naming the cpu tier, got %v", err)
	}
}

// TestNoAltCPUWithoutTheBinDir: an empty AltCPULlamaBin renders the plain tier,
// byte-identical to before this file existed.
func TestNoAltCPUWithoutTheBinDir(t *testing.T) {
	p := altCPUParams("linux")
	p.AltCPULlamaBin = ""
	out, err := Render(readTmpl(t, "llama-swap.linux-vulkan.yaml"), p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "-cpu:") || strings.Contains(out, "ldcpu") {
		t.Error("no CPU family may render without a CPU build dir")
	}
}

func TestModelFileFor(t *testing.T) {
	tmpl := "models:\n  a:\n    cmd: >-\n      /x/llama-server --model /m/a.gguf\n      --parallel 1\n  b:\n    cmd: >-\n      /x/llama-server -m /m/b.gguf\n\nmatrix:\n"
	if got := modelFileFor(tmpl, "a"); got != "/m/a.gguf" {
		t.Errorf("a = %q", got)
	}
	if got := modelFileFor(tmpl, "b"); got != "/m/b.gguf" {
		t.Errorf("b = %q", got)
	}
	if got := modelFileFor(tmpl, "c"); got != "" {
		t.Errorf("c = %q, want empty", got)
	}
}
