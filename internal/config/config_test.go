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
