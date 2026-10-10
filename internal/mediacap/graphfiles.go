package mediacap

// What a ComfyUI API-format graph names that the node running it must HOLD (P0 plan, S4; ADR 0082 amendment).
//
// A fleet node that is handed a graph (run-graph) used to find out it lacked a weight file only after it had taken its
// lease and cold-started ComfyUI, and the preflight inside the runner then deferred. A node that checks the names at the
// door (internal/fleetnode buildRunGraph) answers 503 before any job exists, so the delegator places the graph on the
// next node instead. This file is the reading half: which names a graph carries, and which of them a tree does not hold.
//
// The harness never interprets a graph's semantics (internal/rungraph), and this stays generic on purpose:
//
//   - a model file is any string input, under a key that contains "_name" (ckpt_name, unet_name, clip_name1, vae_name,
//     lora_name, control_net_name, model_name, ...), whose value ends in one of the weight extensions. A link
//     (["12", 0]), a number or a word without such an extension ("builtin", "None") is not a file.
//   - an input image is the `image` input of a LoadImage, LoadImageMask or LoadImageOutput node, in the directory its
//     ComfyUI annotation names ("name.png [output]"; none = input).
//
// "Held" is by NAME, as ComfyUI's loaders open a file: the name relative to a class directory, subfolders included, never a
// search for the basename. It is not a comparison of content: a same-named file of other bytes is held. Loader nodes whose
// file input is not named like that (a text prompt, a LoadVideo or LoadAudio node, a custom node's own key) are not read.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// modelExts are the weight file extensions a loader opens (ComfyUI's supported_pt_extensions and GGUF).
var modelExts = []string{".safetensors", ".gguf", ".pth", ".pt", ".ckpt", ".sft"}

// GraphImage is an input image a graph's LoadImage node reads: the name, and which of ComfyUI's directories it is in.
type GraphImage struct {
	Name string
	// Dir is "input", "output" or "temp": the annotation on the name, "input" when it has none.
	Dir string
}

// GraphFiles is what a graph names.
type GraphFiles struct {
	// Models are the weight file names, each once, ordered by node id and then input key.
	Models []string
	// Images are the LoadImage inputs, in node id order.
	Images []GraphImage
}

var imageNodes = map[string]bool{"LoadImage": true, "LoadImageMask": true, "LoadImageOutput": true}

var annotation = regexp.MustCompile(`^(.*) \[(input|output|temp)\]$`)

// ScanGraph reads the files a graph names. It is an error only for a document that is not a JSON object; a node or an
// input it cannot read is skipped (ComfyUI's own preflight is the authority on a malformed graph).
func ScanGraph(graph []byte) (GraphFiles, error) {
	var nodes map[string]json.RawMessage
	if err := json.Unmarshal(graph, &nodes); err != nil {
		return GraphFiles{}, fmt.Errorf("graph must be a JSON object: %w", err)
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out GraphFiles
	seen := map[string]bool{}
	for _, id := range ids {
		var n struct {
			Class  string                     `json:"class_type"`
			Inputs map[string]json.RawMessage `json:"inputs"`
		}
		if json.Unmarshal(nodes[id], &n) != nil {
			continue
		}
		keys := make([]string, 0, len(n.Inputs))
		for k := range n.Inputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			var v string
			if json.Unmarshal(n.Inputs[k], &v) != nil {
				continue
			}
			switch {
			case strings.Contains(strings.ToLower(k), "_name") && isModelFile(v):
				if !seen[v] {
					seen[v] = true
					out.Models = append(out.Models, v)
				}
			case k == "image" && imageNodes[n.Class] && strings.TrimSpace(v) != "":
				out.Images = append(out.Images, imageOf(v))
			}
		}
	}
	return out, nil
}

func isModelFile(v string) bool {
	l := strings.ToLower(strings.TrimSpace(v))
	for _, e := range modelExts {
		if strings.HasSuffix(l, e) {
			return true
		}
	}
	return false
}

func imageOf(v string) GraphImage {
	if m := annotation.FindStringSubmatch(v); m != nil {
		return GraphImage{Name: m[1], Dir: m[2]}
	}
	return GraphImage{Name: v, Dir: "input"}
}

// safeRel turns a name a peer sent into a relative slash path that cannot leave the directory it is joined to: no empty
// name, no absolute path or drive letter, no ".." segment, no NUL. ok is false for anything else, and such a name is
// never looked up.
func safeRel(name string) (string, bool) {
	n := strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	if n == "" || strings.ContainsRune(n, 0) || strings.HasPrefix(n, "/") || (len(n) >= 2 && n[1] == ':') {
		return "", false
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return "", false
		}
	}
	return n, true
}

// MissingModelFiles returns, in order, the names that do not resolve under ANY class directory of ANY root. The class is
// not read from the node that loads the file: the harness does not know every loader (a custom node names its own
// class), and a node holding the file under a class its loader will not open fails in ComfyUI's own preflight, which
// stays the authority. A name that is not a safe relative path is missing.
func MissingModelFiles(roots []ModelRoot, names []string) []string {
	var classes []string
	seen := map[string]bool{}
	for _, r := range roots {
		for _, cd := range classDirs(r) {
			if !seen[cd[0]] {
				seen[cd[0]] = true
				classes = append(classes, cd[0])
			}
		}
	}
	var missing []string
	for _, name := range names {
		rel, ok := safeRel(name)
		if ok {
			_, ok = ResolveModelFile(roots, ModelFile{Name: rel, Classes: classes}, nil)
		}
		if !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// MissingInputImages returns, in order, the input images that are not under comfyDir's input, output or temp directory
// (as each one's annotation says). An empty comfyDir holds nothing.
func MissingInputImages(comfyDir string, images []GraphImage) []GraphImage {
	var missing []GraphImage
	for _, im := range images {
		held := false
		if rel, ok := safeRel(im.Name); ok && comfyDir != "" {
			if _, found := osStatSize(filepath.Join(comfyDir, im.Dir, filepath.FromSlash(rel))); found {
				held = true
			}
		}
		if !held {
			missing = append(missing, im)
		}
	}
	return missing
}

// ManifestSourcedModels is the set of model names (the part after models/<class>/) that a run-graph manifest tells the
// node to FETCH: the entries with a source_url. The node's satisfier downloads them before it starts ComfyUI, so a graph
// that names one is not missing it. An entry without a source_url is not in the set: the satisfier defers when such a
// file is absent. A manifest that does not parse provisions nothing.
func ManifestSourcedModels(manifest []byte) map[string]bool {
	out := map[string]bool{}
	var m struct {
		Models []struct {
			Path      string `json:"path"`
			SourceURL string `json:"source_url"`
		} `json:"models"`
	}
	if len(manifest) == 0 || json.Unmarshal(manifest, &m) != nil {
		return out
	}
	for _, e := range m.Models {
		if strings.TrimSpace(e.SourceURL) == "" {
			continue
		}
		p := strings.ReplaceAll(strings.TrimSpace(e.Path), `\`, "/")
		parts := strings.SplitN(p, "/", 3)
		if len(parts) == 3 && parts[0] == "models" && parts[2] != "" {
			out[parts[2]] = true
		}
	}
	return out
}
