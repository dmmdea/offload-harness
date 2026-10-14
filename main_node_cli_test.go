package main

import (
	"flag"
	"strings"
	"testing"
)

// The three lane verbs that can name a fleet node (ADR 0082 Amendment 3): generate-video, animate-character and generate-audio
// take --node (a node id or one of the delegate_remotes URLs); the first two also take --min-vram-gb, as run-graph does. The
// flag reaches the request's params under the names the MCP doors use (node, min_vram_gb), and only when it was given.

func TestGenerateVideoCLIParsesNodeAndMinVRAMGB(t *testing.T) {
	for _, args := range [][]string{
		{"out.mp4", "still.png", "a slow pan", "--node", "render-a", "--min-vram-gb", "16"},
		{"--node", "render-a", "--min-vram-gb", "16", "out.mp4", "still.png", "a slow pan"},
		{"out.mp4", "--node=render-a", "still.png", "--min-vram-gb=16", "a slow pan"},
	} {
		c, err := parseGenerateVideo(args, flag.ContinueOnError)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if c.video.out != "out.mp4" || c.video.still != "still.png" || c.prompt != "a slow pan" {
			t.Errorf("%v: --node must not eat a positional: out %q still %q prompt %q", args, c.video.out, c.video.still, c.prompt)
		}
		p := buildVideoParams(c.video)
		if p["node"] != "render-a" {
			t.Errorf("%v: node = %#v", args, p["node"])
		}
		if v, ok := p["min_vram_gb"].(float64); !ok || v != 16 {
			t.Errorf("%v: min_vram_gb = %#v, want the number 16", args, p["min_vram_gb"])
		}
	}
	c, err := parseGenerateVideo([]string{"out.mp4", "still.png", "a slow pan", "--min-vram-gb", "0"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := buildVideoParams(c.video)["min_vram_gb"]; !ok || v != float64(0) {
		t.Errorf("an explicit 0 is a number the caller gave: %#v", buildVideoParams(c.video))
	}
	c, err = parseGenerateVideo([]string{"out.mp4", "still.png", "a slow pan"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	p := buildVideoParams(c.video)
	for _, k := range []string{"node", "min_vram_gb"} {
		if _, has := p[k]; has {
			t.Errorf("%s must be absent when the flag was not given: %v", k, p)
		}
	}
}

func TestAMalformedMinVRAMGBIsRefusedByNameOnVideoAndAnimate(t *testing.T) {
	for _, bad := range []string{"sixteen", "-1", "NaN", "Inf"} {
		if _, err := parseGenerateVideo([]string{"out.mp4", "still.png", "p", "--min-vram-gb", bad}, flag.ContinueOnError); err == nil || !strings.Contains(err.Error(), "--min-vram-gb") {
			t.Errorf("generate-video --min-vram-gb %q: err = %v, want a refusal naming --min-vram-gb", bad, err)
		}
		if _, err := parseAnimateCharacter([]string{"out.mp4", "r.png", "d.mp4", "p", "--min-vram-gb", bad}, flag.ContinueOnError); err == nil || !strings.Contains(err.Error(), "--min-vram-gb") {
			t.Errorf("animate-character --min-vram-gb %q: err = %v, want a refusal naming --min-vram-gb", bad, err)
		}
	}
}

func TestAnimateCharacterCLIParsesNodeAndMinVRAMGB(t *testing.T) {
	c, err := parseAnimateCharacter([]string{"--node", "render-a", "out.mp4", "r.png", "d.mp4", "a fox", "--min-vram-gb", "24", "--frames", "49"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if c.animate.out != "out.mp4" || c.animate.ref != "r.png" || c.animate.driver != "d.mp4" || c.animate.prompt != "a fox" {
		t.Errorf("--node must not eat a positional: %+v", c.animate)
	}
	p := buildAnimateParams(c.animate)
	if p["node"] != "render-a" || p["frames"] != 49 {
		t.Errorf("params = %v", p)
	}
	if v, ok := p["min_vram_gb"].(float64); !ok || v != 24 {
		t.Errorf("min_vram_gb = %#v, want the number 24", p["min_vram_gb"])
	}
	c, err = parseAnimateCharacter([]string{"out.mp4", "r.png", "d.mp4", "a fox"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"node", "min_vram_gb"} {
		if _, has := buildAnimateParams(c.animate)[k]; has {
			t.Errorf("%s must be absent when the flag was not given", k)
		}
	}
}

func TestGenerateAudioCLIParsesNodeButNotMinVRAMGB(t *testing.T) {
	c, err := parseGenerateAudio([]string{"--node", "render-a", "out.wav", "hola", "--kind", "music"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if c.audio.out != "out.wav" || c.text != "hola" {
		t.Errorf("--node must not eat a positional: out %q text %q", c.audio.out, c.text)
	}
	if p := buildAudioParams(c.audio); p["node"] != "render-a" || p["kind"] != "music" {
		t.Errorf("params = %v", p)
	}
	c, err = parseGenerateAudio([]string{"out.wav", "hola"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := buildAudioParams(c.audio)["node"]; has {
		t.Error("node must be absent when the flag was not given")
	}
	if _, err := parseGenerateAudio([]string{"out.wav", "hola", "--min-vram-gb", "16"}, flag.ContinueOnError); err == nil {
		t.Error("audio has no VRAM floor to ask for: --min-vram-gb is an unknown flag there")
	}
}

// run-graph does not take --node in this release: the flag is refused as unknown instead of being dropped.
func TestRunGraphDoesNotTakeNode(t *testing.T) {
	_, cleanup, err := runGraphParams([]string{"--graph-json", "{}", "--node", "render-a"})
	cleanup()
	if err == nil || !strings.Contains(err.Error(), "node") {
		t.Errorf("run-graph --node: err = %v, want the flag refused by name", err)
	}
}

func TestUsageNamesTheNodeAndTheMinVRAMFlags(t *testing.T) {
	out := captureStderr(t, usage)
	for _, verb := range []string{"generate-video", "animate-character", "generate-audio"} {
		var line string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "local-offload "+verb) {
				line = l
				break
			}
		}
		if line == "" || !strings.Contains(line, "--node") {
			t.Errorf("%s: the usage line must list --node: %q", verb, line)
		}
		if verb != "generate-audio" && !strings.Contains(line, "--min-vram-gb") {
			t.Errorf("%s: the usage line must list --min-vram-gb: %q", verb, line)
		}
	}
}
