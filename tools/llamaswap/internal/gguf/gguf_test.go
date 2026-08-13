// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package gguf

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// Real model files on this box. The header values below were read out of
// the files themselves with an independent (Python) GGUF parser before this
// reader existed, so the assertions are ground truth, not this package's own
// output fed back to itself. Tests skip when the model volume is absent so
// the suite still passes on a machine without V:.
const modelsRoot = `V:\models`

func requireModel(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join(modelsRoot, rel)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("model file not present on this host: %s", p)
	}
	return p
}

func TestReadGemma4E4B(t *testing.T) {
	path := requireModel(t, `gemma-4-E4B-it-qat-GGUF\gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf`)
	r, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !r.IsGGUF {
		t.Fatalf("IsGGUF=false, reason=%s", r.NotGGUFReason)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"version", r.Version, uint32(3)},
		{"tensor_count", r.TensorCount, uint64(666)},
		{"kv_count", r.KVCount, uint64(47)},
		{"architecture", r.Architecture, "gemma4"},
		{"block_count", r.BlockCount, 42},
		{"context_length", r.ContextLength, 131072},
		{"embedding_length", r.EmbeddingLength, 2560},
		{"head_count", r.HeadCount, 8},
		// The whole point of the reader: GQA. 8 heads but only 2 KV heads
		// means the naive n_heads KV formula overestimates by 4x.
		{"head_count_kv", r.HeadCountKV, 2},
		{"head_count_kv_source", r.HeadCountKVSource, "kv"},
		{"key_length", r.KeyLength, 512},
		{"value_length", r.ValueLength, 512},
		{"length_source", r.LengthSource, "kv:attention.key_length/value_length"},
		{"sliding_window", r.SlidingWindow, 512},
		{"shared_kv_layers", r.SharedKVLayers, 18},
		{"key_length_swa", r.KeyLengthSWA, 256},
		{"file_type", r.FileType, 2},
		{"quantization", r.Quantization, "Q4_0"},
		{"file_size_bytes", r.FileSizeBytes, int64(4215693760)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if r.HeadCountKV == r.HeadCount {
		t.Errorf("expected GQA (head_count_kv != head_count), got both = %d", r.HeadCount)
	}
	if got := len(r.SlidingWindowPattern); got != 42 {
		t.Fatalf("sliding_window_pattern length = %d, want 42", got)
	}
	// Pattern is 5 SWA layers then 1 full-attention layer, repeating.
	for i, swa := range r.SlidingWindowPattern {
		want := (i+1)%6 != 0
		if swa != want {
			t.Fatalf("sliding_window_pattern[%d] = %v, want %v", i, swa, want)
		}
	}
	// Header + metadata only: the reader must not have walked the 4 GB body.
	if r.MetadataBytes >= r.FileSizeBytes/10 {
		t.Errorf("metadata_bytes = %d for a %d byte file; reader read too much", r.MetadataBytes, r.FileSizeBytes)
	}
}

func TestReadEmbeddingGemma(t *testing.T) {
	path := requireModel(t, `embeddinggemma-300M-Q8_0.gguf`)
	r, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !r.IsGGUF {
		t.Fatalf("IsGGUF=false, reason=%s", r.NotGGUFReason)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"architecture", r.Architecture, "gemma-embedding"},
		{"tensor_count", r.TensorCount, uint64(316)},
		{"block_count", r.BlockCount, 24},
		{"context_length", r.ContextLength, 2048},
		{"embedding_length", r.EmbeddingLength, 768},
		{"head_count", r.HeadCount, 3},
		{"head_count_kv", r.HeadCountKV, 1},
		{"head_count_kv_source", r.HeadCountKVSource, "kv"},
		{"key_length", r.KeyLength, 256},
		{"file_type", r.FileType, 7},
		// llama-server's /props reports model_ftype "Q8_0" for this file:
		// the ftype table is validated against the server, not invented.
		{"quantization", r.Quantization, "Q8_0"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if r.HeadCountKV >= r.HeadCount {
		t.Errorf("expected GQA (1 KV head vs 3 heads), got kv=%d heads=%d", r.HeadCountKV, r.HeadCount)
	}
	// 768/3 = 256 coincides with the declared key_length here; assert the
	// source is still the declared key, not the derivation.
	if r.LengthSource != "kv:attention.key_length/value_length" {
		t.Errorf("length_source = %q, want the declared key", r.LengthSource)
	}
}

// A model with no attention.head_count_kv key must fall back to head_count
// explicitly (MHA), never to zero and never silently.
func TestReadBertRerankerFallsBackToMHA(t *testing.T) {
	path := requireModel(t, `bge-reranker-v2-m3-Q4_K_M.gguf`)
	r, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.Architecture != "bert" {
		t.Fatalf("architecture = %q, want bert", r.Architecture)
	}
	if r.HeadCount != 16 || r.HeadCountKV != 16 {
		t.Errorf("head_count/head_count_kv = %d/%d, want 16/16", r.HeadCount, r.HeadCountKV)
	}
	if r.HeadCountKVSource == "kv" {
		t.Errorf("head_count_kv_source = %q, but this file declares no GQA key", r.HeadCountKVSource)
	}
	if r.Quantization != "Q4_K_M" {
		t.Errorf("quantization = %q, want Q4_K_M", r.Quantization)
	}
	if r.LengthSource != "derived:embedding_length/head_count" {
		t.Errorf("length_source = %q, want the derivation (bert declares no key_length)", r.LengthSource)
	}
	if r.KeyLength != 64 {
		t.Errorf("key_length = %d, want 1024/16 = 64", r.KeyLength)
	}
}

// whisper.cpp weights are ggml containers, not GGUF. They must classify as
// a typed non-GGUF result, not an error and not a fabricated header.
func TestReadWhisperBinIsTypedNotGGUF(t *testing.T) {
	path := requireModel(t, `whisper\ggml-large-v3-turbo.bin`)
	r, err := Read(path)
	if err != nil {
		t.Fatalf("Read returned an error for a non-GGUF file; want a typed result: %v", err)
	}
	if r.IsGGUF {
		t.Fatalf("whisper .bin classified as GGUF")
	}
	if r.NotGGUFReason == "" {
		t.Errorf("NotGGUFReason empty; caller cannot explain the skip")
	}
	if r.FileSizeBytes <= 0 {
		t.Errorf("file size not reported for a non-GGUF file")
	}
	if got := KnownNonGGUF(r.MagicSeen); got == "" {
		t.Errorf("KnownNonGGUF(%q) returned no label for a ggml container", r.MagicSeen)
	}
}

func TestReadNonGGUFTempFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "not-a-model.txt")
	if err := os.WriteFile(p, []byte("hello world, definitely not a model"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Read(p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.IsGGUF {
		t.Fatalf("text file classified as GGUF")
	}
	if r.NotGGUFReason == "" {
		t.Errorf("expected a reason")
	}
}

func TestReadTruncatedHeader(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "truncated.gguf")
	buf := make([]byte, 0, 24)
	buf = append(buf, []byte("GGUF")...)
	buf = binary.LittleEndian.AppendUint32(buf, 3)
	buf = binary.LittleEndian.AppendUint64(buf, 10) // tensors
	buf = binary.LittleEndian.AppendUint64(buf, 5)  // 5 KVs that are not there
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(p); err == nil {
		t.Fatalf("expected a truncation error")
	}
}

func TestFileTypeNameUnknownStaysVisible(t *testing.T) {
	if got := FileTypeName(9999); got != "unknown-ftype-9999" {
		t.Errorf("FileTypeName(9999) = %q", got)
	}
	if got := FileTypeName(15); got != "Q4_K_M" {
		t.Errorf("FileTypeName(15) = %q", got)
	}
}
