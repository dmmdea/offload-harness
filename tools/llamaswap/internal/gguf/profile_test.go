// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package gguf

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustContain(t *testing.T, label, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("%s does not mention %q\ngot: %s", label, n, haystack)
		}
	}
}

// ---------------------------------------------------------------------------
// file_type GUESSED bit
// ---------------------------------------------------------------------------

func TestFileTypeGuessedBitIsMaskedAndLabeled(t *testing.T) {
	cases := []struct {
		ft   int
		want string
	}{
		{7, "Q8_0"},
		{FileTypeGuessedBit | 7, "Q8_0 (guessed)"},
		{15, "Q4_K_M"},
		{FileTypeGuessedBit | 15, "Q4_K_M (guessed)"},
		// A value with the bit set but an unknown base must still show the
		// MASKED number, not the raw one: 1999 is not a quantization type.
		{FileTypeGuessedBit | 975, "unknown-ftype-975 (guessed)"},
		{999, "unknown-ftype-999"},
	}
	for _, c := range cases {
		if got := FileTypeName(c.ft); got != c.want {
			t.Errorf("FileTypeName(%d) = %q, want %q", c.ft, got, c.want)
		}
	}
	if base, guessed := SplitFileType(FileTypeGuessedBit | 7); base != 7 || !guessed {
		t.Errorf("SplitFileType(1031) = %d,%v; want 7,true", base, guessed)
	}
	if base, guessed := SplitFileType(7); base != 7 || guessed {
		t.Errorf("SplitFileType(7) = %d,%v; want 7,false", base, guessed)
	}
}

// The enum refresh: values that only exist on current llama.cpp master must
// resolve, and the deliberate gaps must stay unknown.
func TestFileTypeEnumMatchesCurrentLlamaH(t *testing.T) {
	for ft, want := range map[int]string{
		38: "MXFP4_MOE",
		39: "NVFP4",
		40: "Q1_0",
		41: "Q2_0",
		32: "BF16",
		36: "TQ1_0",
	} {
		if got := FileTypeName(ft); got != want {
			t.Errorf("FileTypeName(%d) = %q, want %q", ft, got, want)
		}
	}
	// Removed formats keep their gaps so a stale file is reported honestly.
	for _, gap := range []int{4, 5, 6, 33, 34, 35} {
		if got := FileTypeName(gap); !strings.HasPrefix(got, "unknown-ftype-") {
			t.Errorf("FileTypeName(%d) = %q; removed formats must stay unknown", gap, got)
		}
	}
}

func TestFileTypeReportedOnHeader(t *testing.T) {
	g := base("llama")
	g.U32("general.file_type", uint32(FileTypeGuessedBit|7))
	g.Tensor("token_embd.weight", 8, 512, 256)
	r := g.read(t, "guessed.gguf")
	if r.FileType != FileTypeGuessedBit|7 {
		t.Errorf("FileType = %d, want the raw value preserved", r.FileType)
	}
	if r.FileTypeBase != 7 || !r.FileTypeGuessed {
		t.Errorf("FileTypeBase/Guessed = %d/%v, want 7/true", r.FileTypeBase, r.FileTypeGuessed)
	}
	if r.Quantization != "Q8_0 (guessed)" {
		t.Errorf("Quantization = %q", r.Quantization)
	}
}

// ---------------------------------------------------------------------------
// shards
// ---------------------------------------------------------------------------

func TestShardHeaderSaysItIsOnlyOneShard(t *testing.T) {
	g := base("llama")
	g.U16("split.no", 0)
	g.U16("split.count", 3)
	g.U32("split.tensors.count", 900)
	g.Tensor("token_embd.weight", 8, 512, 256)
	dir := t.TempDir()
	p := g.write(t, filepath.Join(dir, "model-00001-of-00003.gguf"))
	r, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Split == nil {
		t.Fatal("Split is nil for a file declaring split.count=3")
	}
	if !r.Split.IsShard() {
		t.Fatal("IsShard() = false for split.count=3")
	}
	if r.Split.HumanIndex != 1 {
		t.Errorf("HumanIndex = %d; split.no is 0-based so shard 1 is expected", r.Split.HumanIndex)
	}
	if r.Split.TensorsTotal != 900 {
		t.Errorf("TensorsTotal = %d, want 900", r.Split.TensorsTotal)
	}
	if r.Split.Disagreement != "" {
		t.Errorf("unexpected disagreement: %s", r.Split.Disagreement)
	}
	mustContain(t, "shard summary", r.Split.Summary, "shard 1 of 3", "THIS SHARD ONLY", "summed")
}

