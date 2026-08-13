// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package gguf

import (
	"fmt"
	"sort"
	"strings"
)

// SplitInfo describes shard membership. llama.cpp's gguf-split writes
// split.no ZERO-BASED (tools/gguf-split/gguf-split.cpp: i_split starts at 0)
// while the filename it generates is ONE-based
// ("%s-%05d-of-%05d.gguf" formatted with split_no + 1). Both spellings are
// reported here rather than silently reconciled, because picking one and
// being wrong is an off-by-one in an operator-facing sentence.
type SplitInfo struct {
	No           int    `json:"split_no"`
	Count        int    `json:"split_count"`
	TensorsTotal int    `json:"split_tensors_count,omitempty"`
	HumanIndex   int    `json:"shard_index_1_based"`
	Summary      string `json:"summary"`
	// FilenameIndex/FilenameCount are parsed from the -NNNNN-of-NNNNN.gguf
	// filename when it follows the convention; 0 when it does not.
	FilenameIndex int `json:"filename_index,omitempty"`
	FilenameCount int `json:"filename_count,omitempty"`
	// Disagreement is set when metadata and filename tell different stories.
	Disagreement string `json:"disagreement,omitempty"`
}

// IsShard reports whether this file is one piece of a multi-file model.
func (s *SplitInfo) IsShard() bool { return s != nil && s.Count > 1 }

// RoPEScaling is the context-extension metadata. A model whose
// context_length is 131072 while rope.scaling.original_context_length is 8192
// was TRAINED at 8k and reaches 128k only through YaRN; reporting the 128k
// alone as "native context" is the fabrication this struct exists to stop.
type RoPEScaling struct {
	Type              string  `json:"type,omitempty"`
	Factor            float64 `json:"factor,omitempty"`
	OriginalContext   int     `json:"original_context_length,omitempty"`
	AttnFactor        float64 `json:"attn_factor,omitempty"`
	YarnLogMultiplier float64 `json:"yarn_log_multiplier,omitempty"`
	YarnExtFactor     float64 `json:"yarn_ext_factor,omitempty"`
	YarnBetaFast      float64 `json:"yarn_beta_fast,omitempty"`
	YarnBetaSlow      float64 `json:"yarn_beta_slow,omitempty"`
	Finetuned         bool    `json:"finetuned,omitempty"`
	Keys              []string
}

// MoEInfo reports the two parameter counts a mixture-of-experts model has.
// Quoting only one of them is how an MoE gets described as either far larger
// or far cheaper than it is: total drives the weights footprint, active
// drives the compute per token.
type MoEInfo struct {
	ExpertCount     int `json:"expert_count"`
	ExpertUsedCount int `json:"expert_used_count,omitempty"`
	SharedCount     int `json:"expert_shared_count,omitempty"`

	ParamsTotal  uint64 `json:"params_total,omitempty"`
	ParamsExpert uint64 `json:"params_in_expert_tensors,omitempty"`
	ParamsActive uint64 `json:"params_active_per_token,omitempty"`

	ExpertTensors int    `json:"expert_tensor_count,omitempty"`
	ActiveSource  string `json:"active_params_source"`
	Note          string `json:"note,omitempty"`
}

// QuantTypeShare is one ggml type's contribution to the file.
type QuantTypeShare struct {
	Type        string  `json:"type"`
	Tensors     int     `json:"tensors"`
	Elements    uint64  `json:"elements"`
	Bytes       uint64  `json:"bytes"`
	BitsPerElem float64 `json:"bits_per_element"`
	ShareBytes  float64 `json:"share_of_bytes"`
}

// QuantProfile is the measured bits-per-weight breakdown. It is derived from
// the tensor table (type tag + dims per tensor), never from the file-type
// label, which is exactly why it can contradict that label.
type QuantProfile struct {
	Tensors        int              `json:"tensors"`
	Elements       uint64           `json:"elements"`
	Bytes          uint64           `json:"bytes"`
	BitsPerWeight  float64          `json:"bits_per_weight"`
	Types          []QuantTypeShare `json:"types"`
	DominantType   string           `json:"dominant_type,omitempty"`
	DeclaredType   string           `json:"declared_file_type,omitempty"`
	LabelMismatch  bool             `json:"label_mismatch"`
	MismatchNote   string           `json:"label_mismatch_note,omitempty"`
	UnknownTypes   []string         `json:"unknown_types,omitempty"`
	UnsizedTensors int              `json:"unsized_tensors,omitempty"`
	PartialShard   bool             `json:"partial_shard,omitempty"`
}

// poolingTypeNames is llama_pooling_type from include/llama.h.
var poolingTypeNames = map[int]string{
	-1: "UNSPECIFIED",
	0:  "NONE",
	1:  "MEAN",
	2:  "CLS",
	3:  "LAST",
	4:  "RANK",
}

