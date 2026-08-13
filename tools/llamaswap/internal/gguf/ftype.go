// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package gguf

import "fmt"

// FileTypeGuessedBit is llama.cpp's LLAMA_FTYPE_GUESSED (1024): the loader
// ORs it into llama_ftype when the model file declared no general.file_type
// and the type was inferred from the tensors. A reader that does not mask it
// reports "unknown-ftype-1031" for what is really "Q8_0, guessed" - the
// number is not a new quantization, it is a flag plus a known one.
//
// Source of truth: include/llama.h, enum llama_ftype (fetched from
// ggml-org/llama.cpp master, 2026-08-13).
const FileTypeGuessedBit = 1024

// fileTypeNames maps llama.cpp's llama_ftype enum (stored in the
// general.file_type metadata key) to its quantization label. Gaps in the
// numbering are removed formats and stay absent so an unknown value is
// reported as unknown rather than mislabeled:
//
//	4,5,6   Q4_1_SOME_F16, Q4_2, Q4_3  - support removed
//	33,34,35 Q4_0_4_4 / 4_8 / 8_8      - removed from gguf files (runtime repack)
//
// Transcribed from include/llama.h on master 2026-08-13; entries 39-41
// (NVFP4, Q1_0, Q2_0) postdate the previous table here.
//
// Verified on this box: embeddinggemma-300M-Q8_0.gguf declares file_type 7
// and llama-server's /props reports model_ftype "Q8_0";
// bge-reranker-v2-m3-Q4_K_M.gguf declares 15 -> Q4_K_M.
var fileTypeNames = map[int]string{
	0:  "F32",
	1:  "F16",
	2:  "Q4_0",
	3:  "Q4_1",
	7:  "Q8_0",
	8:  "Q5_0",
	9:  "Q5_1",
	10: "Q2_K",
	11: "Q3_K_S",
	12: "Q3_K_M",
	13: "Q3_K_L",
	14: "Q4_K_S",
	15: "Q4_K_M",
	16: "Q5_K_S",
	17: "Q5_K_M",
	18: "Q6_K",
	19: "IQ2_XXS",
	20: "IQ2_XS",
	21: "Q2_K_S",
	22: "IQ3_XS",
	23: "IQ3_XXS",
	24: "IQ1_S",
	25: "IQ4_NL",
	26: "IQ3_S",
	27: "IQ3_M",
	28: "IQ2_S",
	29: "IQ2_M",
	30: "IQ4_XS",
	31: "IQ1_M",
	32: "BF16",
	36: "TQ1_0",
	37: "TQ2_0",
	38: "MXFP4_MOE",
	39: "NVFP4",
	40: "Q1_0",
	41: "Q2_0",
}

// fileTypeBaseGGML maps an llama_ftype to the ggml tensor type its name
// promises, for the mislabeled-quant check in the tensor histogram.
//
// Deliberately partial. The IQ1/IQ2/IQ3 ftypes mix several ggml types per
// tensor role and their ftype->type correspondence is not one-to-one, so
// they are omitted: an absent mapping means "not checkable" and produces no
// mismatch claim. Claiming a mismatch from a mapping this table is not sure
// of would be the exact false positive the check exists to avoid.
var fileTypeBaseGGML = map[int]string{
	0:  "F32",
	1:  "F16",
	2:  "Q4_0",
	3:  "Q4_1",
	7:  "Q8_0",
	8:  "Q5_0",
	9:  "Q5_1",
	10: "Q2_K",
	11: "Q3_K",
	12: "Q3_K",
	13: "Q3_K",
	14: "Q4_K",
	15: "Q4_K",
	16: "Q5_K",
	17: "Q5_K",
	18: "Q6_K",
	21: "Q2_K",
	25: "IQ4_NL",
	30: "IQ4_XS",
	32: "BF16",
	36: "TQ1_0",
	37: "TQ2_0",
	38: "MXFP4",
	39: "NVFP4",
	40: "Q1_0",
	41: "Q2_0",
}

// FileTypeName labels a general.file_type value, masking and reporting the
// LLAMA_FTYPE_GUESSED bit. Unknown values keep the raw number visible so a
// newer quantization type is obvious rather than silently rendered as a
// familiar one.
func FileTypeName(ft int) string {
	base, guessed := SplitFileType(ft)
	name, ok := fileTypeNames[base]
	if !ok {
		name = fmt.Sprintf("unknown-ftype-%d", base)
	}
	if guessed {
		return name + " (guessed)"
	}
	return name
}

// SplitFileType separates a raw general.file_type into its llama_ftype value
// and the GUESSED flag.
func SplitFileType(ft int) (base int, guessed bool) {
	if ft < 0 {
		return ft, false
	}
	return ft &^ FileTypeGuessedBit, ft&FileTypeGuessedBit != 0
}

// FileTypeBaseGGMLType returns the ggml tensor type an ftype's name promises,
// or ok=false when the correspondence is not one-to-one and no mismatch claim
// can honestly be made.
func FileTypeBaseGGMLType(ft int) (string, bool) {
	base, _ := SplitFileType(ft)
	name, ok := fileTypeBaseGGML[base]
	return name, ok
}
