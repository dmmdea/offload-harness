package fleetnode

// The ACK check of a run-graph dispatch (P0 plan, S4; ADR 0082 amendment).
//
// A graph is the caller's, and it names files: the weights its loaders open and the images its LoadImage nodes read. A
// node that lacks one used to take its lease, cold-start ComfyUI and defer afterwards, so a delegator that tried several
// nodes paid a render's setup per miss. Admission now reads the graph before any job, temp file or lease exists and, when
// the node does not hold everything the graph names, answers with the existing re-placeable refusal (routeNotReadyError,
// 503) naming what is missing. That is also what makes "the node holds the files" an identity a delegator can rely on when
// it places a graph: the node itself decides it, at the door, from the files on its disk now.
//
// What "holds" means (mediacap/graphfiles.go): a weight file resolves by NAME under any class directory of any models root,
// as a ComfyUI loader would open it; an input image resolves under the ComfyUI input (or output, or temp) directory. It is
// a name, not content: a same-named file of other bytes is held, as it is for a recipe digest. A file the graph's
// manifest tells the node to fetch (a models[] entry with a source_url) is not missing: the satisfier downloads it before
// ComfyUI starts. Input images do not travel with a graph (the dispatch door carries JSON; the media-job door carries no
// input for run-graph), so a LoadImage the node does not hold cannot be repaired by sending it: the refusal says so, and
// carrying them is a named seam, not built. Other input loaders (LoadVideo, LoadAudio, a custom node's own file key) are not
// read.

import (
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// graphNamesCap is how many missing names one refusal lists before it counts the rest. A graph with a long list is almost
// always a whole workflow this node was never meant to run; six names say so and keep the sentence one a caller reads.
const graphNamesCap = 6

// graphFilesRefusal returns the refusal for a run-graph payload whose graph names a file this node does not hold, nil when
// it holds every one. graph and manifest are the payload's raw JSON (manifest may be absent). A graph the scan cannot read
// is not this check's to refuse: the builder's own validation has already ruled on it.
func graphFilesRefusal(cfg config.Config, graph, manifest []byte) *routeNotReadyError {
	files, err := mediacap.ScanGraph(graph)
	if err != nil || (len(files.Models) == 0 && len(files.Images) == 0) {
		return nil
	}
	var parts []string
	if len(files.Models) > 0 {
		fetched := mediacap.ManifestSourcedModels(manifest)
		var names []string
		for _, n := range files.Models {
			if !fetched[strings.ReplaceAll(n, `\`, "/")] {
				names = append(names, n)
			}
		}
		if missing := mediacap.MissingModelFiles(mediacap.ModelRoots(cfg.ComfyDir), names); len(missing) > 0 {
			parts = append(parts, fmt.Sprintf("the graph loads %s this node does not hold (looked under every class directory of its model roots, by the name the loader opens): %s",
				plural(len(missing), "file", "files"), listCapped(missing)))
		}
	}
	if len(files.Images) > 0 {
		if missing := mediacap.MissingInputImages(cfg.ComfyDir, files.Images); len(missing) > 0 {
			names := make([]string, len(missing))
			for i, m := range missing {
				names[i] = m.Name
				if m.Dir != "input" {
					names[i] += " [" + m.Dir + "]"
				}
			}
			parts = append(parts, fmt.Sprintf("the graph reads %s this node does not hold: %s (input images do not travel with a graph: a LoadImage input must already be on the node)",
				plural(len(missing), "input image", "input images"), listCapped(names)))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return &routeNotReadyError{task: "run-graph", detail: strings.Join(parts, "; ")}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func listCapped(names []string) string {
	if len(names) <= graphNamesCap {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:graphNamesCap], ", "), len(names)-graphNamesCap)
}
