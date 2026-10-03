package fleetnode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

func touchFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// wanVideoBox is a node whose video-gen route mediacap can derive CONFIGURED from real files: the script,
// the four weights the default Wan graph loads (safetensors experts, so the native loader needs no custom
// node but VideoHelperSuite) and the VideoHelperSuite pack.
func wanVideoBox(t *testing.T) (cfg config.Config, vae string) {
	t.Helper()
	root := t.TempDir()
	script := filepath.Join(root, "comfy-video.mjs")
	comfy := filepath.Join(root, "comfy")
	touchFile(t, script)
	touchFile(t, filepath.Join(comfy, "custom_nodes", "ComfyUI-VideoHelperSuite", "__init__.py"))
	touchFile(t, filepath.Join(comfy, "models", "unet", "high.safetensors"))
	touchFile(t, filepath.Join(comfy, "models", "unet", "low.safetensors"))
	touchFile(t, filepath.Join(comfy, "models", "text_encoders", "te.safetensors"))
	vae = filepath.Join(comfy, "models", "vae", "wan_2.1_vae.safetensors")
	touchFile(t, vae)
	cfg = config.Config{
		VideoGenScript: script, ComfyDir: comfy, NodePath: "node",
		VideoGenUnetHigh: "high.safetensors", VideoGenUnetLow: "low.safetensors", VideoGenTextEncoder: "te.safetensors",
		FleetAuthToken: "tok", MediaDir: t.TempDir(),
	}
	return cfg, vae
}

func routeState(routes []MediaRouteHealth, name string) string {
	for _, r := range routes {
		if r.Route == name {
			return r.State
		}
	}
	return ""
}

