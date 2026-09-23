package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The OPERATOR-GUIDE's family how-to is copy-paste material: an operator pastes that
// block into a live config. It must load and resolve as the doc says, or the doc
// teaches a config the load refuses.
func TestOperatorGuideFamilyExampleLoads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "OPERATOR-GUIDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(raw), "\r\n", "\n")
	i := strings.Index(doc, "### Add a named image or edit family")
	if i < 0 {
		t.Fatal("OPERATOR-GUIDE lost its family how-to — this gate went blind")
	}
	sec := doc[i:]
	start := strings.Index(sec, "```json\n")
	if start < 0 {
		t.Fatal("no json block in the family how-to")
	}
	block := sec[start+len("```json\n"):]
	block = block[:strings.Index(block, "```")]
	cfgJSON := `{"model":"x","imagegen_script":"render/comfy-generate.mjs","imagegen_family":"krea2",
		"imagegen_ckpt":"krea2_turbo_bf16.safetensors","gen_edit_script":"render/comfy-edit.mjs",
		"gen_edit_unet":"qwen-image-edit-2511-Q5_1.gguf",` + block + `}`
	c, err := Load(writeCfg(t, cfgJSON))
	if err != nil {
		t.Fatalf("the OPERATOR-GUIDE family example does not load: %v\n%s", err, block)
	}
	fc, fi, err := c.ResolveImageFamily("qwen-image-2.1")
	if err != nil || !fi.NonCommercial() || fc.ImageGenFamily != FamilyQwenImage21 || fc.ComfyCudaDevice != "2" || fc.ComfyDynamicVRAM != "on" {
		t.Errorf("image family: %+v %+v %v", fi, fc.ImageGenFamily, err)
	}
	if !fc.SupportsTransparentImage() {
		t.Error("the doc renders --transparent through this family; it must support it")
	}
	ec, ei, err := c.ResolveEditFamily("qwen-image-2.1")
	if err != nil || !ei.NonCommercial() || ec.GenEditFamily != FamilyQwenImage21 || ec.GenEditCacheDevice != "gpu" {
		t.Errorf("edit family: %+v %+v %v", ei, ec.GenEditFamily, err)
	}
}
