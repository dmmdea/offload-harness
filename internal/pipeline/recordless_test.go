package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// The routing default must be FAIL-SAFE. RunTier does tasks.Build + a TEXT generate
// and never enters the media dispatch, and tasks.Build SUCCEEDS for the visual tasks —
// so a visual task sent down that path returns a confident answer about an image the
// model never saw. Anything not explicitly text-only must therefore take Run.
func TestOnlyTheFourTextTasksMayTakeTheSingleTierPath(t *testing.T) {
	for _, task := range []core.TaskType{
		core.TaskSummarize, core.TaskClassify, core.TaskTriage, core.TaskExtract,
	} {
		if !textOnlyTask(task) {
			t.Errorf("%s should use RunTier (one named tier, no model swap under the planner)", task)
		}
	}
	// Every visual/media task, and anything new, must go through Run.
	for _, task := range []core.TaskType{
		core.TaskVQA, core.TaskOCR, core.TaskAssessImage, core.TaskVideoDescribe,
		core.TaskTranscribe, core.TaskGenerateImage, core.TaskInpaintImage,
		core.TaskGenerateVideo, core.TaskGenerateAudio, core.TaskRunGraph,
		core.TaskEditImage, core.TaskMedia, core.TaskExtractImage, core.TaskGenerateSVG,
		core.TaskType("some_future_task"),
	} {
		if textOnlyTask(task) {
			t.Errorf("%s must NOT take RunTier — tasks.Build may succeed and yield a text hallucination", task)
		}
	}
}

// The file-bearing params must land on the FIELDS the pipeline reads. These were never
// assigned, which is the whole reason the agent loop could not see, listen or draw.
func TestOffloadRequestLiftsFileParamsOntoTheRequest(t *testing.T) {
	req := offloadRequest("vqa", "what is in this?", map[string]any{
		"image":    "  /tmp/a.png  ", // whitespace is trimmed
		"video":    "/tmp/b.mp4",
		"audio":    "/tmp/c.wav",
		"question": "kept in params",
	})
	if req.Image != "/tmp/a.png" {
		t.Errorf("Image = %q, want the trimmed path", req.Image)
	}
	if req.Video != "/tmp/b.mp4" || req.Audio != "/tmp/c.wav" {
		t.Errorf("Video/Audio not lifted: %q / %q", req.Video, req.Audio)
	}
	if req.Task != core.TaskVQA {
		t.Errorf("Task = %q", req.Task)
	}
	// params must survive intact — the task builders still read them.
	if req.Params["question"] != "kept in params" {
		t.Errorf("params were not preserved: %+v", req.Params)
	}
}

func TestOffloadRequestToleratesNilAndNonStringParams(t *testing.T) {
	if got := offloadRequest("summarize", "x", nil); got.Image != "" || got.Video != "" || got.Audio != "" {
		t.Errorf("nil params should yield empty file fields, got %+v", got)
	}
	got := offloadRequest("vqa", "x", map[string]any{"image": 42})
	if got.Image != "" {
		t.Errorf("a non-string image param must not be coerced, got %q", got.Image)
	}
}

// THE FAIL-OPEN REGRESSION TEST. An agent asking for OCR without an image must get a
// DEFER it can react to — never a confident text answer about an image that was never
// loaded. Before the routing fix this path did tasks.Build (which succeeds for ocr) and
// a text generate.
//
// No vision model is configured here, so Run refuses at its first gate without any
// network call; the point is that the result is a defer, not prose.
func TestOfflineOCRWithoutAnImageDefersRatherThanHallucinating(t *testing.T) {
	cfg := config.Default()
	cfg.VisionModel = "" // no vision route configured
	off := NewRecordlessOffload(cfg, cfg.Model, 5*time.Second)

	out, err := off(context.Background(), "ocr", "transcribe this page", nil)
	if err != nil {
		t.Fatalf("offload returned a fatal error instead of a defer: %v", err)
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("result is not JSON (%v): %s", jerr, out)
	}
	if got["deferred"] != true {
		t.Fatalf("an imageless OCR must DEFER, got: %s", out)
	}
	if r, _ := got["reason"].(string); !strings.Contains(strings.ToLower(r), "vision") {
		t.Errorf("defer reason should name the missing vision route, got %q", r)
	}
}

// Same shape for a bound vision model but a missing image file: the second gate.
func TestVisionTaskWithAMissingImageFileDefers(t *testing.T) {
	cfg := config.Default()
	cfg.VisionModel = "some-vlm"
	off := NewRecordlessOffload(cfg, cfg.Model, 5*time.Second)

	out, err := off(context.Background(), "ocr", "transcribe", map[string]any{
		"image": "/definitely/not/a/real/file.png",
	})
	if err != nil {
		t.Fatalf("fatal error instead of a defer: %v", err)
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("result is not JSON (%v): %s", jerr, out)
	}
	if got["deferred"] != true {
		t.Fatalf("a missing image must DEFER, got: %s", out)
	}
}
