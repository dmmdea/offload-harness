package mediacap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func manifestWith(packs string) []byte {
	return []byte(`{"schema_version":1,"node_packs":` + packs + `,"models":[]}`)
}

// A manifest may name a pack only when its repository is an allowlisted one: a node that clones what a caller
// names runs the caller's code. Admission is by repository; a commit the manifest names is not read.
func TestAManifestMayNameOnlyAnAllowlistedPack(t *testing.T) {
	const vhs = "https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite"
	cases := []struct {
		name     string
		packs    string
		wantPack string // "" = accepted
	}{
		{"no packs", `[]`, ""},
		{"packs absent", `null`, ""},
		{"an allowlisted pack", `[{"name":"ComfyUI-VideoHelperSuite","repo":"` + vhs + `"}]`, ""},
		{"an allowlisted pack with the name left out", `[{"repo":"` + vhs + `"}]`, ""},
		{"an allowlisted pack naming a commit it will not be held to", `[{"repo":"` + vhs + `","commit":"main"}]`, ""},
		{"the archived pack, upstream", `[{"repo":"https://github.com/pollockjj/ComfyUI-MultiGPU"}]`, ""},
		{"the archived pack, mirror", `[{"repo":"https://github.com/dmmdea/ComfyUI-MultiGPU-mirror"}]`, ""},
		{"an unlisted repository", `[{"name":"payload","repo":"https://example.invalid/payload"}]`, "payload"},
		{"an allowlisted repository with a trailing slash", `[{"repo":"` + vhs + `/"}]`, "ComfyUI-VideoHelperSuite"},
		{"an allowlisted pack among an unlisted one", `[{"repo":"` + vhs + `"},{"name":"second","repo":"https://example.invalid/b"}]`, "second"},
		{"a name that climbs out of custom_nodes", `[{"name":"../../x","repo":"` + vhs + `"}]`, "../../x"},
		{"a name that is another pack's directory", `[{"name":"ComfyUI-Manager","repo":"` + vhs + `"}]`, "ComfyUI-Manager"},
		{"a pack with no repository", `[{"name":"x"}]`, "x"},
		{"a repository key spelled in another case beside the real one", `[{"repo":"https://example.invalid/x","REPO":"` + vhs + `"}]`, "x"},
		{"a pack that is not an object", `["` + vhs + `"]`, "#0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckManifestNodePacks(manifestWith(c.packs))
			if c.wantPack == "" {
				if err != nil {
					t.Fatalf("refused a manifest that names only allowlisted packs: %v", err)
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
func TestNodePackRowsMatchTheRunnersFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "render", "node-packs.json"))
	if err != nil {
		t.Fatalf("the runner's table: %v", err)
	}
	var file struct {
		Packs []struct {
			Name string `json:"name"`
			Repo string `json:"repo"`
		} `json:"packs"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Packs) == 0 {
		t.Fatal("render/node-packs.json lists no pack")
	}
	var fromFile []AllowedNodePack
	for _, p := range file.Packs {
		fromFile = append(fromFile, AllowedNodePack{Name: p.Name, Repo: p.Repo})
	}
	got := AllowedNodePacks()
	if len(got) != len(fromFile) {
		t.Fatalf("the node's table has %d rows, render/node-packs.json has %d", len(got), len(fromFile))
	}
	for i := range got {
		if got[i] != fromFile[i] {
			t.Errorf("row %d: node %+v, runner file %+v", i, got[i], fromFile[i])
		}
	}
}

// Every pack the doctor tells an operator to install is a pack the manifest door would take.
func TestNodePackRowsCoverEveryPackTheDoctorNames(t *testing.T) {
	allowed := map[string]bool{}
	for _, p := range AllowedNodePacks() {
		allowed[p.Repo] = true
	}
	for name, np := range nodePacks {
		if !allowed[np.url] {
			t.Errorf("%s (%s) is a pack the doctor names and has no allowlist row", name, np.url)
		}
	}
}
