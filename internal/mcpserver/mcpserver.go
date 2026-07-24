// Package mcpserver exposes the offload pipeline as MCP tools over stdio so
// Claude Code can delegate grunt work. Tools return the full Result JSON as
// text — a defer is a valid result (Claude then does the task itself), not an
// error.
package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediaops"
	"github.com/dmmdea/offload-harness/internal/nimclient"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

type Server struct{ p *pipeline.Pipeline }

func New(p *pipeline.Pipeline) *Server { return &Server{p: p} }

// parseArgs unmarshals the raw tool arguments into in. On a decode error it
// returns a non-nil {deferred:true, reason:"bad arguments: <err>"} result that
// the handler must return verbatim (LO-10: previously every handler did
// `_ = json.Unmarshal(...)`, silently running the tool on zero values — e.g. a
// wrongly-typed "text" became an empty input and produced a misleading
// "input too small to offload" defer). Absent/null arguments keep the prior
// zero-value behavior: required-field validation stays with the task itself.
func parseArgs(raw json.RawMessage, in any) *mcp.CallToolResult {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, in); err != nil {
		res, _ := jsonResult(map[string]any{"deferred": true, "reason": "bad arguments: " + err.Error()})
		return res
	}
	return nil
}

// Run serves the MCP tools on stdin/stdout until the client disconnects.
func (s *Server) Run(ctx context.Context, version string) error {
	srv := mcp.NewServer(&mcp.Implementation{Name: "local-offload", Version: version}, nil)

	// Discovery FIRST (LO-18): before this tool existed, offload_nim was the only
	// tool that named or listed any model, so an agent inspecting the harness
	// concluded the text/LLM capability was NIM's cloud catalog and never
	// discovered the LOCAL model cascade every other tool runs on. offload_status
	// makes the local surface enumerable: configured roster + live served models
	// + which media engines this machine has + the (only) remote surface.
	srv.AddTool(&mcp.Tool{
		Name:        "offload_status",
		Description: "Discover this harness's capability — call this FIRST when inspecting what the harness can do. Returns {local:{endpoint, roster{workhorse,triage,escalation,reasoning,vision,stt,stt_hq,embed}, served_now[...] (live model ids from the LOCAL llama-swap endpoint)}, media:{...this machine's configured generation engines}, remote:{nim_endpoint, nim_default_model, nim_key_present}}. Every offload_* tool except offload_nim runs on the LOCAL models in the roster (free, on-box, no cloud); offload_nim is the ONLY remote/cloud surface. An empty roster entry means that capability defers on this machine.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, s.handleStatus)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_summarize",
		Description: "Summarize text on the LOCAL model cascade (free, on-box, no cloud — see offload_status for the live roster). Use for bulk/low-judgment summaries to keep tokens out of your context. Returns {summary, bullets}; if it can't do it confidently it returns deferred:true and you should summarize it yourself. Triggers: summarize / tl;dr / gist / digest / recap / condense a doc, log, transcript, article, or thread.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to summarize"},"max_points":{"type":"integer","description":"max bullet points (default 5)"}},"required":["text"]}`),
	}, s.handleSummarize)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_classify",
		Description: "Classify text into one of the given labels on the LOCAL model cascade (free, on-box, no cloud). Returns {label, confidence}; low-confidence results are deferred back to you. Triggers: classify / categorize / label / tag / bucket / route text into one of a known set.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to classify"},"labels":{"type":"array","items":{"type":"string"},"description":"allowed labels (>=2)"}},"required":["text","labels"]}`),
	}, s.handleClassify)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_extract",
		Description: "Extract structured fields from text on the LOCAL model cascade (free, on-box, no cloud), constrained to the provided JSON schema. Returns the extracted object or defers. Triggers: extract / parse / pull out structured fields from text into a schema (names, dates, amounts, entities).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to extract fields from"},"schema":{"type":"object","description":"JSON schema with a properties object describing the fields to extract"}},"required":["text","schema"]}`),
	}, s.handleExtract)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_triage",
		Description: "Answer a yes/no/unsure question about text on the LOCAL model cascade (free, on-box, no cloud). Returns {decision, reason} or defers. Triggers: a yes/no/unsure check on text — 'does this contain X?', 'is this relevant/spam/safe?', 'should this be flagged?'.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to evaluate"},"question":{"type":"string","description":"a yes/no question about the text"}},"required":["text","question"]}`),
	}, s.handleTriage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_vqa",
		Description: "Answer a question about an IMAGE on a free local vision model (VQA). image is a local file path or a data:image/... URI; question is what to ask about it. Returns {answer}; if it can't answer confidently it returns deferred:true and you should look at the image yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"question":{"type":"string","description":"the question to answer about the image"}},"required":["image","question"]}`),
	}, s.handleVQA)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_video_describe",
		Description: "Answer a question about a VIDEO on a free local vision model. It samples frames from the video and reasons over them. video is a LOCAL file path; question is what to ask. Returns {answer} (which notes what the relevant frames show); if it can't answer confidently it returns deferred:true and you should watch the video yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"video":{"type":"string","description":"local video file path"},"question":{"type":"string","description":"the question to answer about the video"}},"required":["video","question"]}`),
	}, s.handleVideoDescribe)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_transcribe",
		Description: "Transcribe a local AUDIO or VIDEO file to text on a free local whisper model (STT). audio is a LOCAL file path (mp3/m4a/wav/mp4/...); language is optional ('en','es', or 'auto' — default auto-detect); set hq=true for the higher-quality (slower) model on hard/noisy clips. Returns {gist (preview), language, duration_sec, num_segments, segments[{id,start,end,text}] (timestamped spans — pull only the ones you need), srt_path, text_path, json_path}. The full transcript + SRT are written to disk; read the spans/paths you need. If it can't transcribe confidently it returns deferred:true and you should handle the audio yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"audio":{"type":"string","description":"local audio or video file path"},"language":{"type":"string","description":"en, es, or auto (default auto-detect); ignored by an openai-protocol hq tier (mtmd ASR detects language itself)"},"hq":{"type":"boolean","description":"use the configured higher-accuracy STT tier (slower) for hard/noisy/multilingual audio; note the accuracy tier may return a single full-span segment instead of timestamps"},"select":{"type":"array","items":{"type":"string"},"description":"optional: return ONLY these top-level result fields (e.g. [\"gist\",\"language\",\"num_segments\",\"srt_path\"]) to skip the verbose segments[] and keep your context lean — read the full transcript/spans from srt_path or json_path when you need them"}},"required":["audio"]}`),
	}, s.handleTranscribe)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_extract_image",
		Description: "Extract structured fields from an IMAGE on a free local model: it OCRs the image, then extracts the fields from the transcribed text constrained to the provided JSON schema (values are grounded against the OCR text). image is a local file path or a data:image/... URI; schema is a JSON schema with a properties object. Returns the extracted object or defers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"schema":{"type":"object","description":"JSON schema with a properties object describing the fields to extract"}},"required":["image","schema"]}`),
	}, s.handleExtractImage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_assess_image",
		Description: "QA a generated IMAGE against hard exclusions on a free local vision model. Emits a grammar-constrained {has_people, has_text, matches_brief, notes}: has_people=true if any person/face/body part is visible, has_text=true if any readable letters/words/numbers are rendered, matches_brief=whether it matches the optional brief (true if no brief), notes=one short phrase. image is a local file path or a data:image/... URI; brief is optional. Returns the object or deferred:true.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"brief":{"type":"string","description":"optional description the image should match"}},"required":["image"]}`),
	}, s.handleAssessImage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_ocr",
		Description: "Transcribe ALL text in an IMAGE on a free local vision model (OCR). image is a local file path or a data:image/... URI. Returns {text} with the transcribed text in reading order; if it can't transcribe confidently it returns deferred:true and you should read the image yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"}},"required":["image"]}`),
	}, s.handleOCR)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_image",
		Description: "Generate an IMAGE from a text prompt on the LOCAL ComfyUI for FREE — no cloud, runs on the local GPU, using THIS machine's configured model at its highest-quality settings (see offload_status media; e.g. HiDream-O1 bf16 at native 2048 via its official graph, or SDXL on smaller boxes). QUALITY-FIRST: renders can take many minutes — that is intended; do not lower steps/resolution to speed things up unless the caller explicitly asks for a draft. prompt is required (prose sentences beat tag lists on DiT models; quoted text renders as literal text); optional: negative (active on models served with real CFG), width/height (default = the model's native resolution), steps, seed, out. It auto-starts ComfyUI and takes the shared single-slot GPU lock, so it serializes with other local gen/inference and may wait. Returns {image_path, width, height, seed}. On any failure it returns deferred:true — then generate the image another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"positive text prompt describing the image"},"negative":{"type":"string","description":"hard exclusions, e.g. people, text, watermark"},"out":{"type":"string","description":"output PNG path (optional; default under the media dir)"},"width":{"type":"integer","description":"width px (default 1024)"},"height":{"type":"integer","description":"height px (default 1024)"},"steps":{"type":"integer","description":"sampler steps (default 30)"},"seed":{"type":"integer","description":"RNG seed for reproducibility"}},"required":["prompt"]}`),
	}, s.handleGenerateImage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_run_graph",
		Description: "Execute an arbitrary ComfyUI API-format graph on the LOCAL ComfyUI, satisfying a per-workflow node manifest (custom node packs @ pinned commits + model files) first. Generic: the caller owns ALL graph semantics — the harness passes the graph opaquely, provisions its environment, runs it under the shared single-slot GPU lock, and returns node-addressed outputs. Provide the graph as graph_path (a file) OR graph_json (inline API-format JSON); optionally manifest_path/manifest_json (the node manifest), out_dir (where output files land), reserve_vram (ComfyUI VRAM held back for the display). Returns {outputs:{node_id:[{path,type,kind}]}, image_path (first image, convenience alias), unverified_models[]}. On ANY failure it returns deferred:true with a typed reason (SATISFIER_UNAVAILABLE, VENV_INCOHERENT, SATISFIER_SPAWN_FAILED [a provisioning subprocess failed to START, retried once — transient/retryable, NOT a venv problem], NODE_CLASS_MISSING, PREFLIGHT_MISSING_INPUTS, MODEL_SHA_MISMATCH, GPU_BUSY, TIMEOUT, ...) — it NEVER falls back to cloud; then run the graph another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"graph_path":{"type":"string","description":"path to a ComfyUI API-format graph JSON file (provide this or graph_json)"},"graph_json":{"type":"string","description":"inline ComfyUI API-format graph JSON (alternative to graph_path)"},"manifest_path":{"type":"string","description":"path to a node manifest JSON (custom node packs @ pinned commits + model files to provision)"},"manifest_json":{"type":"string","description":"inline node manifest JSON (alternative to manifest_path)"},"out_dir":{"type":"string","description":"directory for the graph's output files (optional; default under the media dir)"},"reserve_vram":{"type":"string","description":"ComfyUI --reserve-vram override (VRAM held back for the display; per-workflow)"}}}`),
	}, s.handleRunGraph)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_svg",
		Description: "Render a crisp, brand-agnostic data-viz SVG locally for FREE (no model, no GPU) — the right tool for precise diagrams/icons SDXL fakes badly. kind is one of: gauge, comparison-bar, chromatogram, icon. spec is the component's JSON (colors/data are inputs; defaults are neutral — pass a theme {fg,bg,accent,muted,font} to brand it). Optional out = .svg path (default under the svg dir). Examples — gauge: {\"value\":72,\"max\":100,\"label\":\"Purity\",\"unit\":\"%\"}; comparison-bar: {\"items\":[{\"label\":\"A\",\"value\":10},{\"label\":\"B\",\"value\":20}],\"highlight\":1}; chromatogram: {\"peaks\":[{\"rt\":2.5,\"height\":80,\"label\":\"API\"}]}; icon: {\"name\":\"check\",\"color\":\"#22c55e\"}. Returns {svg_path, width, height}. Defers only on a bad kind/spec.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","description":"gauge | comparison-bar | chromatogram | icon"},"spec":{"type":"object","description":"the component spec (see description for fields; include an optional theme object to set colors)"},"out":{"type":"string","description":"output .svg path (optional; default under the svg dir)"}},"required":["kind","spec"]}`),
	}, s.handleGenerateSVG)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_video",
		Description: "Animate a still image into a short b-roll VIDEO clip on the LOCAL ComfyUI (Wan 2.2 14B I2V by default; HunyuanVideo 1.5 via model:hunyuan where its files are installed) for FREE — no cloud, runs on the local GPU. QUALITY-FIRST DEFAULT: the native two-stage recipe (no distill LoRA, 20 steps, cfg 3.5, the model's official negative) — a render takes tens of minutes and that is intended; set fast=true ONLY when the caller explicitly wants a draft (8-step lightx2v distill — visibly weaker motion). still (a local image path) + prompt describe the motion (prose, one camera move, ~80-120 words works best); optional: model (hunyuan|wan), frames (16fps; 81 ≈ 5s is the native ceiling), width/height (per-machine config may default 720p), steps, seed, negative (defaults to the model's official training negative), reserve_vram, out. It auto-starts ComfyUI and takes the shared single-slot GPU lock, so it serializes with other local gen/inference and may wait for the slot before deferring. Returns {video_path, seed}. On any failure it returns deferred:true — then make the clip another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"prose motion prompt (one camera move, ~80-120 words works best)"},"still":{"type":"string","description":"local path to the input still image (I2V)"},"model":{"type":"string","description":"wan (default; Wan 2.2 14B two-stage, native quality recipe) | hunyuan (opt-in; needs Hunyuan 1.5 files installed)"},"negative":{"type":"string","description":"hard exclusions (default: the model's official training-time negative)"},"out":{"type":"string","description":"output .mp4 path (optional; default under the media dir)"},"frames":{"type":"integer","description":"frame count at 16fps (81 ~5s is the native ceiling)"},"width":{"type":"integer","description":"width px"},"height":{"type":"integer","description":"height px"},"steps":{"type":"integer","description":"sampler steps"},"seed":{"type":"integer","description":"RNG seed for reproducibility"},"reserve_vram":{"type":"number","description":"VRAM held back for the display (per-workflow override; default ~1.0, raise for Wan)"},"fast":{"type":"boolean","description":"OPT-IN draft mode: 8-step lightx2v distill (visibly weaker motion). The default is the native quality recipe — only set when the caller explicitly accepts draft quality"},"hero":{"type":"boolean","description":"deprecated: the native quality pass IS the default now; no-op kept for compatibility"},"upscale":{"type":"boolean","description":"post-decode upscale using this machine's configured upscale model (e.g. 720p->1080p; no-op if the machine has none)"}},"required":["prompt"]}`),
	}, s.handleGenerateVideo)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_audio",
		Description: "Synthesize AUDIO on the LOCAL GPU for FREE — no cloud. kind=voice (default) is text-to-speech narration via Chatterbox Multilingual (commercial-safe, default Spanish; pass clone=<ref.wav> for zero-shot voice cloning, lang for the language). kind=music is a text-to-music bed via ACE-Step (style-tag prompt; seconds for length; optional lyrics). text is the narration text or the music style prompt. Optional: out (output path; default under the media dir), seed, reserve_vram (music only). It takes the shared single-slot GPU lock, so it serializes with other local gen/inference and may wait before deferring. Returns {audio_path, kind, seed}. On any failure (GPU busy, no route, worker error, timeout) it returns deferred:true — then synthesize it another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"narration text (voice) or music style prompt (music)"},"kind":{"type":"string","description":"voice (default, Chatterbox TTS) | music (ACE-Step)"},"voice":{"type":"string","description":"generalist | finetuned (default generalist; finetuned requires this machine's voicegen_ft_* config)"},"clone":{"type":"string","description":"voice: local path to a reference .wav for zero-shot voice cloning"},"lang":{"type":"string","description":"voice: language code (default es)"},"seconds":{"type":"integer","description":"music: clip length in seconds"},"out":{"type":"string","description":"output audio path (optional; default under the media dir)"},"seed":{"type":"integer","description":"RNG seed for reproducibility"},"reserve_vram":{"type":"number","description":"music: VRAM held back for the display (per-workflow override)"}},"required":["text"]}`),
	}, s.handleGenerateAudio)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_edit_image",
		Description: "Apply a DETERMINISTIC edit pipeline to a local image — free, CPU-only (no GPU lock: runs in parallel with renders, never evicts models). ops is an ARRAY applied in order in one call: crop{x,y,width,height}, resize{width and/or height, keep_aspect}, convert{format png|jpg|webp}, composite{overlay,x,y,opacity}, text{text,x,y,size,color,font,anchor}, mask_boxes{boxes,pad?,feather?,invert?} (REPLACES the working image with a white-on-black inpaint mask at its size — ready for offload_inpaint_image), grade{levels{black,white,gamma}?,curve{points[[in,out],...]}?,wb{mode:gray_world|scale,r,g,b}?,luminance_only?} (tone/color grade — everything composes into ONE LUT per channel, single quantize, no banding; alpha untouched), lut_cube{path,strength?} (.cube 3D LUT look at strength 0-1), perspective_composite{overlay,quad:[[x,y]x4]} (warp the overlay into the destination quad — UL,UR,LR,LL winding — and alpha-composite: mockup placement), finish{sharpen{radius,percent,threshold}?,median 3|5?} (delivery sharpening, defaults tuned for post-AI-upscale web output — MUST be the LAST op, after any resize: sharpening before a resize is undone by resampling), flatten_design{} (FIRST op only: opens a .xcf/.psd via GIMP, flattens it, returns its layer list), and instantiate_design{set_text{LayerName:new copy},replace_image{LayerName:image path}} (FIRST op only: GIMP layered-template factory — sets named text layers' copy, swaps named pixel layers for new images at the same offsets, flattens; the remaining ops then run on the result — a one-call brand-variant factory). Optional renditions[] exports a platform matrix from the master out ({width/height,format,suffix} each → <out-stem><suffix>.<format>). Engines are per-machine (see offload_status media): PIL for the pipeline, GIMP only for flatten_design/instantiate_design. Returns {image_path,width,height,ops_applied,layers?,renditions?}. On any failure (engine absent, bad op, tool error) it returns deferred:true — then edit the image another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local input image path (.png/.jpg/... ; .xcf/.psd when ops starts with flatten_design or instantiate_design)"},"ops":{"type":"array","items":{"type":"object"},"description":"edit operations applied in order; each is {op:..., ...args} (see tool description)"},"out":{"type":"string","description":"output path (optional; default under the media dir)"},"renditions":{"type":"array","items":{"type":"object"},"description":"optional export matrix from the master out: [{width and/or height, format png|jpg|webp, suffix}] — each writes <out-stem><suffix>.<format> and is listed in the result's renditions[]"}},"required":["image","ops"]}`),
	}, s.handleEditImage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_inpaint_image",
		Description: "Generatively INPAINT a local image on the LOCAL ComfyUI for FREE — re-renders ONLY the masked region from a prompt, leaving the rest untouched. Use to REMOVE unwanted content (gibberish text, objects, blemishes) or replace a region with new content. mask is a white-on-black image the same size as image (white = repaint). NOTE: diffusion cannot write specific legible text — inpaint-to-clean, then add real type with offload_edit_image's text op. Takes the shared single-slot GPU lock (serializes with other local gen). Returns {image_path, seed}. On any failure (no SDXL-class inpaint binding on this machine, missing files, render error) it returns deferred:true.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local path of the image to retouch"},"mask":{"type":"string","description":"local path of the white-on-black mask (white = repaint)"},"prompt":{"type":"string","description":"what the masked region should become"},"negative":{"type":"string","description":"hard exclusions for the repainted region"},"denoise":{"type":"number","description":"0-1; default 1.0 (full re-imagination inside the mask). Values well below 1.0 can produce muted/gray fill on the stock VAEEncodeForInpaint path — prefer 1.0 unless you know the tradeoff"},"grow_mask":{"type":"integer","description":"expand+feather the mask by N px in latent space (default 16; 0 = tight mask, no dilation — seam blending comes from this, not mask feathering)"},"steps":{"type":"integer","description":"sampler steps (default: machine binding)"},"seed":{"type":"integer","description":"RNG seed for reproducibility"},"out":{"type":"string","description":"output PNG path (optional; default under the media dir)"}},"required":["image","mask","prompt"]}`),
	}, s.handleInpaintImage)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_media",
		Description: "Run ONE ffmpeg av operation on local media — free, CPU-only (no GPU lock). op: trim{in,start,end|duration, reencode? (default false = fast keyframe-snapped stream copy)}, concat{inputs[] (same codec)}, extract_frames{in, fps OR count, out = directory}, convert{in (target by out extension; audio_only/video_only)}, mux_audio{in (video), audio, shortest}, probe{in} -> {duration_sec, streams[], format}. Inputs are LOCAL paths. Returns op-specific JSON. On any failure (ffmpeg absent, bad args) it returns deferred:true — then do it another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"op":{"type":"string","description":"trim | concat | extract_frames | convert | mux_audio | probe"},"in":{"type":"string","description":"input media path (all ops except concat)"},"inputs":{"type":"array","items":{"type":"string"},"description":"concat: >=2 input paths, same codec"},"out":{"type":"string","description":"output path (optional; extract_frames: a directory). probe has no output"},"start":{"type":"string","description":"trim: start (seconds or hh:mm:ss)"},"end":{"type":"string","description":"trim: absolute end time"},"duration":{"type":"string","description":"trim: duration in seconds (alternative to end)"},"reencode":{"type":"boolean","description":"trim: re-encode for exact cuts (default false = keyframe-snapped -c copy, fast)"},"fps":{"type":"number","description":"extract_frames: sampling rate"},"count":{"type":"integer","description":"extract_frames: total frames (resolved to fps via probe)"},"audio":{"type":"string","description":"mux_audio: audio input path"},"shortest":{"type":"boolean","description":"mux_audio: stop at the shorter input (default true)"},"audio_only":{"type":"boolean","description":"convert: drop video (-vn)"},"video_only":{"type":"boolean","description":"convert: drop audio (-an)"}},"required":["op"]}`),
	}, s.handleMedia)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_nim",
		Description: "Send a prompt to a remote OpenAI-compatible NVIDIA NIM endpoint — NVIDIA's hosted build.nvidia.com catalog (dozens of FREE models: nemotron, llama, gpt-oss, qwen, deepseek, glm, kimi…) by default, or a self-hosted NIM via base. This is the ONLY cloud/remote tool on this server — every other offload_* tool runs on the LOCAL models (see offload_status for that roster). It is OPT-IN (the hosted endpoint needs NVIDIA_API_KEY in the server env; a self-hosted NIM via base is keyless): use it deliberately for a stronger model than the local cascade, NOT for routine grunt work. The local GBNF grammar path and the savings ledger are untouched (NIM calls are never ledgered). Set list_models=true to browse available model ids. Returns {model, content, reasoning_content, tokens_in, tokens_out, truncated}; on any failure (no key, endpoint down, bad model) it returns deferred:true with a reason and you handle the prompt yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"the user prompt"},"model":{"type":"string","description":"model id (default from config; set list_models=true to browse)"},"system":{"type":"string","description":"optional system prompt"},"base":{"type":"string","description":"override the OpenAI-compatible base URL incl. /v1 (e.g. a self-hosted NIM http://host:8000/v1)"},"max_tokens":{"type":"integer","description":"max completion tokens (default from config; reasoning models need headroom)"},"temperature":{"type":"number","description":"sampling temperature (default 0)"},"list_models":{"type":"boolean","description":"list available model ids instead of generating"}},"required":["prompt"]}`),
	}, s.handleNIM)

	// agent_run (P5 drive mode "a", MCP front door): Claude commands the LOCAL
	// autonomous agent loop. The agent plans + iterates IN-PROCESS over read-only
	// tools + the offload cascade; MCP is the front door, never how the agent
	// reaches its tools (those are in-process). The agent's offload_* calls run on a
	// fresh nil-cache/nil-ledger pipeline via RunTier(record=false) so the savings
	// ledger, cache, and shadow store are untouched. Read-only here (no write/exec/
	// net); a failure is a clean defer, not a server error.
	srv.AddTool(&mcp.Tool{
		Name:        "agent_run",
		Description: "Run the LOCAL autonomous agent loop on a goal: a free local model plans and iterates over read-only tools (list_dir, read_file) plus the offload_* cascade, multi-step, and returns a final answer. DELEGATE a bounded multi-step read-and-reason job — map how X flows through a repo, summarize a doc set, extract facts across many files — to the local stack to keep that work out of your own context. It is READ-ONLY: it cannot write files, run commands, or touch the network. The savings ledger is untouched (the agent's offload calls run record=false). Returns {output, steps, stop_reason, tools}; on any failure it returns deferred:true with a reason and you do the task yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","description":"the task for the local agent to accomplish"},"read_root":{"type":"string","description":"absolute directory the agent may read; it cannot read outside it (default: the server working dir)"},"max_steps":{"type":"integer","description":"hard step budget (default 12)"},"model":{"type":"string","description":"planner model id; must support tool-calling (default: the configured workhorse model)"},"timeout_sec":{"type":"integer","description":"wall-clock budget in seconds (default 180)"}},"required":["goal"]}`),
	}, s.handleAgentRun)

	return srv.Run(ctx, &mcp.StdioTransport{})
}

// --- tool handlers (named methods so they are directly unit-testable) ---

// handleStatus (LO-18): the capability-discovery tool. Reports the configured
// LOCAL model roster, live-probes the local endpoint's /v1/models, lists this
// machine's media engines, and names the single remote surface (NIM). A failed
// live probe is reported alongside the roster — never a defer: the configured
// roster is the answer even when the stack is cold.
func (s *Server) handleStatus(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct{}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	cfg := s.p.Cfg()

	roster := map[string]any{
		"workhorse":  cfg.Model,
		"triage":     cfg.TriageModel,
		"escalation": cfg.EscalationModel,
		"reasoning":  cfg.ReasoningModel,
		"vision":     cfg.VisionModel,
		"stt":        cfg.STTModel,
		"stt_hq":     cfg.STTModelHQ,
		"embed":      cfg.EmbedModel(),
	}
	local := map[string]any{
		"endpoint": cfg.Endpoint,
		"roster":   roster,
		"note":     "every offload_* tool except offload_nim runs on these LOCAL models — free, on-box, no cloud; an empty entry means that capability defers on this machine",
	}
	if ids, err := probeServedModels(ctx, cfg.Endpoint); err != nil {
		local["served_probe_error"] = err.Error()
	} else {
		local["served_now"] = ids
	}

	// edit/media engine presence (existence-checked, per this machine's config).
	editPy := mediaops.ResolveEditPython(cfg.EditPython, cfg.ComfyDir)
	gimpPresent := false
	if cfg.GimpConsolePath != "" {
		if _, err := os.Stat(cfg.GimpConsolePath); err == nil {
			gimpPresent = true
		}
	}
	ffmpegPresent := false
	if cfg.FFmpegPath != "" {
		if _, err := os.Stat(cfg.FFmpegPath); err == nil {
			ffmpegPresent = true
		} else if _, err := exec.LookPath(cfg.FFmpegPath); err == nil {
			ffmpegPresent = true // bare "ffmpeg" resolved via PATH
		}
	}
	media := map[string]any{
		"image_engine":        "ComfyUI (local)",
		"image_ckpt":          cfg.ImageGenCkpt, // "" = the render script's default checkpoint
		"video_engine":        "ComfyUI Wan 2.2 I2V (local; model:hunyuan opt-in)",
		"video_upscale_model": cfg.VideoGenUpscaleModel,
		"audio_voice_engine":  "Chatterbox TTS (local)",
		"audio_music_engine":  "ACE-Step (local)",
		"svg_engine":          "deterministic component kit (local, no model)",
		"edit_pil":            editPy != "", // offload_edit_image pipeline engine
		"edit_gimp":           gimpPresent,  // flatten_design (.xcf/.psd) engine
		"media_ffmpeg":        ffmpegPresent, // offload_media engine
		"note":                "media tools defer cleanly when this machine lacks the engine/files",
	}

	remote := map[string]any{
		"nim_endpoint":      cfg.NIMEndpoint,
		"nim_default_model": cfg.NIMModel,
		"nim_key_present":   nimclient.KeyForBase(cfg.NIMEndpoint) != "",
		"note":              "offload_nim is the ONLY remote/cloud tool on this server (opt-in escalation)",
	}

	return jsonResult(map[string]any{"local": local, "media": media, "remote": remote})
}

// probeServedModels GETs <endpoint>/v1/models (OpenAI list shape — llama-swap
// serves it) and returns the live model ids. Short timeout: status must stay
// snappy even when the endpoint is a black hole.
func probeServedModels(ctx context.Context, endpoint string) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(body.Data))
	for _, d := range body.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}

func (s *Server) handleSummarize(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Text      string `json:"text"`
		MaxPoints int    `json:"max_points"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	if in.MaxPoints > 0 {
		params["max_points"] = in.MaxPoints
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskSummarize, Input: in.Text, Params: params}))
}

