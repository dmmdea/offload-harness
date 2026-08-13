// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

// Package gguf is a dependency-free GGUF header reader.
//
// It reads ONLY the magic, the file header, and the metadata key/value
// section — never the tensor data. On this box that is ~15 MB of a 4 GB
// file (the KV section is dominated by a 262k-entry tokenizer vocabulary),
// so `gguf` on a multi-GB weights file costs a fraction of a second and
// never pulls gigabytes through the page cache.
//
// Non-GGUF files are a first-class, non-error outcome: the roster on this
// box includes whisper .bin weights (ggml magic, not GGUF). Read returns a
// Result with IsGGUF=false and a typed reason so callers classify the seat
// `non-llama-server` instead of emitting a false positive. One noisy false
// positive kills lint adoption.
package gguf

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

// Magic is the 4-byte file magic of every GGUF file.
const Magic = "GGUF"

// ErrTruncated is returned when the metadata section ends before the
// declared key count is satisfied (a partial download, typically).
var ErrTruncated = errors.New("gguf: file truncated inside metadata section")

// Value type tags from the GGUF spec (little-endian on disk).
const (
	typeUint8   = 0
	typeInt8    = 1
	typeUint16  = 2
	typeInt16   = 3
	typeUint32  = 4
	typeInt32   = 5
	typeFloat32 = 6
	typeBool    = 7
	typeString  = 8
	typeArray   = 9
	typeUint64  = 10
	typeInt64   = 11
	typeFloat64 = 12
)

// Sanity caps. A corrupt or hostile header must not make the reader
// allocate unbounded memory; every cap is far above any real model.
const (
	maxKVCount     = 1 << 20
	maxKeyLen      = 1 << 16
	maxStringLen   = 1 << 24 // chat templates run to ~100 KB; 16 MB is slack.
	maxKeptArray   = 4096    // numeric/bool arrays kept whole (sliding-window patterns are one per layer)
	maxArrayLen    = 1 << 28
	maxInlineChars = 4096 // strings longer than this are summarized, not stored
)

// Result is everything Read extracts. Fields that the file does not declare
// stay at their zero value and are reported as "unknown" by callers — this
// package never substitutes a plausible default (the 4096-context class of
// fabrication).
type Result struct {
	Path          string `json:"path"`
	FileSizeBytes int64  `json:"file_size_bytes"`
	FileSizeMiB   int64  `json:"file_size_mib"`

	IsGGUF        bool   `json:"is_gguf"`
	NotGGUFReason string `json:"not_gguf_reason,omitempty"`
	MagicSeen     string `json:"magic_seen,omitempty"`

	Version       uint32 `json:"version,omitempty"`
	TensorCount   uint64 `json:"tensor_count"`
	KVCount       uint64 `json:"kv_count"`
	MetadataBytes int64  `json:"metadata_bytes"`

	Architecture string `json:"architecture,omitempty"`
	Name         string `json:"name,omitempty"`
	SizeLabel    string `json:"size_label,omitempty"`

	BlockCount      int `json:"block_count,omitempty"`
	HeadCount       int `json:"head_count,omitempty"`
	HeadCountKV     int `json:"head_count_kv,omitempty"`
	ContextLength   int `json:"context_length,omitempty"`
	EmbeddingLength int `json:"embedding_length,omitempty"`

	// HeadCountKVSource is "kv" when <arch>.attention.head_count_kv was
	// present and "fallback:head_count" when the file declares no GQA key
	// (BERT-class encoders). Never guessed silently: a wrong n_kv_heads is
	// a 4-8x KV error.
	HeadCountKVSource string `json:"head_count_kv_source,omitempty"`

	// KeyLength/ValueLength are the per-head K and V dimensions. Source is
	// "kv" when the file declares attention.key_length/value_length and
	// "derived:embedding_length/head_count" otherwise.
	KeyLength    int    `json:"key_length,omitempty"`
	ValueLength  int    `json:"value_length,omitempty"`
	LengthSource string `json:"key_value_length_source,omitempty"`

	// Sliding-window attention. SlidingWindowPattern[i]=true means layer i
	// is a SWA layer whose KV cache is bounded by SlidingWindow rather than
	// by n_ctx.
	SlidingWindow        int    `json:"sliding_window,omitempty"`
	SlidingWindowPattern []bool `json:"sliding_window_pattern,omitempty"`
	KeyLengthSWA         int    `json:"key_length_swa,omitempty"`
	ValueLengthSWA       int    `json:"value_length_swa,omitempty"`

	// SharedKVLayers is <arch>.attention.shared_kv_layers: the number of
	// trailing layers that reuse an earlier layer's KV cache and therefore
	// allocate none of their own.
	SharedKVLayers int `json:"shared_kv_layers,omitempty"`

	FileType     int    `json:"file_type"`
	Quantization string `json:"quantization"`

	ChatTemplateChars  int    `json:"chat_template_chars,omitempty"`
	ChatTemplateSHA256 string `json:"chat_template_sha256,omitempty"`

	// KV holds every scalar metadata entry (and small numeric/bool arrays)
	// for the raw dump. Large arrays (tokenizer vocabularies) are recorded
	// as a summary string, never materialized.
	KV map[string]any `json:"kv,omitempty"`
}

