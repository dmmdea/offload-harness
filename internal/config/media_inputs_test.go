package config

import "testing"

// The media-job door (ADR 0072) is open only with the opt-in, a fleet token and a bound media task.
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

func TestEffectiveMediaInputsMaxBytes(t *testing.T) {
	if got := (Config{}).EffectiveMediaInputsMaxBytes(); got != 512<<20 {
		t.Errorf("default cap %d, want 512 MiB", got)
	}
	if got := (Config{FleetMediaInputsMaxMB: 3}).EffectiveMediaInputsMaxBytes(); got != 3<<20 {
		t.Errorf("3 MB cap %d", got)
	}
	if got := (Config{FleetMediaInputsMaxMB: -1}).EffectiveMediaInputsMaxBytes(); got != 512<<20 {
		t.Errorf("a negative cap falls back to the default, got %d", got)
	}
}