func (s *Server) handleClassify(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Text   string   `json:"text"`
		Labels []string `json:"labels"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskClassify, Input: in.Text, Params: map[string]any{"labels": in.Labels}}))
}

func (s *Server) handleExtract(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Text   string         `json:"text"`
		Schema map[string]any `json:"schema"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskExtract, Input: in.Text, Params: map[string]any{"schema": in.Schema}}))
}

func (s *Server) handleTriage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Text     string `json:"text"`
		Question string `json:"question"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskTriage, Input: in.Text, Params: map[string]any{"question": in.Question}}))
}

func (s *Server) handleVQA(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image    string `json:"image"`
		Question string `json:"question"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskVQA, Image: in.Image, Params: map[string]any{"question": in.Question}}))
}

func (s *Server) handleVideoDescribe(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Video    string `json:"video"`
		Question string `json:"question"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskVideoDescribe, Video: in.Video, Params: map[string]any{"question": in.Question}}))
}

func (s *Server) handleTranscribe(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Audio    string   `json:"audio"`
		Language string   `json:"language"`
		HQ       bool     `json:"hq"`
		Select   []string `json:"select"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	if in.Language != "" {
		params["language"] = in.Language
	}
	if in.HQ {
		params["hq"] = true
	}
	res := s.p.Run(ctx, core.Request{Task: core.TaskTranscribe, Audio: in.Audio, Params: params})
	if len(in.Select) > 0 {
		res.Data = core.ProjectFields(res.Data, in.Select)
	}
	return result(res)
}