// ggufNonModelTypes are the general.type values that are NOT model weights
// (gguf-py/gguf/constants.py, class GGUFType).
var ggufNonModelTypes = map[string]string{
	"adapter": "a LoRA adapter, not a model: it carries delta weights that are applied ON TOP of a base model",
	"imatrix": "an importance matrix used to guide quantization, not a model: it has no servable weights",
	"mmproj":  "a multimodal projector (vision/audio encoder) paired with a text model, not a servable model on its own",
}

func (res *Result) finishGeneralType() {
	res.GeneralType, _ = res.KV["general.type"].(string)
	res.IsModel = true
	if res.GeneralType == "" {
		// Absent general.type means "model" by convention; the writer only
		// emits the key for the non-model kinds.
		return
	}
	if reason, ok := ggufNonModelTypes[strings.ToLower(res.GeneralType)]; ok {
		res.IsModel = false
		res.NotAModelReason = fmt.Sprintf("general.type=%q: %s", res.GeneralType, reason)
		return
	}
	if !strings.EqualFold(res.GeneralType, "model") {
		res.IsModel = false
		res.NotAModelReason = fmt.Sprintf("general.type=%q is not %q and is not a kind this reader knows; treating it as a non-model rather than guessing", res.GeneralType, "model")
	}
}

func (res *Result) finishPooling(arch string) {
	if arch == "" {
		return
	}
	v, ok := res.Int(arch + ".pooling_type")
	if !ok {
		return
	}
	pt := v
	res.PoolingType = &pt
	name, known := poolingTypeNames[v]
	if !known {
		name = fmt.Sprintf("unknown-pooling-%d", v)
	}
	res.PoolingTypeName = name
	switch v {
	case 4:
		res.PoolingRole = "reranker (RANK pooling attaches a classification head; serve it on /v1/rerank, not /v1/embeddings)"
	case 1, 2, 3:
		res.PoolingRole = "embedding model (" + name + " pooling)"
	}
}

func (res *Result) finishSplit() {
	count, hasCount := res.Int("split.count")
	no, hasNo := res.Int("split.no")
	fIdx, fCount := parseShardFilename(res.Path)
	if !hasCount && !hasNo && fCount == 0 {
		return
	}
	si := &SplitInfo{No: no, Count: count, FilenameIndex: fIdx, FilenameCount: fCount}
	if n, ok := res.Int("split.tensors.count"); ok {
		si.TensorsTotal = n
	}
	if !hasCount && fCount > 0 {
		si.Count = fCount
	}
	si.HumanIndex = si.No + 1
	if !hasNo && fIdx > 0 {
		si.HumanIndex = fIdx
	}
	if hasNo && fIdx > 0 && fIdx != si.No+1 {
		si.Disagreement = fmt.Sprintf(
			"split.no=%d (0-based -> shard %d) but the filename says shard %d; the two disagree, so neither is asserted as the answer",
			si.No, si.No+1, fIdx)
	}
	if hasCount && fCount > 0 && fCount != count {
		if si.Disagreement != "" {
			si.Disagreement += "; "
		}
		si.Disagreement += fmt.Sprintf("split.count=%d but the filename says %d shards", count, fCount)
	}
	switch {
	case si.Count > 1:
		si.Summary = fmt.Sprintf(
			"shard %d of %d - every total in this header (file size, tensor count, parameters) covers THIS SHARD ONLY and reflects the whole model only if summed across all %d shards",
			si.HumanIndex, si.Count, si.Count)
	case si.Count == 1:
		si.Summary = "single-file model (split.count=1)"
	default:
		si.Summary = "split metadata present but split.count is not set; shard membership is undetermined"
	}
	res.Split = si
}

func (res *Result) finishRoPE(arch string) {
	if arch == "" {
		return
	}
	r := &RoPEScaling{}
	get := func(suffix string) (float64, bool) {
		v, ok := res.Float(arch + ".rope.scaling." + suffix)
		if ok {
			r.Keys = append(r.Keys, arch+".rope.scaling."+suffix)
		}
		return v, ok
	}
	if s, ok := res.KV[arch+".rope.scaling.type"].(string); ok {
		r.Type = s
		r.Keys = append(r.Keys, arch+".rope.scaling.type")
	}
	r.Factor, _ = get("factor")
	if v, ok := get("original_context_length"); ok {
		r.OriginalContext = int(v)
	}
	r.AttnFactor, _ = get("attn_factor")
	r.YarnLogMultiplier, _ = get("yarn_log_multiplier")
	r.YarnExtFactor, _ = get("yarn_ext_factor")
	r.YarnBetaFast, _ = get("yarn_beta_fast")
	r.YarnBetaSlow, _ = get("yarn_beta_slow")
	if b, ok := res.KV[arch+".rope.scaling.finetuned"].(bool); ok {
		r.Finetuned = b
		r.Keys = append(r.Keys, arch+".rope.scaling.finetuned")
	}
	if len(r.Keys) == 0 {
		return
	}
	res.RoPE = r
}

