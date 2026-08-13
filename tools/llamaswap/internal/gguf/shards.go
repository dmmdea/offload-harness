// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package gguf

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// shardNameRE matches llama.cpp's SPLIT_PATH_FORMAT, "%s-%05d-of-%05d.gguf"
// (src/llama-model-loader / tools/gguf-split). The index in the FILENAME is
// one-based even though the split.no metadata key is zero-based.
var shardNameRE = regexp.MustCompile(`(?i)^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// parseShardFilename extracts the one-based index and the shard count from a
// conventionally named shard. Returns 0,0 for any other name — a model may be
// sharded without following the convention, in which case only the metadata
// speaks.
func parseShardFilename(path string) (index, count int) {
	m := shardNameRE.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return 0, 0
	}
	i, err1 := strconv.Atoi(m[2])
	c, err2 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return i, c
}

// Shard is one file of a sharded model.
type Shard struct {
	Index  int    `json:"index"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Exists bool   `json:"exists"`
}

// ShardSet is every file of a sharded model, resolved from one member.
type ShardSet struct {
	Count      int     `json:"count"`
	Shards     []Shard `json:"shards"`
	TotalBytes int64   `json:"total_bytes"`
	Missing    []int   `json:"missing_shard_indexes,omitempty"`
	Complete   bool    `json:"complete"`
	// Convention is the filename pattern the set was resolved through, or the
	// reason no set could be resolved.
	Convention string `json:"convention"`
}

// ResolveShards finds every sibling shard of path on disk.
//
// This exists because a shard's own header is an honest description of a
// FRACTION of a model: file size, tensor count and parameter count are all
// per-shard. Any command that turns those into a VRAM verdict must either sum
// the whole set or refuse — reporting shard 1 of 3 as if it were the model
// under-reports the weights by ~3x, which is a fits/does-not-fit inversion,
// not a rounding error.
//
// An incomplete set is returned with Complete=false and the missing indexes
// named; callers must refuse rather than sum what is there.
func ResolveShards(path string, declaredCount int) (*ShardSet, error) {
	idx, fileCount := parseShardFilename(path)
	count := declaredCount
	if count <= 0 {
		count = fileCount
	}
	if count <= 1 {
		return nil, fmt.Errorf("gguf: %s is not part of a multi-shard set", filepath.Base(path))
	}
	if idx == 0 {
		return &ShardSet{
			Count:      count,
			Complete:   false,
			Convention: fmt.Sprintf("filename %q does not follow the -NNNNN-of-NNNNN.gguf convention, so the sibling shards cannot be located from it; pass every shard explicitly or point at the first shard", filepath.Base(path)),
		}, nil
	}
	m := shardNameRE.FindStringSubmatch(filepath.Base(path))
	prefix := filepath.Join(filepath.Dir(path), m[1])

	set := &ShardSet{
		Count:      count,
		Complete:   true,
		Convention: "llama.cpp SPLIT_PATH_FORMAT (<prefix>-%05d-of-%05d.gguf, one-based index)",
	}
	for i := 1; i <= count; i++ {
		p := fmt.Sprintf("%s-%05d-of-%05d.gguf", prefix, i, count)
		sh := Shard{Index: i, Path: p}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			sh.Exists, sh.Bytes = true, st.Size()
			set.TotalBytes += st.Size()
		} else {
			set.Complete = false
			set.Missing = append(set.Missing, i)
		}
		set.Shards = append(set.Shards, sh)
	}
	return set, nil
}
