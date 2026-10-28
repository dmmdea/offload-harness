package mediacap

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The node packs a run-graph manifest may name.
//
// A manifest's node_packs makes the node `git clone` each repository (or fast-forward it) and pip-install the
// pack's requirements into its ComfyUI venv: the caller's code, running on the node. The fleet bearer lets a caller
// run renders, not code, so a manifest is taken only when every pack it names is a row below, by repository, under
// the row's directory name. The node installs the head of the repository's default branch and records the commit it
// got (render/manifest-satisfy.mjs); a commit a manifest names is not read. render/node-packs.json is the runner's
// copy of the same table and the file a reviewer edits (the runner refuses on it too, before its first git command);
// TestNodePackRowsMatchTheRunnersFile holds the two together.
//
// The match is exact (see render/node-packs.mjs): the string compared is the one `git clone` would be given.

// NodePackPin is one pack a manifest may name: the directory it installs under and the repository to clone.
type NodePackPin struct {
	Name string
	Repo string
}

// nodePackPins mirrors render/node-packs.json, row for row. MultiGPU is listed twice, upstream (archived) and the
// private mirror (routeneeds.go).
var nodePackPins = []NodePackPin{
	{"ComfyUI-VideoHelperSuite", "https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite"},
	{"ComfyUI-MultiGPU", "https://github.com/pollockjj/ComfyUI-MultiGPU"},
	{"ComfyUI-MultiGPU", multiGPUMirrorURL},
	{"ComfyUI-GGUF", "https://github.com/city96/ComfyUI-GGUF"},
}

// NodePackPins returns the allowlisted packs.
func NodePackPins() []NodePackPin { return append([]NodePackPin(nil), nodePackPins...) }

// NodePackRefusedError is a manifest's refusal: it names a pack whose repository this node does not allowlist.
type NodePackRefusedError struct {
	// Pack is the pack as the manifest named it (its name, else the name its repository gives it, else its place in the list).
	Pack string
	// Reason says which part of the pack did not match.
	Reason string
}

func (e *NodePackRefusedError) Error() string {
	return fmt.Sprintf("run-graph manifest: node pack %q is not allowed (%s); a manifest may name only the allowlisted node packs, by repository (render/node-packs.json)", clipPack(e.Pack), e.Reason)
}

// clipPack bounds what a refusal echoes of a caller's text.
func clipPack(s string) string {
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "..."
	}
	return s
}

// packNameFromRepo is render/manifest.mjs packNameFromRepo: the directory name a manifest's pack gets when it gives none.
func packNameFromRepo(repo string) string {
	if n := len(repo); n >= 4 && strings.EqualFold(repo[n-4:], ".git") {
		repo = repo[:n-4]
	}
	repo = strings.TrimRight(repo, `/\`)
	if i := strings.LastIndexAny(repo, `/\`); i >= 0 {
		repo = repo[i+1:]
	}
	var b strings.Builder
	for _, r := range repo {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if out := strings.TrimLeft(b.String(), "-"); out != "" {
		return out
	}
	return "pack"
}

// CheckManifestNodePacks refuses a run-graph manifest whose node_packs names anything but an allowlisted pack, with a
// *NodePackRefusedError naming the first one that is not. A manifest with no node_packs is fine. The keys are read
// exactly as the runner reads them (case-sensitively), so a spelling the runner would ignore cannot carry a value
// this check read.
func CheckManifestNodePacks(manifest []byte) error {
	if s := strings.TrimSpace(string(manifest)); s == "" || s == "null" {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(manifest, &top); err != nil {
		return &NodePackRefusedError{Pack: "manifest", Reason: "the manifest is not a JSON object"}
	}
	raw, ok := top["node_packs"]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var packs []json.RawMessage
	if err := json.Unmarshal(raw, &packs); err != nil {
		return &NodePackRefusedError{Pack: "node_packs", Reason: "node_packs is not a list"}
	}
	for i, pr := range packs {
		if err := checkNodePack(i, pr); err != nil {
			return err
		}
	}
	return nil
}

func checkNodePack(i int, raw json.RawMessage) error {
	slot := fmt.Sprintf("#%d", i)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return &NodePackRefusedError{Pack: slot, Reason: "the pack is not an object"}
	}
	str := func(key string) (string, bool) {
		v, present := obj[key]
		if !present {
			return "", true
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", false
		}
		return s, true
	}
	name, nameOK := str("name")
	repo, repoOK := str("repo")
	label := slot
	switch {
	case name != "":
		label = name
	case repo != "":
		label = packNameFromRepo(repo)
	}
	refuse := func(why string) error { return &NodePackRefusedError{Pack: label, Reason: why} }
	if !nameOK || !repoOK {
		return refuse("name and repo must be strings")
	}
	if repo == "" {
		return refuse("the pack names no repository")
	}
	var row *NodePackPin
	for k := range nodePackPins {
		if nodePackPins[k].Repo == repo {
			row = &nodePackPins[k]
			break
		}
	}
	if row == nil {
		return refuse(fmt.Sprintf("%q is not an allowlisted repository", clipPack(repo)))
	}
	if name != "" && name != row.Name && name != packNameFromRepo(row.Repo) {
		return refuse(fmt.Sprintf("the directory name %q is not %q", clipPack(name), row.Name))
	}
	return nil
}
