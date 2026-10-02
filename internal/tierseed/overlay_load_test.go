package tierseed

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// captureStderr runs f with os.Stderr redirected and returns what it wrote: config.Load reports a
// half-bound recipe or a dead flag as a WARNING, not an error, and a warning on a shipped seed is a
// defect the box would only ever print.
func captureStderr(f func()) string {
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		f()
		return ""
	}
	os.Stderr = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	_ = w.Close()
	os.Stderr = old
	return <-done
}

// installerOwnedWarning is the one load warning every seeded tier prints by design: compose_script is
// seeded by the tier and hyperframes_dir/hyperframes_browser_path by the installer's own HyperFrames step
// (Get-HyperframesSeed), which has not run on a bare resolved seed.
const installerOwnedWarning = "compose_script is set but hyperframes_dir/hyperframes_browser_path is not"

// leftoverToken matches a placeholder that survived expansion: __OFFLOAD_HOME__, __EXE__, __HAILO_HOME__...
var leftoverToken = regexp.MustCompile(`__[A-Z0-9]+(?:_[A-Z0-9]+)*__`)

// TestEveryShippedOverlayLoadsAndValidates runs what a fresh install actually writes - the tier seed
// WITH its RAM overlay - through config.Load, the door every harness process opens. Nothing else in
// the tree does: TestEveryShippedSeedIsValid stops at "every key is a Config field", and the closure
// gates resolve the BASE seed only, so a family overlay with a typo'd key, a missing license pair, a
// half-bound recipe or a placeholder that never expanded (expand() substitutes tokens in strings and
// string arrays, never inside an object) could ship and die only on the first box that installed it.
func TestEveryShippedOverlayLoadsAndValidates(t *testing.T) {
	// A pooled tier warns when the launch environment lacks --disable-dynamic-vram; model a launch that
	// carries it, so the gate judges the seed and not the host it runs on (a CI runner sets nothing).
	t.Setenv("COMFY_EXTRA_ARGS", "--disable-dynamic-vram")
	profiles, err := Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, goos := range []string{"windows", "linux"} {
			for _, ram := range []string{"mid", "high"} {
				seed, err := Resolve(profiles[id], id, Options{Home: "/opt/offload", GOOS: goos, RAMTier: ram})
				if err != nil {
					t.Errorf("tier %s (%s, ram %s) does not resolve: %v", id, goos, ram, err)
					continue
				}
				if seed == nil {
					continue // a text-only tier ships no seed
				}
				raw, err := json.Marshal(seed)
				if err != nil {
					t.Fatal(err)
				}
				if m := leftoverToken.FindAllString(string(raw), -1); len(m) > 0 {
					t.Errorf("tier %s (%s, ram %s): placeholder(s) %v survive expansion - a path token inside a family block "+
						"must expand like a top-level seed value, or the config names a file that never existed", id, goos, ram, m)
				}
				var asMap map[string]any
				if err := json.Unmarshal(raw, &asMap); err != nil {
					t.Fatal(err)
				}
				asMap["state_dir"] = t.TempDir() // hermetic: Load arms the GPU gate from state_dir
				raw, err = json.Marshal(asMap)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "config.json")
				if err := os.WriteFile(path, raw, 0o644); err != nil {
					t.Fatal(err)
				}
				var cfg config.Config
				var loadErr error
				stderr := captureStderr(func() { cfg, loadErr = config.Load(path) })
				for _, line := range strings.Split(stderr, "\n") {
					if strings.HasPrefix(line, "warning:") && !strings.Contains(line, installerOwnedWarning) {
						t.Errorf("tier %s (%s, ram %s): config.Load warns about the seed an install writes: %s", id, goos, ram, line)
					}
				}
				if loadErr != nil {
					t.Errorf("tier %s (%s, ram %s): config.Load refuses the seed an install writes: %v", id, goos, ram, loadErr)
					continue
				}
				for name := range cfg.ImageGenFamilies {
					if _, fi, err := cfg.ResolveImageFamily(name); err != nil {
						t.Errorf("tier %s: image family %q does not resolve: %v", id, name, err)
					} else if fi.License == "" || fi.CommercialUse == nil {
						t.Errorf("tier %s: image family %q resolves without its license pair", id, name)
					}
				}
				for name := range cfg.GenEditFamilies {
					if _, fi, err := cfg.ResolveEditFamily(name); err != nil {
						t.Errorf("tier %s: edit family %q does not resolve: %v", id, name, err)
					} else if fi.License == "" || fi.CommercialUse == nil {
						t.Errorf("tier %s: edit family %q resolves without its license pair", id, name)
					}
				}
			}
		}
	}
}

// TestSeededWanLoaderMatchesTheExperts: videogen_wan_loader "native" is REFUSED by the builder when
// either Wan expert is a .gguf (ComfyUI's DynamicVRAM cannot stream GGUF), so a tier that rolls its
// experts back to the Q8_0 pair without clearing the loader would seed a video route that fails on
// every render. The pair and the loader move together, and a table lint is the only place that says so.
//
// Each tier is checked as a box receives it: the base layer alone (a low-RAM box) and the base with the
// RAM overlay on top (a mid/high-RAM box; Resolve lets the overlay win key by key). Checking the layers
// one at a time would miss a pair rolled back in one layer while the other still says "native".
func TestSeededWanLoaderMatchesTheExperts(t *testing.T) {
	profiles, err := Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	for id, p := range profiles {
		withOverlay := map[string]any{}
		for k, v := range p.ConfigSeed {
			withOverlay[k] = v
		}
		for k, v := range p.ConfigSeedMidHigh {
			withOverlay[k] = v
		}
		for view, seed := range map[string]map[string]any{
			"config_seed":                            p.ConfigSeed,
			"config_seed + config_seed_ram_mid_high": withOverlay,
		} {
			if seed["videogen_wan_loader"] != "native" {
				continue
			}
			for _, k := range []string{"videogen_unet_high", "videogen_unet_low"} {
				if s, _ := seed[k].(string); strings.HasSuffix(strings.ToLower(s), ".gguf") {
					t.Errorf("tier %s (%s) seeds videogen_wan_loader \"native\" with %s=%q: native refuses a .gguf expert "+
						"- roll the loader back to \"\" (auto) or \"gguf-distorch\" together with the experts", id, view, k, s)
				}
			}
		}
	}
}
