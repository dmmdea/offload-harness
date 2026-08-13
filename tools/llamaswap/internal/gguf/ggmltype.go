// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package gguf

import "fmt"

// GGMLTypeTraits is one ggml tensor type's block geometry: BlockSize elements
// are stored in TypeSize bytes. Everything the BPW histogram needs.
//
// Transcribed from ggml/include/ggml.h (enum ggml_type) and the
// static_assert(sizeof(block_*)) lines in ggml/src/ggml-common.h on
// ggml-org/llama.cpp master, fetched 2026-08-13. Those asserts are the
// authoritative sizes - they are what the compiler enforces against the real
// structs - so the numbers below are transcribed arithmetic, not estimates.
//
// Spot checks against published bits-per-weight: Q4_K = 144*8/256 = 4.5 bpw,
// Q6_K = 210*8/256 = 6.5625 bpw, Q8_0 = 34*8/32 = 8.5 bpw.
type GGMLTypeTraits struct {
	Name      string `json:"name"`
	BlockSize int    `json:"block_size"`
	TypeSize  int    `json:"type_size_bytes"`
}

// BitsPerWeight is the type's storage cost per element, scales included.
func (t GGMLTypeTraits) BitsPerWeight() float64 {
	if t.BlockSize <= 0 {
		return 0
	}
	return float64(t.TypeSize) * 8 / float64(t.BlockSize)
}

// ggmlTypes is indexed by the ggml_type value stored in each tensor-info
// record. Removed types (4,5 Q4_2/Q4_3; 31-33 Q4_0_4_4 family; 36-38
// IQ4_NL_4_4 family) are absent: a file that still carries one is reported as
// an unknown type rather than sized with a made-up block geometry.
var ggmlTypes = map[uint32]GGMLTypeTraits{
	0:  {"F32", 1, 4},
	1:  {"F16", 1, 2},
	2:  {"Q4_0", 32, 18},
	3:  {"Q4_1", 32, 20},
	6:  {"Q5_0", 32, 22},
	7:  {"Q5_1", 32, 24},
	8:  {"Q8_0", 32, 34},
	9:  {"Q8_1", 32, 36},
	10: {"Q2_K", 256, 84},
	11: {"Q3_K", 256, 110},
	12: {"Q4_K", 256, 144},
	13: {"Q5_K", 256, 176},
	14: {"Q6_K", 256, 210},
	15: {"Q8_K", 256, 292},
	16: {"IQ2_XXS", 256, 66},
	17: {"IQ2_XS", 256, 74},
	18: {"IQ3_XXS", 256, 98},
	19: {"IQ1_S", 256, 50},
	20: {"IQ4_NL", 32, 18},
	21: {"IQ3_S", 256, 110},
	22: {"IQ2_S", 256, 82},
	23: {"IQ4_XS", 256, 136},
	24: {"I8", 1, 1},
	25: {"I16", 1, 2},
	26: {"I32", 1, 4},
	27: {"I64", 1, 8},
	28: {"F64", 1, 8},
	29: {"IQ1_M", 256, 56},
	30: {"BF16", 1, 2},
	34: {"TQ1_0", 256, 54},
	35: {"TQ2_0", 256, 66},
	39: {"MXFP4", 32, 17},
	40: {"NVFP4", 64, 36},
	41: {"Q1_0", 128, 18},
	42: {"Q2_0", 64, 18},
}

// GGMLType resolves a tensor-info type tag. Unknown tags return ok=false and
// a placeholder name that keeps the raw number visible; callers must not size
// such a tensor.
func GGMLType(t uint32) (GGMLTypeTraits, bool) {
	if tr, ok := ggmlTypes[t]; ok {
		return tr, true
	}
	return GGMLTypeTraits{Name: fmt.Sprintf("unknown-ggml-type-%d", t)}, false
}
