// Package config loads harness configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// Config controls endpoint, model, thresholds, and storage paths.
type Config struct {
	// Endpoint is the llama-server base URL (e.g. http://127.0.0.1:11436).
	Endpoint string `json:"endpoint"`
	// CompletionPath is the native completion route used to pass a GBNF grammar.
	CompletionPath string `json:"completion_path"`
	// Model is the default workhorse (E4B) — used for summarize/extract and as
	// the fallback for any task without a specific route. Empty = dedicated server.
	Model string `json:"model"`
	// TriageModel serves the fast tier (triage/classify). Empty = use Model.
	// NOTE on 8GB: the tiers are swap-exclusive, so routing a few triages to a
	// cold E2B costs a model swap; for latency-critical mixed workloads set this
	// to "" (let E4B handle triage) or batch same-tier calls.
	TriageModel string `json:"triage_model"`
	// EscalationModel is tried once when the primary defers (validation fail / low
	// confidence) BEFORE deferring to Opus. Empty = no escalation (defer directly).
	EscalationModel string `json:"escalation_model"`
	// ReasoningModel is the terminal LOCAL tier (grammar tasks only): after the whole
	// cascade defers, a thinking model gets one shot under a think-wrapped grammar
	// (gbnf.WrapThinking) to reclaim the deferral before falling through to Opus. Run
	// it thinking-OFF — the grammar, not the chat template, supplies the <think> span.
	// Empty = no reasoning tier (defer straight to Opus, the prior behavior).
	ReasoningModel string `json:"reasoning_model,omitempty"`
	// VisionModel is the VLM alias used for the vqa task (multimodal). Empty = no
	// vision route (vqa defers).
	VisionModel string `json:"vision_model,omitempty"`
	// VisionMaxImageBytes caps a single decoded image before it is rejected
	// (guards context/VRAM blowups). 0 = use the loader default.
	VisionMaxImageBytes int `json:"vision_max_image_bytes,omitempty"`
	// OCRMaxTokens caps the OCR (image transcription) output. A dense document page
	// exceeds the built-in 1024 default, hits finish_reason=length, and defers the
	// whole OCR to cloud — so a machine with a strong VLM (e.g. qwen3-vl-8b) can raise
	// this to transcribe long pages locally. 0 = the in-package default (1024).
	OCRMaxTokens int `json:"ocr_max_tokens,omitempty"`
	// VideoFPS is the frame-sampling rate for video_describe (frames/second).
	VideoFPS float64 `json:"video_fps,omitempty"`
	// VideoMaxFrames caps how many sampled frames are sent to the VLM (bounds the
	// 8GB activation budget — frames, not weights, are the VRAM pressure).
	VideoMaxFrames int `json:"video_max_frames,omitempty"`
	// VideoFrameWidth scales each sampled frame to this width (px), aspect kept.
	VideoFrameWidth int `json:"video_frame_width,omitempty"`
	// FFmpegPath is the ffmpeg executable used to sample frames. Default "ffmpeg".
	FFmpegPath string `json:"ffmpeg_path,omitempty"`
	// --- STT / transcribe (Phase A.2) ---
	// STTModel is the llama-swap alias for the default whisper-server upstream
	// (large-v3-turbo). Empty = no STT route (transcribe defers).
	STTModel string `json:"stt_model,omitempty"`
	// STTModelHQ is the quality-escalation whisper upstream (large-v3), used when
	// a transcribe request passes hq=true. Empty = hq falls back to STTModel.
	STTModelHQ string `json:"stt_model_hq,omitempty"`
	// STTLanguage forces the transcription language ("en"/"es"); "" = auto-detect.
	// Per-call params["language"] overrides this. Forcing is more reliable on
	// noisy/code-switching field audio than auto-detect.
	STTLanguage string `json:"stt_language,omitempty"`
	// STTVAD enables Silero VAD (server launched with -vm + --vad) — the biggest
	// hallucination reducer on noisy field audio. Default true.
	STTVAD bool `json:"stt_vad,omitempty"`
	// STTMaxInlineSegments caps how many timestamped segments are inlined in the
	// result (the rest live in the on-disk .segments.json pointer). Default 120.
	STTMaxInlineSegments int `json:"stt_max_inline_segments,omitempty"`
	// STTUnloadAfter force-unloads the whisper upstream after each transcription
	// (zero-always-warm). Default true; set false for a known batch loop.
	STTUnloadAfter bool `json:"stt_unload_after,omitempty"`
	// STTRequestTimeoutSec bounds one transcription HTTP call (long audio at
	// 5-8x realtime). Default 1800 (30 min). Separate from RequestTimeoutSec.
	STTRequestTimeoutSec int `json:"stt_request_timeout_sec,omitempty"`
	// MediaDir is where transcribe writes .srt/.txt/.segments.json. Default
	// <base>/media.
	MediaDir string `json:"media_dir,omitempty"`
	// SVGDir is where generate_svg writes rendered .svg files. Default <base>/svg.
	SVGDir string `json:"svg_dir,omitempty"`
	// --- image generation (generate_image) ---
	// ImageGenScript is the absolute path to render/comfy-generate.mjs (the Node
	// lifecycle wrapper around comfy-render.mjs). Empty = no image route (the task
	// defers), exactly like an empty STTModel/VisionModel.
	ImageGenScript string `json:"imagegen_script,omitempty"`
	// NodePath is the node executable used to run the image script. Default "node".
	NodePath string `json:"node_path,omitempty"`
	// ComfyDir is the local ComfyUI install dir (passed to the script as COMFY_DIR).
	// Default "C:/ComfyUI".
	ComfyDir string `json:"comfy_dir,omitempty"`
	// ImageGenTimeoutSec bounds one render: ComfyUI cold-start (~4min) + first SDXL
	// render (~6min) + margin. Default 720 (12min).
	ImageGenTimeoutSec int `json:"imagegen_timeout_sec,omitempty"`
	// --- image model binding (PER-MACHINE; the harness stays model-agnostic) ---
	// Each box serves the image model its hardware can actually run — an 8GB laptop
	// runs SDXL; a 16GB workstation may run a DiT (HiDream) instead. NONE of those
	// names may be baked into shared code, so the model + its sampler tuning are
	// config, not constants. Every field below is OPTIONAL: leave it empty/zero and
	// comfy-render.mjs keeps its own defaults, so a machine that never sets them
	// (e.g. the laptop's SDXL path) is byte-for-byte unchanged.
	//
	// ImageGenCkpt is the ComfyUI checkpoint filename (CheckpointLoaderSimple), e.g.
	// "RealVisXL_V5.0_fp16.safetensors" (SDXL) or "hidream_o1_image_dev_mxfp8.safetensors"
	// (HiDream, a DiT). Empty = the script's default.
	ImageGenCkpt string `json:"imagegen_ckpt,omitempty"`
	// ImageGenFamily selects the model-correct render graph for this machine's
	// checkpoint: "" = generic SDXL-shaped graph; "hidream-o1" / "hidream-o1-dev" =
	// the official HiDream-O1 pixel-space graph (ModelNoiseScale, patch-seam
	// smoothing, SamplerCustom stack, native 2048). Quality-first: a DiT on the
	// generic graph produces measurable 32px patch blocking.
	ImageGenFamily string `json:"imagegen_family,omitempty"`
	// ImageGenVAE is the standalone VAE filename, or "builtin" to decode with the VAE
	// the CHECKPOINT LOADER supplies. "builtin" is REQUIRED for HiDream: that
	// checkpoint carries no VAE weights (it is pixel-space), so a standalone
	// 4-channel sdxl_vae cannot decode its output. Empty = the script's default
	// (a standalone SDXL VAE).
	ImageGenVAE string `json:"imagegen_vae,omitempty"`
	// ImageGenSteps / ImageGenCFG / ImageGenSampler / ImageGenScheduler are the
	// sampler defaults for THIS machine's image model (SDXL wants cfg~7 + karras;
	// HiDream wants cfg~5 + simple). A per-request "steps" param still overrides.
	// Zero/empty = the script's default.
	ImageGenSteps     int     `json:"imagegen_steps,omitempty"`
	ImageGenCFG       float64 `json:"imagegen_cfg,omitempty"`
	ImageGenSampler   string  `json:"imagegen_sampler,omitempty"`
	ImageGenScheduler string  `json:"imagegen_scheduler,omitempty"`
	// --- generative inpainting (inpaint_image) ---
	// InpaintScript is the absolute path to render/comfy-inpaint.mjs. Empty = no
	// inpaint route (the task defers cleanly), like an empty ImageGenScript.
	InpaintScript string `json:"inpaint_script,omitempty"`
	// InpaintCkpt is the SDXL-class checkpoint this machine inpaints with. Inpainting
	// is a latent-space technique (VAEEncodeForInpaint); a pixel-space DiT binding
	// (HiDream) is NOT valid here even when it is the machine's imagegen_ckpt.
	InpaintCkpt      string  `json:"inpaint_ckpt,omitempty"`
	InpaintVAE       string  `json:"inpaint_vae,omitempty"`
	InpaintSteps     int     `json:"inpaint_steps,omitempty"`
	InpaintCFG       float64 `json:"inpaint_cfg,omitempty"`
	InpaintSampler   string  `json:"inpaint_sampler,omitempty"`
	InpaintScheduler string  `json:"inpaint_scheduler,omitempty"`
	// InpaintTimeoutSec bounds one inpaint render (default 900).
	InpaintTimeoutSec int `json:"inpaint_timeout_sec,omitempty"`
	// --- video / audio generation (generate_video / generate_audio) ---
	// VideoGenScript is the path to render/comfy-video.mjs (the I2V lifecycle wrapper).
	// Empty = no video route (the task defers cleanly), like an empty ImageGenScript.
	VideoGenScript string `json:"videogen_script,omitempty"`
	// RunGraphScript is the generic run-graph runner (offload_run_graph / run-graph):
	// render/comfy-run-graph.mjs. It executes an arbitrary ComfyUI API-format graph and
	// satisfies its per-workflow node manifest. Empty = no run-graph route (defers).
	RunGraphScript string `json:"run_graph_script,omitempty"`
	// VoiceGenScript is the path to render/tts.mjs (Chatterbox TTS; comfyManaged:false).
	// It serves generate_audio kind=voice. Empty = voice defers.
	VoiceGenScript string `json:"voicegen_script,omitempty"`
	// VoiceGenRef is the default reference clip for the GENERALIST voice path (es-MX
	// zero-shot clone). Empty = Chatterbox's built-in voice. Used only when the request
	// has no "clone" param. Per-machine path — never a hardcoded voice.
	VoiceGenRef string `json:"voicegen_ref,omitempty"`
	// VoiceGenFTModel / VoiceGenFTBaseDir / VoiceGenFTRef locate a per-machine FINE-TUNED
	// voice: the merged Chatterbox T3 safetensors, the base engine dir it loads from
	// (ve/s3gen/t3_cfg + grapheme tokenizer.json), and a reference clip. All empty = this
	// box has no fine-tuned voice, so voice=finetuned defers. Shared code ships NO path.
	VoiceGenFTModel   string `json:"voicegen_ft_model,omitempty"`
	VoiceGenFTBaseDir string `json:"voicegen_ft_base_dir,omitempty"`
	VoiceGenFTRef     string `json:"voicegen_ft_ref,omitempty"`
	// VoiceGenFTLang is metadata/record for the fine-tuned voice (e.g. "es"). The FT
	// English-engine path does NOT thread language_id; kept for record + future use.
	VoiceGenFTLang string `json:"voicegen_ft_lang,omitempty"`
	// VoiceGenFT{Temperature,CFGWeight,Exaggeration,RepetitionPenalty} are the per-machine
	// generate() recipe for the fine-tuned voice. 0 = no flag passed (the worker's own
	// default). The tuned values live only in this machine's config, never in shared code.
	VoiceGenFTTemperature       float64 `json:"voicegen_ft_temperature,omitempty"`
	VoiceGenFTCFGWeight         float64 `json:"voicegen_ft_cfg_weight,omitempty"`
	VoiceGenFTExaggeration      float64 `json:"voicegen_ft_exaggeration,omitempty"`
	VoiceGenFTRepetitionPenalty float64 `json:"voicegen_ft_repetition_penalty,omitempty"`
	// MusicGenScript is the path to the ACE-Step music worker (render/comfy-music.mjs,
	// ComfyUI). It serves generate_audio kind=music. Default render/comfy-music.mjs (the
	// B3 worker); set "" to disable (music defers) if the ACE-Step checkpoint isn't present.
	MusicGenScript string `json:"musicgen_script,omitempty"`
	// VideoGenTimeoutSec bounds one video render: ComfyUI cold-start + a long I2V
	// render (Wan native two-stage is slow). Default 1500 (25min).
	VideoGenTimeoutSec int `json:"videogen_timeout_sec,omitempty"`
	// VideoGenWidth / VideoGenHeight / VideoGenFrames are this machine's DEFAULT I2V
	// output size — a per-machine knob so a 16GB box renders 720p while an 8GB box
	// stays at the builder's 480p default. A per-request width/height/frames always
	// wins; 0 = use the builder default (no flag passed). Keeps the harness
	// hardware-agnostic (resolution is config, not a constant).
	VideoGenWidth  int `json:"videogen_width,omitempty"`
	VideoGenHeight int `json:"videogen_height,omitempty"`
	VideoGenFrames int `json:"videogen_frames,omitempty"`
	// VideoGenUpscaleModel is this machine's post-decode upscale model (e.g. an ESRGAN
	// "4x-UltraSharp.pth"); when a request asks to upscale, the video runner uses it +
	// the target size below. Empty = the machine has no upscale model, so upscale is a
	// no-op there. Per-machine + config-driven — never a hardcoded model name.
	VideoGenUpscaleModel  string `json:"videogen_upscale_model,omitempty"`
	VideoGenUpscaleWidth  int    `json:"videogen_upscale_width,omitempty"`
	VideoGenUpscaleHeight int    `json:"videogen_upscale_height,omitempty"`
	// EditPython is the PIL engine for edit_image (a python with Pillow). Empty =
	// derive <comfy_dir>/.venv python (mediaops.ResolveEditPython); unresolvable =
	// edit ops defer. GimpConsolePath is the gimp-console binary for flatten_design
	// (.xcf/.psd -> flat raster + layer list); empty = that op defers. Both are pure
	// CPU (no GPU lock). EditTimeoutSec bounds one edit/media op (default 300).
	EditPython      string `json:"edit_python,omitempty"`
	GimpConsolePath string `json:"gimp_console_path,omitempty"`
	EditTimeoutSec  int    `json:"edit_timeout_sec,omitempty"`
	// VideoGenUnetHigh / VideoGenUnetLow / VideoGenTextEncoder bind THIS machine's Wan 2.2
	// expert weights + text encoder by filename (quality-first weight binding — e.g. a box
	// with the VRAM/RAM headroom names fp8_scaled/fp16 files instead of the render script's
	// Q4 GGUF defaults). Empty = the render script's defaults, exactly as before. The loader
	// is chosen by extension in the script (.gguf vs .safetensors); no model name is baked
	// into shared code.
	VideoGenUnetHigh    string `json:"videogen_unet_high,omitempty"`
	VideoGenUnetLow     string `json:"videogen_unet_low,omitempty"`
	VideoGenTextEncoder string `json:"videogen_text_encoder,omitempty"`
	// AudioGenTimeoutSec bounds one audio synthesis (TTS or ACE-Step). Default 720 (12min).
	AudioGenTimeoutSec int `json:"audiogen_timeout_sec,omitempty"`
	// VideoGenWaitMs is how long a queued video job waits for the single GPU slot before
	// deferring (passed to the runner as GPU_LOCK waitMs). Long because video is the hero
	// job. Default 1200000 (20min).
	VideoGenWaitMs int `json:"videogen_wait_ms,omitempty"`
	// AudioGenWaitMs is how long a queued audio job waits for the GPU slot before deferring.
	// Kept SHORTER than video so a cheap queued TTS isn't starved by a 20-min video job —
	// it defers cleanly after this window. Default 120000 (2min).
	AudioGenWaitMs int `json:"audiogen_wait_ms,omitempty"`
	// GPULockPath overrides the single-slot GPU lock DIRECTORY shared with the render
	// runners (render/gpu-lock.mjs). Empty = the runners' own default (the GPU_LOCK env,
	// else <os-tmpdir>/local-offload-gpu.lock). When set it is also threaded to every gen
	// runner as the GPU_LOCK env, so the Go-side vision gate (LO-1) and the Node runners
	// always contend on the SAME lock.
	GPULockPath string `json:"gpu_lock_path,omitempty"`
	// VisionGPUWaitSec is how long a vision call (vqa/ocr/assess_image/video_describe)
	// waits for the GPU lock held by a generation job before deferring (polled every 2s).
	// While a gen job owns the GPU, llama-swap cannot (re)load the VLM — calling anyway
	// just burns an http_5xx defer to the expensive cloud model (LO-1: 295 of the 337
	// all-time defers landed in ONE such hour). Default 90.
	VisionGPUWaitSec int `json:"vision_gpu_wait_sec,omitempty"`
	// MemoryStack lists the CPU-only, zero-VRAM llama-swap models the GPU-free helper
	// must NEVER unload (the load-bearing mem0 stack). Sourced here (not a buried const)
	// so a renamed/added 3rd CPU member is honored. Threaded to the runner via the
	// MEMORY_STACK env. Default {embeddinggemma, bge-reranker-v2-m3}. This is an
	// UNORDERED keep-alive set — do NOT infer roles from position (use EmbedModel).
	MemoryStack []string `json:"memory_stack,omitempty"`
	// EmbedModelName is the embedding model the judge/kNN embedder requests. Kept
	// separate from MemoryStack (an unordered keep-alive set) so it is unambiguous and
	// reorder-proof. Empty falls back to MemoryStack[0] then "embeddinggemma".
	// Read via the EmbedModel() accessor.
	EmbedModelName string `json:"embed_model,omitempty"`
	// Temperature for deterministic structured output (default 0).
	Temperature float64 `json:"temperature"`
	// MaxRetries is how many correction re-prompts before deferring.
	MaxRetries int `json:"max_retries"`
	// ClassifyMinConfidence: classify results below this (self-reported) defer.
	ClassifyMinConfidence float64 `json:"classify_min_confidence"`
	// ConfidenceMarginThreshold: for triage/classify, if the logprob-derived
	// top-2 legal-class margin at the decision token is below this, escalate to a
	// larger tier (catches genuinely torn calls, e.g. eager-YES). 0 disables the
	// logprob gate. Default 0.35.
	ConfidenceMarginThreshold float64 `json:"confidence_margin_threshold"`
	// MaxInputChars caps input length before context-budget trimming.
	MaxInputChars int `json:"max_input_chars"`
	// CachePath / LedgerPath are bbolt files.
	CachePath  string `json:"cache_path"`
	LedgerPath string `json:"ledger_path"`
	// --- self-learning artifacts (written by the offline `calibrate`/`health`/
	// `train-router`/`optimize` jobs; read at pipeline startup). Empty = feature off. ---
	ThresholdsPath         string             `json:"thresholds_path,omitempty"`          // Phase 2: per-task conformal margin thresholds
	TierOverridesPath      string             `json:"tier_overrides_path,omitempty"`      // Phase 4: health-driven entry-tier bumps + P95 timeouts
	RouterWeightsPath      string             `json:"router_weights_path,omitempty"`      // Phase 5: logistic entry-tier router
	ConfHeadPath           string             `json:"confhead_path,omitempty"`            // Phase 2: logistic correctness head
	RouterLabelsPath       string             `json:"router_labels_path,omitempty"`       // Phase B: synthesized E2B-counterfactual router label sidecar
	ConfHeadLabelsPath     string             `json:"confhead_labels_path,omitempty"`     // Phase 2: cascade-agreement correctness-label sidecar (classify/triage)
	ConfHeadThresholdsPath string             `json:"confhead_thresholds_path,omitempty"` // Phase 2: per-task conformal p(correct) escalation thresholds (Task 3)
	ConfHeadEnabled        bool               `json:"confhead_enabled,omitempty"`         // Phase 2 Task 4: opt-in — gate ADOPT tasks on the head's p(correct). Default false.
	ExemplarsDir           string             `json:"exemplars_dir,omitempty"`            // Phase 6: few-shot exemplar sidecar + selected pool
	ExemplarShots          int                `json:"exemplar_shots,omitempty"`           // Phase 6: 0 = disabled
	AutoHeal               bool               `json:"auto_heal,omitempty"`                // Phase 7: opt-in autonomous tier reload
	TargetErrorRate        map[string]float64 `json:"target_error_rate,omitempty"`        // Phase 2: per-task α for calibration
	// OpusInputPricePerMTok estimates dollar savings ($ per 1M input tokens).
	OpusInputPricePerMTok float64 `json:"opus_input_price_per_mtok"`
	// RequestTimeoutSec for a single model call.
	RequestTimeoutSec int `json:"request_timeout_sec"`
	// --- shadow-labeling flywheel (Phase A.3) ---
	// ShadowEnabled gates sampled capture of non-escalated classify/triage/extract
	// entry-tier rows to the shadow queue for nightly counterfactual labeling.
	// Default false (off by default; never affects request latency or behavior).
	ShadowEnabled bool `json:"shadow_enabled,omitempty"`
	// ShadowRate is the fraction of eligible rows to capture (0.0–1.0).
	// Default 0.10 (10 %). A value of 0 disables capture even when ShadowEnabled.
	ShadowRate float64 `json:"shadow_rate,omitempty"`
	// ShadowQueuePath is the append-only JSONL file for captured shadow items.
	// Default <DataDir>/shadow-queue.jsonl.
	ShadowQueuePath string `json:"shadow_queue_path,omitempty"`
	// SummarizeSimThreshold is the minimum cosine similarity (0..1) between the
	// entry-tier and escalation-tier summary embeddings to count as "agreed".
	// Used by the B2 summarize judge in shadow-label. Default 0.80.
	SummarizeSimThreshold float64 `json:"summarize_sim_threshold,omitempty"`
	// --- P6: agentic-trajectory flywheel (off by default; mirrors ShadowEnabled) ---
	// AgentTrajectoryCaptureEnabled gates sampled capture of standalone agent
	// trajectories to the trajectory queue for offline counterfactual labeling.
	// Default false (off; never affects request latency or agent behavior).
	AgentTrajectoryCaptureEnabled bool `json:"agent_trajectory_capture_enabled,omitempty"`
	// AgentTrajectoryRate is the fraction of completed goals to capture (0.0–1.0).
	// Default 0.10. A value of 0 disables capture even when enabled.
	AgentTrajectoryRate float64 `json:"agent_trajectory_rate,omitempty"`
	// AgentTrajectoryQueuePath is the append-only JSONL capture queue.
	// Default <DataDir>/agent-trajectory-queue.jsonl.
	AgentTrajectoryQueuePath string `json:"agent_trajectory_queue_path,omitempty"`
	// AgentTrajectoryLabelsPath is the correctness-label SIDECAR the P6 flywheel
	// writes (via ledger.AppendLabel — NEVER the savings ledger).
	// Default <DataDir>/agent-trajectory-labels.jsonl.
	AgentTrajectoryLabelsPath string `json:"agent_trajectory_labels_path,omitempty"`
	// --- zero-training kNN entry-tier pre-filter (meta-router v2) ---
	// KNNPreFilterEnabled gates the kNN pre-filter — a bridge before the LR
	// router (router_weights) is trained. When on, classify/triage inputs are
	// embedded and matched against KNNIndexPath to decide whether to skip the
	// E2B tier; it yields to the LR router once that task is trained. Default
	// false; with it off the request path is byte-identical to the prior build.
	KNNPreFilterEnabled bool `json:"knn_prefilter_enabled,omitempty"`
	// KNNIndexPath is the JSONL substrate (task, vec, accept) appended by the
	// nightly shadow-label drain. Default <base>/knn-index.jsonl.
	KNNIndexPath string `json:"knn_index_path,omitempty"`
	// KNNPreFilterK is how many nearest neighbors to poll. Default 7.
	KNNPreFilterK int `json:"knn_prefilter_k,omitempty"`
	// KNNMinNeighbors is the minimum usable rows-per-task before the kNN acts
	// (guards against a too-thin substrate). Default 20.
	KNNMinNeighbors int `json:"knn_min_neighbors,omitempty"`
	// KNNPreFilterThreshold: skip E2B when the fraction of the k nearest
	// neighbors that accepted at E2B is below this. Default 0.5.
	KNNPreFilterThreshold float64 `json:"knn_prefilter_threshold,omitempty"`
	// KNNEmbedTimeoutMs bounds the request-path embedding call (fail-open on
	// timeout). Default 2000.
	KNNEmbedTimeoutMs int `json:"knn_embed_timeout_ms,omitempty"`
	// --- explicit remote NIM tool (`nim` subcommand / offload_nim) ---
	// An opt-in path to an OpenAI-compatible NVIDIA NIM endpoint, separate from the
	// local cascade: the GBNF grammar path and the savings ledger are untouched.
	// NIMEndpoint is the base (with the /v1 segment): NVIDIA's hosted API by
	// default, or a self-hosted NIM container (e.g. http://127.0.0.1:8000/v1).
	NIMEndpoint string `json:"nim_endpoint,omitempty"`
	// NIMModel is the default model id (override per call with --model). The hosted
	// catalog has dozens of free endpoints — browse with `nim --list-models`.
	NIMModel string `json:"nim_model,omitempty"`
	// NIMMaxTokens caps a nim completion when the caller sets none. Reasoning models
	// need headroom (they spend tokens thinking before answering). Default 1024.
	NIMMaxTokens int `json:"nim_max_tokens,omitempty"`
	// NIMTimeoutSec bounds one nim call (large hosted models can be slow). Default 120.
	NIMTimeoutSec int `json:"nim_timeout_sec,omitempty"`
	// NOTE: the NIM API key is deliberately NOT a config field — it is read from the
	// NVIDIA_API_KEY (or NGC_API_KEY) env var so a secret never lands in a tracked
	// config file or the public repo. A self-hosted NIM needs no key.
	// --- fleet-node server (`fleet-serve` / `fleet-measure`; docs/FLEET-NODE.md) ---
	// FleetListen is the fleet-serve bind address. Loopback by default; the
	// production binding is the machine's TAILSCALE address behind
	// --listen-trusted-network (never 0.0.0.0). Port 18811 — the dispatcher
	// owns 18810.
	FleetListen string `json:"fleet_listen,omitempty"`
	// FleetNodeID names this node in /fleet/health. Empty = the OS hostname,
	// resolved at serve time (so a shared config never bakes one box's name).
	FleetNodeID string `json:"fleet_node_id,omitempty"`
	// FleetSampler selects the per-render VRAM footprint source: "auto" (PDH
	// per-process tree on Windows, nvidia-smi global-delta elsewhere), "pdh"
	// (force the tree sampler), "global" (force global-delta — set this when
	// the FLEET-NODE.md Afterburner validation shows PDH disagreeing >15%).
	FleetSampler string `json:"fleet_sampler,omitempty"`
}

