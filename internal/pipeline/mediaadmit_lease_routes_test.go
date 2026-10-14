package pipeline

// Every route that takes the media lease takes it for ITS OWN timeout, and its render is killed at that same value (register
// C-33). gpuLeaseCases (gpulease_coverage_test.go) is the table of every task that touches the GPU; this file adds the one fact
// about each of them that the operator guide's "engine + lease" row states: which config key the route's timeout is read
// from. The kill and window pins below run over that table, and the guide's row is held to naming each key
// (operator_guide_lease_row_test.go), so a route cannot be added, re-keyed or moved onto the fallback window without one of
// them going red.

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// leaseRoute is one gpuLeaseCases route and the config key its timeout comes from. set writes that key (and no other) on a
// config, so a pin can give the route's own key one value and every other key another.
type leaseRoute struct {
	name string // a gpuLeaseCases name
	key  string // the json key of the config field the route reads its timeout from
	set  func(c *config.Config, sec int)
}

func setImageTimeout(c *config.Config, sec int)   { c.ImageGenTimeoutSec = sec }
func setVideoTimeout(c *config.Config, sec int)   { c.VideoGenTimeoutSec = sec }
func setInpaintTimeout(c *config.Config, sec int) { c.InpaintTimeoutSec = sec }
func setUpscaleTimeout(c *config.Config, sec int) { c.UpscaleTimeoutSec = sec }
func setGenEditTimeout(c *config.Config, sec int) { c.GenEditTimeoutSec = sec }
func setAnimateTimeout(c *config.Config, sec int) { c.AnimateGenTimeoutSec = sec }
func setAudioTimeout(c *config.Config, sec int)   { c.AudioGenTimeoutSec = sec }

// leaseRoutes is keyed by gpuLeaseCases name. The lanes that take no media lease (the tts_endpoint voice lane and a
// pipeline-job) are not GPU tasks of that table and are not here; the guide's row names them as the exceptions.
var leaseRoutes = []leaseRoute{
	{"generate_image (comfy)", "imagegen_timeout_sec", setImageTimeout},
	{"generate_image (comfy-render direct)", "imagegen_timeout_sec", setImageTimeout},
	{"generate_image (sdcpp)", "imagegen_timeout_sec", setImageTimeout},
	{"edit_image_generative", "gen_edit_timeout_sec", setGenEditTimeout},
	{"inpaint_image", "inpaint_timeout_sec", setInpaintTimeout},
	{"upscale_image", "upscale_timeout_sec", setUpscaleTimeout},
	{"image batch", "imagegen_timeout_sec", setImageTimeout},
	{"run_graph", "imagegen_timeout_sec", setImageTimeout},
	{"generate_video", "videogen_timeout_sec", setVideoTimeout},
	{"generate_video (pooled)", "videogen_timeout_sec", setVideoTimeout},
	{"animate_character", "animategen_timeout_sec", setAnimateTimeout},
	{"generate_audio (voice)", "audiogen_timeout_sec", setAudioTimeout},
	{"generate_audio (music)", "audiogen_timeout_sec", setAudioTimeout},
	{"generate_video (sdcpp)", "videogen_timeout_sec", setVideoTimeout},
	{"animate_character (sdcpp)", "animategen_timeout_sec", setAnimateTimeout},
	{"generate_audio (voice, audiocpp)", "audiogen_timeout_sec", setAudioTimeout},
	{"generate_audio (music, audiocpp)", "audiogen_timeout_sec", setAudioTimeout},
}

// leaseRouteKeys is the distinct timeout keys of the table, sorted.
func leaseRouteKeys() []string {
	seen := map[string]bool{}
	var keys []string
	for _, r := range leaseRoutes {
		if !seen[r.key] {
			seen[r.key] = true
			keys = append(keys, r.key)
		}
	}
	sort.Strings(keys)
	return keys
}

// The table covers exactly the GPU tasks gpuLeaseCases lists: a task added there without a timeout key here (or left here after
// it is gone) fails, so the pins and the guide's key list cannot silently miss a route.
func TestTheLeaseRouteTableCoversEveryGPUTask(t *testing.T) {
	have := map[string]bool{}
	for _, r := range leaseRoutes {
		if have[r.name] {
			t.Errorf("the lease route table lists %q twice", r.name)
		}
		have[r.name] = true
	}
	want := map[string]bool{}
	for _, c := range gpuLeaseCases() {
		want[c.name] = true
		if !have[c.name] {
			t.Errorf("gpuLeaseCases has %q, which the lease route table does not: say which config key its timeout is read from", c.name)
		}
	}
	for n := range have {
		if !want[n] {
			t.Errorf("the lease route table lists %q, which gpuLeaseCases does not", n)
		}
	}
}

// The key each route names is the json key of the field set writes, so the guide's row (which quotes these keys) names keys a
// config file really carries.
func TestTheLeaseRouteTableNamesRealConfigKeys(t *testing.T) {
	const sentinel = 12345
	for _, r := range leaseRoutes {
		cfg := config.Default()
		r.set(&cfg, sentinel)
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if got, ok := m[r.key]; !ok || got != float64(sentinel) {
			t.Errorf("%s: its setter does not write config key %q (the config carries %v)", r.name, r.key, got)
		}
	}
}
