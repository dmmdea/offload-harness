package hostneed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// A call that runs the caller's OWN ComfyUI graph (`render/comfy-render.mjs --graph <file>`, a `run-graph`) loads whatever the graph
// names, which no family table knows. The media class default stands in for it, the largest family the box binds, and on a node
// that binds a video pair it is a figure far above what a small image graph loads: a 512x512 graph of 23 GiB was declared 32.9 GiB,
// held by the host-RAM brake, and never ran although the box could take it. The graph itself says what it loads: its loaders
// name the weight files. ForGraph reads those names with the reader the node door already uses (mediacap.ScanGraph), finds each
// under the box's model roots (mediacap.ModelFileSizes, the lookup the door's own held-by-name check makes) and declares the sum.
//
// It declares a figure only when it can stand behind all of it. A graph it cannot read, ONE file
// it cannot find or size, or a weight-like name the scan did not list, is not sized at all, and the caller keeps the class default exactly as before: a size that is not
// known must never shrink a declaration. A graph that names no weight file at all (an API-format graph with a node, and no weight-like
// name anywhere in it) is not that case: nothing is left to load but the ComfyUI process, and it is declared the measured process floor
// (ProcessFloor: 3 GiB touched, 4 GiB of commit stated), not the class default of the largest family the box binds.

// maxGraphBytes bounds the graph document ForGraphFile reads. A ComfyUI API-format graph is a few KiB, and a fleet node's dispatch
// body is 1 MiB at most; a larger file is not a graph worth a read on every lease.
const maxGraphBytes = 8 << 20

// graphDetailFiles is how many files the detail names before it says "and N more".
const graphDetailFiles = 8

// ForGraph sizes a ComfyUI API-format graph by the weight files its own loaders name: the SUM of their sizes, each file once.
// Every file counts, a VAE and a LoRA with the checkpoint, because the graph names them. The sum goes through the family
// estimate's own rule (fitToCard): in full when together the files do not fit the card (or the card is not known), and 0 when
// they fit, because a set that fits streams nothing from RAM. The figure is an estimate from the files' sizes, not a measurement.
//
// A graph that names no weight file is the ProcessFloor, ok true. ok is false, and the caller keeps the media class default, when graph is
// not a JSON object (or holds no node), or names a weight file that does not resolve under the box's model roots (or is empty), or when the document holds a weight-like name the sizing did not
// cover (unsizedWeight: another extension, a nested widget object, a list). Facts.Stat sizes the files when set, as for every estimate.
//
// LIMIT: a weight is recognised by its extension (the scan's six and otherWeightExts) and a graph can name one by another, or load
// a file whose name is built at run time; such a graph is sized by the files that were seen, an estimate that can be low.
func ForGraph(graph []byte, f Facts) (Need, bool) {
	files, err := mediacap.ScanGraph(graph)
	if err != nil {
		return Need{}, false
	}
	if len(files.Models) == 0 {
		// A graph that names no weight file loads nothing a file size could say; what it holds is the ComfyUI process (ProcessFloor). Only a
		// document that is an API-format graph (a node with a class_type) and holds no weight-like name anywhere (unsizedWeight: another
		// extension, a nested object, a list) and no node of a weight-loader class (hasWeightLoader: its file may be fed by a link or built at run
// time) is given the floor: a UI-format workflow, a name the scan did not list or a loader that names nothing keeps the class default.
		if !hasGraphNode(graph) || hasWeightLoader(graph) {
			return Need{}, false
		}
		if _, doubt := unsizedWeight(graph, map[string]bool{}); doubt {
			return Need{}, false
		}
		return ProcessFloor("the graph"), true
	}
	stat := f.Stat
	if stat == nil {
		stat = defaultStat
	}
	sizes, _, ok := mediacap.ModelFileSizes(mediacap.ModelRoots(f.Cfg.ComfyDir), files.Models, stat)
	if !ok || len(sizes) != len(files.Models) { // the second is the contract of ModelFileSizes; a figure is never built on a short list
		return Need{}, false
	}
	// The scan lists only the top-level string inputs that end in one of its six extensions. A weight it did not list (another extension, a
	// nested widget object, a list) is a file the sizes above do not cover, and a sum without it is a figure that shrinks: on a card the
	// listed files fit it would declare 0 where the class default stood. Any such name keeps the class default.
	sized := make(map[string]bool, len(files.Models))
	for _, m := range files.Models {
		sized[m] = true
	}
	if _, doubt := unsizedWeight(graph, sized); doubt {
		return Need{}, false
	}
	var total float64
	var parts []string
	for i, name := range files.Models {
		g := float64(sizes[i]) / gib
		total += g
		if i < graphDetailFiles {
			parts = append(parts, fmt.Sprintf("%s %.1f", name, g))
		}
	}
	set := strings.Join(parts, " + ")
	if more := len(files.Models) - graphDetailFiles; more > 0 {
		set += fmt.Sprintf(" + %d more", more)
	}
	return fitToCard(SourceGraphLoaders, fmt.Sprintf("the graph's own loaders name %d weight file(s)", len(files.Models)), set, total, f, " (an estimate from the files' sizes, not a measurement)"), true
}

