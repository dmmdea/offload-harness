package fleetnode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// CT-51 I1: a box whose lanes run on the CT-49 engines (sd.cpp video and animate, audio.cpp voice and
// music) sets no ComfyUI/python script, so it was never advertised or admitted for them even when
// mediacap read its routes CONFIGURED. These tests build such a box from real files and use the real
// derivation.

type engineBox struct {
	cfg   config.Config
	files map[string]string // short name -> absolute path, for removing one
}

// engineOnlyBox binds video (family fastwan, default), animate and voice+music to the native engines
// with every file present, and no ComfyUI script of any kind.
func engineOnlyBox(t *testing.T) engineBox {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{}
	mk := func(name string) string {
		p := filepath.Join(root, filepath.FromSlash(name))
		touchFile(t, p)
		files[name] = p
		return p
	}
	cfg := config.Config{
		NodePath: "node", FleetAuthToken: "tok", MediaDir: t.TempDir(),
		VideoGenFamily:      "fastwan",
		VideoGenSdcppScript: mk("render/sdcpp-video.mjs"),
		VideoGenFamilies: map[string]config.VideoFamilyBinding{"fastwan": {
			Engine: config.EngineSdcpp, SdcppBackend: "vulkan0",
			SdcppBin: mk("bin/sd-cli"), SdcppModel: mk("models/fastwan.gguf"),
			SdcppVAE: mk("models/wan-vae.safetensors"), SdcppT5xxl: mk("models/umt5.gguf"),
			License: "Apache-2.0",
		}},
		AnimateGenEngine: config.EngineSdcpp, AnimateGenSdcppBackend: "vulkan0",
		AnimateGenSdcppScript: mk("render/sdcpp-animate.mjs"),
		AnimateGenSdcppBin:    mk("bin/sd-cli-animate"), AnimateGenDepthBin: mk("bin/da3-cli"),
		AnimateGenSdcppModel: mk("models/vace.safetensors"), AnimateGenSdcppVAE: mk("models/wan21-vae.safetensors"),
		AnimateGenSdcppT5xxl: mk("models/umt5-animate.gguf"), AnimateGenDepthModel: mk("models/depth.gguf"),
		VoiceGenEngine: config.EngineAudiocpp, MusicGenEngine: config.EngineAudiocpp,
		AudiocppBackend: "vulkan", AudiocppScript: mk("render/audiocpp-generate.mjs"),
		AudiocppBin: mk("bin/audiocpp_cli"), AudiocppVoiceModel: mk("models/chatterbox.gguf"),
		AudiocppMusicModel: mk("models/ace-step.gguf"),
	}
	return engineBox{cfg: cfg, files: files}
}

var engineTasks = []struct{ task, payload string }{
	{"video-gen", `{"prompt":"p"}`},
	{"animate", `{"prompt":"p","ref":"ref.png","driver":"drv.mp4"}`},
	{"audio-gen", `{"text":"hello"}`},
	{"audio-gen", `{"text":"calm","kind":"music"}`},
}

func TestEngineOnlyBoxAdvertisesAndAdmitsItsMediaLanes(t *testing.T) {
	useRealMediaRoutes(t)
	box := engineOnlyBox(t)
	s, _ := newTestServer(t, box.cfg, &fakeRunner{}, nil)

	tasks, routes := mediaHealthOf(t, s)
	for _, want := range []string{"video-gen", "animate", "audio-gen"} {
		if !slices.Contains(tasks, want) {
			t.Errorf("%s not advertised on an engine-only box: tasks %v routes %+v", want, tasks, routes)
		}
	}
	for _, r := range []string{"generate_video", "animate_character", "generate_audio:voice", "generate_audio:music"} {
		if st := routeState(routes, r); st != "CONFIGURED" {
			t.Errorf("route %s = %q, want CONFIGURED (routes %+v)", r, st, routes)
		}
	}
	if slices.Contains(tasks, "run-graph") {
		t.Errorf("run-graph is ComfyUI-only and must stay unadvertised: %v", tasks)
	}
	for _, c := range engineTasks {
		_, cleanup, err := BuildRequest(context.Background(), box.cfg, true, c.task, json.RawMessage(c.payload))
		if err != nil {
			t.Errorf("admission refused %s on an engine-only box: %v", c.task, err)
			continue
		}
		cleanup()
	}
	// The advertised families are the ones the lanes record footprints under.
	got := Families(box.cfg)
	for _, want := range []string{"fastwan", config.AnimateSdcppFootprintFamily, "ace_step", "chatterbox"} {
		if !slices.Contains(got, want) {
			t.Errorf("Families = %v, missing %q", got, want)
		}
	}
	for _, bad := range []string{"wan2.2", "wan-animate2", "acestep"} {
		if slices.Contains(got, bad) {
			t.Errorf("Families = %v advertises the ComfyUI family %q the engines never record", got, bad)
		}
	}
}