// NativeContext returns the context the model was trained at and the context
// it is declared to serve, with a one-line explanation of the difference.
// Both numbers come from the file; neither is inferred.
func (res *Result) NativeContext() (native, declared int, note string) {
	declared = res.ContextLength
	native = declared
	if res.RoPE == nil {
		return native, declared, ""
	}
	if res.RoPE.OriginalContext > 0 {
		native = res.RoPE.OriginalContext
	}
	if native == declared {
		if res.RoPE.Type != "" && res.RoPE.Factor > 1 {
			return native, declared, fmt.Sprintf(
				"rope.scaling.type=%s factor=%.4g is declared but rope.scaling.original_context_length is absent, so the pre-extension window is unknown; %d is the declared maximum",
				res.RoPE.Type, res.RoPE.Factor, declared)
		}
		return native, declared, ""
	}
	return native, declared, fmt.Sprintf(
		"trained at %d tokens; %d is reachable only through %s scaling (factor %.4g). Serving past %d trades quality for window",
		native, declared, ropeTypeLabel(res.RoPE.Type), res.RoPE.Factor, native)
}

func ropeTypeLabel(t string) string {
	if t == "" {
		return "rope"
	}
	return t
}

// mlaKeySuffixes are the multi-head latent attention keys. A model with these
// compresses KV into a latent vector, so the standard
// n_kv_heads x head_dim x 2 formula does not describe its cache at all.
var mlaKeySuffixes = []string{
	".attention.kv_lora_rank",
	".attention.q_lora_rank",
	".attention.key_length_mla",
	".attention.value_length_mla",
}

func (res *Result) finishUnsupportedKV(arch string) {
	if arch == "" {
		return
	}
	var mla, ssm []string
	for _, suffix := range mlaKeySuffixes {
		if _, ok := res.KV[arch+suffix]; ok {
			mla = append(mla, arch+suffix)
		}
	}
	ssmPrefix := arch + ".ssm."
	for k := range res.KV {
		if strings.HasPrefix(k, ssmPrefix) {
			ssm = append(ssm, k)
		}
	}
	sort.Strings(ssm)
	switch {
	case len(mla) > 0 && len(ssm) > 0:
		res.UnsupportedKVArch = "MLA + SSM"
		res.UnsupportedKVKeys = append(append([]string{}, mla...), ssm...)
	case len(mla) > 0:
		res.UnsupportedKVArch = "MLA (multi-head latent attention: KV is compressed to a latent vector of rank kv_lora_rank, not stored per head)"
		res.UnsupportedKVKeys = mla
	case len(ssm) > 0:
		res.UnsupportedKVArch = "SSM/Mamba (recurrent state of fixed size per sequence, not a per-token KV cache that grows with context)"
		res.UnsupportedKVKeys = ssm
	}
}

// expertTensorMarker matches the MoE expert weight tensors, which llama.cpp
// names blk.N.ffn_{gate,up,down,gate_up,norm}_exps (gguf-py constants.py).
// Stacking every expert into one tensor per role is what makes the
// total-vs-active split computable from the tensor table alone.
const expertTensorMarker = "_exps"

func (res *Result) finishTensors(tensors []TensorInfo) {
	res.finishQuantProfile(tensors)
	res.finishMoE(tensors)
}

