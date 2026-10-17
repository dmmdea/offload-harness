package mediacap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	pinVHS      = "4d907bee61e92c2e65af3bd6383a4e4d356126d1"
	pinMultiGPU = "ed1ffaef7cec1a66f35106c6a4c7a40927c2dc83"
)

func manifestWith(packs string) []byte {
	return []byte(`{"schema_version":1,"node_packs":` + packs + `,"models":[]}`)
}

// A manifest may name a pack only when its repository and its commit are the pinned pair: a node that clones
// what a caller names runs the caller's code.
func TestAManifestMayNameOnlyAPinnedPack(t *testing.T) {
	const vhs = "https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite"
	cases := []struct {
		name     string
		packs    string
		wantPack string // "" = accepted
	}{
		{"no packs", `[]`, ""},
		{"packs absent", `null`, ""},
		{"a pinned pack", `[{"name":"ComfyUI-VideoHelperSuite","repo":"` + vhs + `","commit":"` + pinVHS + `"}]`, ""},
		{"a pinned pack with the name left out", `[{"repo":"` + vhs + `","commit":"` + pinVHS + `"}]`, ""},
		{"the archived pack at its pin, upstream", `[{"repo":"https://github.com/pollockjj/ComfyUI-MultiGPU","commit":"` + pinMultiGPU + `"}]`, ""},
		{"the archived pack at its pin, mirror", `[{"repo":"https://github.com/dmmdea/ComfyUI-MultiGPU-mirror","commit":"` + pinMultiGPU + `"}]`, ""},
		{"an unlisted repository", `[{"name":"payload","repo":"https://example.invalid/payload","commit":"` + pinVHS + `"}]`, "payload"},
		{"a listed repository at another commit", `[{"repo":"` + vhs + `","commit":"0000000000000000000000000000000000000000"}]`, "ComfyUI-VideoHelperSuite"},
		{"a listed repository at a branch name", `[{"repo":"` + vhs + `","commit":"main"}]`, "ComfyUI-VideoHelperSuite"},
		{"a listed repository at an abbreviated commit", `[{"repo":"` + vhs + `","commit":"4d907bee"}]`, "ComfyUI-VideoHelperSuite"},
		{"the pinned commit of ANOTHER pack's repository", `[{"repo":"` + vhs + `","commit":"` + pinMultiGPU + `"}]`, "ComfyUI-VideoHelperSuite"},
		{"a pinned pair with a trailing slash", `[{"repo":"` + vhs + `/","commit":"` + pinVHS + `"}]`, "ComfyUI-VideoHelperSuite"},
		{"a pinned pack among an unlisted one", `[{"repo":"` + vhs + `","commit":"` + pinVHS + `"},{"name":"second","repo":"https://example.invalid/b","commit":"` + pinVHS + `"}]`, "second"},
		{"a name that climbs out of custom_nodes", `[{"name":"../../x","repo":"` + vhs + `","commit":"` + pinVHS + `"}]`, "../../x"},
		{"a name that is another pack's directory", `[{"name":"ComfyUI-Manager","repo":"` + vhs + `","commit":"` + pinVHS + `"}]`, "ComfyUI-Manager"},
		{"a pack with no commit", `[{"repo":"` + vhs + `"}]`, "ComfyUI-VideoHelperSuite"},
		{"a pack with no repository", `[{"name":"x","commit":"` + pinVHS + `"}]`, "x"},
		{"a repository key spelled in another case beside the real one", `[{"repo":"https://example.invalid/x","REPO":"` + vhs + `","commit":"` + pinVHS + `"}]`, "x"},
		{"a pack that is not an object", `["` + vhs + `"]`, "#0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckManifestNodePacks(manifestWith(c.packs))
			if c.wantPack == "" {
				if err != nil {
					t.Fatalf("refused a manifest that names only pinned packs: %v", err)
				}
				return
			}
			var refused *NodePackRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("err = %v, want a *NodePackRefusedError", err)
			}
			if refused.Pack != c.wantPack || !strings.Contains(err.Error(), c.wantPack) {
				t.Errorf("the refusal names %q (%v), want the pack %q", refused.Pack, err, c.wantPack)
			}
		})
	}
}

func TestAManifestWhoseNodePacksIsNotAListIsRefused(t *testing.T) {
	for _, packs := range []string{`{"a":1}`, `"ComfyUI-GGUF"`, `7`, `true`} {
		var refused *NodePackRefusedError
		if err := CheckManifestNodePacks(manifestWith(packs)); !errors.As(err, &refused) {
			t.Errorf("node_packs %s: err = %v, want a *NodePackRefusedError", packs, err)
		}
	}
	for _, body := range [][]byte{nil, []byte(`null`), []byte(`{}`), []byte(`{"models":[]}`)} {
		if err := CheckManifestNodePacks(body); err != nil {
			t.Errorf("a manifest %q that names no packs was refused: %v", body, err)
		}
	}
}

// render/node-packs.json is the runner's table and the file a reviewer edits; this table is the node's copy of it.
func TestNodePackPinsMatchTheRunnersFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "render", "node-packs.json"))
	if err != nil {
		t.Fatalf("the runner's table: %v", err)
	}
	var file struct {
		Packs []struct {
			Name   string `json:"name"`
			Repo   string `json:"repo"`
			Commit string `json:"commit"`
		} `json:"packs"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Packs) == 0 {
		t.Fatal("render/node-packs.json lists no pack")
	}
	var fromFile []NodePackPin
	for _, p := range file.Packs {
		fromFile = append(fromFile, NodePackPin{Name: p.Name, Repo: p.Repo, Commit: p.Commit})
	}
	got := NodePackPins()
	if len(got) != len(fromFile) {
		t.Fatalf("the node's table has %d rows, render/node-packs.json has %d", len(got), len(fromFile))
	}
	for i := range got {
		if got[i] != fromFile[i] {
			t.Errorf("row %d: node %+v, runner file %+v", i, got[i], fromFile[i])
		}
	}
}

// Every pack the doctor tells an operator to install is a pack the manifest door would take at the documented pin.
func TestNodePackPinsCoverEveryPackTheDoctorNames(t *testing.T) {
	pinned := map[string]bool{}
	for _, p := range NodePackPins() {
		pinned[p.Repo] = true
	}
	for name, np := range nodePacks {
		if !pinned[np.url] {
			t.Errorf("%s (%s) is a pack the doctor names and has no pinned row", name, np.url)
		}
	}
	for _, p := range NodePackPins() {
		if len(p.Commit) != 40 || strings.Trim(p.Commit, "0123456789abcdef") != "" {
			t.Errorf("%s: commit %q is not a full lower-case commit id", p.Repo, p.Commit)
		}
		if p.Repo == multiGPUMirrorURL && p.Commit != multiGPUPinnedCommit {
			t.Errorf("the mirror is pinned at %s, the documented pin is %s", p.Commit, multiGPUPinnedCommit)
		}
	}
}