// A bound model file that goes missing: the lane stops being advertised and a job for it is refused with
// the 503 route-not-ready (re-placeable), not the 400 of a malformed request; restoring it brings it back.
func TestEngineOnlyBoxRefusesALaneWhoseBoundFileIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name, file, task, route, payload string
	}{
		{"sdcpp video model", "models/fastwan.gguf", "video-gen", "generate_video", `{"prompt":"p"}`},
		{"sdcpp animate depth model", "models/depth.gguf", "animate", "animate_character", `{"prompt":"p","ref":"r.png","driver":"d.mp4"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useRealMediaRoutes(t)
			box := engineOnlyBox(t)
			s, _ := newTestServer(t, box.cfg, &fakeRunner{}, nil)
			if err := os.Remove(box.files[tc.file]); err != nil {
				t.Fatal(err)
			}
			ResetMediaRoutesCache()
			tasks, routes := mediaHealthOf(t, s)
			if slices.Contains(tasks, tc.task) {
				t.Fatalf("%s still advertised with %s missing: %v", tc.task, tc.file, tasks)
			}
			if st := routeState(routes, tc.route); st != "BOUND-BUT-MISSING" {
				t.Fatalf("%s = %q, want BOUND-BUT-MISSING (%+v)", tc.route, st, routes)
			}
			_, cleanup, err := BuildRequest(context.Background(), box.cfg, true, tc.task, json.RawMessage(tc.payload))
			if err == nil {
				cleanup()
				t.Fatal("admission accepted a lane whose bound file is gone")
			}
			var nre *routeNotReadyError
			if !errors.As(err, &nre) || !strings.Contains(err.Error(), tc.route) {
				t.Fatalf("want the route-not-ready refusal naming %s, got %v", tc.route, err)
			}
			body := `{"job_id":"e-1","task_type":"` + tc.task + `","payload":` + tc.payload + `}`
			if rec := do(t, s, "POST", "/fleet/dispatch", body, nil); rec.Code != 503 {
				t.Fatalf("dispatch status %d, want 503: %s", rec.Code, rec.Body.String())
			}
			touchFile(t, box.files[tc.file])
			ResetMediaRoutesCache()
			if tasks, _ := mediaHealthOf(t, s); !slices.Contains(tasks, tc.task) {
				t.Fatalf("restoring %s must bring %s back: %v", tc.file, tc.task, tasks)
			}
		})
	}
	// audio-gen is served while either kind is, so it drops only when both engine models are gone.
	t.Run("audio.cpp models", func(t *testing.T) {
		useRealMediaRoutes(t)
		box := engineOnlyBox(t)
		s, _ := newTestServer(t, box.cfg, &fakeRunner{}, nil)
		if err := os.Remove(box.files["models/chatterbox.gguf"]); err != nil {
			t.Fatal(err)
		}
		ResetMediaRoutesCache()
		if tasks, routes := mediaHealthOf(t, s); !slices.Contains(tasks, "audio-gen") || routeState(routes, "generate_audio:voice") != "BOUND-BUT-MISSING" {
			t.Fatalf("music still serves audio-gen: tasks %v routes %+v", tasks, routes)
		}
		if err := os.Remove(box.files["models/ace-step.gguf"]); err != nil {
			t.Fatal(err)
		}
		ResetMediaRoutesCache()
		if tasks, _ := mediaHealthOf(t, s); slices.Contains(tasks, "audio-gen") {
			t.Fatalf("audio-gen advertised with both audio.cpp models missing: %v", tasks)
		}
		_, cleanup, err := BuildRequest(context.Background(), box.cfg, true, "audio-gen", json.RawMessage(`{"text":"hello"}`))
		var nre *routeNotReadyError
		if err == nil {
			cleanup()
			t.Fatal("admission accepted audio-gen with both models gone")
		} else if !errors.As(err, &nre) {
			t.Fatalf("want the route-not-ready refusal, got %v", err)
		}
	})
}

// A box with neither a script nor an engine keeps its behaviour exactly: nothing advertised, and a job for
// the task is the plain unsupported-task 400, never the route-not-ready 503.
func TestABoxWithNeitherScriptNorEngineKeepsItsBehaviour(t *testing.T) {
	useRealMediaRoutes(t)
	cfg := config.Config{NodePath: "node", FleetAuthToken: "tok", MediaDir: t.TempDir()}
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)
	tasks, _ := mediaHealthOf(t, s)
	for _, n := range []string{"video-gen", "animate", "audio-gen", "run-graph"} {
		if slices.Contains(tasks, n) {
			t.Errorf("%s advertised with nothing bound: %v", n, tasks)
		}
		if mediaTaskBound(cfg, n) {
			t.Errorf("mediaTaskBound(%s) true with nothing bound", n)
		}
	}
	_, cleanup, err := BuildRequest(context.Background(), cfg, true, "video-gen", json.RawMessage(`{"prompt":"p"}`))
	var nre *routeNotReadyError
	if err == nil {
		cleanup()
		t.Fatal("admitted video-gen with nothing bound")
	} else if errors.As(err, &nre) || !strings.Contains(err.Error(), "unsupported task_type") {
		t.Fatalf("want the plain unsupported-task refusal, got %v", err)
	}
	// A family name with no videogen_families entry is not a bound lane either.
	cfg.VideoGenFamily = "fastwan"
	if cfg.VideoGenBound() || mediaTaskBound(cfg, "video-gen") {
		t.Error("a family name with no binding is not a bound video lane")
	}
}

// The media-job door binds a task the way the advertisement does (CT-51, REL4): an engine-only box, with no
// script key of any kind, that opted in and holds a fleet token advertises media-job and takes a bearer'd job
// whose input file travels in the bundle (a still, a driver video, a clone sample). Before, the door looked only
// at the script keys, so the box advertised video-gen, animate and audio-gen and could not be sent their inputs.
// A box with nothing bound, or without the opt-in or the token, keeps the door shut.
func TestAnEngineOnlyBoxOpensTheMediaJobDoor(t *testing.T) {
	useRealMediaRoutes(t)
	box := engineOnlyBox(t)
	if c := box.cfg; c.VideoGenScript != "" || c.AnimateGenScript != "" || c.VoiceGenScript != "" || c.MusicGenScript != "" ||
		c.TTSEndpoint != "" || c.RunGraphScript != "" || c.ImageGenAdvertisable() {
		t.Fatal("the fixture binds a script or an image lane, so it no longer proves an engine-only box")
	}
	box.cfg.FleetMediaInputs = true
	s, _ := newTestServer(t, box.cfg, &inputRunner{}, nil)

	tasks, _ := mediaHealthOf(t, s)
	if !slices.Contains(tasks, MediaJobTask) {
		t.Fatalf("an opted-in engine-only box with a token must advertise media-job: %v", tasks)
	}
	if rec := do(t, s, http.MethodPost, MediaJobPath, mjStillBody(t, "mj-engine"), bearer()); rec.Code != http.StatusAccepted {
		t.Fatalf("media-job on an engine-only box = %d (%s), want 202: a remote caller could not send a still", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, http.MethodPost, MediaJobPath, mjStillBody(t, "mj-engine-anon"), nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("media-job without the bearer = %d, want 401", rec.Code)
	}

	nothing := config.Config{NodePath: "node", FleetMediaInputs: true, FleetAuthToken: "tok", MediaDir: t.TempDir()}
	for name, cfg := range map[string]config.Config{
		"nothing bound":     nothing,
		"engine, no opt-in": func() config.Config { c := box.cfg; c.FleetMediaInputs = false; return c }(),
		"engine, no token":  func() config.Config { c := box.cfg; c.FleetAuthToken = ""; return c }(),
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestServer(t, cfg, &inputRunner{}, nil)
			if tasks, _ := mediaHealthOf(t, s); slices.Contains(tasks, MediaJobTask) {
				t.Errorf("media-job advertised: %v", tasks)
			}
			if rec := do(t, s, http.MethodPost, MediaJobPath, mjStillBody(t, "mj-closed"), bearer()); rec.Code != http.StatusForbidden {
				t.Errorf("media-job = %d (%s), want 403: the door is closed", rec.Code, rec.Body.String())
			}
		})
	}
}

// The advertised family of an engine lane is the family it records, whatever the operator named it.
func TestEngineLaneFamiliesAreTheRecordedOnes(t *testing.T) {
	sd := func(name string) config.Config {
		return config.Config{VideoGenFamily: name, VideoGenFamilies: map[string]config.VideoFamilyBinding{
			name: {Engine: config.EngineSdcpp, SdcppBackend: "vulkan0"}}}
	}
	for _, c := range []struct {
		cfg  config.Config
		want string
	}{
		{sd("fastwan"), "fastwan"},
		{sd("ltx25"), "ltx25"},
		{sd("wan22"), "wan2.2"},                            // the sentinel keeps the store's spelling
		{sd("LTX25"), "LTX25"},                             // an sdcpp family is matched and recorded by its own name
		{config.Config{VideoGenFamily: "LTX25"}, "wan2.2"}, // a ComfyUI box still folds to Wan
	} {
		if got := familyFor(c.cfg, "video-gen"); got != c.want {
			t.Errorf("videogen_family=%q: familyFor = %q, want %q", c.cfg.VideoGenFamily, got, c.want)
		}
	}
	if got := familyFor(config.Config{AnimateGenEngine: config.EngineSdcpp}, "animate"); got != config.AnimateSdcppFootprintFamily {
		t.Errorf("sdcpp animate family = %q", got)
	}
	if got := familyFor(config.Config{AnimateGenScript: "a.mjs"}, "animate"); got != "wan-animate2" {
		t.Errorf("ComfyUI animate family = %q", got)
	}
	for _, c := range []struct {
		cfg  config.Config
		want []string
	}{
		{config.Config{}, []string{"acestep"}},
		{config.Config{MusicGenScript: "m.mjs", VoiceGenScript: "v.mjs"}, []string{"acestep"}},
		{config.Config{MusicGenEngine: config.EngineAudiocpp}, []string{"ace_step"}},
		{config.Config{VoiceGenEngine: config.EngineAudiocpp}, []string{"chatterbox"}},
		{config.Config{VoiceGenEngine: config.EngineAudiocpp, MusicGenScript: "m.mjs"}, []string{"acestep", "chatterbox"}},
		{config.Config{VoiceGenEngine: config.EngineAudiocpp, MusicGenEngine: config.EngineAudiocpp, AudiocppVoiceFamily: "vx", AudiocppMusicFamily: "mx"}, []string{"mx", "vx"}},
	} {
		if got := audioFamilies(c.cfg); !slices.Equal(got, c.want) {
			t.Errorf("audioFamilies(%+v) = %v, want %v", c.cfg, got, c.want)
		}
	}
}
