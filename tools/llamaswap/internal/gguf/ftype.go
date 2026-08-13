// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package gguf

import "fmt"

// fileTypeNames maps llama.cpp's llama_ftype enum (stored in the
// general.file_type metadata key) to its quantization label. Gaps in the
// numbering are removed formats (Q4_2, Q4_3, the Q4_0_4_4 family) and stay
// absent so an unknown value is reported as unknown rather than mislabeled.
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
}

// FileTypeName labels a general.file_type value. Unknown values keep the
// raw number visible so a newer quantization type is obvious rather than
// silently rendered as a familiar one.
func FileTypeName(ft int) string {
	if name, ok := fileTypeNames[ft]; ok {
		return name
	}
	return fmt.Sprintf("unknown-ftype-%d", ft)
}