// Default returns a config suitable for the verified E4B-QAT+MTP setup.
func Default() Config {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".local-offload")
	return Config{
		Endpoint:                    "http://127.0.0.1:11436",
		CompletionPath:              "/v1/chat/completions", // chat route: server applies the Gemma template; we pass a raw "grammar" field
		Model:                       "offload-e4b",
		TriageModel:                 "gemma4-e2b",     // fast tier for triage/classify
		EscalationModel:             "gemma4-26b-a4b", // MoE escalation tier (experts in RAM via --cpu-moe); part of the served offload family.
		ReasoningModel:              "gemma4-26b-a4b", // terminal local reasoning tier (think-wrapped grammar) before defer-to-cloud. "" disables.
		VisionModel:                 "",               // opt-in: set the machine's VLM alias (empty = vision defers, no phantom)
		VisionMaxImageBytes:         6000000,          // ~6MB cap per image
		OCRMaxTokens:                1024,             // default OCR output cap; raise per-machine for dense docs
		VideoFPS:                    2.0,
		VideoMaxFrames:              12,
		VideoFrameWidth:             512,
		FFmpegPath:                  "ffmpeg",
		STTModel:                    "whisper-stt",
		STTModelHQ:                  "", // opt-in: an accuracy STT tier alias (empty = hq falls back to STTModel)
		STTLanguage:                 "", // auto-detect unless overridden per call
		STTVAD:                      true,
		STTMaxInlineSegments:        120,
		STTUnloadAfter:              true,
		STTRequestTimeoutSec:        1800,
		MediaDir:                    filepath.Join(base, "media"),
		SVGDir:                      filepath.Join(base, "svg"),
		ImageGenScript:              "",
		NodePath:                    "node",
		ComfyDir:                    "C:/ComfyUI",
		ImageGenTimeoutSec:          720,
		InpaintScript:               "", // inpaint route: per-machine SDXL-class binding; empty = defer
		InpaintCkpt:                 "",
		InpaintTimeoutSec:           900,
		VideoGenScript:              "render/comfy-video.mjs",
		RunGraphScript:              "render/comfy-run-graph.mjs",
		VoiceGenScript:              "render/tts.mjs",
		VoiceGenRef:                 "", // generalist default ref clip: per-machine, never shipped
		VoiceGenFTModel:             "", // fine-tuned voice: all empty => voice=finetuned defers
		VoiceGenFTBaseDir:           "",
		VoiceGenFTRef:               "",
		VoiceGenFTLang:              "",
		VoiceGenFTTemperature:       0, // 0 = worker default; tuned recipe lives per-machine
		VoiceGenFTCFGWeight:         0,
		VoiceGenFTExaggeration:      0,
		VoiceGenFTRepetitionPenalty: 0,
		MusicGenScript:              "render/comfy-music.mjs", // B3 ACE-Step music worker; "" => music defers
		VideoGenTimeoutSec:          1500,
		AudioGenTimeoutSec:          720,
		EditTimeoutSec:              300, // edit_image / media ops (CPU; no GPU lock)
		VideoGenWaitMs:              1200000, // 20min — video is the hero job
		AudioGenWaitMs:              120000,  // 2min — a queued TTS defers fast, never starved by a long video
		GPULockPath:                 "",      // runners' default (GPU_LOCK env, else <tmpdir>/local-offload-gpu.lock)
		VisionGPUWaitSec:            90,      // LO-1: bounded wait for the gen lock before a vision call defers
		MemoryStack:                 []string{"embeddinggemma", "bge-reranker-v2-m3"},
		EmbedModelName:              "embeddinggemma", // explicit; reorder-proof (not MemoryStack position)
		Temperature:                 0,
		MaxRetries:                  1,
		ClassifyMinConfidence:       0.45,
		ConfidenceMarginThreshold:   0.35,
		MaxInputChars:               24000, // ~6k tokens, well under ctx 8192
		CachePath:                   filepath.Join(base, "cache.db"),
		LedgerPath:                  filepath.Join(base, "ledger.jsonl"), // append-only JSONL (concurrent read/append)
		ThresholdsPath:              filepath.Join(base, "thresholds.json"),
		TierOverridesPath:           filepath.Join(base, "tier_overrides.json"),
		RouterWeightsPath:           filepath.Join(base, "router-weights.json"),
		RouterLabelsPath:            filepath.Join(base, "router-labels.jsonl"),
		ConfHeadPath:                filepath.Join(base, "confhead-weights.json"),
		ConfHeadLabelsPath:          filepath.Join(base, "confhead-labels.jsonl"),
		ConfHeadThresholdsPath:      filepath.Join(base, "confhead-thresholds.json"),
		ExemplarsDir:                filepath.Join(base, "exemplars"),
		ExemplarShots:               0, // off until the pool is built + measured
		AutoHeal:                    false,
		OpusInputPricePerMTok:       15.0,
		RequestTimeoutSec:           120,
		ShadowEnabled:               false,
		ShadowRate:                  0.10,
		ShadowQueuePath:             filepath.Join(base, "shadow-queue.jsonl"),
		SummarizeSimThreshold:       0.80,

		AgentTrajectoryCaptureEnabled: false,
		AgentTrajectoryRate:           0.10,
		AgentTrajectoryQueuePath:      filepath.Join(base, "agent-trajectory-queue.jsonl"),
		AgentTrajectoryLabelsPath:     filepath.Join(base, "agent-trajectory-labels.jsonl"),
		KNNPreFilterEnabled:           false,
		KNNIndexPath:                  filepath.Join(base, "knn-index.jsonl"),
		KNNPreFilterK:                 7,
		KNNMinNeighbors:               20,
		KNNPreFilterThreshold:         0.5,
		KNNEmbedTimeoutMs:             2000,
		NIMEndpoint:                   "https://integrate.api.nvidia.com/v1", // NVIDIA hosted; or a self-hosted NIM /v1 base
		NIMModel:                      "nvidia/nemotron-3-ultra-550b-a55b",
		NIMMaxTokens:                  1024,
		NIMTimeoutSec:                 120,
		FleetListen:                   "127.0.0.1:18811", // fleet-serve bind (18810 = the dispatcher's)
		FleetNodeID:                   "",                // "" = hostname at serve time
		FleetSampler:                  "auto",            // auto|pdh|global (FLEET-NODE.md)
	}
}

