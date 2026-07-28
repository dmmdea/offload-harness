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

// TestDroppingThe26BRemovesItsVarAndSetMembershipToo: llama-swap REJECTS a config
// whose set names an unknown var, and a var naming a model that does not exist is the
// same dangling reference one level down. Removing the block without both bricks the
// service on a tier that simply does not serve the 26B.
func TestDroppingThe26BRemovesItsVarAndSetMembershipToo(t *testing.T) {
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
	if !strings.Contains(got, `interactive: "+residents & (e4b | e2b)"`) {
		t.Errorf("set expression not rewritten cleanly after dropping the 26B:\n%s", got)
	}
	if strings.Contains(got, "m26") {
		t.Errorf("the 26B's matrix var survived its removal:\n%s", got)
	}
}

// TestTheMemoryStackIsInEverySet encodes a MEASURED lesson as a test, now in matrix
// terms. Under `groups:` this was "support must stay swap:false" — and that guarantee
// turned out to be worth less than it read: on the workstation node an exclusive group
// evicted a persistent:true group anyway, the reranker could not start, and the memory
// stack silently degraded to dense-only ordering. A matrix set IS a valid concurrent
// combination, so a resident that appears in EVERY set can never be evicted to satisfy
// one — the guarantee is structural rather than advisory.
func TestTheMemoryStackIsInEverySet(t *testing.T) {
	got, err := Render(linuxCUDA(t), params())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "\ngroups:") {
		t.Error("legacy groups: block resurfaced — a config may use matrix OR groups, never both")
	}
	for _, want := range []string{
		`residents: "emb & rer"`,       // the memory stack IS the resident set
		`interactive: "+residents & (`, // and every other set includes it
		"emb: 1000",                    // maximally expensive to stop
		"rer: 1000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered matrix missing %q:\n%s", want, got)
		}
	}
}

// TestNoTemplateStillDeclaresLegacyGroups: the migration is only real if every
// template moved. One left behind would keep the measured eviction bug alive on
// whichever tier renders it.
func TestNoTemplateStillDeclaresLegacyGroups(t *testing.T) {
	dir := filepath.Join("..", "..", "setup", "templates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "llama-swap.") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.Contains(s, "\ngroups:") {
			t.Errorf("%s still declares legacy `groups:`", e.Name())
		}
		if !strings.Contains(s, "\nmatrix:") {
			t.Errorf("%s declares no `matrix:` — its residency is left to llama-swap's default "+
				"rather than stated, which is exactly what the all-resident tiers got wrong", e.Name())
		}
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
