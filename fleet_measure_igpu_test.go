package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// captureFleetMeasure runs `fleet-measure --config <cfg>` in-process and returns what it wrote to
// stderr (the progress notes) and stdout (the footprint records).
func captureFleetMeasure(t *testing.T, cfgPath string) (stderr, stdout string) {
	t.Helper()
	oldErr, oldOut := os.Stderr, os.Stdout
	er, ew, _ := os.Pipe()
	or, ow, _ := os.Pipe()
	os.Stderr, os.Stdout = ew, ow
	errCh, outCh := make(chan string, 1), make(chan string, 1)
	go func() { b, _ := io.ReadAll(er); errCh <- string(b) }()
	go func() { b, _ := io.ReadAll(or); outCh <- string(b) }()
	runErr := runFleetMeasure([]string{"--config", cfgPath})
	_ = ew.Close()
	_ = ow.Close()
	os.Stderr, os.Stdout = oldErr, oldOut
	stderr, stdout = <-errCh, <-outCh
	if runErr != nil {
		t.Fatalf("runFleetMeasure: %v\nstderr:\n%s", runErr, stderr)
	}
	return stderr, stdout
}

// writeFleetMeasureCfg writes cfg as a config file. Config fields are omitempty, so a script a
// test blanks (the shipped default is a ComfyUI runner) would silently come back from the
// defaults on load: the named keys are written as explicit empty strings, which load as empty.
func writeFleetMeasureCfg(t *testing.T, cfg config.Config, blank ...string) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range blank {
		m[k] = ""
	}
	raw, _ = json.Marshal(m)
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A runner stand-in: records its argv beside the out path (the first positional after the `--`
// terminator the iGPU lanes send) and writes the out file.
const fleetMeasureStub = `import {writeFileSync} from "node:fs";
const argv = process.argv.slice(2);
const out = argv[argv.indexOf("--") + 1];
writeFileSync(out + ".args", argv.join("\n"));
writeFileSync(out, "stub-output");
`

// G15: the fleet-measure probe keyed on videogen_script / musicgen_script, so an sdcpp video or
// audiocpp music box printed "skipped (no videogen_script configured)" and never recorded its
// iGPU footprints. It asks the same *Bound seam the fleet advertisement uses (CT-51).
func TestFleetMeasureProbesAnSdcppVideoAndAnAudiocppMusicBox(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; the stub runner needs it")
	}
	home := t.TempDir()
	t.Setenv("LOCAL_OFFLOAD_HOME", home)
	media := filepath.Join(home, "media")
	stub := filepath.Join(home, "stub.mjs")
	if err := os.WriteFile(stub, []byte(fleetMeasureStub), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "engine")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.MediaDir = media
	// a private lease: the probe takes the media lease, and a test must never touch the machine's
	cfg.StateDir, cfg.GPULockPath = filepath.Join(home, "state"), filepath.Join(home, "gpu.lock")
	cfg.VideoGenScript, cfg.MusicGenScript, cfg.ImageGenScript = "", "", ""
	cfg.VideoGenFamily = "fastwan"
	cfg.VideoGenSdcppScript = stub
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"fastwan": {
		Engine: config.EngineSdcpp, SdcppBin: bin, SdcppModel: "m", SdcppVAE: "v", SdcppT5xxl: "t", SdcppBackend: "vulkan0",
	}}
	cfg.MusicGenEngine = config.EngineAudiocpp
	cfg.AudiocppScript = stub
	cfg.AudiocppBin, cfg.AudiocppMusicModel, cfg.AudiocppBackend = bin, "m", "vulkan"

	stderr, _ := captureFleetMeasure(t, writeFleetMeasureCfg(t, cfg, "videogen_script", "musicgen_script", "imagegen_script"))
	for _, skipped := range []string{"video-gen: skipped", "audio-gen: skipped"} {
		if strings.Contains(stderr, skipped) {
			t.Errorf("a box that binds the sdcpp video family and audio.cpp music must be probed, got %q in:\n%s", skipped, stderr)
		}
	}
	for _, want := range []string{"video-gen: rendering the fast recipe at 9 frames", "video-gen: done", "audio-gen: rendering 5s of music", "audio-gen: done"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr must contain %q:\n%s", want, stderr)
		}
	}
	// and the renders really went through the iGPU lanes' stub runners
	matches, _ := filepath.Glob(filepath.Join(media, "*.args"))
	if len(matches) != 2 {
		t.Fatalf("want two runner invocations (video + music) under %s, got %v", media, matches)
	}
}

func TestFleetMeasureStillSkipsALaneWithNoRendererBoundAndSaysWhich(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOCAL_OFFLOAD_HOME", home)
	cfg := config.Default()
	cfg.MediaDir = filepath.Join(home, "media")
	cfg.StateDir, cfg.GPULockPath = filepath.Join(home, "state"), filepath.Join(home, "gpu.lock")
	cfg.VideoGenScript, cfg.MusicGenScript, cfg.ImageGenScript = "", "", ""
	cfg.VideoGenFamilies, cfg.MusicGenEngine = nil, ""
	stderr, _ := captureFleetMeasure(t, writeFleetMeasureCfg(t, cfg, "videogen_script", "musicgen_script", "imagegen_script"))
	for _, want := range []string{
		"video-gen: skipped (no video route configured - neither videogen_script nor an sdcpp video family)",
		"audio-gen: skipped (no music route configured - neither musicgen_script nor musicgen_engine audiocpp)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr must contain %q:\n%s", want, stderr)
		}
	}
}