// Missing reports the requested-but-absent header fields a caller needs for
// KV math, so commands can say exactly what is unknown instead of guessing.
func (r *Result) Missing() []string {
	var out []string
	if r.BlockCount == 0 {
		out = append(out, "block_count")
	}
	if r.HeadCount == 0 {
		out = append(out, "attention.head_count")
	}
	if r.HeadCountKV == 0 {
		out = append(out, "attention.head_count_kv")
	}
	if r.EmbeddingLength == 0 {
		out = append(out, "embedding_length")
	}
	if r.ContextLength == 0 {
		out = append(out, "context_length")
	}
	return out
}

type reader struct {
	br  *bufio.Reader
	n   int64
	buf [8]byte
}

func (r *reader) read(p []byte) error {
	got, err := io.ReadFull(r.br, p)
	r.n += int64(got)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return ErrTruncated
		}
		return err
	}
	return nil
}

func (r *reader) skip(n int64) error {
	for n > 0 {
		chunk := n
		if chunk > 1<<20 {
			chunk = 1 << 20
		}
		got, err := r.br.Discard(int(chunk))
		r.n += int64(got)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return ErrTruncated
			}
			return err
		}
		n -= int64(got)
	}
	return nil
}

func (r *reader) u32() (uint32, error) {
	if err := r.read(r.buf[:4]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(r.buf[:4]), nil
}

func (r *reader) u64() (uint64, error) {
	if err := r.read(r.buf[:8]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(r.buf[:8]), nil
}

func (r *reader) str(max uint64) (string, error) {
	n, err := r.u64()
	if err != nil {
		return "", err
	}
	if n > max {
		return "", fmt.Errorf("gguf: string length %d exceeds cap %d (corrupt header?)", n, max)
	}
	if n == 0 {
		return "", nil
	}
	b := make([]byte, n)
	if err := r.read(b); err != nil {
		return "", err
	}
	return string(b), nil
}

// skipStr consumes a length-prefixed string without materializing it.
func (r *reader) skipStr() error {
	n, err := r.u64()
	if err != nil {
		return err
	}
	if n > maxStringLen*16 {
		return fmt.Errorf("gguf: string length %d implausible (corrupt header?)", n)
	}
	return r.skip(int64(n))
}

func scalarSize(t uint32) (int, bool) {
	switch t {
	case typeUint8, typeInt8, typeBool:
		return 1, true
	case typeUint16, typeInt16:
		return 2, true
	case typeUint32, typeInt32, typeFloat32:
		return 4, true
	case typeUint64, typeInt64, typeFloat64:
		return 8, true
	}
	return 0, false
}

func decodeScalar(t uint32, b []byte) any {
	switch t {
	case typeUint8:
		return uint64(b[0])
	case typeInt8:
		return int64(int8(b[0]))
	case typeBool:
		return b[0] != 0
	case typeUint16:
		return uint64(binary.LittleEndian.Uint16(b))
	case typeInt16:
		return int64(int16(binary.LittleEndian.Uint16(b)))
	case typeUint32:
		return uint64(binary.LittleEndian.Uint32(b))
	case typeInt32:
		return int64(int32(binary.LittleEndian.Uint32(b)))
	case typeFloat32:
		return float64(math32(binary.LittleEndian.Uint32(b)))
	case typeUint64:
		return binary.LittleEndian.Uint64(b)
	case typeInt64:
		return int64(binary.LittleEndian.Uint64(b))
	case typeFloat64:
		return math64(binary.LittleEndian.Uint64(b))
	}
	return nil
}

// Read parses path's GGUF header and metadata. A non-GGUF file is reported
// through Result.IsGGUF=false with a reason, not through an error; errors
// are reserved for unreadable or structurally corrupt files.
func Read(path string) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("gguf: %s is a directory, not a GGUF file", path)
	}

	res := &Result{
		Path:          path,
		FileSizeBytes: st.Size(),
		FileSizeMiB:   st.Size() / (1024 * 1024),
		FileType:      -1,
		KV:            map[string]any{},
	}

	r := &reader{br: bufio.NewReaderSize(f, 1<<20)}

	magic := make([]byte, 4)
	if err := r.read(magic); err != nil {
		res.NotGGUFReason = "file shorter than a 4-byte magic"
		return res, nil
	}
	res.MagicSeen = describeMagic(magic)
	if string(magic) != Magic {
		res.NotGGUFReason = fmt.Sprintf("magic %s is not %q", res.MagicSeen, Magic)
		return res, nil
	}
	res.IsGGUF = true

	if res.Version, err = r.u32(); err != nil {
		return res, err
	}
	if res.Version < 2 || res.Version > 3 {
		// v1 predates the u64 length fields; anything above v3 may add
		// fields we do not know. Say so instead of misreading bytes.
		res.NotGGUFReason = fmt.Sprintf("GGUF version %d is outside the supported v2/v3 range", res.Version)
		return res, nil
	}
	if res.TensorCount, err = r.u64(); err != nil {
		return res, err
	}
	if res.KVCount, err = r.u64(); err != nil {
		return res, err
	}
	if res.KVCount > maxKVCount {
		return res, fmt.Errorf("gguf: metadata key count %d exceeds cap %d (corrupt header?)", res.KVCount, maxKVCount)
	}

	for i := uint64(0); i < res.KVCount; i++ {
		key, err := r.str(maxKeyLen)
		if err != nil {
			return res, err
		}
		t, err := r.u32()
		if err != nil {
			return res, err
		}
		val, err := readValue(r, t, key)
		if err != nil {
			return res, fmt.Errorf("gguf: key %q: %w", key, err)
		}
		if val != nil {
			res.KV[key] = val
		}
	}
	res.MetadataBytes = r.n

	res.finish()
	return res, nil
}

