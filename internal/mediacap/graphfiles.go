package mediacap

// What a ComfyUI API-format graph names that the node running it must HOLD (P0 plan, S4; ADR 0082 amendment).
//
// A fleet node that is handed a graph (run-graph) used to find out it lacked a weight file only after it had taken its
// lease and cold-started ComfyUI, and the preflight inside the runner then deferred. A node that checks the names at the
// door (internal/fleetnode buildRunGraph) answers 503 before any job exists, so the delegator places the graph on the
// next node instead. This file is the reading half: which names a graph carries, and which of them a tree does not hold (and,
// for the host-RAM estimate of a graph, how large the ones it holds are: ModelFileSizes).
//
// The harness never interprets a graph's semantics (internal/rungraph), and this stays generic on purpose:
//
//   - a model file is any string input whose value ends in one of the weight extensions, whatever the input is called:
//     ckpt_name, unet_name, clip_name1, vae_name, lora_name, but also a ModelPatchLoader's `name`, a wrapper loader's `model`,
//     an `ipadapter_file`, a primitive's `value`. The loaders' key names are open-ended (every custom node picks its own), the
//     extension is not. A link (["12", 0]), a number, a word without such an extension ("builtin", "None"), an output prefix
//     (filename_prefix) and a URL are not files to hold. The price is a text that itself ends in a weight extension: it reads
//     as a file name, and costs a re-placeable 503 (the graph still runs on its own machine).
//   - an input file is the file input of an input loader, in the directory its ComfyUI annotation names ("name.png [output]";
//     none = input): `image` of LoadImage, LoadImageMask and LoadImageOutput, `file` of LoadVideo, `audio` of LoadAudio.
//
// "Held" is by NAME, as ComfyUI's loaders open a file: the name relative to a class directory, subfolders included, never a
// search for the basename. It is not a comparison of content: a same-named file of other bytes is held. For an INPUT file (the
// caller's own, with a name like image.png that two machines easily share with different bytes) that is not enough to run a
// graph on a node instead of its own machine, so mediaremote keeps a graph that reads one off the auto-overflow. Loader nodes
// whose file input is none of the above (a custom node's own image or video key) are not read.

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

// GraphInput is an input file (an image, a video or an audio clip) that an input loader of a graph reads: the name, and which
// of ComfyUI's directories it is in.
type GraphInput struct {
	Name string
	// Dir is "input", "output" or "temp": the annotation on the name, "input" when it has none.
	Dir string
}

// GraphFiles is what a graph names.
type GraphFiles struct {
	// Models are the weight file names, each once, ordered by node id and then input key.
	Models []string
	// Inputs are the files an input loader reads (LoadImage, LoadVideo, LoadAudio), in node id order.
	Inputs []GraphInput
}

// inputLoaders are the input loader classes the scan reads, each with the name of the input that holds the file.
var inputLoaders = map[string]string{
	"LoadImage": "image", "LoadImageMask": "image", "LoadImageOutput": "image",
	"LoadVideo": "file", "LoadAudio": "audio",
}

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
			case isInputKey(n.Class, k) && strings.TrimSpace(v) != "":
				out.Inputs = append(out.Inputs, imageOf(v))
			case k != "filename_prefix" && !strings.Contains(v, "://") && isModelFile(v):
				if !seen[v] {
					seen[v] = true
					out.Models = append(out.Models, v)
				}
			}
		}
	}
	return out, nil
}