func (s *Server) handleExtractImage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image  string         `json:"image"`
		Schema map[string]any `json:"schema"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskExtractImage, Image: in.Image, Params: map[string]any{"schema": in.Schema}}))
}

func (s *Server) handleAssessImage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image string `json:"image"`
		Brief string `json:"brief"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	if in.Brief != "" {
		params["brief"] = in.Brief
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskAssessImage, Image: in.Image, Params: params}))
}

func (s *Server) handleOCR(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image string `json:"image"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskOCR, Image: in.Image}))
}

func (s *Server) handleGenerateImage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Prompt   string `json:"prompt"`
		Negative string `json:"negative"`
		Out      string `json:"out"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		Steps    int    `json:"steps"`
		Seed     int    `json:"seed"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	if in.Negative != "" {
		params["negative"] = in.Negative
	}
	if in.Out != "" {
		params["out"] = in.Out
	}
	if in.Width > 0 {
		params["width"] = in.Width
	}
	if in.Height > 0 {
		params["height"] = in.Height
	}
	if in.Steps > 0 {
		params["steps"] = in.Steps
	}
	if in.Seed > 0 {
		params["seed"] = in.Seed
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskGenerateImage, Input: in.Prompt, Params: params}))
}

func (s *Server) handleInpaintImage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image    string  `json:"image"`
		Mask     string  `json:"mask"`
		Prompt   string  `json:"prompt"`
		Negative string  `json:"negative"`
		Denoise  float64 `json:"denoise"`
		GrowMask *int    `json:"grow_mask"` // pointer: an explicit 0 (no dilation) is distinct from absent
		Steps    int     `json:"steps"`
		Seed     int     `json:"seed"`
		Out      string  `json:"out"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	if in.Image != "" {
		params["image"] = in.Image
	}
	if in.Mask != "" {
		params["mask"] = in.Mask
	}
	if in.Negative != "" {
		params["negative"] = in.Negative
	}
	if in.Denoise > 0 {
		params["denoise"] = in.Denoise
	}
	if in.GrowMask != nil && *in.GrowMask >= 0 {
		params["grow_mask"] = *in.GrowMask
	}
	if in.Steps > 0 {
		params["steps"] = in.Steps
	}
	if in.Seed > 0 {
		params["seed"] = in.Seed
	}
	if in.Out != "" {
		params["out"] = in.Out
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskInpaintImage, Input: in.Prompt, Params: params}))
}

