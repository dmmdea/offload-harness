package pipeline

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// byte-identical: these goldens are the argv the ComfyUI / python paths produce with
// no engine key set. They are asserted against the unmodified base as well (the scratch
// run is recorded in the CT-49 report), so a passing run on both is the proof.
func TestEveryRouteWithNoEngineKeyKeepsItsExactArgv(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	stub := writeArgStub(t, dir)
	run := func(t *testing.T, mut func(*config.Config), req core.Request, key string) []string {
		t.Helper()
		cfg := config.Default()
		cfg.MediaDir = dir
		mut(&cfg)
		p := &Pipeline{cfg: cfg}
		res := p.Run(context.Background(), req)
		if !res.OK {
			t.Fatalf("defer: %s", res.Reason)
		}
		var m map[string]any
		if err := json.Unmarshal(res.Data, &m); err != nil {
			t.Fatal(err)
		}
		return readArgs(t, m[key].(string))
	}
	out := func(name string) string { return filepath.Join(dir, name) }

	got := run(t, func(c *config.Config) { c.VideoGenScript = stub }, core.Request{
		Task: core.TaskGenerateVideo, Input: "p", Params: map[string]any{"out": out("v1.mp4"), "seed": 123},
	}, "video_path")
	// 0.178.0 added videogen_wan_decode (default "tiled"), which the runner receives as --wan-decode.
	if want := []string{out("v1.mp4"), "p", "--seed", "123", "--wan-vvram-gb", "7", "--wan-decode", "tiled"}; !reflect.DeepEqual(got, want) {
		t.Errorf("video argv changed:\n got %v\nwant %v", got, want)
	}

	got = run(t, func(c *config.Config) { c.AnimateGenScript = stub }, core.Request{
		Task: core.TaskAnimateCharacter, Input: "p", Image: "ref.png", Video: "drive.mp4", Params: map[string]any{"out": out("a1.mp4"), "seed": 123},
	}, "video_path")
	if want := []string{out("a1.mp4"), "ref.png", "drive.mp4", "p", "--seed", "123"}; !reflect.DeepEqual(got, want) {
		t.Errorf("animate argv changed:\n got %v\nwant %v", got, want)
	}

	got = run(t, func(c *config.Config) { c.VoiceGenScript = stub }, core.Request{
		Task: core.TaskGenerateAudio, Input: "hola", Params: map[string]any{"kind": "voice", "out": out("s1.wav"), "seed": 123, "lang": "es", "clone": "ref.wav"},
	}, "audio_path")
	if want := []string{out("s1.wav"), "hola", "--clone", "ref.wav", "--lang", "es"}; !reflect.DeepEqual(got, want) {
		t.Errorf("voice argv changed:\n got %v\nwant %v", got, want)
	}

	got = run(t, func(c *config.Config) { c.MusicGenScript = stub }, core.Request{
		Task: core.TaskGenerateAudio, Input: "lofi", Params: map[string]any{"kind": "music", "out": out("m1.flac"), "seed": 123, "seconds": 30, "lyrics": "la"},
	}, "audio_path")
	if want := []string{out("m1.flac"), "lofi", "--seed", "123", "--seconds", "30", "--lyrics", "la"}; !reflect.DeepEqual(got, want) {
		t.Errorf("music argv changed:\n got %v\nwant %v", got, want)
	}
}
