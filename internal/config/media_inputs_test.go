package config

import "testing"

// The media-job door (ADR 0077) is open only with the opt-in, a fleet token and a bound media task.
func TestMediaInputsAdmissibleNeedsOptInTokenAndATask(t *testing.T) {
	open := Config{FleetMediaInputs: true, FleetAuthToken: "tok", VideoGenScript: "render/comfy-video.mjs"}
	if !open.MediaInputsAdmissible() {
		t.Fatal("opted in, with a token and a bound video task, the door must be open")
	}
	for name, mut := range map[string]func(*Config){
		"not opted in":  func(c *Config) { c.FleetMediaInputs = false },
		"no token":      func(c *Config) { c.FleetAuthToken = "" },
		"no media task": func(c *Config) { c.VideoGenScript = "" },
	} {
		c := open
		mut(&c)
		if c.MediaInputsAdmissible() {
			t.Errorf("%s: the door is open", name)
		}
	}
	for name, mut := range map[string]func(*Config){
		"animate":    func(c *Config) { c.VideoGenScript, c.AnimateGenScript = "", "render/comfy-animate.mjs" },
		"voice":      func(c *Config) { c.VideoGenScript, c.VoiceGenScript = "", "render/tts.mjs" },
		"music":      func(c *Config) { c.VideoGenScript, c.MusicGenScript = "", "render/comfy-music.mjs" },
		"run-graph":  func(c *Config) { c.VideoGenScript, c.RunGraphScript = "", "render/comfy-run-graph.mjs" },
		"image":      func(c *Config) { c.VideoGenScript, c.ImageGenScript = "", "render/comfy-generate.mjs" },
		"tts server": func(c *Config) { c.VideoGenScript, c.TTSEndpoint = "", "http://192.0.2.7:8000" },
	} {
		c := open
		mut(&c)
		if !c.MediaInputsAdmissible() {
			t.Errorf("a node with only %s bound must open the door", name)
		}
	}
}

// The door binds a media task the way the fleet advertisement does (CT-51): through the helpers that count the
// CT-49 engines as well as the scripts. A node whose only renderers are sd.cpp and audio.cpp has every script
// key blank (the shape the iGPU seed and the CT-49 tests use), and before this it advertised video-gen, animate
// and audio-gen but could not open the door that carries their input files.
func TestMediaInputsAdmissibleBindsThroughTheEngineAwareHelpers(t *testing.T) {
	sdcppVideo := map[string]VideoFamilyBinding{"fastwan": {Engine: EngineSdcpp, SdcppBackend: "vulkan0"}}
	engineOnly := map[string]func(*Config){
		"sd.cpp video (the default family)": func(c *Config) { c.VideoGenFamily, c.VideoGenFamilies = "fastwan", sdcppVideo },
		"sd.cpp animate":                    func(c *Config) { c.AnimateGenEngine = EngineSdcpp },
		"audio.cpp voice":                   func(c *Config) { c.VoiceGenEngine = EngineAudiocpp },
		"audio.cpp music":                   func(c *Config) { c.MusicGenEngine = EngineAudiocpp },
	}
	optedIn := Config{FleetMediaInputs: true, FleetAuthToken: "tok"}
	for name, bind := range engineOnly {
		c := optedIn
		bind(&c)
		if c.VideoGenScript != "" || c.AnimateGenScript != "" || c.VoiceGenScript != "" || c.MusicGenScript != "" || c.TTSEndpoint != "" || c.RunGraphScript != "" {
			t.Fatalf("%s: the fixture sets a script key, so it no longer proves an engine-only box", name)
		}
		if !c.MediaInputsAdmissible() {
			t.Errorf("a node with only %s bound and every script key blank must open the door", name)
		}
		// the opt-in and the token are still required
		for gate, mut := range map[string]func(*Config){
			"not opted in": func(c *Config) { c.FleetMediaInputs = false },
			"no token":     func(c *Config) { c.FleetAuthToken = "" },
		} {
			g := c
			mut(&g)
			if g.MediaInputsAdmissible() {
				t.Errorf("%s, %s: the door is open", name, gate)
			}
		}
	}
	// Nothing bound keeps the door closed, however the engine keys are spelled: a family named but not bound
	// to the sdcpp engine, an animate engine that is not sdcpp, and an audio engine that is not audiocpp are
	// not renderers.
	for name, c := range map[string]Config{
		"nothing at all":                 optedIn,
		"a family name with no binding":  {FleetMediaInputs: true, FleetAuthToken: "tok", VideoGenFamily: "fastwan"},
		"a ComfyUI-bound family entry":   {FleetMediaInputs: true, FleetAuthToken: "tok", VideoGenFamily: "ltx25", VideoGenFamilies: map[string]VideoFamilyBinding{"ltx25": {}}},
		"animate engine that is unknown": {FleetMediaInputs: true, FleetAuthToken: "tok", AnimateGenEngine: "comfy"},
		"voice engine that is unknown":   {FleetMediaInputs: true, FleetAuthToken: "tok", VoiceGenEngine: "comfy", MusicGenEngine: "comfy"},
	} {
		if c.MediaInputsAdmissible() {
			t.Errorf("%s: the door is open with no media task bound", name)
		}
	}
}

func TestEffectiveMediaInputsMaxBytes(t *testing.T) {
	if got := (Config{}).EffectiveMediaInputsMaxBytes(); got != 256<<20 {
		t.Errorf("default cap %d, want 256 MiB (the node holds the base64 body and the decoded bundle at once)", got)
	}
	if got := (Config{FleetMediaInputsMaxMB: 3}).EffectiveMediaInputsMaxBytes(); got != 3<<20 {
		t.Errorf("3 MB cap %d", got)
	}
	if got := (Config{FleetMediaInputsMaxMB: -1}).EffectiveMediaInputsMaxBytes(); got != 256<<20 {
		t.Errorf("a negative cap falls back to the default, got %d", got)
	}
}
