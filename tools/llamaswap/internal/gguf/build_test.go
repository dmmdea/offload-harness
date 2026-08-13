// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package gguf

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// A minimal GGUF writer for the header shapes this box has no local model of:
// sharded sets, MoE rosters, YaRN-extended context, MLA and SSM attention,
// LoRA adapters, and a file whose general.file_type lies about its tensors.
// Fixtures are built to the on-disk spec (magic, version, counts, KV section,
// tensor-info table) so the reader under test walks real bytes rather than a
// mocked struct.
type ggufBuilder struct {
	kv      []byte
	kvCount uint64
	tensors []byte
	tCount  uint64
	offset  uint64
}

func str(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint64(b, uint64(len(s)))
	return append(b, s...)
}

func (g *ggufBuilder) key(k string, vtype uint32) {
	g.kv = str(g.kv, k)
	g.kv = binary.LittleEndian.AppendUint32(g.kv, vtype)
	g.kvCount++
}

func (g *ggufBuilder) U32(k string, v uint32) *ggufBuilder {
	g.key(k, typeUint32)
	g.kv = binary.LittleEndian.AppendUint32(g.kv, v)
	return g
}

func (g *ggufBuilder) U16(k string, v uint16) *ggufBuilder {
	g.key(k, typeUint16)
	g.kv = binary.LittleEndian.AppendUint16(g.kv, v)
	return g
}

func (g *ggufBuilder) F32(k string, v float32) *ggufBuilder {
	g.key(k, typeFloat32)
	g.kv = binary.LittleEndian.AppendUint32(g.kv, math.Float32bits(v))
	return g
}

func (g *ggufBuilder) Str(k, v string) *ggufBuilder {
	g.key(k, typeString)
	g.kv = str(g.kv, v)
	return g
}

func (g *ggufBuilder) Bool(k string, v bool) *ggufBuilder {
	g.key(k, typeBool)
	var b byte
	if v {
		b = 1
	}
	g.kv = append(g.kv, b)
	return g
}

// Tensor appends one tensor-info record. Only the table is written; the
// fixtures carry no tensor DATA, which is exactly what the reader promises
// never to touch.
func (g *ggufBuilder) Tensor(name string, ggmlType uint32, dims ...uint64) *ggufBuilder {
	g.tensors = str(g.tensors, name)
	g.tensors = binary.LittleEndian.AppendUint32(g.tensors, uint32(len(dims)))
	elems := uint64(1)
	for _, d := range dims {
		g.tensors = binary.LittleEndian.AppendUint64(g.tensors, d)
		elems *= d
	}
	g.tensors = binary.LittleEndian.AppendUint32(g.tensors, ggmlType)
	g.tensors = binary.LittleEndian.AppendUint64(g.tensors, g.offset)
	g.tCount++
	if tr, ok := GGMLType(ggmlType); ok && tr.BlockSize > 0 {
		g.offset += elems / uint64(tr.BlockSize) * uint64(tr.TypeSize)
	}
	return g
}

func (g *ggufBuilder) write(t *testing.T, path string) string {
	t.Helper()
	buf := []byte(Magic)
	buf = binary.LittleEndian.AppendUint32(buf, 3)
	buf = binary.LittleEndian.AppendUint64(buf, g.tCount)
	buf = binary.LittleEndian.AppendUint64(buf, g.kvCount)
	buf = append(buf, g.kv...)
	buf = append(buf, g.tensors...)
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("writing fixture %s: %v", path, err)
	}
	return path
}

// newFixture writes a fixture into t.TempDir() under name and reads it back.
func (g *ggufBuilder) read(t *testing.T, name string) *Result {
	t.Helper()
	p := g.write(t, filepath.Join(t.TempDir(), name))
	r, err := Read(p)
	if err != nil {
		t.Fatalf("Read(%s): %v", name, err)
	}
	if !r.IsGGUF {
		t.Fatalf("fixture %s did not parse as GGUF: %s", name, r.NotGGUFReason)
	}
	if r.TensorInfoError != "" {
		t.Fatalf("fixture %s tensor table: %s", name, r.TensorInfoError)
	}
	return r
}

// base seeds the architecture keys every fixture needs to reach finish().
func base(arch string) *ggufBuilder {
	g := &ggufBuilder{}
	g.Str("general.architecture", arch)
	g.U32(arch+".block_count", 4)
	g.U32(arch+".attention.head_count", 8)
	g.U32(arch+".attention.head_count_kv", 2)
	g.U32(arch+".embedding_length", 512)
	g.U32(arch+".context_length", 4096)
	return g
}