// The off-by-one that this metadata invites: a writer that emits a 1-based
// split.no disagrees with the filename, and the reader must SAY so instead of
// silently picking a side.
func TestShardMetadataFilenameDisagreementIsReported(t *testing.T) {
	g := base("llama")
	g.U16("split.no", 2) // 0-based would mean shard 3
	g.U16("split.count", 3)
	g.Tensor("token_embd.weight", 8, 512, 256)
	dir := t.TempDir()
	p := g.write(t, filepath.Join(dir, "model-00001-of-00003.gguf"))
	r, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Split.Disagreement == "" {
		t.Fatal("filename says shard 1, split.no says shard 3, and nothing was reported")
	}
	mustContain(t, "disagreement", r.Split.Disagreement, "split.no=2", "filename says shard 1", "neither is asserted")
}

func TestResolveShardsSumsEveryShard(t *testing.T) {
	dir := t.TempDir()
	var want int64
	for i := 1; i <= 3; i++ {
		g := base("llama")
		g.U16("split.no", uint16(i-1))
		g.U16("split.count", 3)
		g.Tensor("blk.0.attn_q.weight", 8, 512, 512)
		p := g.write(t, filepath.Join(dir, fmt.Sprintf("m-%05d-of-%05d.gguf", i, 3)))
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want += st.Size()
	}
	set, err := ResolveShards(filepath.Join(dir, "m-00001-of-00003.gguf"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Complete {
		t.Fatalf("complete 3-shard set reported incomplete: missing %v", set.Missing)
	}
	if set.TotalBytes != want {
		t.Errorf("TotalBytes = %d, want the sum of all three shards %d", set.TotalBytes, want)
	}
	if len(set.Shards) != 3 {
		t.Errorf("resolved %d shards, want 3", len(set.Shards))
	}
	// The whole point: the set is bigger than the member it was resolved from.
	if st, err := os.Stat(set.Shards[0].Path); err == nil && set.TotalBytes <= st.Size() {
		t.Errorf("summed set (%d) is not larger than shard 1 (%d)", set.TotalBytes, st.Size())
	}
}

func TestResolveShardsNamesMissingMembersInsteadOfSumming(t *testing.T) {
	dir := t.TempDir()
	g := base("llama")
	g.U16("split.count", 3)
	g.Tensor("blk.0.attn_q.weight", 8, 512, 512)
	g.write(t, filepath.Join(dir, "m-00001-of-00003.gguf"))
	set, err := ResolveShards(filepath.Join(dir, "m-00001-of-00003.gguf"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if set.Complete {
		t.Fatal("a set with 2 of 3 shards absent was reported complete")
	}
	if len(set.Missing) != 2 || set.Missing[0] != 2 || set.Missing[1] != 3 {
		t.Errorf("Missing = %v, want [2 3]", set.Missing)
	}
}

func TestResolveShardsRejectsUnshardedFile(t *testing.T) {
	if _, err := ResolveShards("V:/models/plain.gguf", 1); err == nil {
		t.Fatal("expected an error for a single-file model")
	}
}

// ---------------------------------------------------------------------------
// MoE
// ---------------------------------------------------------------------------

func TestMoEReportsTotalAndActiveSeparately(t *testing.T) {
	g := base("qwen3moe")
	g.U32("qwen3moe.expert_count", 8)
	g.U32("qwen3moe.expert_used_count", 2)
	// One dense tensor plus one stacked expert tensor per role.
	g.Tensor("token_embd.weight", 8, 1000, 512)             // 512,000 dense
	g.Tensor("blk.0.ffn_gate_exps.weight", 8, 8, 512, 1024) // 4,194,304 expert
	g.Tensor("blk.0.ffn_up_exps.weight", 8, 8, 512, 1024)   // 4,194,304 expert
	g.Tensor("blk.0.ffn_down_exps.weight", 8, 8, 1024, 512) // 4,194,304 expert
	r := g.read(t, "moe.gguf")

	m := r.MoE
	if m == nil {
		t.Fatal("MoE is nil for a file declaring expert_count=8")
	}
	if m.ExpertCount != 8 || m.ExpertUsedCount != 2 {
		t.Fatalf("expert_count/used = %d/%d, want 8/2", m.ExpertCount, m.ExpertUsedCount)
	}
	if m.ExpertTensors != 3 {
		t.Errorf("ExpertTensors = %d, want the 3 *_exps tensors", m.ExpertTensors)
	}
	const dense, expert = 512_000, 3 * 4_194_304
	if m.ParamsTotal != dense+expert {
		t.Errorf("ParamsTotal = %d, want %d", m.ParamsTotal, dense+expert)
	}
	if m.ParamsExpert != expert {
		t.Errorf("ParamsExpert = %d, want %d", m.ParamsExpert, expert)
	}
	// Active = dense + 2/8 of the expert weights. The two numbers must differ:
	// reporting one of them as "the" parameter count is the bug.
	wantActive := uint64(dense) + uint64(expert)*2/8
	if m.ParamsActive != wantActive {
		t.Errorf("ParamsActive = %d, want %d", m.ParamsActive, wantActive)
	}
	if m.ParamsActive >= m.ParamsTotal {
		t.Errorf("active (%d) must be below total (%d) for a 2-of-8 router", m.ParamsActive, m.ParamsTotal)
	}
	mustContain(t, "active source", m.ActiveSource, "tensor-classified", "_exps", "2 of 8 experts")
}

// No *_exps tensors means the expert share is unknown. The reader must say
// so and leave ParamsActive at zero rather than quoting a ratio estimate as
// if it were measured.
func TestMoEWithoutExpertTensorsRefusesToInventActive(t *testing.T) {
	g := base("qwen3moe")
	g.U32("qwen3moe.expert_count", 8)
	g.U32("qwen3moe.expert_used_count", 2)
	g.Tensor("token_embd.weight", 8, 1000, 512)
	r := g.read(t, "moe-noexps.gguf")
	if r.MoE.ParamsActive != 0 {
		t.Errorf("ParamsActive = %d; with no expert tensors it must not be invented", r.MoE.ParamsActive)
	}
	mustContain(t, "active source", r.MoE.ActiveSource, "ESTIMATE", "not reported rather than guessed")
}

func TestNonMoEHasNoMoEBlock(t *testing.T) {
	g := base("llama")
	g.Tensor("token_embd.weight", 8, 512, 256)
	if r := g.read(t, "dense.gguf"); r.MoE != nil {
		t.Errorf("MoE reported for a dense model: %+v", r.MoE)
	}
}

// ---------------------------------------------------------------------------
// RoPE scaling / native vs usable context
// ---------------------------------------------------------------------------

func TestYaRNSeparatesNativeFromDeclaredContext(t *testing.T) {
	g := &ggufBuilder{}
	g.Str("general.architecture", "llama")
	g.U32("llama.block_count", 4)
	g.U32("llama.context_length", 131072)
	g.Str("llama.rope.scaling.type", "yarn")
	g.F32("llama.rope.scaling.factor", 16)
	g.U32("llama.rope.scaling.original_context_length", 8192)
	g.F32("llama.rope.scaling.attn_factor", 1)
	g.Bool("llama.rope.scaling.finetuned", true)
	g.Tensor("token_embd.weight", 8, 512, 256)
	r := g.read(t, "yarn.gguf")

	if r.RoPE == nil {
		t.Fatal("RoPE is nil for a file declaring rope.scaling.type")
	}
	if r.RoPE.Type != "yarn" || r.RoPE.Factor != 16 || r.RoPE.OriginalContext != 8192 {
		t.Fatalf("RoPE = %+v", r.RoPE)
	}
	if !r.RoPE.Finetuned {
		t.Error("finetuned flag was dropped")
	}
	native, declared, note := r.NativeContext()
	if native != 8192 {
		t.Errorf("native = %d, want the 8192 training window", native)
	}
	if declared != 131072 {
		t.Errorf("declared = %d, want the 131072 context_length", declared)
	}
	mustContain(t, "context note", note, "trained at 8192", "131072", "yarn")
}

// A model with no scaling keys must report native == declared and no note:
// inventing an extension story for an unextended model is the mirror bug.
func TestNoRoPEScalingMeansNativeEqualsDeclared(t *testing.T) {
	g := base("llama")
	g.Tensor("token_embd.weight", 8, 512, 256)
	r := g.read(t, "norope.gguf")
	native, declared, note := r.NativeContext()
	if native != declared || native != 4096 {
		t.Errorf("native/declared = %d/%d, want 4096/4096", native, declared)
	}
	if note != "" {
		t.Errorf("unexpected note for an unscaled model: %s", note)
	}
}

func TestRoPEFactorWithoutOriginalContextIsNotResolvedIntoANumber(t *testing.T) {
	g := &ggufBuilder{}
	g.Str("general.architecture", "llama")
	g.U32("llama.context_length", 32768)
	g.Str("llama.rope.scaling.type", "linear")
	g.F32("llama.rope.scaling.factor", 4)
	g.Tensor("token_embd.weight", 8, 512, 256)
	r := g.read(t, "rope-nofactor.gguf")
	native, declared, note := r.NativeContext()
	if native != declared {
		t.Errorf("native = %d; without original_context_length the pre-extension window is unknown and must not be divided out", native)
	}
	mustContain(t, "note", note, "original_context_length is absent", "unknown")
}

// ---------------------------------------------------------------------------
// MLA / SSM detection guards
// ---------------------------------------------------------------------------

func TestMLAKeysMarkKVMathUnsupported(t *testing.T) {
	g := base("deepseek2")
	g.U32("deepseek2.attention.kv_lora_rank", 512)
	g.U32("deepseek2.attention.key_length_mla", 576)
	g.Tensor("token_embd.weight", 8, 512, 256)
	r := g.read(t, "mla.gguf")
	if r.UnsupportedKVArch == "" {
		t.Fatal("kv_lora_rank present and no unsupported-KV marker was set")
	}
	mustContain(t, "unsupported arch", r.UnsupportedKVArch, "MLA", "latent")
	if len(r.UnsupportedKVKeys) != 2 {
		t.Errorf("UnsupportedKVKeys = %v, want both MLA keys named", r.UnsupportedKVKeys)
	}
}

func TestSSMKeysMarkKVMathUnsupported(t *testing.T) {
	g := base("mamba")
	g.U32("mamba.ssm.state_size", 16)
	g.U32("mamba.ssm.conv_kernel", 4)
	g.U32("mamba.ssm.inner_size", 1024)
	g.Tensor("token_embd.weight", 8, 512, 256)
	r := g.read(t, "ssm.gguf")
	mustContain(t, "unsupported arch", r.UnsupportedKVArch, "SSM", "recurrent state")
	if len(r.UnsupportedKVKeys) != 3 {
		t.Errorf("UnsupportedKVKeys = %v, want all three ssm.* keys", r.UnsupportedKVKeys)
	}
}

func TestStandardAttentionIsNotFlaggedUnsupported(t *testing.T) {
	g := base("llama")
	g.Tensor("token_embd.weight", 8, 512, 256)
	if r := g.read(t, "plain.gguf"); r.UnsupportedKVArch != "" {
		t.Errorf("plain GQA model flagged as %q", r.UnsupportedKVArch)
	}
}

// ---------------------------------------------------------------------------
// general.type and pooling
// ---------------------------------------------------------------------------

func TestNonModelGGUFsAreIdentified(t *testing.T) {
	for kind, needle := range map[string]string{
		"adapter": "LoRA",
		"imatrix": "importance matrix",
		"mmproj":  "projector",
	} {
		g := base("llama")
		g.Str("general.type", kind)
		g.Tensor("token_embd.weight", 8, 512, 256)
		r := g.read(t, kind+".gguf")
		if r.IsModel {
			t.Errorf("general.type=%q classified as a model", kind)
		}
		mustContain(t, kind+" reason", r.NotAModelReason, kind, needle)
	}
}

func TestModelTypeAndAbsentTypeBothCountAsModels(t *testing.T) {
	g := base("llama")
	g.Str("general.type", "model")
	g.Tensor("token_embd.weight", 8, 512, 256)
	if r := g.read(t, "typed.gguf"); !r.IsModel {
		t.Error(`general.type="model" was not treated as a model`)
	}
	g2 := base("llama")
	g2.Tensor("token_embd.weight", 8, 512, 256)
	r := g2.read(t, "untyped.gguf")
	if !r.IsModel || r.NotAModelReason != "" {
		t.Errorf("absent general.type must default to model; got IsModel=%v reason=%q", r.IsModel, r.NotAModelReason)
	}
}

func TestPoolingRankIdentifiesAReranker(t *testing.T) {
	g := base("bert")
	g.U32("bert.pooling_type", 4)
	g.Tensor("token_embd.weight", 8, 512, 256)
	r := g.read(t, "rank.gguf")
	if r.PoolingType == nil || *r.PoolingType != 4 {
		t.Fatalf("PoolingType = %v, want 4", r.PoolingType)
	}
	if r.PoolingTypeName != "RANK" {
		t.Errorf("PoolingTypeName = %q, want RANK", r.PoolingTypeName)
	}
	mustContain(t, "pooling role", r.PoolingRole, "reranker", "/v1/rerank")
}

// pooling_type 0 is NONE, a real value. A reader that used the zero value as
// "absent" would erase it.
func TestPoolingNoneIsDistinctFromAbsent(t *testing.T) {
	g := base("bert")
	g.U32("bert.pooling_type", 0)
	g.Tensor("token_embd.weight", 8, 512, 256)
	r := g.read(t, "poolnone.gguf")
	if r.PoolingType == nil || *r.PoolingType != 0 || r.PoolingTypeName != "NONE" {
		t.Fatalf("pooling NONE lost: PoolingType=%v name=%q", r.PoolingType, r.PoolingTypeName)
	}
	g2 := base("bert")
	g2.Tensor("token_embd.weight", 8, 512, 256)
	if r2 := g2.read(t, "poolabsent.gguf"); r2.PoolingType != nil {
		t.Errorf("absent pooling_type produced %v", *r2.PoolingType)
	}
}

// ---------------------------------------------------------------------------
// BPW histogram
// ---------------------------------------------------------------------------

func TestQuantProfileComputesBPWFromTheTensorTable(t *testing.T) {
	g := base("llama")
	g.U32("general.file_type", 15) // Q4_K_M
	// 262,144 Q4_K elements = 1024 blocks x 144 B = 147,456 B
	g.Tensor("blk.0.ffn_down.weight", 12, 512, 512)
	// 65,536 Q6_K elements = 256 blocks x 210 B = 53,760 B
	g.Tensor("blk.0.attn_q.weight", 14, 256, 256)
	// 512 F32 elements = 2,048 B
	g.Tensor("blk.0.attn_norm.weight", 0, 512)
	r := g.read(t, "bpw.gguf")

	q := r.Quant
	if q == nil {
		t.Fatal("Quant profile is nil")
	}
	const wantBytes = 147456 + 53760 + 2048
	const wantElems = 262144 + 65536 + 512
	if q.Bytes != wantBytes {
		t.Errorf("Bytes = %d, want %d", q.Bytes, wantBytes)
	}
	if q.Elements != wantElems {
		t.Errorf("Elements = %d, want %d", q.Elements, wantElems)
	}
	wantBPW := float64(wantBytes) * 8 / float64(wantElems)
	if math.Abs(q.BitsPerWeight-wantBPW) > 1e-9 {
		t.Errorf("BitsPerWeight = %v, want %v", q.BitsPerWeight, wantBPW)
	}
	if q.DominantType != "Q4_K" {
		t.Errorf("DominantType = %q, want Q4_K (the largest share of bytes)", q.DominantType)
	}
	if len(q.Types) != 3 {
		t.Fatalf("Types = %d entries, want 3", len(q.Types))
	}
	if q.Types[0].Type != "Q4_K" || q.Types[0].Tensors != 1 {
		t.Errorf("types are not sorted by byte share: %+v", q.Types)
	}
	if q.LabelMismatch {
		t.Errorf("Q4_K_M with Q4_K tensors flagged as mislabeled: %s", q.MismatchNote)
	}
}

// The reason the histogram exists: a file can declare one quantization and
// store another. The claim is only made when the declared type is entirely
// absent, so the mixed *_K_M / UD-*_XL quants that legitimately blend types
// never trip it.
func TestQuantProfileCatchesAMislabeledQuant(t *testing.T) {
	g := base("llama")
	g.U32("general.file_type", 7) // claims Q8_0
	g.Tensor("blk.0.ffn_down.weight", 12, 512, 512)
	g.Tensor("blk.0.attn_q.weight", 14, 256, 256)
	r := g.read(t, "mislabeled.gguf")
	if !r.Quant.LabelMismatch {
		t.Fatal("a Q8_0-labelled file with zero Q8_0 tensors was not flagged")
	}
	mustContain(t, "mismatch note", r.Quant.MismatchNote,
		"Q8_0", "NO Q8_0 tensor is stored", "Trust the tensor table")
}

func TestQuantProfileDoesNotClaimMismatchForUncheckableFtypes(t *testing.T) {
	g := base("llama")
	g.U32("general.file_type", 27) // IQ3_M: ftype->ggml type is not one-to-one
	g.Tensor("blk.0.ffn_down.weight", 12, 512, 512)
	r := g.read(t, "iq3m.gguf")
	if r.Quant.LabelMismatch {
		t.Errorf("IQ3_M has no unambiguous ggml counterpart and must not produce a mismatch claim: %s", r.Quant.MismatchNote)
	}
}

func TestUnknownGGMLTypeIsNotSized(t *testing.T) {
	g := base("llama")
	g.Tensor("blk.0.weird.weight", 250, 512, 512)
	r := g.read(t, "weirdtype.gguf")
	if r.Quant.UnsizedTensors != 1 {
		t.Errorf("UnsizedTensors = %d, want 1", r.Quant.UnsizedTensors)
	}
	if len(r.Quant.UnknownTypes) != 1 || !strings.Contains(r.Quant.UnknownTypes[0], "250") {
		t.Errorf("UnknownTypes = %v, want the raw tag visible", r.Quant.UnknownTypes)
	}
	if r.Quant.Bytes != 0 {
		t.Errorf("Bytes = %d; an unsized tensor must contribute nothing rather than a guess", r.Quant.Bytes)
	}
}

// Ground-truth cross-check against real weights: the summed tensor bytes must
// account for nearly all of the file and can never exceed it. This is what
// validates the block-size table transcribed from ggml-common.h.
func TestQuantProfileTotalsAgreeWithRealFileSizes(t *testing.T) {
	for _, rel := range []string{
		`gemma-4-E4B-it-qat-GGUF\gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf`,
		`embeddinggemma-300M-Q8_0.gguf`,
		`bge-reranker-v2-m3-Q4_K_M.gguf`,
	} {
		path := requireModel(t, rel)
		r, err := Read(path)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if r.Quant == nil {
			t.Fatalf("%s: no quant profile", rel)
		}
		header := r.MetadataBytes + r.TensorInfoBytes
		total := int64(r.Quant.Bytes) + header
		if total > r.FileSizeBytes {
			t.Errorf("%s: tensor bytes + header = %d exceeds the %d byte file; the block-size table is wrong",
				rel, total, r.FileSizeBytes)
		}
		if ratio := float64(total) / float64(r.FileSizeBytes); ratio < 0.98 {
			t.Errorf("%s: tensor bytes + header account for only %.1f%% of the file", rel, ratio*100)
		}
		if r.Quant.BitsPerWeight <= 0 || r.Quant.BitsPerWeight > 33 {
			t.Errorf("%s: implausible bits_per_weight %v", rel, r.Quant.BitsPerWeight)
		}
		if int(r.TensorCount) != r.Quant.Tensors {
			t.Errorf("%s: header declares %d tensors, table walked %d", rel, r.TensorCount, r.Quant.Tensors)
		}
	}
}

// The reader's core promise: never walk the tensor body. Adding the
// tensor-info table must not break it.
func TestTensorTableWalkStaysInTheHeader(t *testing.T) {
	path := requireModel(t, `gemma-4-E4B-it-qat-GGUF\gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf`)
	r, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.TensorInfoBytes <= 0 {
		t.Fatal("tensor-info table was not read")
	}
	if read := r.MetadataBytes + r.TensorInfoBytes; read >= r.FileSizeBytes/10 {
		t.Errorf("read %d bytes of a %d byte file; the reader walked into the tensor body", read, r.FileSizeBytes)
	}
}
