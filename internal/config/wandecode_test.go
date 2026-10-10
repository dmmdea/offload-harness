package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// videogen_wan_decode picks how the Wan 2.2 graph turns its latent into frames: plain VAEDecode,
// tiled VAEDecodeTiled, or auto (the runner reads the render card's size and chooses). These tests
// pin the config half: the default, the vocabulary, the per-family override, and that the Go and
// Node sides name the same modes. The argv half is internal/pipeline's TestRunGenerateVideo_PassesWanDecode.

// TestWanDecodeDefaultsToAuto: a config that never mentions the key renders with auto, and the
// shipped default is the explicit "auto" (config.example.json documents it as such), while an
// explicit empty value reads the same way downstream (nothing is passed and the runner's default,
// auto, applies).
func TestWanDecodeDefaultsToAuto(t *testing.T) {
	if got := Default().VideoGenWanDecode; got != "auto" {
		t.Fatalf("Default().VideoGenWanDecode = %q, want \"auto\"", got)
	}
	c, err := Load(writeCfg(t, `{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.VideoGenWanDecode != "auto" {
		t.Fatalf("a config without the key must load auto, got %q", c.VideoGenWanDecode)
	}
	if fb := c.VideoDefaultFamilyBinding(); fb.WanDecode != "auto" {
		t.Fatalf("the default family binding must carry the flat key, got %q", fb.WanDecode)
	}
}

// TestWanDecodeLoadsAsWritten: every mode in the vocabulary, and the empty string, load as
// written (an explicit empty value is not rewritten to the default).
func TestWanDecodeLoadsAsWritten(t *testing.T) {
	for _, mode := range append([]string{""}, WanDecodeModes...) {
		c, err := Load(writeCfg(t, `{"model":"x","videogen_wan_decode":"`+mode+`"}`))
		if err != nil {
			t.Fatalf("videogen_wan_decode %q: %v", mode, err)
		}
		if c.VideoGenWanDecode != mode {
			t.Errorf("videogen_wan_decode %q loaded as %q", mode, c.VideoGenWanDecode)
		}
	}
}

// TestWanDecodeRefusesAnUnknownMode: a typo is refused at the config door, by key and by value,
// never as a runner exit on every render (the same reasoning as videogen_wan_loader).
func TestWanDecodeRefusesAnUnknownMode(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"flat key", `{"model":"x","videogen_wan_decode":"fast"}`, `videogen_wan_decode: "fast" is not "", "auto", "plain" or "tiled"`},
		{"flat key, wrong case", `{"model":"x","videogen_wan_decode":"Plain"}`, `videogen_wan_decode: "Plain"`},
		{"family entry", `{"model":"x","videogen_families":{"wan22":{"wan_decode":"vaedecode"}}}`, `videogen_families["wan22"].wan_decode: "vaedecode" is not "", "auto", "plain" or "tiled"`},
		{"family entry for another family", `{"model":"x","videogen_families":{"ltx25":{"wan_decode":"sometimes"}}}`, `videogen_families["ltx25"].wan_decode: "sometimes"`},
	} {
		_, err := Load(writeCfg(t, tc.body))
		if err == nil {
			t.Errorf("%s: want a load error naming the key, got none", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not contain %q", tc.name, err, tc.want)
		}
	}
	// And the accepted spellings load, in a family entry too.
	for _, mode := range WanDecodeModes {
		body := `{"model":"x","videogen_families":{"wan22":{"wan_decode":"` + mode + `"}}}`
		if _, err := Load(writeCfg(t, body)); err != nil {
			t.Errorf("videogen_families wan_decode %q must load: %v", mode, err)
		}
	}
}

// TestWanDecodeReachesTheResolvedBinding: the pipeline reads the mode off the binding of the family
// that renders. The box's own default family reads the flat key; an explicit videogen_families entry
// for any other family replaces the binding wholesale (so an entry that sets no wan_decode gives the
// runner none, and the runner's default applies); a family with no entry falls back to the flat key.
func TestWanDecodeReachesTheResolvedBinding(t *testing.T) {
	c := Config{VideoGenFamily: "ltx25", VideoGenWanDecode: "plain"}
	for _, fam := range []string{"", "ltx25", "wan22", "hunyuan"} {
		if got := c.ResolveVideoFamilyBinding(fam).WanDecode; got != "plain" {
			t.Errorf("family %q with no videogen_families: WanDecode = %q, want the flat key", fam, got)
		}
	}
	c.VideoGenFamilies = map[string]VideoFamilyBinding{"wan22": {WanDecode: "tiled"}}
	if got := c.ResolveVideoFamilyBinding("wan22").WanDecode; got != "tiled" {
		t.Errorf("wan22 with its own entry: WanDecode = %q, want the entry's", got)
	}
	if got := c.ResolveVideoFamilyBinding("ltx25").WanDecode; got != "plain" {
		t.Errorf("the box's own default family must keep the flat key, got %q", got)
	}
	c.VideoGenFamilies = map[string]VideoFamilyBinding{"wan22": {UnetHigh: "h.safetensors"}}
	if got := c.ResolveVideoFamilyBinding("wan22").WanDecode; got != "" {
		t.Errorf("a wan22 entry with no wan_decode replaces the binding wholesale: WanDecode = %q, want empty", got)
	}
}

// TestWanDecodeMirrorsTheBuilder: the Go vocabulary and default are the runner's own. Read from
// render/wf-wan22-i2v.mjs, the real artifact, never from another Go literal: a mode added on one
// side only would be refused at load by the other, or accepted here and refused by the runner on
// every render.
func TestWanDecodeMirrorsTheBuilder(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "render", "wf-wan22-i2v.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	m := regexp.MustCompile(`WAN_DECODE_MODES = Object\.freeze\(\[([^\]]*)\]\)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("no WAN_DECODE_MODES list in render/wf-wan22-i2v.mjs")
	}
	var js []string
	for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
		js = append(js, q[1])
	}
	if strings.Join(js, ",") != strings.Join(WanDecodeModes, ",") {
		t.Errorf("render/wf-wan22-i2v.mjs WAN_DECODE_MODES = %v, config.WanDecodeModes = %v: keep them equal", js, WanDecodeModes)
	}
	d := regexp.MustCompile(`WAN_DECODE_DEFAULT = "([a-z]+)"`).FindStringSubmatch(src)
	if d == nil {
		t.Fatal("no WAN_DECODE_DEFAULT in render/wf-wan22-i2v.mjs")
	}
	if want := Default().VideoGenWanDecode; d[1] != want {
		t.Errorf("render/wf-wan22-i2v.mjs WAN_DECODE_DEFAULT = %q vs config default videogen_wan_decode %q: keep them equal", d[1], want)
	}
	// The runner must actually take the flag the pipeline passes.
	cv, err := os.ReadFile(filepath.Join("..", "..", "render", "comfy-video.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cv), `"wan-decode"`) {
		t.Error(`render/comfy-video.mjs never reads the "wan-decode" flag the pipeline passes`)
	}
}