// Load merges a JSON config file over the defaults. Missing file => defaults.
// A leading "~/" in any path-typed field is expanded to the user home dir
// (LO-4: config.example.json ships "~/.local-offload/..." paths that were
// previously taken literally, silently creating a "~" directory in the cwd).
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	warnUnknownKeys(b)
	if home, herr := os.UserHomeDir(); herr == nil {
		expandUserPaths(&c, home)
	}
	return c, nil
}

// pathFields enumerates every path-typed Config field (file, dir, script, or
// executable path) for tilde expansion. Keep in sync with the struct — a new
// *Path/*Dir/*Script field belongs here.
func pathFields(c *Config) []*string {
	return []*string{
		&c.FFmpegPath, &c.MediaDir, &c.SVGDir,
		&c.ImageGenScript, &c.NodePath, &c.ComfyDir,
		&c.InpaintScript,
		&c.VideoGenScript, &c.RunGraphScript, &c.VoiceGenScript, &c.MusicGenScript, &c.GPULockPath,
		&c.VoiceGenRef, &c.VoiceGenFTModel, &c.VoiceGenFTBaseDir, &c.VoiceGenFTRef,
		&c.EditPython, &c.GimpConsolePath,
		&c.CachePath, &c.LedgerPath,
		&c.ThresholdsPath, &c.TierOverridesPath, &c.RouterWeightsPath,
		&c.ConfHeadPath, &c.RouterLabelsPath, &c.ConfHeadLabelsPath,
		&c.ConfHeadThresholdsPath, &c.ExemplarsDir,
		&c.ShadowQueuePath, &c.AgentTrajectoryQueuePath, &c.AgentTrajectoryLabelsPath,
		&c.KNNIndexPath,
	}
}

