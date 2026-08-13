// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Tests for the measurement command family (wave C).

package cli

import (
	"bytes"
	"strings"
	"testing"
)

// A real /running command line from this box. Every parser below is asserted
// against the actual shape, not a tidied-up invention.
const measureRealSeatCmd = `C:/llama.cpp-b10356/llama-server.exe --port 9207 --host 127.0.0.1 ` +
	`-m V:/models/gemma-4-E2B-it-qat-GGUF/gemma-4-E2B-it-qat-UD-Q4_K_XL.gguf -ngl 99 -sm none --jinja ` +
	`--reasoning off --flash-attn on --cache-type-k f16 --cache-type-v f16 -c 131072`

const measureEmbedSeatCmd = `C:/llama.cpp-b10356/llama-server.exe --port 9201 --host 127.0.0.1 ` +
	`-m V:/models/embeddinggemma-300M-Q8_0.gguf --embeddings --pooling mean --ctx-size 2048 ` +
	`--batch-size 2048 --ubatch-size 2048`

// Every measurement command must resolve and render help. Catches wiring
// regressions (a missing registration, a panicking RunE) before review.
func TestMeasureCommandsAreWired(t *testing.T) {
	cases := [][]string{
		{"gguf"}, {"vram"}, {"fit"}, {"ctx"},
		{"bench"}, {"bench", "aux"},
		{"gate"}, {"gate", "grammar"}, {"gate", "tools"},
		{"scratch"}, {"build"}, {"build", "check"}, {"verify"},
	}
	for _, args := range cases {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			cmd := RootCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(append(append([]string{}, args...), "--help"))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%s --help error = %v (command not wired?)", name, err)
			}
			help := out.String()
			for _, want := range []string{"Usage:", args[len(args)-1]} {
				if !strings.Contains(help, want) {
					t.Fatalf("%s --help missing %q:\n%s", name, want, help)
				}
			}
			if !strings.Contains(help, "Examples:") && !strings.Contains(help, "Example") {
				t.Errorf("%s --help carries no example", name)
			}
		})
	}
}

// Commands that mutate GPU state must never carry mcp:read-only=true, and the
// read-only ones must carry it. An agent picks tools by this annotation.
func TestMeasureReadOnlyAnnotations(t *testing.T) {
	readOnly := map[string]bool{
		"gguf": true, "vram": true, "fit": true, "ctx": true, "verify": true,
		"build": true,
		// These load models / start processes.
		"bench": false, "scratch": false, "gate": false,
	}
	root := RootCmd()
	for _, c := range root.Commands() {
		want, tracked := readOnly[c.Name()]
		if !tracked {
			continue
		}
		got := c.Annotations["mcp:read-only"] == "true"
		if got != want {
			t.Errorf("%s: mcp:read-only=%v, want %v", c.Name(), got, want)
		}
	}
}

// The verify-friendly contract: --dry-run short-circuits before any IO, and
// nothing lives in cobra's Args or MarkFlagRequired (which run before RunE).
func TestMeasureDryRunShortCircuits(t *testing.T) {
	cases := [][]string{
		{"vram", "--dry-run"},
		{"gguf", "some-model", "--dry-run"},
		{"fit", "some-model", "--dry-run"},
		{"ctx", "some-model", "--dry-run"},
		{"bench", "some-model", "--dry-run"},
		{"bench", "aux", "--dry-run"},
		{"gate", "grammar", "some-model", "--dry-run"},
		{"scratch", "some-model", "--dry-run"},
		{"build", "check", "--dry-run"},
		{"verify", "--dry-run"},
	}
	for _, args := range cases {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			cmd := RootCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%s error = %v; dry-run must short-circuit before any IO", name, err)
			}
			if !strings.Contains(out.String(), "dry-run") {
				t.Errorf("%s produced no dry-run report: %q", name, out.String())
			}
		})
	}
}