// readValue decodes one metadata value. Tokenizer vocabularies and other
// oversized arrays are skipped (a summary string is stored) so a 262k-entry
// token list never lands in memory.
func readValue(r *reader, t uint32, key string) (any, error) {
	switch t {
	case typeString:
		n, err := r.u64()
		if err != nil {
			return nil, err
		}
		if n > maxStringLen {
			if err := r.skip(int64(n)); err != nil {
				return nil, err
			}
			return fmt.Sprintf("<string, %d bytes, not loaded>", n), nil
		}
		b := make([]byte, n)
		if err := r.read(b); err != nil {
			return nil, err
		}
		s := string(b)
		if len(s) > maxInlineChars {
			sum := sha256.Sum256(b)
			return map[string]any{
				"chars":  len(s),
				"sha256": hex.EncodeToString(sum[:]),
				"head":   s[:120],
			}, nil
		}
		return s, nil
	case typeArray:
		et, err := r.u32()
		if err != nil {
			return nil, err
		}
		n, err := r.u64()
		if err != nil {
			return nil, err
		}
		if n > maxArrayLen {
			return nil, fmt.Errorf("array length %d implausible (corrupt header?)", n)
		}
		if et == typeString {
			for i := uint64(0); i < n; i++ {
				if err := r.skipStr(); err != nil {
					return nil, err
				}
			}
			return fmt.Sprintf("<%d strings, not loaded>", n), nil
		}
		if et == typeArray {
			return nil, errors.New("nested arrays are not part of the GGUF spec")
		}
		size, ok := scalarSize(et)
		if !ok {
			return nil, fmt.Errorf("unknown array element type %d", et)
		}
		total := int64(size) * int64(n)
		if n > maxKeptArray {
			if err := r.skip(total); err != nil {
				return nil, err
			}
			return fmt.Sprintf("<%d values of type %d, not loaded>", n, et), nil
		}
		b := make([]byte, total)
		if err := r.read(b); err != nil {
			return nil, err
		}
		out := make([]any, 0, n)
		for i := uint64(0); i < n; i++ {
			out = append(out, decodeScalar(et, b[int64(i)*int64(size):]))
		}
		return out, nil
	default:
		size, ok := scalarSize(t)
		if !ok {
			return nil, fmt.Errorf("unknown value type %d", t)
		}
		b := make([]byte, size)
		if err := r.read(b); err != nil {
			return nil, err
		}
		return decodeScalar(t, b), nil
	}
}