func mediaHealthOf(t *testing.T, s *Server) (tasks []string, routes []MediaRouteHealth) {
	t.Helper()
	rec := do(t, s, "GET", "/fleet/health", "", nil)
	if rec.Code != 200 {
		t.Fatalf("health status %d: %s", rec.Code, rec.Body.String())
	}
	var h struct {
		Tasks  []string           `json:"supported_task_types"`
		Routes []MediaRouteHealth `json:"media_routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	return h.Tasks, h.Routes
}

// A node with a missing weight stops advertising video-gen and says why in media_routes; restoring the
// file brings the task back. The same predicate gates admission, so a job for it is refused, not run.
func TestAMissingWeightDropsVideoGenFromTheAdvertisement(t *testing.T) {
	useRealMediaRoutes(t)
	cfg, vae := wanVideoBox(t)
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)

	tasks, routes := mediaHealthOf(t, s)
	if !slices.Contains(tasks, "video-gen") || routeState(routes, "generate_video") != "CONFIGURED" {
		t.Fatalf("a fully provisioned node must advertise video-gen: tasks %v routes %+v", tasks, routes)
	}
	if _, cleanup, err := BuildRequest(context.Background(), cfg, true, "video-gen", json.RawMessage(`{"prompt":"p"}`)); err != nil {
		t.Fatalf("admission must accept a configured route: %v", err)
	} else {
		cleanup()
	}
	for _, r := range routes {
		if r.Route == "comfyui" || r.Route == "node" {
			t.Errorf("shared prerequisites are not task routes: %+v", r)
		}
	}

	if err := os.Remove(vae); err != nil {
		t.Fatal(err)
	}
	ResetMediaRoutesCache() // the next read goes to the disk instead of waiting out the cache
	tasks, routes = mediaHealthOf(t, s)
	if slices.Contains(tasks, "video-gen") {
		t.Fatalf("video-gen is still advertised with its VAE missing: %v", tasks)
	}
	if st := routeState(routes, "generate_video"); st != "BOUND-BUT-MISSING" {
		t.Fatalf("generate_video = %q, want BOUND-BUT-MISSING (routes %+v)", st, routes)
	}
	if _, cleanup, err := BuildRequest(context.Background(), cfg, true, "video-gen", json.RawMessage(`{"prompt":"p"}`)); err == nil {
		cleanup()
		t.Fatal("admission accepted a task health no longer advertises: advertisement and admission must be one predicate")
	} else if !strings.Contains(err.Error(), "unsupported task_type") {
		t.Fatalf("refusal: %v", err)
	}
	if rec := do(t, s, "POST", "/fleet/dispatch", `{"job_id":"v-1","task_type":"video-gen","payload":{"prompt":"p"}}`, nil); rec.Code != 400 {
		t.Fatalf("dispatch of an unadvertised task: status %d, want 400: %s", rec.Code, rec.Body.String())
	}

	touchFile(t, vae)
	ResetMediaRoutesCache()
	tasks, routes = mediaHealthOf(t, s)
	if !slices.Contains(tasks, "video-gen") || routeState(routes, "generate_video") != "CONFIGURED" {
		t.Fatalf("restoring the file must bring video-gen back: tasks %v routes %+v", tasks, routes)
	}
}

// run-graph needs only its script, the cheapest real-file proof of the same mechanism.
func TestAMissingRunGraphScriptDropsRunGraph(t *testing.T) {
	useRealMediaRoutes(t)
	script := filepath.Join(t.TempDir(), "comfy-run-graph.mjs")
	touchFile(t, script)
	cfg := config.Config{RunGraphScript: script}
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	if tasks, routes := mediaHealthOf(t, s); !slices.Contains(tasks, "run-graph") || routeState(routes, "run_graph") != "CONFIGURED" {
		t.Fatalf("tasks %v routes %+v", tasks, routes)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	ResetMediaRoutesCache()
	tasks, routes := mediaHealthOf(t, s)
	if slices.Contains(tasks, "run-graph") || routeState(routes, "run_graph") != "BOUND-BUT-MISSING" {
		t.Fatalf("tasks %v routes %+v", tasks, routes)
	}
	touchFile(t, script)
	ResetMediaRoutesCache()
	if tasks, _ := mediaHealthOf(t, s); !slices.Contains(tasks, "run-graph") {
		t.Fatalf("restored script must bring run-graph back: %v", tasks)
	}
}

// The derivation is cached for at most 60 s: inside the window a vanished file is still read as present,
// after it the disk is read again. Health is polled every few seconds; this is what keeps it off the disk.
func TestMediaRoutesAreCachedForAtMostSixtySeconds(t *testing.T) {
	calls := 0
	prev, prevClock := mediaRoutesFn, mediaClock
	t.Cleanup(func() { mediaRoutesFn, mediaClock = prev, prevClock; ResetMediaRoutesCache() })
	mediaRoutesFn = func(config.Config) []mediacap.Route {
		calls++
		return []mediacap.Route{{Name: "run_graph", Engine: "comfyui", State: mediacap.Configured}}
	}
	now := time.Now()
	mediaClock = func() time.Time { return now }
	ResetMediaRoutesCache()
	cfg := config.Config{RunGraphScript: "x.mjs"}
	for i := 0; i < 5; i++ {
		MediaRoutesHealth(cfg)
		SupportedTasksFor(cfg, true)
	}
	if calls != 1 {
		t.Fatalf("%d derivations inside one window, want 1", calls)
	}
	now = now.Add(59 * time.Second)
	MediaRoutesHealth(cfg)
	if calls != 1 {
		t.Fatalf("a read at 59 s must still hit the cache, calls %d", calls)
	}
	now = now.Add(2 * time.Second)
	MediaRoutesHealth(cfg)
	if calls != 2 {
		t.Fatalf("a read past 60 s must derive again, calls %d", calls)
	}
	other := config.Config{RunGraphScript: "y.mjs"}
	MediaRoutesHealth(other)
	if calls != 3 {
		t.Fatalf("a different config must not share the entry, calls %d", calls)
	}
}

// Audio-gen is served when voice OR music OR the speech endpoint is derived CONFIGURED; a bound script
// whose route is not CONFIGURED does not count.
func TestAudioGenIsAdvertisedWhenAnyOfItsRoutesIsConfigured(t *testing.T) {
	prev := mediaRoutesFn
	t.Cleanup(func() { mediaRoutesFn = prev; ResetMediaRoutesCache() })
	cfg := config.Config{VoiceGenScript: "tts.mjs", MusicGenScript: "music.mjs", TTSEndpoint: "http://192.0.2.9:8000"}
	for name, tc := range map[string]struct {
		configured []string
		want       bool
	}{
		"voice only":     {[]string{"generate_audio:voice"}, true},
		"music only":     {[]string{"generate_audio:music"}, true},
		"endpoint only":  {[]string{"generate_audio:voice:endpoint"}, true},
		"none":           {nil, false},
		"unrelated only": {[]string{"generate_video"}, false},
	} {
		mediaRoutesFn = func(config.Config) []mediacap.Route {
			var out []mediacap.Route
			for _, n := range []string{"generate_audio:voice", "generate_audio:music", "generate_audio:voice:endpoint", "generate_video"} {
				st := mediacap.BoundButMissing
				if slices.Contains(tc.configured, n) {
					st = mediacap.Configured
				}
				out = append(out, mediacap.Route{Name: n, State: st})
			}
			return out
		}
		ResetMediaRoutesCache()
		if got := slices.Contains(SupportedTasks(cfg), "audio-gen"); got != tc.want {
			t.Errorf("%s: audio-gen advertised=%v, want %v", name, got, tc.want)
		}
	}
	// An endpoint-only node (no script at all) serves voice=endpoint, so it advertises audio-gen too.
	mediaRoutesFn = bindingOnlyRoutes
	ResetMediaRoutesCache()
	if !slices.Contains(SupportedTasks(config.Config{TTSEndpoint: "http://192.0.2.9:8000"}), "audio-gen") {
		t.Error("a node with only a speech endpoint must advertise audio-gen")
	}
	if slices.Contains(SupportedTasks(config.Config{}), "audio-gen") {
		t.Error("a node with no audio binding advertised audio-gen")
	}
}

// image-gen keeps config.ImageGenAdvertisable: the route derivation does not gate it.
func TestImageGenAdvertisementIsNotGatedByTheRouteDerivation(t *testing.T) {
	prev := mediaRoutesFn
	t.Cleanup(func() { mediaRoutesFn = prev; ResetMediaRoutesCache() })
	mediaRoutesFn = func(config.Config) []mediacap.Route { return nil }
	ResetMediaRoutesCache()
	if !slices.Contains(SupportedTasks(config.Config{ImageGenScript: "render/comfy-generate.mjs"}), "image-gen") {
		t.Error("image-gen must stay advertised on its binding")
	}
}

// media-job is advertised only while at least one inner task is served, so it is never offered over nothing.
func TestMediaJobIsNotAdvertisedOverNoServedTask(t *testing.T) {
	prev := mediaRoutesFn
	t.Cleanup(func() { mediaRoutesFn = prev; ResetMediaRoutesCache() })
	mediaRoutesFn = func(config.Config) []mediacap.Route {
		return []mediacap.Route{{Name: "generate_video", State: mediacap.BoundButMissing}}
	}
	ResetMediaRoutesCache()
	cfg := config.Config{VideoGenScript: "render/comfy-video.mjs", FleetMediaInputs: true, FleetAuthToken: "tok"}
	tasks := SupportedTasks(cfg)
	if slices.Contains(tasks, "video-gen") || slices.Contains(tasks, MediaJobTask) {
		t.Fatalf("neither video-gen nor media-job may be advertised over a missing weight: %v", tasks)
	}
	mediaRoutesFn = func(config.Config) []mediacap.Route {
		return []mediacap.Route{{Name: "generate_video", State: mediacap.Configured}}
	}
	ResetMediaRoutesCache()
	tasks = SupportedTasks(cfg)
	if !slices.Contains(tasks, "video-gen") || !slices.Contains(tasks, MediaJobTask) {
		t.Fatalf("both must be advertised once the route is configured: %v", tasks)
	}
	if i, j := slices.Index(tasks, "run-graph"), slices.Index(tasks, MediaJobTask); i >= 0 && j < i {
		t.Errorf("media-job is advertised after run-graph: %v", tasks)
	}
}
