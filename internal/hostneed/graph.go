package hostneed

import (
	"fmt"
	"io"
	"os"
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
// It declares a figure only when it can stand behind all of it. A graph it cannot read, one that names no weight file, or ONE file
// it cannot find or size is not sized at all, and the caller keeps the class default exactly as before: a size that is not
// known must never shrink a declaration.

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
// ok is false, and the caller keeps the media class default, when graph is not a JSON object, names no weight file, or names one
// that does not resolve under the box's model roots (or is empty). Facts.Stat sizes the files when set, as for every estimate.
func ForGraph(graph []byte, f Facts) (Need, bool) {
	files, err := mediacap.ScanGraph(graph)
	if err != nil || len(files.Models) == 0 {
		return Need{}, false
	}
	stat := f.Stat
	if stat == nil {
		stat = defaultStat
	}
	sizes, _, ok := mediacap.ModelFileSizes(mediacap.ModelRoots(f.Cfg.ComfyDir), files.Models, stat)
	if !ok || len(sizes) != len(files.Models) { // the second is the contract of ModelFileSizes; a figure is never built on a short list
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
	path = strings.TrimSpace(path)
	if path == "" {
		return Need{}, false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxGraphBytes {
		return Need{}, false
	}
	fh, err := os.Open(path)
	if err != nil {
		return Need{}, false
	}
	defer fh.Close()
	raw, err := io.ReadAll(io.LimitReader(fh, maxGraphBytes+1)) // the file may have grown since the stat
	if err != nil || len(raw) > maxGraphBytes {
		return Need{}, false
	}
	return ForGraph(raw, f)
}