func (s *Server) handleRunGraph(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		GraphPath    string `json:"graph_path"`
		GraphJSON    string `json:"graph_json"`
		ManifestPath string `json:"manifest_path"`
		ManifestJSON string `json:"manifest_json"`
		OutDir       string `json:"out_dir"`
		ReserveVram  string `json:"reserve_vram"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	// Inline graph_json/manifest_json are written to a temp file whose path is threaded
	// on (the mjs reads files, not inline). Empty+empty manifest → "" (manifest optional).
	graphPath, err := materialize(in.GraphPath, in.GraphJSON, "run-graph-*.json")
	if err != nil {
		res, _ := jsonResult(map[string]any{"deferred": true, "reason": "bad arguments: graph: " + err.Error()})
		return res, nil
	}
	if graphPath == "" {
		res, _ := jsonResult(map[string]any{"deferred": true, "reason": "bad arguments: graph_path or graph_json required"})
		return res, nil
	}
	manifestPath, err := materialize(in.ManifestPath, in.ManifestJSON, "run-graph-manifest-*.json")
	if err != nil {
		res, _ := jsonResult(map[string]any{"deferred": true, "reason": "bad arguments: manifest: " + err.Error()})
		return res, nil
	}
	params := map[string]any{
		"graph_path":    graphPath,
		"manifest_path": manifestPath,
		"out_dir":       in.OutDir,
		"reserve_vram":  in.ReserveVram,
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskRunGraph, Params: params}))
}

// materialize returns path if set, else writes inline json to a temp file and returns
// that path (the mjs reads files, not inline). Empty+empty → ("", nil) for the optional
// manifest; a required graph is validated by the caller when this returns "".
func materialize(path, inline, pattern string) (string, error) {
	if path != "" {
		return path, nil
	}
	if inline == "" {
		return "", nil
	}
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(inline); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func (s *Server) handleGenerateSVG(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Kind string         `json:"kind"`
		Spec map[string]any `json:"spec"`
		Out  string         `json:"out"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{"kind": in.Kind, "spec": in.Spec}
	if in.Out != "" {
		params["out"] = in.Out
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskGenerateSVG, Params: params}))
}

