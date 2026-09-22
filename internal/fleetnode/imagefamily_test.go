package fleetnode

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// familyNodeCfg: a krea2 node that also serves a named, non-commercial
// qwen-image-2.1 family (ADR 0058).
func familyNodeCfg() config.Config {
	cfg := fullCfg()
	cfg.ImageGenFamily = "krea2"
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{"qwen-image-2.1": {
		"license":         json.RawMessage(`"Qwen Research License"`),
		"commercial_use":  json.RawMessage(`false`),
		"imagegen_family": json.RawMessage(`"qwen-image-2.1"`),
		"imagegen_ckpt":   json.RawMessage(`"qwen_image_2.1_bf16.safetensors"`),
	}}
	return cfg
}

// The image-gen payload carries family and transparent exactly as the MCP handler
// maps them; absent stays absent (the default binding, byte-for-byte).
func TestBuildRequestImageGenCarriesFamilyAndTransparent(t *testing.T) {
	req, cleanup := mustBuild(t, familyNodeCfg(), "image-gen", `{"prompt":"p","family":"qwen-image-2.1","transparent":true}`)
	defer cleanup()
	want := map[string]any{"family": "qwen-image-2.1", "transparent": true}
	if !reflect.DeepEqual(req.Params, want) {
		t.Fatalf("params = %#v, want %#v", req.Params, want)
	}
	req, cleanup2 := mustBuild(t, familyNodeCfg(), "image-gen", `{"prompt":"p","transparent":false}`)
	defer cleanup2()
	if len(req.Params) != 0 {
		t.Fatalf("absent family / false transparent must add nothing, got %#v", req.Params)
	}
}

// /fleet/health advertises the family NAMES a dispatcher can send, each with its
// license flags, and the named family's graph among the loadable families. A node
// with no named families publishes the pre-0.134 shape (no image_families key).
func TestHealthAdvertisesImageFamiliesWithLicenseFlags(t *testing.T) {
	s, _ := newTestServer(t, familyNodeCfg(), &fakeRunner{}, nil)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		Families      []string         `json:"loadable_model_families"`
		ImageFamilies []map[string]any `json:"image_families"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.ImageFamilies) != 2 {
		t.Fatalf("image_families = %v, want the default and the named family", got.ImageFamilies)
	}
	def, named := got.ImageFamilies[0], got.ImageFamilies[1]
	if def["name"] != "krea2" || def["default"] != true || def["license"] != nil || def["commercial_use"] != nil {
		t.Errorf("default row = %v (an undeclared license must read null, never commercial)", def)
	}
	if named["name"] != "qwen-image-2.1" || named["license"] != "Qwen Research License" || named["commercial_use"] != false || named["default"] != false {
		t.Errorf("named row = %v", named)
	}
	if !strings.Contains(strings.Join(got.Families, ","), "qwen-image-2.1") {
		t.Errorf("loadable_model_families = %v, want the named family's graph too", got.Families)
	}

	plain, _ := newTestServer(t, fullCfg(), &fakeRunner{}, nil)
	if body := do(t, plain, http.MethodGet, "/fleet/health", "", nil).Body.String(); strings.Contains(body, "image_families") {
		t.Errorf("a node without named families must omit image_families: %s", body)
	}
}
