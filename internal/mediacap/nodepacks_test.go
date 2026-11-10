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

	// The same holds for the refused node classes: render/refused-node-classes.json is the runner's table, refusednodes.go
	// the node's copy, and a graph holding a listed class is refused with the class named.
	rawRefused, err := os.ReadFile(filepath.Join("..", "..", "render", "refused-node-classes.json"))
	if err != nil {
		t.Fatalf("the runner's refused-classes table: %v", err)
	}
	var refusedFile struct {
		Classes []struct {
			Class string `json:"class"`
			Pack  string `json:"pack"`
			Why   string `json:"why"`
		} `json:"classes"`
	}
	if err := json.Unmarshal(rawRefused, &refusedFile); err != nil {
		t.Fatal(err)
	}
	if len(refusedFile.Classes) == 0 {
		t.Fatal("render/refused-node-classes.json lists no class")
	}
	gotRefused := RefusedNodeClasses()
	if len(gotRefused) != len(refusedFile.Classes) {
		t.Fatalf("the node's refused classes have %d rows, render/refused-node-classes.json has %d", len(gotRefused), len(refusedFile.Classes))
	}
	for i, c := range refusedFile.Classes {
		if want := (RefusedNodeClass{Class: c.Class, Pack: c.Pack, Why: c.Why}); gotRefused[i] != want {
			t.Errorf("refused class row %d: node %+v, runner file %+v", i, gotRefused[i], want)
		}
		var refused *NodeClassRefusedError
		if err := CheckGraphNodeClasses([]byte(`{"1":{"class_type":"` + c.Class + `","inputs":{}}}`)); !errors.As(err, &refused) || refused.Row.Class != c.Class || !strings.Contains(err.Error(), "NODE_CLASS_REFUSED") {
			t.Errorf("a graph holding %s: err = %v, want a *NodeClassRefusedError naming it", c.Class, err)
		}
		if err := CheckGraphNodeClasses([]byte(`{"nodes":[{"id":1,"type":"` + c.Class + `"}]}`)); err == nil {
			t.Errorf("a UI-format workflow holding %s was not refused", c.Class)
		}
		if err := CheckGraphNodeClasses([]byte(`{"1":{"class_type":"` + strings.ToLower(c.Class) + `","inputs":{}}}`)); err != nil {
			t.Errorf("the match is case-sensitive, but %q was refused: %v", strings.ToLower(c.Class), err)
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