func (s *Server) handleGenerateVideo(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Prompt      string  `json:"prompt"`
		Still       string  `json:"still"`
		Model       string  `json:"model"`
		Negative    string  `json:"negative"`
		Out         string  `json:"out"`
		Frames      int     `json:"frames"`
		Width       int     `json:"width"`
		Height      int     `json:"height"`
		Steps       int     `json:"steps"`
		Seed        int     `json:"seed"`
		ReserveVRAM float64 `json:"reserve_vram"`
		Fast        bool    `json:"fast"`
		Hero        bool    `json:"hero"`
		Upscale     bool    `json:"upscale"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	// LO-19: hero/upscale were ADVERTISED in the schema but never mapped — MCP callers
	// asking for the quality pass silently got the draft path. fast/hero/upscale now flow.
	if in.Fast {
		params["fast"] = true
	}
	if in.Hero {
		params["hero"] = true
	}
	if in.Upscale {
		params["upscale"] = true
	}
	if in.Still != "" {
		params["still"] = in.Still
	}
	if in.Model != "" {
		params["model"] = in.Model
	}
	if in.Negative != "" {
		params["negative"] = in.Negative
	}
	if in.Out != "" {
		params["out"] = in.Out
	}
	if in.Frames > 0 {
		params["frames"] = in.Frames
	}
	if in.Width > 0 {
		params["width"] = in.Width
	}
	if in.Height > 0 {
		params["height"] = in.Height
	}
	if in.Steps > 0 {
		params["steps"] = in.Steps
	}
	if in.Seed > 0 {
		params["seed"] = in.Seed
	}
	if in.ReserveVRAM > 0 {
		params["reserve_vram"] = strconv.FormatFloat(in.ReserveVRAM, 'f', -1, 64)
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskGenerateVideo, Input: in.Prompt, Image: in.Still, Params: params}))
}

func (s *Server) handleGenerateAudio(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Text        string  `json:"text"`
		Kind        string  `json:"kind"`
		Voice       string  `json:"voice"`
		Clone       string  `json:"clone"`
		Lang        string  `json:"lang"`
		Seconds     int     `json:"seconds"`
		Out         string  `json:"out"`
		Seed        int     `json:"seed"`
		ReserveVRAM float64 `json:"reserve_vram"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	if in.Kind != "" {
		params["kind"] = in.Kind
	}
	if in.Voice != "" {
		params["voice"] = in.Voice
	}
	if in.Clone != "" {
		params["clone"] = in.Clone
	}
	if in.Lang != "" {
		params["lang"] = in.Lang
	}
	if in.Seconds > 0 {
		params["seconds"] = in.Seconds
	}
	if in.Out != "" {
		params["out"] = in.Out
	}
	if in.Seed > 0 {
		params["seed"] = in.Seed
	}
	if in.ReserveVRAM > 0 {
		params["reserve_vram"] = strconv.FormatFloat(in.ReserveVRAM, 'f', -1, 64)
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskGenerateAudio, Input: in.Text, Params: params}))
}

