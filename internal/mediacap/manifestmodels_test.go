package mediacap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type modelCase struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SourceURL string `json:"source_url"`
	OK        bool   `json:"ok"`
}

// loadModelCases reads the table the runner's tests read too (render/testdata/manifest-model-cases.json), so the
// node's door and the runner's satisfier take and refuse the same entries.
func loadModelCases(t *testing.T) []modelCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "render", "testdata", "manifest-model-cases.json"))
	if err != nil {
		t.Fatalf("the shared model table: %v", err)
	}
	var file struct {
		Cases []modelCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) < 20 {
		t.Fatalf("the shared model table lists %d cases", len(file.Cases))
	}
	return file.Cases
}

func manifestWithModel(t *testing.T, path, source string) []byte {
	t.Helper()
	entry := map[string]any{"path": path}
	if source != "" {
		entry["source_url"] = source
	}
	b, err := json.Marshal(map[string]any{"schema_version": 1, "node_packs": []any{}, "models": []any{entry}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A model is provisioned under models/<class>/ and fetched over https from a listed host: anything else is a file
// written where the caller chose, or a fetch from where the caller chose.
func TestAManifestModelLandsUnderTheModelsTreeFromAListedHost(t *testing.T) {
	for _, c := range loadModelCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			err := CheckManifestModels(manifestWithModel(t, c.Path, c.SourceURL))
			if c.OK {
				if err != nil {
					t.Fatalf("refused an entry the node provisions (path %q, source %q): %v", c.Path, c.SourceURL, err)
				}
				return
			}
			var refused *ManifestModelRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("path %q, source %q: err = %v, want a *ManifestModelRefusedError", c.Path, c.SourceURL, err)
			}
			if refused.Reason == "" || !strings.Contains(err.Error(), "not allowed") {
				t.Errorf("the refusal says no reason: %v", err)
			}
		})
	}
}

func TestAManifestWithBadModelsIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"models is not a list":           `{"models":{"path":"models/vae/x.safetensors"}}`,
		"an entry that is not an object": `{"models":["models/vae/x.safetensors"]}`,
		"a path that is not a string":    `{"models":[{"path":5}]}`,
		"a path that is a list":          `{"models":[{"path":["models/vae/x.safetensors"]}]}`,
		"a source that is not a string":  `{"models":[{"path":"models/vae/x.safetensors","source_url":5}]}`,
		"a path in the second entry":     `{"models":[{"path":"models/vae/ok.safetensors"},{"path":"custom_nodes/x/__init__.py"}]}`,
	} {
		var refused *ManifestModelRefusedError
		if err := CheckManifestModels([]byte(body)); !errors.As(err, &refused) {
			t.Errorf("%s: err = %v, want a *ManifestModelRefusedError", name, err)
		}
	}
	for _, body := range [][]byte{nil, []byte(`null`), []byte(`{}`), []byte(`{"models":null}`), []byte(`{"models":[]}`), []byte(`{"node_packs":[]}`)} {
		if err := CheckManifestModels(body); err != nil {
			t.Errorf("a manifest %q that names no models was refused: %v", body, err)
		}
	}
	// A refusal names the entry and says nothing a caller can use to probe the node.
	err := CheckManifestModels(manifestWithModel(t, "custom_nodes/evil/__init__.py", "https://huggingface.co/x/y"))
	var refused *ManifestModelRefusedError
	if !errors.As(err, &refused) || refused.Model != "custom_nodes/evil/__init__.py" {
		t.Errorf("the refusal names %v, want the path of the entry", err)
	}
}

// render/node-packs.json is the runner's table and the file a reviewer edits; the hosts here are the node's copy.
func TestModelSourceHostsMatchTheRunnersFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "render", "node-packs.json"))
	if err != nil {
		t.Fatalf("the runner's table: %v", err)
	}
	var file struct {
		ModelHosts []string `json:"model_hosts"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.ModelHosts) == 0 {
		t.Fatal("render/node-packs.json lists no model_hosts: every download would be refused")
	}
	if got := ModelSourceHosts(); !reflect.DeepEqual(got, file.ModelHosts) {
		t.Errorf("the node's hosts are %v, render/node-packs.json lists %v", got, file.ModelHosts)
	}
}
