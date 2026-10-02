package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// config.Default().MemoryStack (what `gpu reserve --unload-seat` keeps resident when a config names no stack) and
// DEFAULT_MEMORY_STACK in render/gpu-lock.mjs (what the render runner's helper keeps when the pipeline exports no
// MEMORY_STACK) are two literals for ONE fact: the models nothing that clears the cards for a GPU job may unload.
// They lived apart, and neither named embeddinggemma-ams, the id the memory authority node serves its embedder
// under, so the first lease over that node would have unloaded it (register A-122b, 2026-10-01). This reads the
// Node literal and requires it to equal the Go default, in order.
func TestMemoryStackDefaultsAgreeBetweenGoAndTheRenderHelper(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("render", "gpu-lock.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)const DEFAULT_MEMORY_STACK\s*=\s*\[(.*?)\]`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("render/gpu-lock.mjs no longer declares DEFAULT_MEMORY_STACK as an array literal: this gate went blind")
	}
	var node []string
	for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(m[1], -1) {
		node = append(node, string(q[1]))
	}
	if got := config.Default().MemoryStack; !reflect.DeepEqual(got, node) {
		t.Errorf("config.Default().MemoryStack = %v but render/gpu-lock.mjs DEFAULT_MEMORY_STACK = %v: the two defaults must be the same list, because a lease unloads whatever the one it reads does not name, and the memory embedder has absolute priority (register A-122b)", got, node)
	}
}
