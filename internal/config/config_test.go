package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultVideoFields(t *testing.T) {
	c := Default()
	if c.VideoFPS != 2.0 {
		t.Errorf("VideoFPS = %v, want 2.0", c.VideoFPS)
	}
	if c.VideoMaxFrames != 12 {
		t.Errorf("VideoMaxFrames = %d, want 12", c.VideoMaxFrames)
	}
	if c.VideoFrameWidth != 512 {
		t.Errorf("VideoFrameWidth = %d, want 512", c.VideoFrameWidth)
	}
	if c.FFmpegPath != "ffmpeg" {
		t.Errorf("FFmpegPath = %q, want \"ffmpeg\"", c.FFmpegPath)
	}
}

func TestDefaultSTTFields(t *testing.T) {
	c := Default()
	if c.STTModel != "whisper-stt" {
		t.Errorf("STTModel = %q, want \"whisper-stt\"", c.STTModel)
	}
	if c.STTModelHQ != "whisper-stt-hq" {
		t.Errorf("STTModelHQ = %q, want \"whisper-stt-hq\"", c.STTModelHQ)
	}
	if !c.STTVAD {
		t.Error("STTVAD should default true")
	}
	if c.STTMaxInlineSegments != 120 {
		t.Errorf("STTMaxInlineSegments = %d, want 120", c.STTMaxInlineSegments)
	}
	if !c.STTUnloadAfter {
		t.Error("STTUnloadAfter should default true (zero-always-warm)")
	}
	if c.STTRequestTimeoutSec != 1800 {
		t.Errorf("STTRequestTimeoutSec = %d, want 1800", c.STTRequestTimeoutSec)
	}
	if c.MediaDir == "" {
		t.Error("MediaDir should default to a non-empty path")
	}
}

// TestDefaultGenerationFields: the video/audio generation defaults match the
// brief verbatim — VideoGenScript=render/comfy-video.mjs, VoiceGenScript=render/tts.mjs,
// MusicGenScript=render/comfy-music.mjs (the B3 ACE-Step worker). Per-task timeouts and
// waitMs (so a queued TTS isn't starved by a 20-min video job) are present and positive.
func TestDefaultGenerationFields(t *testing.T) {
	c := Default()
	if c.VideoGenScript != "render/comfy-video.mjs" {
		t.Errorf("VideoGenScript = %q, want \"render/comfy-video.mjs\"", c.VideoGenScript)
	}
	if c.VoiceGenScript != "render/tts.mjs" {
		t.Errorf("VoiceGenScript = %q, want \"render/tts.mjs\"", c.VoiceGenScript)
	}
	if c.MusicGenScript != "render/comfy-music.mjs" {
		t.Errorf("MusicGenScript = %q, want \"render/comfy-music.mjs\" (B3 worker)", c.MusicGenScript)
	}
	if c.VideoGenTimeoutSec <= 0 {
		t.Errorf("VideoGenTimeoutSec = %d, want > 0", c.VideoGenTimeoutSec)
	}
	if c.AudioGenTimeoutSec <= 0 {
		t.Errorf("AudioGenTimeoutSec = %d, want > 0", c.AudioGenTimeoutSec)
	}
	if c.VideoGenWaitMs <= 0 {
		t.Errorf("VideoGenWaitMs = %d, want > 0", c.VideoGenWaitMs)
	}
	if c.AudioGenWaitMs <= 0 {
		t.Errorf("AudioGenWaitMs = %d, want > 0", c.AudioGenWaitMs)
	}
}

// TestDefaultMemoryStack: the CPU memory stack the GPU-free helper must never unload
// is sourced from config (not a buried const) so a renamed/added member is honored.
// Default carries the two canonical CPU-only members.
func TestDefaultMemoryStack(t *testing.T) {
	c := Default()
	want := map[string]bool{"embeddinggemma": true, "bge-reranker-v2-m3": true}
	if len(c.MemoryStack) != len(want) {
		t.Fatalf("MemoryStack = %v, want %v", c.MemoryStack, want)
	}
	for _, m := range c.MemoryStack {
		if !want[m] {
			t.Errorf("MemoryStack has unexpected member %q", m)
		}
	}
}

func TestKNNDefaults(t *testing.T) {
	c := Default()
	if c.KNNPreFilterEnabled {
		t.Fatal("kNN must be OFF by default")
	}
	if c.KNNPreFilterK != 7 {
		t.Fatalf("KNNPreFilterK default: want 7, got %d", c.KNNPreFilterK)
	}
	if c.KNNMinNeighbors != 20 {
		t.Fatalf("KNNMinNeighbors default: want 20, got %d", c.KNNMinNeighbors)
	}
	if c.KNNPreFilterThreshold != 0.5 {
		t.Fatalf("KNNPreFilterThreshold default: want 0.5, got %v", c.KNNPreFilterThreshold)
	}
	if c.KNNEmbedTimeoutMs != 2000 {
		t.Fatalf("KNNEmbedTimeoutMs default: want 2000, got %d", c.KNNEmbedTimeoutMs)
	}
	if filepath.Base(c.KNNIndexPath) != "knn-index.jsonl" {
		t.Fatalf("KNNIndexPath default basename: want knn-index.jsonl, got %q", c.KNNIndexPath)
	}
}