func (res *Result) finishQuantProfile(tensors []TensorInfo) {
	if len(tensors) == 0 {
		return
	}
	type agg struct {
		tensors  int
		elements uint64
		bytes    uint64
		traits   GGMLTypeTraits
		known    bool
	}
	byType := map[uint32]*agg{}
	q := &QuantProfile{Tensors: len(tensors)}
	for _, t := range tensors {
		a, ok := byType[t.Type]
		if !ok {
			traits, known := GGMLType(t.Type)
			a = &agg{traits: traits, known: known}
			byType[t.Type] = a
		}
		a.tensors++
		a.elements += t.Elements
		if !a.known || a.traits.BlockSize <= 0 {
			q.UnsizedTensors++
			continue
		}
		a.bytes += t.Elements / uint64(a.traits.BlockSize) * uint64(a.traits.TypeSize)
	}
	for _, a := range byType {
		if !a.known {
			q.UnknownTypes = append(q.UnknownTypes, a.traits.Name)
		}
		q.Elements += a.elements
		q.Bytes += a.bytes
		q.Types = append(q.Types, QuantTypeShare{
			Type:        a.traits.Name,
			Tensors:     a.tensors,
			Elements:    a.elements,
			Bytes:       a.bytes,
			BitsPerElem: a.traits.BitsPerWeight(),
		})
	}
	sort.Strings(q.UnknownTypes)
	sort.Slice(q.Types, func(i, j int) bool {
		if q.Types[i].Bytes != q.Types[j].Bytes {
			return q.Types[i].Bytes > q.Types[j].Bytes
		}
		return q.Types[i].Type < q.Types[j].Type
	})
	if q.Bytes > 0 {
		for i := range q.Types {
			q.Types[i].ShareBytes = float64(q.Types[i].Bytes) / float64(q.Bytes)
		}
		q.DominantType = q.Types[0].Type
	}
	if q.Elements > 0 {
		q.BitsPerWeight = float64(q.Bytes) * 8 / float64(q.Elements)
	}
	q.PartialShard = res.Split.IsShard()
	q.DeclaredType = res.Quantization

	// Mislabeled-quant check. Only claimed when the declared file type has an
	// unambiguous ggml counterpart AND that counterpart is entirely absent
	// from the file. Anything softer produces false positives on the mixed
	// quants (UD-*_XL, *_K_M) that legitimately blend types.
	if want, checkable := FileTypeBaseGGMLType(res.FileType); checkable {
		present := false
		for _, ts := range q.Types {
			if ts.Type == want {
				present = true
				break
			}
		}
		if !present {
			q.LabelMismatch = true
			q.MismatchNote = fmt.Sprintf(
				"general.file_type says %s (ggml type %s) but NO %s tensor is stored; the file is actually %s-dominant at %.2f bits/weight. Trust the tensor table, not the label",
				res.Quantization, want, want, q.DominantType, q.BitsPerWeight)
		}
	}
	res.Quant = q
}

func (res *Result) finishMoE(tensors []TensorInfo) {
	arch := res.Architecture
	if arch == "" {
		return
	}
	count, hasCount := res.Int(arch + ".expert_count")
	if !hasCount || count <= 1 {
		return
	}
	m := &MoEInfo{ExpertCount: count}
	if v, ok := res.Int(arch + ".expert_used_count"); ok {
		m.ExpertUsedCount = v
	}
	if v, ok := res.Int(arch + ".expert_shared_count"); ok {
		m.SharedCount = v
	}

	for _, t := range tensors {
		m.ParamsTotal += t.Elements
		if strings.Contains(t.Name, expertTensorMarker) {
			m.ParamsExpert += t.Elements
			m.ExpertTensors++
		}
	}

	switch {
	case m.ExpertUsedCount <= 0 || m.ExpertUsedCount > count:
		m.ActiveSource = "unavailable: expert_used_count is absent or out of range, so active parameters cannot be derived"
	case m.ExpertTensors > 0 && m.ParamsTotal > 0:
		// Active = everything that is not an expert weight, plus the share of
		// the expert weights that a single token actually routes through.
		dense := m.ParamsTotal - m.ParamsExpert
		m.ParamsActive = dense + m.ParamsExpert*uint64(m.ExpertUsedCount)/uint64(count)
		m.ActiveSource = fmt.Sprintf(
			"tensor-classified: %d expert tensors (*%s) carry %s of the %s parameters; %d of %d experts fire per token",
			m.ExpertTensors, expertTensorMarker, humanParams(m.ParamsExpert), humanParams(m.ParamsTotal),
			m.ExpertUsedCount, count)
	case m.ParamsTotal > 0:
		m.ActiveSource = fmt.Sprintf(
			"ESTIMATE: no *%s tensors were classified in this file, so the expert share is unknown; active parameters are not reported rather than guessed (%d of %d experts fire per token)",
			expertTensorMarker, m.ExpertUsedCount, count)
	case res.Split.IsShard():
		// A split set's first shard is frequently metadata-only: it declares
		// the roster (expert_count and friends) while the weights live in the
		// later shards. Saying "could not be read" there would report a
		// correct file as broken.
		m.ActiveSource = fmt.Sprintf(
			"unavailable from this shard: it carries %d tensors, so the parameter counts live in the sibling shards. Read the whole set to count them",
			len(tensors))
	default:
		m.ActiveSource = "unavailable: the tensor table is empty or could not be read, so parameter counts are unknown"
	}
	if res.Split.IsShard() {
		m.Note = "these parameter counts cover THIS SHARD ONLY (" + res.Split.Summary + ")"
	}
	res.MoE = m
}

func humanParams(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// Float reads a metadata entry as a float regardless of its on-disk width.
func (res *Result) Float(key string) (float64, bool) {
	switch v := res.KV[key].(type) {
	case float64:
		return v, true
	case uint64:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}