// isInputKey reports whether key is the file input of an input loader of the given class.
func isInputKey(class, key string) bool {
	want, ok := inputLoaders[class]
	return ok && key == want
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

func imageOf(v string) GraphInput {
	if m := annotation.FindStringSubmatch(v); m != nil {
		return GraphInput{Name: m[1], Dir: m[2]}
	}
	return GraphInput{Name: v, Dir: "input"}
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
//
// It lists every missing name; MissingModelFilesUpTo is the form that stops looking.
func MissingModelFiles(roots []ModelRoot, names []string) []string {
	missing, _ := MissingModelFilesUpTo(roots, names, 0)
	return missing
}

// statModel is the stat of one candidate path of a weight file: osStatSize, a seam so a test can count how many a call makes.
var statModel = osStatSize

// MissingModelFilesUpTo is MissingModelFiles for a caller that shows only the first few: it returns at most limit of the
// missing names, in order, and more is true when there was a further one (it looks no further than the limit+1th missing
// name). A limit of 0 or less means no limit, and more is then false.
//
// The cost of a call is bounded by the limit and not by the graph. A graph can name thousands of files (a dispatch body may
// be 1 MiB), the door that asks answers any holder of the fleet bearer on the tailnet, and a name that is missing costs one stat per class
// directory of every root, so a name-by-name resolve of a 16,875-name graph took 3.5 s on a 20-class-directory tree (a
// scratch measurement, 2026-10-10, one host) before it ruled. Two things bound it now: each root's class directories are
// listed ONCE per call, not once per name (the listing of a root is a directory read plus a stat for every entry that is
// not itself a directory), and the search stops at the limit+1th missing name. The count of the missing names beyond that
// is therefore not known, and a caller says "more" and not a number.
func MissingModelFilesUpTo(roots []ModelRoot, names []string, limit int) (missing []string, more bool) {
	var dirs []string
	for _, r := range roots {
		for _, cd := range classDirs(r) {
			dirs = append(dirs, cd[1])
		}
	}
	for _, name := range names {
		held := false
		if rel, ok := safeRel(name); ok {
			rel = filepath.FromSlash(rel)
			for _, dir := range dirs {
				if _, found := statModel(filepath.Join(dir, rel)); found {
					held = true
					break
				}
			}
		}
		if held {
			continue
		}
		if limit > 0 && len(missing) == limit {
			return missing, true
		}
		missing = append(missing, name)
	}
	return missing, false
}

// MissingInputFiles returns, in order, the input files that are not under comfyDir's input, output or temp directory (as each
// one's annotation says). An empty comfyDir holds nothing. By name only (see the top of this file).
func MissingInputFiles(comfyDir string, inputs []GraphInput) []GraphInput {
	var missing []GraphInput
	for _, im := range inputs {
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

// ModelFileSizes returns the size in bytes of each of names, in order: the reading of a graph's weight files for the one reader
// that needs their SIZES, the host-RAM estimate of a graph (internal/hostneed.ForGraph). A name is looked up exactly as
// MissingModelFiles looks it up, under every class directory of every root and only as a safe relative path (a graph carries no
// class, see the top of this file), and the roots are listed once per call.
//
// It is all or nothing: the first name that does not resolve ends the look and is returned in missing, with ok false and no
// sizes, because a sum that leaves a file out is a figure that shrinks, and the caller keeps what it had. A file of no length
// is not a size and reads as unresolved. A name held under more than one class directory is sized as the LARGER of them: the
// graph does not say which the loader opens, and the smaller would shrink the figure. stat is os.Stat unless a test supplies a
// table.
func ModelFileSizes(roots []ModelRoot, names []string, stat func(path string) (size int64, ok bool)) (sizes []int64, missing string, ok bool) {
	if stat == nil {
		stat = statModel
	}
	var dirs []string
	for _, r := range roots {
		for _, cd := range classDirs(r) {
			dirs = append(dirs, cd[1])
		}
	}
	for _, name := range names {
		var largest int64
		if rel, safe := safeRel(name); safe {
			rel = filepath.FromSlash(rel)
			for _, dir := range dirs {
				if n, found := stat(filepath.Join(dir, rel)); found && n > largest {
					largest = n
				}
			}
		}
		if largest <= 0 {
			return nil, name, false
		}
		sizes = append(sizes, largest)
	}
	return sizes, "", true
}

// ModelExtensions are the weight file extensions the scan reads a loader's file by (a copy: the list is the scan's own).
func ModelExtensions() []string { return append([]string(nil), modelExts...) }