func (s *Server) handleEditImage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image      string           `json:"image"`
		Ops        []map[string]any `json:"ops"`
		Out        string           `json:"out"`
		Renditions []map[string]any `json:"renditions"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{"ops": in.Ops}
	if in.Out != "" {
		params["out"] = in.Out
	}
	if len(in.Renditions) > 0 {
		params["renditions"] = in.Renditions
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskEditImage, Image: in.Image, Params: params}))
}

func (s *Server) handleMedia(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Op        string   `json:"op"`
		In        string   `json:"in"`
		Inputs    []string `json:"inputs"`
		Out       string   `json:"out"`
		Start     string   `json:"start"`
		End       string   `json:"end"`
		Duration  string   `json:"duration"`
		Reencode  bool     `json:"reencode"`
		FPS       float64  `json:"fps"`
		Count     int      `json:"count"`
		Audio     string   `json:"audio"`
		Shortest  *bool    `json:"shortest"`
		AudioOnly bool     `json:"audio_only"`
		VideoOnly bool     `json:"video_only"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{"op": in.Op}
	for k, v := range map[string]string{"in": in.In, "out": in.Out, "start": in.Start, "end": in.End, "duration": in.Duration, "audio": in.Audio} {
		if v != "" {
			params[k] = v
		}
	}
	if len(in.Inputs) > 0 {
		params["inputs"] = in.Inputs
	}
	if in.Reencode {
		params["reencode"] = true
	}
	if in.AudioOnly {
		params["audio_only"] = true
	}
	if in.VideoOnly {
		params["video_only"] = true
	}
	if in.FPS > 0 {
		params["fps"] = in.FPS
	}
	if in.Count > 0 {
		params["count"] = in.Count
	}
	if in.Shortest != nil {
		params["shortest"] = *in.Shortest
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskMedia, Params: params}))
}

