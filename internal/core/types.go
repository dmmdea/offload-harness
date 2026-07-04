// Package core holds the shared types used across the local-offload harness.
package core

import "encoding/json"

// TaskType is one of the supported offload task kinds.
type TaskType string

const (
	TaskSummarize TaskType = "summarize"
	TaskClassify  TaskType = "classify"
	TaskExtract   TaskType = "extract"
	TaskTriage    TaskType = "triage"
	TaskVQA       TaskType = "vqa"
	TaskOCR       TaskType = "ocr"
	// TaskExtractImage is a COMPOSITE: OCR the image, then run the existing text
	// extract over the OCR text (its GBNF grammar + verbatim grounding + schema).
	// It is NOT a vision task (it composes ocr + extract, each of which dispatches
	// itself), so it is deliberately excluded from isVisionTask.
	TaskExtractImage TaskType = "extract_image"
	// TaskAssessImage is a GBNF-constrained QA over an image: emit
	// {has_people, has_text, matches_brief, notes} so a generated render can be
	// checked against hard exclusions (no people / no text). It IS a vision task
	// (single multimodal call with a grammar), so it is in isVisionTask.
	TaskAssessImage TaskType = "assess_image"
	// TaskVideoDescribe samples frames from a video and asks the VLM a free-text
	// question about them. Frames are fed to the SAME multi-image vision path as
	// vqa; it is its own branch (frame sampling happens first), so it is NOT in
	// isVisionTask.
	TaskVideoDescribe TaskType = "video_describe"
	// TaskTranscribe converts a local audio/video file to text on a free local
	// whisper.cpp whisper-server (STT). The audio NEVER passes through the text
	// Gemma cascade; it is dispatched directly in pipeline.Run (no prompt, no
	// grammar), like extract_image. Returns {gist, segments[]} + writes SRT.
	TaskTranscribe TaskType = "transcribe"
	// TaskGenerateImage renders a text prompt to a PNG on the LOCAL ComfyUI (SDXL/
	// RealVisXL) for free. Its own branch in pipeline.Run (no prompt cascade, no
	// grammar) — it shells out to render/comfy-generate.mjs, which takes the shared
	// GPU lock and starts/stops ComfyUI. Returns {image_path,width,height,seed}.
	TaskGenerateImage TaskType = "generate_image"
	// TaskGenerateSVG renders a brand-agnostic parametric SVG component (gauge,
	// comparison-bar, chromatogram, icon) from a JSON spec via internal/svgkit —
	// pure Go, no model/GPU. Its own branch in pipeline.Run. params: kind (string),
	// spec (object), out (string path). Returns {svg_path, width, height}.
	TaskGenerateSVG TaskType = "generate_svg"
	// TaskGenerateVideo animates a still into a short b-roll clip on the LOCAL
	// ComfyUI (HunyuanVideo 1.5 480p I2V / Wan 2.2). Its own branch in pipeline.Run
	// (no text cascade, no grammar) — it shells out to render/comfy-video.mjs via
	// internal/gpugen, which takes the shared single-slot GPU lock and starts/stops
	// ComfyUI with process-tree-kill on timeout. Returns {video_path, seed}.
	TaskGenerateVideo TaskType = "generate_video"
	// TaskGenerateAudio synthesizes audio on the LOCAL GPU: kind=voice (Chatterbox
	// TTS via render/tts.mjs, no ComfyUI) or kind=music (ACE-Step via ComfyUI). Its
	// own branch in pipeline.Run, dispatching by kind to VoiceGenScript/MusicGenScript
	// through internal/gpugen (shared GPU lock + process-tree-kill). Returns
	// {audio_path, kind, seed}.
	TaskGenerateAudio TaskType = "generate_audio"
)

// Valid reports whether t is a known task type.
func (t TaskType) Valid() bool {
	switch t {
	case TaskSummarize, TaskClassify, TaskExtract, TaskTriage, TaskVQA, TaskOCR, TaskExtractImage, TaskAssessImage, TaskVideoDescribe, TaskTranscribe, TaskGenerateImage, TaskGenerateSVG, TaskGenerateVideo, TaskGenerateAudio:
		return true
	}
	return false
}

// Request is a normalized offload request handed to the pipeline.
type Request struct {
	Task   TaskType       `json:"task"`
	Input  string         `json:"input"`            // the text to operate on
	Image  string         `json:"image,omitempty"`  // vqa: a local image path or a data:image/... URI
	Video  string         `json:"video,omitempty"`  // video_describe: a local video file path
	Audio  string         `json:"audio,omitempty"`  // transcribe: a local audio/video file path
	Params map[string]any `json:"params,omitempty"` // labels []string, schema map, question string, max_points int
}

// Meta is per-call telemetry returned to the caller and recorded in the ledger.
type Meta struct {
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	LatencyMs int64   `json:"latency_ms"`
	TokPerSec float64 `json:"tok_per_s"`
	CacheHit  bool    `json:"cache_hit"`
	Model     string  `json:"model"`
	Retries   int     `json:"retries"`
	// Escalations counts how many cascade tiers were climbed before this result
	// (0 = answered by the entry tier; >0 = a bigger local model was needed).
	Escalations int `json:"escalations,omitempty"`
	// Reasoning marks a result produced by the terminal local reasoning tier (a
	// reclaimed cloud deferral). It runs on the SAME model as the escalation tier
	// (qwythos) and so shares Model — this flag is what tells the two apart in the
	// ledger / `stats` reports.
	Reasoning bool `json:"reasoning,omitempty"`
	// --- self-learning signals (logged to the ledger; free, no extra inference) ---
	Margin          float64            `json:"margin,omitempty"`           // logprob decision margin (triage/classify); 0 = N/A
	Truncated       bool               `json:"truncated,omitempty"`        // hit token limit
	Grounded        *bool              `json:"grounded,omitempty"`         // extract/summary values appear in source (nil = N/A)
	EscalatedAgreed *bool              `json:"escalated_agreed,omitempty"` // higher tier agreed with the smaller (nil = no escalation)
	ErrClass        string             `json:"err_class,omitempty"`        // oom|timeout|http_5xx|conn_refused on infra failure; gpu_busy = vision call skipped, a gen job held the GPU lock (LO-1)
	Feat            map[string]float64 `json:"feat,omitempty"`             // cheap input features for the entry-tier router
}

// Result is the harness outcome. On success Data holds the validated task output.
// On a defer, Deferred is true and the caller (Claude) should handle the task itself.
type Result struct {
	OK       bool            `json:"ok"`
	Deferred bool            `json:"deferred,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Data     json.RawMessage `json:"result,omitempty"`
	Partial  string          `json:"partial,omitempty"`
	Meta     Meta            `json:"meta"`
}

// Deferf builds a deferred Result (harness could not complete; Claude should).
func Deferf(reason, partial string, meta Meta) Result {
	return Result{OK: false, Deferred: true, Reason: reason, Partial: partial, Meta: meta}
}