// expandUserPaths expands a leading "~/" (or a bare "~") in every path-typed
// field to the given home dir. Only the home shorthand is expanded — "~user"
// forms are left untouched (rare, and ambiguous on Windows).
func expandUserPaths(c *Config, home string) {
	for _, p := range pathFields(c) {
		*p = ExpandTilde(*p, home)
	}
}

// ExpandTilde expands a leading "~/" (or "~\" or a bare "~") in p to home.
// Anything else — including "~user/..." — is returned unchanged.
func ExpandTilde(p, home string) string {
	if home == "" || p == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home, p[2:])
	}
	return p
}

// warnUnknownKeys prints a stderr warning for any JSON key that doesn't map to a
// Config field — so a typo like "escalaton_model" surfaces instead of being
// silently ignored. It never fails: the valid fields still load.
func warnUnknownKeys(b []byte) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return
	}
	known := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		name := strings.SplitN(t.Field(i).Tag.Get("json"), ",", 2)[0]
		if name != "" && name != "-" {
			known[name] = true
		}
	}
	for k := range raw {
		if !known[k] {
			fmt.Fprintf(os.Stderr, "warning: unknown config key %q (ignored — typo?)\n", k)
		}
	}
}

// EmbedModel is the embedding model shared code (the judge/kNN embedder) requests —
// a config accessor, never a hardcode, so the harness stays model-agnostic. Resolution
// order: the explicit EmbedModelName; else MemoryStack[0] (back-compat for configs that
// only set the stack); else the "embeddinggemma" fallback (lives ONLY here, mirroring
// the router pattern) so an empty config never sends a blank model name to the server.
// It does NOT infer a role from MemoryStack position beyond that back-compat step —
// set embed_model explicitly if the stack is not embedder-first.
func (c Config) EmbedModel() string {
	if c.EmbedModelName != "" {
		return c.EmbedModelName
	}
	if len(c.MemoryStack) > 0 && c.MemoryStack[0] != "" {
		return c.MemoryStack[0]
	}
	return "embeddinggemma"
}

// EnsureDirs creates the parent dirs for the store files.
func (c Config) EnsureDirs() error {
	for _, p := range []string{c.CachePath, c.LedgerPath, c.ThresholdsPath, c.RouterWeightsPath, c.TierOverridesPath, c.ConfHeadPath, c.ConfHeadLabelsPath, c.ConfHeadThresholdsPath, c.KNNIndexPath} {
		if p == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
	}
	if c.ExemplarsDir != "" { // a directory, not a file
		if err := os.MkdirAll(c.ExemplarsDir, 0o755); err != nil {
			return err
		}
	}
	if c.MediaDir != "" { // a directory, not a file
		if err := os.MkdirAll(c.MediaDir, 0o755); err != nil {
			return err
		}
	}
	if c.SVGDir != "" { // a directory, not a file
		if err := os.MkdirAll(c.SVGDir, 0o755); err != nil {
			return err
		}
	}
	return nil
}
