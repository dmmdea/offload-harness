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
// retargets a model or names an embedding (the runner refuses one that tries; text that string nodes assemble while the graph runs is not seen, see docs/systems/mcp-server.md). The manifest is the catalog's static reading of the template and the graph is the
// converter's, two readers of one file, so the manifest's set is the final graph's only as far as they agree. The gate in front of a run
// (render/template-manifest.mjs verdictFor) refuses a template the catalog walk left a subgraph instance unexpanded in, and the catalog reads
// the widget values the converter routes into a loader (both forms of widgets_values, and an active instance's own), but neither makes the
// list complete by construction. The check that does is the runner's: before it posts a template run, comfy-run-graph.mjs compares the model
// files of the converted graph with the manifest (render/template-run.mjs unlistedModels) and defers TEMPLATE_MODEL_UNLISTED for any the
// manifest does not name, whatever the cause. A figure summed here from a short list can therefore be declared for a run that is then
// deferred, and never for one that loads the file the list lacked. The manifest carries no completeness mark, so one written by hand that
// leaves a loader out is sized as written. ForManifestModels sizes that set with the reader ForGraph uses (mediacap.ModelFileSizes under the box's
// model roots) and declares it by the same rule (fitToCard), source SourceGraphLoaders: it is the same kind of figure, the weight files a
// ComfyUI graph loads, read from the manifest and not from a document.
//
// It declares a figure only when it can stand behind all of it, as ForGraph does: an empty list, an entry that is not `models/<class>/<name>`
// (a null path included), or ONE file it cannot find or size is not sized at all, and the caller keeps the class default. A manifest whose
// `models` list is present and empty is a template that loads no weight file: it is the ComfyUI process floor (ProcessFloor), as a graph that names none is. There is no walk
// for a weight the list did not name, because the list is the set the gate, the licence check and the satisfier read, and an override cannot
// add a file to it. There is no Go-side backstop for an incomplete list (the class default kept when the manifest says so): the manifest has
// no field that could say so, and the runner's check above stops a run on a list that is short before anything is posted; a new field would
// change a file the satisfier reads too.
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
		if paths == nil { // no list at all is a manifest the lease could not read as one, not a template that loads no model
			return Need{}, false
		}
		return ProcessFloor("the template's runtime manifest"), true
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
	if doc.Models == nil { // no `models` list: not the shape of a runtime manifest
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
