// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package mirror_test

import (
	"os"
	"path/filepath"
	"testing"

	"llamaswap-pp-cli/internal/mirror"
)

// yamlFixture mirrors the real llama-swap config's shape: quoted model keys,
// flow-sequence aliases, trailing comments on the ttl line, a block scalar, and
// a whisper seat whose cmd is not llama-server.
const yamlFixture = `
healthCheckTimeout: 300
startPort: 9200

macros:
  server: "C:/llama.cpp/llama-server.exe --port ${PORT} --host 127.0.0.1"

models:
  # a swappable seat
  "gemma-4-e4b":
    cmd: "${server} -m V:/models/gemma-4-e4b.gguf --ctx-size 8192"
    aliases: ["offload-e4b"]
    ttl: 300
    name: "Gemma 4 E4B"

  "embeddinggemma":
    cmd: "${server} -m V:/models/embeddinggemma.gguf --embeddings --pooling mean"
    aliases: ["text-embedding", "local-embed"]
    ttl: -1                 # never auto-unload — mem0 needs it up
    name: "EmbeddingGemma-300m — mem0 embedder"

  "bge-reranker-v2-m3":
    cmd: |
      C:/llama.cpp/llama-server.exe --port 9200
        --reranking --pooling rank
    aliases:
      - reranker-v2-m3
      - v0.12-reranker
    ttl: -1

  "whisper-stt":
    cmd: "C:/whisper.cpp/whisper-server.exe -m V:/models/ggml-large-v3-turbo.bin"
    ttl: 300
    aliases: ["whisper", "stt"]

groups:
  exclusive: true
`

func writeYAML(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(path, []byte(yamlFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestParseYAMLSeats(t *testing.T) {
	seats, err := mirror.ParseYAMLSeats(writeYAML(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byID := map[string]mirror.YAMLSeat{}
	for _, s := range seats {
		byID[s.ID] = s
	}
	if len(byID) != 4 {
		t.Fatalf("parsed %d seats, want 4: %+v", len(byID), byID)
	}
	emb := byID["embeddinggemma"]
	if emb.TTL != -1 || !emb.Resident() {
		t.Fatalf("embeddinggemma ttl=%d resident=%v, want -1/true", emb.TTL, emb.Resident())
	}
	if len(emb.Aliases) != 2 || emb.Aliases[0] != "text-embedding" || emb.Aliases[1] != "local-embed" {
		t.Fatalf("embeddinggemma aliases = %v", emb.Aliases)
	}
	rer := byID["bge-reranker-v2-m3"]
	if len(rer.Aliases) != 2 || rer.Aliases[0] != "reranker-v2-m3" {
		t.Fatalf("block-sequence aliases = %v", rer.Aliases)
	}
	if !rer.Resident() {
		t.Fatal("bge-reranker-v2-m3 must be resident (ttl:-1) even with a block-scalar cmd above it")
	}
	if byID["gemma-4-e4b"].Resident() {
		t.Fatal("a ttl:300 seat must NOT be in the keep-set")
	}
}

// TestKeepSetMatchesAliases is the refusal contract: the mem0 stack is routinely
// addressed by alias, and an id-only keep-set would let `unload local-embed`
// through.
func TestKeepSetMatchesAliases(t *testing.T) {
	t.Setenv(mirror.EnvYAMLPath, writeYAML(t))
	t.Setenv(mirror.EnvKeepSet, "")
	ks := mirror.LoadKeepSet(mirror.KeepSetOptions{})
	if len(ks.Members) != 2 {
		t.Fatalf("keep-set members = %+v, want embeddinggemma + bge-reranker-v2-m3", ks.Members)
	}
	for _, name := range []string{
		"embeddinggemma", "text-embedding", "local-embed",
		"bge-reranker-v2-m3", "reranker-v2-m3", "v0.12-reranker",
		"LOCAL-EMBED", // case-insensitive
	} {
		if _, ok := ks.Match(name); !ok {
			t.Fatalf("keep-set must match %q", name)
		}
	}
	for _, name := range []string{"gemma-4-e4b", "offload-e4b", "whisper", "nonexistent"} {
		if _, ok := ks.Match(name); ok {
			t.Fatalf("keep-set must NOT match %q", name)
		}
	}
}

func TestKeepSetUnionsEnvAndFlag(t *testing.T) {
	t.Setenv(mirror.EnvYAMLPath, writeYAML(t))
	t.Setenv(mirror.EnvKeepSet, "whisper-stt")
	ks := mirror.LoadKeepSet(mirror.KeepSetOptions{Extra: []string{"gemma-4-e4b"}})
	if _, ok := ks.Match("whisper-stt"); !ok {
		t.Fatal("env keep-set entry must apply")
	}
	if _, ok := ks.Match("gemma-4-e4b"); !ok {
		t.Fatal("flag keep-set entry must apply")
	}
}

// TestKeepSetNeverReadsServerTTL documents the binding rule as a test: with no
// YAML and no config, the keep-set is EMPTY and warns, rather than silently
// falling back to the server's (known-wrong) ttl field.
func TestKeepSetIsEmptyAndWarnsWithoutAConfiguredSource(t *testing.T) {
	t.Setenv(mirror.EnvYAMLPath, filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv(mirror.EnvKeepSet, "")
	ks := mirror.LoadKeepSet(mirror.KeepSetOptions{})
	if !ks.Empty() {
		t.Fatalf("expected an empty keep-set, got %+v", ks.Members)
	}
	if len(ks.Warnings) == 0 {
		t.Fatal("an unreadable keep-set source must produce a warning, not silence")
	}
}
