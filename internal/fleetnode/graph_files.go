package fleetnode

// The ACK check of a run-graph dispatch (P0 plan, S4; ADR 0082 amendment).
//
// A graph is the caller's, and it names files: the weights its loaders open and the files its input loaders (LoadImage and
// the like) read. A node that lacks one used to take its lease, cold-start ComfyUI and defer afterwards, so a delegator that
// tried several nodes paid a render's setup per miss. Admission now reads the graph before any job, temp file or lease exists
// and, when the node does not hold everything the graph names, answers with the existing re-placeable refusal
// (routeNotReadyError, 503) naming what is missing. That is also what makes "the node holds the files" an identity a
// delegator can rely on when it places a graph: the node itself decides it, at the door, from the files on its disk now.
//
// What "holds" means (mediacap/graphfiles.go): a weight file resolves by NAME under any class directory of any models root,
// as a ComfyUI loader would open it; an input file (an image, a video, an audio clip) resolves under the ComfyUI input (or
// output, or temp) directory. It is a name, not content, and the two differ in what that proves. A weight file's name is its
// identity in practice (a model is published under one). An input file is the caller's own and gets a generic name, so a
// same-named file of other bytes is held here and the graph would run on it; the delegator therefore never auto-overflows a
// graph that reads one (mediaremote/graph.go); a call that must leave its machine anyway (route remote, or auto on a machine
// with no graph lane of its own) reaches this check with one, and for it the check stays by name. A file
// the graph's manifest tells the node to fetch (a models[] entry with a source_url) is not missing: the satisfier downloads it
// before ComfyUI starts. Input files do not travel with a graph (the dispatch door carries JSON; the media-job door carries no
// input for run-graph), so one the node does not hold cannot be repaired by sending it: the refusal says so, and carrying them
// is a named seam, not built. Other input loaders (a custom node's own file key) are not read.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// RefusalNotCapable is the value of the "refusal" member in the body of a 503 that says "not THIS request" and not "not now": the
// node is not full and not out of route, it lacks a file this graph names (graphFilesRefusal). A delegator reads the member and
// not the sentence, so that a node that cannot run one graph is marked not-capable for that ticket alone and is not left alone as
// a busy one (ADR 0083 Amendment 1). Every other 503 of the dispatch door (queue full, a route that is not CONFIGURED) leaves the
// member out: those are the node's state, and a delegator is right to look elsewhere for a while.
const RefusalNotCapable = "not_capable"

// graphNamesCap is how many missing names one refusal lists. A graph with a long list is almost always a whole workflow this
// node was never meant to run; six names say so and keep the sentence one a caller reads. The delegator's cluster row quotes
// the sentence from the body's "error" member, cut at 600 bytes (mediaremote detailMax), so a graph missing many long names shows
// its first few there; the node's own 503 body lists up to six. For a weight file the node stops looking at the seventh missing name (the door is open
// to any peer, and a graph can name thousands of files), so past six it says "more" and not how many; for an input file,
// which is a short list, it counts the rest.
const graphNamesCap = 6

// graphFilesRefusal returns the refusal for a run-graph payload whose graph names a file this node does not hold, nil when
// it holds every one. graph and manifest are the payload's raw JSON (manifest may be absent). A graph the scan cannot read
// is not this check's to refuse: the builder's own validation has already ruled on it.
func graphFilesRefusal(cfg config.Config, graph, manifest []byte) *routeNotReadyError {
	files, err := mediacap.ScanGraph(graph)
	if err != nil || (len(files.Models) == 0 && len(files.Inputs) == 0) {
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
		if missing, more := mediacap.MissingModelFilesUpTo(mediacap.ModelRoots(cfg.ComfyDir), names, graphNamesCap); len(missing) > 0 {
			if more {
				parts = append(parts, fmt.Sprintf("the graph loads %d or more files this node does not hold: %s and more", graphNamesCap+1, strings.Join(missing, ", ")))
			} else {
				parts = append(parts, fmt.Sprintf("the graph loads %s this node does not hold: %s", plural(len(missing), "file", "files"), strings.Join(missing, ", ")))
			}
		}
	}
	if len(files.Inputs) > 0 {
		if missing := mediacap.MissingInputFiles(cfg.ComfyDir, files.Inputs); len(missing) > 0 {
			names := make([]string, len(missing))
			for i, m := range missing {
				names[i] = m.Name
				if m.Dir != "input" {
					names[i] += " [" + m.Dir + "]"
				}
			}
			parts = append(parts, fmt.Sprintf("the graph reads %s this node does not hold: %s (input files do not travel with a graph)",
				plural(len(missing), "input file", "input files"), listCapped(names)))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return &routeNotReadyError{task: "run-graph", detail: strings.Join(parts, "; ")}
}

// writeRouteNotReady answers a refused dispatch with 503. A refusal that is about THIS request (detail set: the graph names a file
// the node does not hold) carries the "refusal" member RefusalNotCapable, which tells the delegator the node is not full and not
// out of route; any other (a route that is not CONFIGURED) is the node's own state and is answered as it always was.
func writeRouteNotReady(w http.ResponseWriter, err error, nre *routeNotReadyError) {
	if nre.detail == "" {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "error": err.Error(), "refusal": RefusalNotCapable})
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
