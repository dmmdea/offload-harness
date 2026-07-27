package servingtmpl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func linuxCUDA(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "llama-swap.linux-cuda.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func params() Params {
	return Params{
		LlamaBin: "/srv/offload/build/llamacpp/build/bin", ModelsDir: "/srv/offload/models",
		Listen: "127.0.0.1:11436", Ctx: 32768, KVType: "q8_0", FlashAttn: "on",
		MoE26B: "--cpu-moe", Threads: 8, Include26B: true,
	}
}

// TestRenderLeavesNoTokens is the guard install.ps1 only has a comment about: a
// config that still says `--ctx-size __CTX__` starts a server that fails in a way
// reading like a model problem, not a config one.
func TestRenderLeavesNoTokens(t *testing.T) {
	got, err := Render(linuxCUDA(t), params())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "__") {
		t.Errorf("rendered config still contains a token:\n%s", got)
	}
	for _, want := range []string{
		"--ctx-size 32768", "--cache-type-k q8_0", "--cache-type-v q8_0",
		"--flash-attn on", "--threads 8", "listen: 127.0.0.1:11436",
		"/srv/offload/build/llamacpp/build/bin/llama-server",
		"/srv/offload/models/gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered config missing %q", want)
		}
	}
}

// TestUnresolvedTokenIsRefused: the check must actually fire, or it is decoration.
func TestUnresolvedTokenIsRefused(t *testing.T) {
	_, err := Render("cmd: __LLAMA_BIN__/llama-server --something __FUTURE_TOKEN__", params())
	if err == nil || !strings.Contains(err.Error(), "__FUTURE_TOKEN__") {
		t.Fatalf("an unknown token must be refused by name, got %v", err)
	}
}

// TestDroppingThe26BRemovesItsGroupMembershipToo: llama-swap REJECTS a config whose
// group names a model that does not exist, so removing the block without the member
// bricks the service on a tier that simply does not serve the 26B.
func TestDroppingThe26BRemovesItsGroupMembershipToo(t *testing.T) {
	p := params()
	p.Include26B = false
	p.MoE26B = "" // a dropped tier names no placement
	got, err := Render(linuxCUDA(t), p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "gemma4-26b-a4b") {
		t.Errorf("26B still referenced after being dropped:\n%s", got)
	}
	// The seats that remain must survive intact.
	for _, want := range []string{"offload-e4b:", "gemma4-e2b:", "embeddinggemma:", "bge-reranker-v2-m3:"} {
		if !strings.Contains(got, want) {
			t.Errorf("dropping the 26B removed %q as well", want)
		}
	}
	if !strings.Contains(got, "members: [offload-e4b, gemma4-e2b]") {
		t.Errorf("heavy group members not rewritten cleanly:\n%s", got)
	}
}

// TestHeavyGroupIsNeverExclusive encodes a MEASURED lesson as a test. On the 6 GB
// node, exclusive:true on a swapping tier meant the loaded seat evicted everything
// and nothing evicted it — every chat request returned 502 for the full 5-minute TTL
// after any render. The support tier must also stay swap:false so the embedder and
// reranker remain co-resident; when they swapped, one RAG query paid three model loads.
func TestHeavyGroupIsNeverExclusive(t *testing.T) {
	got, err := Render(linuxCUDA(t), params())
	if err != nil {
		t.Fatal(err)
	}
	heavy := section(got, "  heavy:")
	if !strings.Contains(heavy, "swap: true") || !strings.Contains(heavy, "exclusive: false") {
		t.Errorf("heavy group must be swap:true + exclusive:false, got:\n%s", heavy)
	}
	support := section(got, "  support:")
	if !strings.Contains(support, "swap: false") {
		t.Errorf("support group must be swap:false so the small models stay resident, got:\n%s", support)
	}
}

// TestRenderRefusesIncompleteParams: a half-specified tier must fail loudly here
// rather than produce a config that starts and misbehaves.
func TestRenderRefusesIncompleteParams(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Params)
		want string
	}{
		{"no ctx", func(p *Params) { p.Ctx = 0 }, "ctx size"},
		{"no threads", func(p *Params) { p.Threads = 0 }, "threads"},
		{"no models dir", func(p *Params) { p.ModelsDir = "" }, "models dir"},
		{"26B with no placement", func(p *Params) { p.MoE26B = "" }, "26B MoE placement"},
	} {
		p := params()
		tc.mut(&p)
		_, err := Render(linuxCUDA(t), p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error naming %q, got %v", tc.name, tc.want, err)
		}
	}
}

// section returns the lines under a YAML key at its indent, for assertions.
func section(doc, key string) string {
	i := strings.Index(doc, key)
	if i < 0 {
		return ""
	}
	rest := doc[i+len(key):]
	var out []string
	for _, l := range strings.Split(rest, "\n") {
		if strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "    ") && strings.Contains(l, ":") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