func TestMcNormalizeHostKillsLocalhost(t *testing.T) {
	cases := map[string]string{
		"http://localhost:11436":  "http://127.0.0.1:11436",
		"http://localhost:11436/": "http://127.0.0.1:11436",
		"http://[::1]:11436":      "http://127.0.0.1:11436",
		"http://127.0.0.1:11436":  "http://127.0.0.1:11436",
		"http://node-a:18791":     "http://node-a:18791",
	}
	for in, want := range cases {
		if got := mcNormalizeHost(in); got != want {
			t.Errorf("mcNormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMcSplitCmdHonorsQuotes(t *testing.T) {
	got := mcSplitCmd(`"C:/Program Files/llama/llama-server.exe" -m "V:/models/a b.gguf" -c 4096`)
	want := []string{"C:/Program Files/llama/llama-server.exe", "-m", "V:/models/a b.gguf", "-c", "4096"}
	if len(got) != len(want) {
		t.Fatalf("got %d tokens %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMcSeatFlagParsing(t *testing.T) {
	path, ok := mcSeatModelPath(measureRealSeatCmd)
	if !ok || path != "V:/models/gemma-4-E2B-it-qat-GGUF/gemma-4-E2B-it-qat-UD-Q4_K_XL.gguf" {
		t.Errorf("model path = %q (ok=%v)", path, ok)
	}
	if c, ok := mcSeatCtx(measureRealSeatCmd); !ok || c != 131072 {
		t.Errorf("-c = %d (ok=%v), want 131072", c, ok)
	}
	// The embedder spells it --ctx-size, not -c.
	if c, ok := mcSeatCtx(measureEmbedSeatCmd); !ok || c != 2048 {
		t.Errorf("--ctx-size = %d (ok=%v), want 2048", c, ok)
	}
	k, v := mcSeatCacheTypes(measureRealSeatCmd)
	if k != "f16" || v != "f16" {
		t.Errorf("cache types = %q/%q, want f16/f16", k, v)
	}
	if k, v := mcSeatCacheTypes(measureEmbedSeatCmd); k != "" || v != "" {
		t.Errorf("absent cache-type flags must report empty, got %q/%q", k, v)
	}
}

func TestMcFlagValueEqualsSpelling(t *testing.T) {
	tokens := mcSplitCmd(`llama-server.exe --ctx-size=8192 --port=9999`)
	if v, ok := mcFlagValue(tokens, "-c", "--ctx-size"); !ok || v != "8192" {
		t.Errorf("--ctx-size=8192 parsed as %q (ok=%v)", v, ok)
	}
	if n, ok := mcFlagInt(tokens, "--port"); !ok || n != 9999 {
		t.Errorf("--port=9999 parsed as %d (ok=%v)", n, ok)
	}
	if _, ok := mcFlagValue(tokens, "--nope"); ok {
		t.Error("absent flag reported present")
	}
}

// whisper-server seats must be classified non-llama-server so GGUF/ctx/fit
// checks skip them instead of emitting a false positive.
func TestMcIsLlamaServerWhisperEscapeHatch(t *testing.T) {
	if !mcIsLlamaServer(measureRealSeatCmd) {
		t.Error("llama-server.exe not recognized")
	}
	if !mcIsLlamaServer(`/usr/local/bin/llama-server -m model.gguf`) {
		t.Error("posix llama-server not recognized")
	}
	whisper := `C:/whisper.cpp/whisper-server.exe --model V:/models/whisper/ggml-large-v3-turbo.bin --port 9301`
	if mcIsLlamaServer(whisper) {
		t.Error("whisper-server misclassified as llama-server: GGUF/ctx checks would false-positive on a .bin")
	}
}

func TestMcResolveAliasIsCaseInsensitiveAndAliasAware(t *testing.T) {
	roster := []mcRosterEntry{{ID: "embeddinggemma"}, {ID: "bge-reranker-v2-m3"}}
	roster[0].Meta.Llamaswap.Aliases = []string{"text-embedding", "local-embed"}
	roster[1].Meta.Llamaswap.Aliases = []string{"reranker-v2-m3", "v0.12-reranker"}

	for _, in := range []string{"embeddinggemma", "EmbeddingGemma", "text-embedding", "local-embed"} {
		got, ok := mcResolveAlias(roster, in)
		if !ok || got != "embeddinggemma" {
			t.Errorf("resolve(%q) = %q, ok=%v", in, got, ok)
		}
	}
	if got, ok := mcResolveAlias(roster, "v0.12-reranker"); !ok || got != "bge-reranker-v2-m3" {
		t.Errorf("alias resolve = %q, ok=%v", got, ok)
	}
	if got, ok := mcResolveAlias(roster, "not-a-model"); ok || got != "not-a-model" {
		t.Errorf("unknown name should pass through with ok=false, got %q/%v", got, ok)
	}
}

func TestMcLooksLikePath(t *testing.T) {
	paths := []string{"V:/models/x.gguf", `V:\models\x.gguf`, "./x.gguf", "weights.bin"}
	for _, p := range paths {
		if !mcLooksLikePath(p) {
			t.Errorf("%q not recognized as a path", p)
		}
	}
	for _, n := range []string{"gemma-4-e2b", "embeddinggemma", "bge-reranker-v2-m3"} {
		if mcLooksLikePath(n) {
			t.Errorf("%q misread as a path", n)
		}
	}
}

func TestMcSHA256IsStable(t *testing.T) {
	a := mcSHA256(measureRealSeatCmd)
	if a != mcSHA256(measureRealSeatCmd) {
		t.Fatal("config sha is not deterministic")
	}
	if a == mcSHA256(measureRealSeatCmd+" ") {
		t.Fatal("config sha ignores a trailing change; a bench row could join to the wrong config")
	}
	if len(a) != 64 {
		t.Errorf("sha length = %d", len(a))
	}
}

func TestMedianEvenAndOdd(t *testing.T) {
	if got := median([]float64{3, 1, 2}); got != 2 {
		t.Errorf("median(odd) = %v, want 2", got)
	}
	if got := median([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Errorf("median(even) = %v, want 2.5", got)
	}
	if got := median(nil); got != 0 {
		t.Errorf("median(nil) = %v, want 0", got)
	}
	// Median must not reorder the caller's slice.
	in := []float64{3, 1, 2}
	_ = median(in)
	if in[0] != 3 {
		t.Errorf("median mutated its input: %v", in)
	}
}

func TestBuildBinaryOfAndSortedKeys(t *testing.T) {
	if got := buildBinaryOf(measureRealSeatCmd); got != "C:/llama.cpp-b10356/llama-server.exe" {
		t.Errorf("binary = %q", got)
	}
	got := sortedKeys(map[string]bool{"b": true, "a": true, "": true})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("sortedKeys = %v (empty key must be dropped)", got)
	}
}
