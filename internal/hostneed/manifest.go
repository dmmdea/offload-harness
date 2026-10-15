package hostneed

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// A TEMPLATE run (`comfy-run-graph.mjs --template NAME --manifest FILE`, offload_run_template) carries no graph file: the runner converts the
// template once ComfyUI is up. What it loads is the model files of its runtime manifest, the list the readiness check and the licence gate
// already read: `models/<class>/<name>` per entry, each found on the node before the run starts. An override sets values and never
// retargets a model (the door refuses one that tries), so the manifest's set is the final graph's. ForManifestModels sizes that set with the
// reader ForGraph uses (mediacap.ModelFileSizes under the box's model roots) and declares it by the same rule (fitToCard), source
// SourceGraphLoaders: it is the same kind of figure, the weight files a ComfyUI graph loads, read from the manifest and not from a document.
//
// It declares a figure only when it can stand behind all of it, as ForGraph does: an empty list, an entry that is not `models/<class>/<name>`
// (a null path included), or ONE file it cannot find or size is not sized at all, and the caller keeps the class default. There is no walk
// for a weight the list did not name, because the list is the set the gate, the licence check and the satisfier read, and an override cannot
// add a file to it.
//
// LIMIT: the lookup is by name under every class directory of every root (ModelFileSizes: the graph path has no class to go by), so a name
// held under two classes is sized as the larger. The manifest does name the class, which would be exact; the larger can only raise the figure.

// ForManifestModels sizes a template by the model files its runtime manifest names, given as the manifest writes them
// (`models/<class>/<name>`, the name possibly nested): the SUM of their sizes, each file once, fitted to the card by the family
// estimate's rule (in full when together they do not fit, or the card is not known; 0 when they fit). The figure is an estimate from the
// files' sizes, not a measurement. ok is false, and the caller keeps the media class default, as for ForGraph.
func ForManifestModels(paths []string, f Facts) (Need, bool) {
	var names []string
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		key, name, ok := manifestModelName(p)
		if !ok {
			return Need{}, false
		}
		if !seen[key] {
			seen[key] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return Need{}, false
	}
	stat := f.Stat
	if stat == nil {
		stat = defaultStat
	}
	sizes, _, ok := mediacap.ModelFileSizes(mediacap.ModelRoots(f.Cfg.ComfyDir), names, stat)
	if !ok || len(sizes) != len(names) { // the second is the contract of ModelFileSizes; a figure is never built on a short list
		return Need{}, false
	}
	var total float64
	var parts []string
	for i, name := range names {
		g := float64(sizes[i]) / gib
		total += g
		if i < graphDetailFiles {
			parts = append(parts, fmt.Sprintf("%s %.1f", name, g))
		}
	}
	set := strings.Join(parts, " + ")
	if more := len(names) - graphDetailFiles; more > 0 {
		set += fmt.Sprintf(" + %d more", more)
	}
	return fitToCard(SourceGraphLoaders, fmt.Sprintf("the template's runtime manifest names %d model file(s)", len(names)), set, total, f, " (an estimate from the files' sizes, not a measurement)"), true
}

// ForManifestFile is ForManifestModels for the runtime manifest at path (`--manifest <file>`, the run's `manifest_path`): a JSON object
// with a `models` list whose entries carry a `path`. A path that is empty, not a regular file, larger than maxGraphBytes or unreadable,
// a document that is not that shape, or an entry with no path is not sized: ok is false, as for a manifest ForManifestModels cannot size.
func ForManifestFile(path string, f Facts) (Need, bool) {
	raw, ok := readBoundedFile(path)
	if !ok {
		return Need{}, false
	}
	var doc struct {
		Models []struct {
			Path string `json:"path"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return Need{}, false
	}
	paths := make([]string, 0, len(doc.Models))
	for _, m := range doc.Models {
		paths = append(paths, m.Path)
	}
	return ForManifestModels(paths, f)
}

// manifestModelName splits a manifest model path `models/<class>/<name>` into the key it is counted once by and the name ModelFileSizes
// looks up under the class directories (the part after the class, possibly nested). A path of any other shape is ok=false; one that
// would leave its class directory is refused by ModelFileSizes (it does not resolve), which also keeps the class default.
func manifestModelName(p string) (key, name string, ok bool) {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	parts := strings.SplitN(p, "/", 3)
	if len(parts) != 3 || parts[0] != "models" || parts[1] == "" || parts[1] == ".." || parts[2] == "" {
		return "", "", false
	}
	return p, parts[2], true
}
