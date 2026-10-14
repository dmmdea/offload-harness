package pipeline

// Every route that takes the media lease takes it for ITS OWN timeout, and its render is killed at that same value (register
// C-33). gpuLeaseCases (gpulease_coverage_test.go) is the table of every task that touches the GPU; this file adds the one fact
// about each of them that the operator guide's "engine + lease" row states: which config key the route's timeout is read
// from. The kill and window pins below run over that table, and the guide's row is held to naming each key
// (operator_guide_lease_row_test.go), so a route cannot be added, re-keyed or moved onto the fallback window without one of
// them going red.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

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

const (
	// ownLeaseSec is the value a pin gives the route's own timeout key; otherLeaseSec is the value of every other timeout key,
	// far from it, so a route that read the wrong key (or fell back to a default) shows as a different window.
	ownLeaseSec   = 2
	otherLeaseSec = 90
	// windowTolerance bounds the read of a lease window: the window is stamped in milliseconds from the timeout and read from
	// the record a moment later.
	windowTolerance = 400 * time.Millisecond
)

// timeoutSetters is the setter of each distinct timeout key in the table.
func timeoutSetters() map[string]func(*config.Config, int) {
	m := map[string]func(*config.Config, int){}
	for _, r := range leaseRoutes {
		m[r.key] = r.set
	}
	return m
}

// setTimeouts gives key ownSec and every other timeout key otherSec.
func setTimeouts(c *config.Config, key string, ownSec, otherSec int) {
	for k, set := range timeoutSetters() {
		if k == key {
			set(c, ownSec)
		} else {
			set(c, otherSec)
		}
	}
}

// windowAndKill runs a media call in the background against the fixture's stub runner (which holds the render until the test
// lets it go), reads the declared window of the one live lease while the render runs, then lets the render run into its
// timeout and checks what killed it. Two facts, one value: the lease is stamped for `own`, and the render was killed at `own`
// (gpugen's timeout error names the Timeout the render ran under: "timeout after 2s"). A render killed later than its lease
// ends, a lease longer than the render may run, and a window read from the wrong key each fail here.
func windowAndKill(t *testing.T, f *admitFixture, what string, own time.Duration, run func() string) {
	t.Helper()
	done := make(chan string, 1)
	go func() { done <- run() }()

	deadline := time.Now().Add(20 * time.Second)
	for len(f.m.Leases()) != 1 {
		select {
		case answer := <-done:
			t.Fatalf("%s: the call ended without ever holding a lease: %q", what, answer)
		default:
		}
		if time.Now().After(deadline) {
			f.letRunnersGo()
			t.Fatalf("%s: no lease was held within 20 s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
	window := theOneLeaseWindow(t, f)
	if d := window - own; d < -windowTolerance || d > windowTolerance {
		t.Errorf("%s: the lease window is %s; the call's own timeout is %s", what, window.Round(time.Millisecond), own)
		f.letRunnersGo() // the pin has failed; end the call now instead of waiting out the runner
	}

	var answer string
	select {
	case answer = <-done:
	case <-time.After(60 * time.Second):
		f.letRunnersGo()
		t.Fatalf("%s: the call never returned", what)
	}
	if want := fmt.Sprintf("timeout after %s", own); !strings.Contains(answer, want) {
		t.Errorf("%s: the render was not killed at the call's own timeout (%s): want %q in what the call answered, got %q", what, own, want, answer)
	}
}

// Every route that takes the media lease leases for ITS OWN timeout key and has its render killed at that same value. The table
// is gpuLeaseCases (every GPU task), the key of each route is leaseRoutes, and each runs on the card-scoped host with the stub
// runner holding the render. The two ComfyUI routes the lease-window pins run on both lease paths, and the whole-node plan is
// what the iGPU, sdcpp and run-graph lanes take on this host, so the table covers both writers.
func TestEveryMediaRouteLeasesForItsOwnTimeoutAndIsKilledThere(t *testing.T) {
	cases := map[string]gpuLeaseCase{}
	for _, c := range gpuLeaseCases() {
		cases[c.name] = c
	}
	for _, r := range leaseRoutes {
		tc, ok := cases[r.name]
		if !ok {
			t.Fatalf("%q is not a gpuLeaseCases route (TestTheLeaseRouteTableCoversEveryGPUTask names it)", r.name)
		}
		t.Run(r.name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) {
				tc.setup(t, c, c.ImageGenScript, c.MediaDir) // the fixture's stub runner stands in for the route's script
				presentWeights(t, c.MediaDir, c)
				setTimeouts(c, r.key, ownLeaseSec, otherLeaseSec)
			}})
			windowAndKill(t, f, r.name+" ("+r.key+")", ownLeaseSec*time.Second, func() string { return tc.invoke(t, f.p, f.dir) })
		})
	}
}

// A batch takes the per-image timeout times its jobs, for the lease and for the kill: three one-second jobs hold the card for
// three seconds, not one.
func TestAnImageBatchLeasesAndIsKilledAtThePerImageTimeoutTimesItsJobs(t *testing.T) {
	const perImageSec, jobs = 1, 3
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: func(c *config.Config) {
		setTimeouts(c, "imagegen_timeout_sec", perImageSec, otherLeaseSec)
	}})
	windowAndKill(t, f, "image batch of three", perImageSec*jobs*time.Second, func() string {
		_, err := f.p.RunImageBatch(context.Background(), []ImageBatchJob{{Prompt: "a red bike"}, {Prompt: "a blue bike"}, {Prompt: "a green bike"}})
		if err != nil {
			return err.Error()
		}
		return ""
	})
}