// finish projects the raw KV map onto the typed fields.
func (res *Result) finish() {
	res.Architecture, _ = res.KV["general.architecture"].(string)
	res.Name, _ = res.KV["general.name"].(string)
	res.SizeLabel, _ = res.KV["general.size_label"].(string)

	arch := res.Architecture
	geti := func(suffix string) int {
		if arch == "" {
			return 0
		}
		v, _ := res.Int(arch + "." + suffix)
		return v
	}

	res.BlockCount = geti("block_count")
	res.HeadCount = geti("attention.head_count")
	res.ContextLength = geti("context_length")
	res.EmbeddingLength = geti("embedding_length")
	res.SlidingWindow = geti("attention.sliding_window")
	res.SharedKVLayers = geti("attention.shared_kv_layers")
	res.KeyLengthSWA = geti("attention.key_length_swa")
	res.ValueLengthSWA = geti("attention.value_length_swa")

	if v := geti("attention.head_count_kv"); v > 0 {
		res.HeadCountKV = v
		res.HeadCountKVSource = "kv"
	} else if res.HeadCount > 0 {
		// No GQA key: multi-head attention, n_kv_heads == n_heads. BERT
		// rerankers on this box take this branch. Recorded, not silent.
		res.HeadCountKV = res.HeadCount
		res.HeadCountKVSource = "fallback:head_count (no attention.head_count_kv key)"
	}

	kl, vl := geti("attention.key_length"), geti("attention.value_length")
	switch {
	case kl > 0 && vl > 0:
		res.KeyLength, res.ValueLength = kl, vl
		res.LengthSource = "kv:attention.key_length/value_length"
	case res.EmbeddingLength > 0 && res.HeadCount > 0:
		d := res.EmbeddingLength / res.HeadCount
		res.KeyLength, res.ValueLength = d, d
		res.LengthSource = "derived:embedding_length/head_count"
	}

	if pat, ok := res.KV[arch+".attention.sliding_window_pattern"].([]any); ok {
		out := make([]bool, 0, len(pat))
		for _, e := range pat {
			b, _ := e.(bool)
			out = append(out, b)
		}
		res.SlidingWindowPattern = out
	}

	if ft, ok := res.Int("general.file_type"); ok {
		res.FileType = ft
		res.Quantization = FileTypeName(ft)
	} else {
		res.Quantization = "unknown (no general.file_type key)"
	}

	switch tpl := res.KV["tokenizer.chat_template"].(type) {
	case string:
		res.ChatTemplateChars = len(tpl)
		sum := sha256.Sum256([]byte(tpl))
		res.ChatTemplateSHA256 = hex.EncodeToString(sum[:])
	case map[string]any:
		if n, ok := tpl["chars"].(int); ok {
			res.ChatTemplateChars = n
		}
		if s, ok := tpl["sha256"].(string); ok {
			res.ChatTemplateSHA256 = s
		}
	}
}

// Int reads a metadata entry as an int regardless of its on-disk integer
// width or signedness. Returns ok=false for absent or non-numeric keys.
func (res *Result) Int(key string) (int, bool) {
	switch v := res.KV[key].(type) {
	case uint64:
		return int(v), true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

func describeMagic(b []byte) string {
	printable := true
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			printable = false
			break
		}
	}
	hexed := "0x" + hex.EncodeToString(b)
	if printable {
		return fmt.Sprintf("%q (%s)", string(b), hexed)
	}
	return hexed
}

// KnownNonGGUF gives a friendlier reason for magics we recognize. whisper.cpp
// writes its magic as a little-endian uint32, so the bytes on disk read
// "lmgg" rather than "ggml"; both spellings are accepted because the caller
// sees the byte order, not the intent.
func KnownNonGGUF(magic string) string {
	switch {
	case strings.Contains(magic, "ggml"), strings.Contains(magic, "lmgg"):
		return "legacy ggml container (whisper.cpp .bin weights)"
	case strings.Contains(magic, "PK"):
		return "zip container (safetensors/HF archive?)"
	}
	return ""
}

func math32(bits uint32) float32 { return math.Float32frombits(bits) }
func math64(bits uint64) float64 { return math.Float64frombits(bits) }