// ForGraphFile is ForGraph for the graph at path (`--graph <file>`). A path that is empty, not a regular file, larger than
// maxGraphBytes or unreadable is not sized: ok is false, as for a graph ForGraph cannot size. Only a regular file is opened, so a
// `--graph` that names a pipe or a device cannot block the verb that resolved it.
func ForGraphFile(path string, f Facts) (Need, bool) {
	raw, ok := readBoundedFile(path)
	if !ok {
		return Need{}, false
	}
	return ForGraph(raw, f)
}

// hasGraphNode reports whether the document is an API-format graph: a JSON object with at least one entry that is a node (an object with a class_type).
func hasGraphNode(graph []byte) bool {
	var nodes map[string]json.RawMessage
	if json.Unmarshal(graph, &nodes) != nil {
		return false
	}
	for _, raw := range nodes {
		var n struct {
			Class string `json:"class_type"`
		}
		if json.Unmarshal(raw, &n) == nil && n.Class != "" {
			return true
		}
	}
	return false
}

// hasWeightLoader reports whether a node's class is one that loads a weight file (a class_type with "loader", "checkpoint" or "lora" in it, or
// "load" with "model"): a graph with such a node and no weight file named for it has a file the scan did not see (fed by a link, or built
// at run time), so it is not the process floor. The class names of the input loaders (LoadImage, LoadVideo, LoadAudio) carry none of those.
func hasWeightLoader(graph []byte) bool {
	var nodes map[string]json.RawMessage
	if json.Unmarshal(graph, &nodes) != nil {
		return true
	}
	for _, raw := range nodes {
		var n struct {
			Class string `json:"class_type"`
		}
		if json.Unmarshal(raw, &n) != nil {
			continue
		}
		c := strings.ToLower(n.Class)
		if strings.Contains(c, "loader") || strings.Contains(c, "checkpoint") || strings.Contains(c, "lora") || (strings.Contains(c, "load") && strings.Contains(c, "model")) {
			return true
		}
	}
	return false
}

// readBoundedFile reads the regular file at path, at most maxGraphBytes of it: the read ForGraphFile and ForManifestFile share. A path
// that is empty, not a regular file, larger than the bound or unreadable is ok=false.
func readBoundedFile(path string) ([]byte, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxGraphBytes {
		return nil, false
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer fh.Close()
	raw, err := io.ReadAll(io.LimitReader(fh, maxGraphBytes+1)) // the file may have grown since the stat
	if err != nil || len(raw) > maxGraphBytes {
		return nil, false
	}
	return raw, true
}

// otherWeightExts are weight-like extensions beyond the scan's six that a loader of some custom node opens. The list is a floor, not
// the set of every weight format: a file this does not recognise is not seen (a limit, named in the docs).
var otherWeightExts = []string{".bin", ".pt2", ".pkl", ".pickle", ".onnx", ".engine", ".trt", ".npz", ".h5"}

// weightLike reports whether a string value ends in a weight extension, the scan's or one of otherWeightExts (case-insensitive).
func weightLike(v string) bool {
	l := strings.ToLower(strings.TrimSpace(v))
	for _, e := range mediacap.ModelExtensions() {
		if strings.HasSuffix(l, e) {
			return true
		}
	}
	for _, e := range otherWeightExts {
		if strings.HasSuffix(l, e) {
			return true
		}
	}
	return false
}

// unsizedWeight walks the WHOLE document, not only the node inputs the scan reads, and returns the first string value that looks like
// a weight file (weightLike) and is not among the names that were sized. An output prefix and a URL are not files, as for the scan.
// doubt is also true when the document cannot be walked, which cannot happen to one the scan read.
func unsizedWeight(graph []byte, sized map[string]bool) (name string, doubt bool) {
	var doc any
	if err := json.NewDecoder(bytes.NewReader(graph)).Decode(&doc); err != nil {
		return "", true
	}
	var walk func(key string, v any) (string, bool)
	walk = func(key string, v any) (string, bool) {
		switch x := v.(type) {
		case string:
			if key != "filename_prefix" && !strings.Contains(x, "://") && weightLike(x) && !sized[x] {
				return x, true
			}
		case []any:
			for _, e := range x {
				if n, d := walk(key, e); d {
					return n, true
				}
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if n, d := walk(k, x[k]); d {
					return n, true
				}
			}
		}
		return "", false
	}
	return walk("", doc)
}