func (s *Server) handleNIM(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Prompt      string  `json:"prompt"`
		Model       string  `json:"model"`
		System      string  `json:"system"`
		Base        string  `json:"base"`
		MaxTokens   int     `json:"max_tokens"`
		Temperature float64 `json:"temperature"`
		ListModels  bool    `json:"list_models"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	// defer-not-crash: defend against an empty prompt even though the schema marks it required.
	if !in.ListModels && in.Prompt == "" {
		return jsonResult(map[string]any{"deferred": true, "reason": "empty prompt"})
	}
	cfg := s.p.Cfg()
	base := in.Base
	if base == "" {
		base = cfg.NIMEndpoint
	}
	key := nimclient.KeyForBase(base) // env key only for NVIDIA hosts; never transmitted to a non-NVIDIA base
	// defer-not-crash: a missing key on the hosted endpoint is a clean defer, not an error.
	if key == "" && nimclient.IsHostedNVIDIA(base) {
		return jsonResult(map[string]any{"deferred": true, "reason": "NVIDIA_API_KEY (or NGC_API_KEY) not set in the MCP server env — required for the hosted NIM endpoint; a self-hosted NIM via base is keyless"})
	}
	timeout := time.Duration(cfg.NIMTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	client := nimclient.New(base, key, timeout)
	if in.ListModels {
		ids, err := client.ListModels(ctx)
		if err != nil {
			return jsonResult(map[string]any{"deferred": true, "reason": err.Error()})
		}
		return jsonResult(map[string]any{"models": ids, "count": len(ids), "endpoint": base})
	}
	model := in.Model
	if model == "" {
		model = cfg.NIMModel
	}
	maxTok := in.MaxTokens
	if maxTok == 0 {
		maxTok = cfg.NIMMaxTokens
	}
	res, err := client.Chat(ctx, model, in.System, in.Prompt, maxTok, in.Temperature)
	if err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": err.Error()})
	}
	return jsonResult(map[string]any{
		"model":             res.Model,
		"content":           res.Content,
		"reasoning_content": res.ReasoningContent,
		"tokens_in":         res.TokensIn,
		"tokens_out":        res.TokensOut,
		"truncated":         res.Truncated,
	})
}

func (s *Server) handleAgentRun(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Goal       string `json:"goal"`
		ReadRoot   string `json:"read_root"`
		MaxSteps   int    `json:"max_steps"`
		Model      string `json:"model"`
		TimeoutSec int    `json:"timeout_sec"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	// defer-not-crash: an empty goal is a clean defer, not an error.
	if strings.TrimSpace(in.Goal) == "" {
		return jsonResult(map[string]any{"deferred": true, "reason": "empty goal"})
	}
	cfg := s.p.Cfg()
	readRoot := in.ReadRoot
	if readRoot == "" {
		wd, werr := os.Getwd()
		if werr != nil {
			return jsonResult(map[string]any{"deferred": true, "reason": "cannot determine working dir for read_root: " + werr.Error()})
		}
		readRoot = wd
	}
	absRoot, err := filepath.Abs(readRoot)
	if err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": "bad read_root: " + err.Error()})
	}
	model := in.Model
	if model == "" {
		model = cfg.Model // this machine's configured workhorse; never a hardcoded alias
	}
	maxSteps := in.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 12
	}
	if maxSteps > 64 {
		maxSteps = 64 // a self-standing ceiling so the step budget doesn't rely solely on the timeout
	}
	timeout := time.Duration(in.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	// In-process offload (record=false, nil cache+ledger) + the SHARED loop
	// builder — identical construction to the CLI and the standalone runner, so
	// the three drive modes stay at parity. Read-only front door: no
	// write/fetch/shell, no audit (the offload cannot write the ledger anyway).
	offload := pipeline.NewRecordlessOffload(cfg, model, timeout)
	built, err := agent.Build(agent.BuildConfig{
		PlannerBase: cfg.Endpoint,
		Model:       model,
		Timeout:     timeout,
		MaxSteps:    maxSteps,
		ReadRoot:    absRoot,
		Offload:     offload,
	})
	if err != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": "building agent: " + err.Error()})
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Budget compaction against the SERVED window (probe; conservative fallback
	// inside ResolveContextTokens when unanswerable) and run the measured-ON
	// ladder rungs — the same defaults as the CLI (flip decision 2026-07-24).
	// The probe runs per request: warm it is one cheap GET; against a stalled
	// endpoint it is bounded by its own HTTP timeout inside the run's cctx
	// budget, and the run was about to talk to that same endpoint anyway. The
	// resolved window is reported in the result so a fallback is visible.
	probed, probeOK := agent.ProbeServedWindow(cctx, cfg.Endpoint, model)
	effCtx, _ := agent.ResolveContextTokens(0, probed, probeOK)
	built.Loop.WithContextTokens(effCtx).WithSkeletonPrune(true).WithGCFCompact(true)
	res, rerr := built.Loop.Run(cctx, in.Goal)
	if rerr != nil {
		return jsonResult(map[string]any{"deferred": true, "reason": rerr.Error(), "steps": res.Steps})
	}
	return jsonResult(map[string]any{
		"output":      res.Output,
		"steps":       res.Steps,
		"stop_reason": res.StopReason,
		"tools":       len(built.Tools),
		"ctx_window":  effCtx, // the window compaction budgeted against (probed, or the conservative fallback)
	})
}

// jsonResult marshals an arbitrary payload (NIM is not a core.Result) into a
// single MCP text-content result.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

func result(r core.Result) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}
