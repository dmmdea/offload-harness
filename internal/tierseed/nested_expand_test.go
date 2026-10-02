package tierseed

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestExpandRecursesIntoObjects: a named family (imagegen_families / gen_edit_families) is an OBJECT of
// strings, string arrays and scalars. expand() used to substitute tokens in strings and string arrays
// only, so a path token inside a family block came out as the literal "__OFFLOAD_HOME__/..." - a config
// that loads fine and fails at render. The cases are the shapes a family really carries.
func TestExpandRecursesIntoObjects(t *testing.T) {
	in := map[string]any{
		"sdcpp_model": "__OFFLOAD_HOME__/models/m.gguf",
		"sdcpp_bin":   "__OFFLOAD_HOME__/sdcpp/sd-cli__EXE__",
		"args":        []any{"--a", "__OFFLOAD_HOME__/x"},
		"steps":       float64(8),
		"on":          true,
		"deeper":      map[string]any{"p": "__OFFLOAD_HOME__/d", "list": []any{"__EXE__"}},
	}
	got, ok := expand(in, "D:/oh", ".exe").(map[string]any)
	if !ok {
		t.Fatalf("expand of an object must return an object, got %T", expand(in, "D:/oh", ".exe"))
	}
	if got["sdcpp_model"] != "D:/oh/models/m.gguf" || got["sdcpp_bin"] != "D:/oh/sdcpp/sd-cli.exe" {
		t.Errorf("strings inside an object did not expand: %v", got)
	}
	if !reflect.DeepEqual(got["args"], []any{"--a", "D:/oh/x"}) {
		t.Errorf("array inside an object did not expand: %v", got["args"])
	}
	if got["steps"] != float64(8) || got["on"] != true {
		t.Errorf("scalars inside an object must pass through untouched: %v", got)
	}
	deeper, _ := got["deeper"].(map[string]any)
	if deeper["p"] != "D:/oh/d" || !reflect.DeepEqual(deeper["list"], []any{".exe"}) {
		t.Errorf("an object nested in an object did not expand: %v", got["deeper"])
	}
	// The seed table is shared by every Resolve call: expansion must build a new object, never rewrite
	// the profile's own.
	if in["sdcpp_model"] != "__OFFLOAD_HOME__/models/m.gguf" || in["deeper"].(map[string]any)["p"] != "__OFFLOAD_HOME__/d" {
		t.Errorf("expand mutated its input: %v", in)
	}
}

// TestExpandMatchesTheSharedParityFixture: setup/install.ps1 Expand-SeedValue is the Windows installer's
// parity copy of expand(). Both suites load testdata/nested-expand-parity.json and must produce its
// `expected` block from its `seed` block, so neither can drift from the other unnoticed
// (setup/tests/install-config-seed.test.ps1 holds the PowerShell half).
func TestExpandMatchesTheSharedParityFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/nested-expand-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Home     string         `json:"home"`
		Seed     map[string]any `json:"seed"`
		Expected map[string]any `json:"expected"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	home := strings.TrimRight(strings.ReplaceAll(fx.Home, `\`, "/"), "/")
	got := map[string]any{}
	for k, v := range fx.Seed {
		got[k] = expand(v, home, ".exe")
	}
	if !reflect.DeepEqual(got, fx.Expected) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(fx.Expected, "", " ")
		t.Errorf("expand(seed) != expected\n got: %s\nwant: %s", g, w)
	}
}

// TestBlackwell8SeedsTheZImageSdcppFamily: the 8 GB Blackwell box runs Z-Image Turbo through the sdcpp
// engine. A named family clears the flat sdcpp_* model keys, so the family must carry its own model
// paths, and those paths must be the install home's, not a literal token.
func TestBlackwell8SeedsTheZImageSdcppFamily(t *testing.T) {
	profiles, err := Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ goos, home string }{{"windows", "D:/oh"}, {"linux", "/opt/offload"}} {
		seed, err := Resolve(profiles["blackwell-8"], "blackwell-8", Options{Home: c.home, GOOS: c.goos, RAMTier: "mid"})
		if err != nil {
			t.Fatalf("%s: %v", c.goos, err)
		}
		fams, _ := seed["imagegen_families"].(map[string]any)
		z, _ := fams["z-image-turbo"].(map[string]any)
		if z == nil {
			t.Fatalf("%s: blackwell-8 does not seed the z-image-turbo family (have %v)", c.goos, fams)
		}
		want := map[string]any{
			"license": "Apache-2.0", "commercial_use": true, "imagegen_engine": "sdcpp",
			"sdcpp_model":      c.home + "/models/z_image_turbo-Q8_0.gguf",
			"sdcpp_model_kind": "diffusion",
			"sdcpp_vae":        c.home + "/models/zimage_ae.safetensors",
			"sdcpp_llm":        c.home + "/models/Qwen3-4B-Instruct-2507-Q4_K_M.gguf",
			"sdcpp_extra_args": []any{"--vae-tiling", "--offload-to-cpu", "--diffusion-fa", "--max-vram", "6.5", "--stream-layers"},
			"imagegen_steps":   float64(8), "imagegen_cfg": float64(1),
		}
		if !reflect.DeepEqual(z, want) {
			g, _ := json.MarshalIndent(z, "", " ")
			w, _ := json.MarshalIndent(want, "", " ")
			t.Errorf("%s: z-image-turbo family differs\n got: %s\nwant: %s", c.goos, g, w)
		}
		// The default image binding is unchanged: the family is a per-request opt-in, not a flip.
		if seed["imagegen_family"] != "hidream-o1-dev" {
			t.Errorf("%s: the default image family moved to %v; z-image-turbo is an opt-in", c.goos, seed["imagegen_family"])
		}
	}
}
