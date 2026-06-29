// Package mcpserver exposes the offload pipeline as MCP tools over stdio so
// Claude Code can delegate grunt work. Tools return the full Result JSON as
// text — a defer is a valid result (Claude then does the task itself), not an
// error.
package mcpserver

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/nimclient"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

type Server struct{ p *pipeline.Pipeline }

func New(p *pipeline.Pipeline) *Server { return &Server{p: p} }

// Run serves the MCP tools on stdin/stdout until the client disconnects.
func (s *Server) Run(ctx context.Context, version string) error {
	srv := mcp.NewServer(&mcp.Implementation{Name: "local-offload", Version: version}, nil)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_summarize",
		Description: "Summarize text on a free local model. Use for bulk/low-judgment summaries to keep tokens out of your context. Returns {summary, bullets}; if it can't do it confidently it returns deferred:true and you should summarize it yourself. Triggers: summarize / tl;dr / gist / digest / recap / condense a doc, log, transcript, article, or thread.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to summarize"},"max_points":{"type":"integer","description":"max bullet points (default 5)"}},"required":["text"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Text      string `json:"text"`
			MaxPoints int    `json:"max_points"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		params := map[string]any{}
		if in.MaxPoints > 0 {
			params["max_points"] = in.MaxPoints
		}
		return result(s.p.Run(ctx, core.Request{Task: core.TaskSummarize, Input: in.Text, Params: params}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_classify",
		Description: "Classify text into one of the given labels on a free local model. Returns {label, confidence}; low-confidence results are deferred back to you. Triggers: classify / categorize / label / tag / bucket / route text into one of a known set.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to classify"},"labels":{"type":"array","items":{"type":"string"},"description":"allowed labels (>=2)"}},"required":["text","labels"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Text   string   `json:"text"`
			Labels []string `json:"labels"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskClassify, Input: in.Text, Params: map[string]any{"labels": in.Labels}}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_extract",
		Description: "Extract structured fields from text on a free local model, constrained to the provided JSON schema. Returns the extracted object or defers. Triggers: extract / parse / pull out structured fields from text into a schema (names, dates, amounts, entities).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to extract fields from"},"schema":{"type":"object","description":"JSON schema with a properties object describing the fields to extract"}},"required":["text","schema"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Text   string         `json:"text"`
			Schema map[string]any `json:"schema"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskExtract, Input: in.Text, Params: map[string]any{"schema": in.Schema}}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_triage",
		Description: "Answer a yes/no/unsure question about text on a free local model. Returns {decision, reason} or defers. Triggers: a yes/no/unsure check on text — 'does this contain X?', 'is this relevant/spam/safe?', 'should this be flagged?'.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to evaluate"},"question":{"type":"string","description":"a yes/no question about the text"}},"required":["text","question"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Text     string `json:"text"`
			Question string `json:"question"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskTriage, Input: in.Text, Params: map[string]any{"question": in.Question}}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_vqa",
		Description: "Answer a question about an IMAGE on a free local vision model (VQA). image is a local file path or a data:image/... URI; question is what to ask about it. Returns {answer}; if it can't answer confidently it returns deferred:true and you should look at the image yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"question":{"type":"string","description":"the question to answer about the image"}},"required":["image","question"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Image    string `json:"image"`
			Question string `json:"question"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskVQA, Image: in.Image, Params: map[string]any{"question": in.Question}}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_video_describe",
		Description: "Answer a question about a VIDEO on a free local vision model. It samples frames from the video and reasons over them. video is a LOCAL file path; question is what to ask. Returns {answer} (which notes what the relevant frames show); if it can't answer confidently it returns deferred:true and you should watch the video yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"video":{"type":"string","description":"local video file path"},"question":{"type":"string","description":"the question to answer about the video"}},"required":["video","question"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Video    string `json:"video"`
			Question string `json:"question"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskVideoDescribe, Video: in.Video, Params: map[string]any{"question": in.Question}}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_transcribe",
		Description: "Transcribe a local AUDIO or VIDEO file to text on a free local whisper model (STT). audio is a LOCAL file path (mp3/m4a/wav/mp4/...); language is optional ('en','es', or 'auto' — default auto-detect); set hq=true for the higher-quality (slower) model on hard/noisy clips. Returns {gist (preview), language, duration_sec, num_segments, segments[{id,start,end,text}] (timestamped spans — pull only the ones you need), srt_path, text_path, json_path}. The full transcript + SRT are written to disk; read the spans/paths you need. If it can't transcribe confidently it returns deferred:true and you should handle the audio yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"audio":{"type":"string","description":"local audio or video file path"},"language":{"type":"string","description":"en, es, or auto (default auto-detect)"},"hq":{"type":"boolean","description":"use the higher-quality large-v3 model (slower) for hard/noisy audio"},"select":{"type":"array","items":{"type":"string"},"description":"optional: return ONLY these top-level result fields (e.g. [\"gist\",\"language\",\"num_segments\",\"srt_path\"]) to skip the verbose segments[] and keep your context lean — read the full transcript/spans from srt_path or json_path when you need them"}},"required":["audio"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Audio    string   `json:"audio"`
			Language string   `json:"language"`
			HQ       bool     `json:"hq"`
			Select   []string `json:"select"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
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
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_extract_image",
		Description: "Extract structured fields from an IMAGE on a free local model: it OCRs the image, then extracts the fields from the transcribed text constrained to the provided JSON schema (values are grounded against the OCR text). image is a local file path or a data:image/... URI; schema is a JSON schema with a properties object. Returns the extracted object or defers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"schema":{"type":"object","description":"JSON schema with a properties object describing the fields to extract"}},"required":["image","schema"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Image  string         `json:"image"`
			Schema map[string]any `json:"schema"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskExtractImage, Image: in.Image, Params: map[string]any{"schema": in.Schema}}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_assess_image",
		Description: "QA a generated IMAGE against hard exclusions on a free local vision model. Emits a grammar-constrained {has_people, has_text, matches_brief, notes}: has_people=true if any person/face/body part is visible, has_text=true if any readable letters/words/numbers are rendered, matches_brief=whether it matches the optional brief (true if no brief), notes=one short phrase. image is a local file path or a data:image/... URI; brief is optional. Returns the object or deferred:true.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"brief":{"type":"string","description":"optional description the image should match"}},"required":["image"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Image string `json:"image"`
			Brief string `json:"brief"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		params := map[string]any{}
		if in.Brief != "" {
			params["brief"] = in.Brief
		}
		return result(s.p.Run(ctx, core.Request{Task: core.TaskAssessImage, Image: in.Image, Params: params}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_ocr",
		Description: "Transcribe ALL text in an IMAGE on a free local vision model (OCR). image is a local file path or a data:image/... URI. Returns {text} with the transcribed text in reading order; if it can't transcribe confidently it returns deferred:true and you should read the image yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"}},"required":["image"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Image string `json:"image"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskOCR, Image: in.Image}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_image",
		Description: "Generate an IMAGE from a text prompt on the LOCAL ComfyUI (SDXL/RealVisXL) for FREE — no cloud, runs on the local GPU. prompt is required; optional: negative (hard exclusions like 'people, text, watermark' — SDXL enforces these at CFG 7), width/height (default 1024), steps (default 30), seed (for reproducibility), out (output PNG path; default under the media dir). It auto-starts ComfyUI and takes the shared single-slot GPU lock, so it serializes with other local gen/inference and may wait. First call cold-starts ComfyUI (~4min) + renders (~6min); warm calls are faster. Returns {image_path, width, height, seed}. On any failure (GPU busy, ComfyUI down, render error, timeout) it returns deferred:true — then generate the image another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"positive text prompt describing the image"},"negative":{"type":"string","description":"hard exclusions, e.g. people, text, watermark"},"out":{"type":"string","description":"output PNG path (optional; default under the media dir)"},"width":{"type":"integer","description":"width px (default 1024)"},"height":{"type":"integer","description":"height px (default 1024)"},"steps":{"type":"integer","description":"sampler steps (default 30)"},"seed":{"type":"integer","description":"RNG seed for reproducibility"}},"required":["prompt"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Prompt   string `json:"prompt"`
			Negative string `json:"negative"`
			Out      string `json:"out"`
			Width    int    `json:"width"`
			Height   int    `json:"height"`
			Steps    int    `json:"steps"`
			Seed     int    `json:"seed"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
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
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_svg",
		Description: "Render a crisp, brand-agnostic data-viz SVG locally for FREE (no model, no GPU) — the right tool for precise diagrams/icons SDXL fakes badly. kind is one of: gauge, comparison-bar, chromatogram, icon. spec is the component's JSON (colors/data are inputs; defaults are neutral — pass a theme {fg,bg,accent,muted,font} to brand it). Optional out = .svg path (default under the svg dir). Examples — gauge: {\"value\":72,\"max\":100,\"label\":\"Purity\",\"unit\":\"%\"}; comparison-bar: {\"items\":[{\"label\":\"A\",\"value\":10},{\"label\":\"B\",\"value\":20}],\"highlight\":1}; chromatogram: {\"peaks\":[{\"rt\":2.5,\"height\":80,\"label\":\"API\"}]}; icon: {\"name\":\"check\",\"color\":\"#22c55e\"}. Returns {svg_path, width, height}. Defers only on a bad kind/spec.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","description":"gauge | comparison-bar | chromatogram | icon"},"spec":{"type":"object","description":"the component spec (see description for fields; include an optional theme object to set colors)"},"out":{"type":"string","description":"output .svg path (optional; default under the svg dir)"}},"required":["kind","spec"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Kind string         `json:"kind"`
			Spec map[string]any `json:"spec"`
			Out  string         `json:"out"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		params := map[string]any{"kind": in.Kind, "spec": in.Spec}
		if in.Out != "" {
			params["out"] = in.Out
		}
		return result(s.p.Run(ctx, core.Request{Task: core.TaskGenerateSVG, Params: params}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_video",
		Description: "Animate a still image into a short b-roll VIDEO clip on the LOCAL ComfyUI (HunyuanVideo 1.5 480p I2V by default; Wan 2.2 14B via model:wan for slow hero shots) for FREE — no cloud, runs on the local GPU. still (a local image path) + prompt describe the motion; optional: model (hunyuan|wan), frames (default ~33; realistic ceiling ~49), width/height, steps, seed, negative, reserve_vram (per-workflow VRAM hold-back override), out (output .mp4 path; default under the media dir). It auto-starts ComfyUI and takes the shared single-slot GPU lock, so it serializes with other local gen/inference and may wait up to ~20min for the slot before deferring. A render itself takes minutes. Returns {video_path, seed}. On any failure (GPU busy, ComfyUI down, render error, timeout) it returns deferred:true — then make the clip another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"text prompt describing the motion/scene"},"still":{"type":"string","description":"local path to the input still image (I2V)"},"model":{"type":"string","description":"hunyuan (default, fast) | wan (slow photoreal hero)"},"negative":{"type":"string","description":"hard exclusions"},"out":{"type":"string","description":"output .mp4 path (optional; default under the media dir)"},"frames":{"type":"integer","description":"frame count (default ~33; realistic ceiling ~49)"},"width":{"type":"integer","description":"width px"},"height":{"type":"integer","description":"height px"},"steps":{"type":"integer","description":"sampler steps"},"seed":{"type":"integer","description":"RNG seed for reproducibility"},"reserve_vram":{"type":"number","description":"VRAM held back for the display (per-workflow override; default ~1.0, raise for Wan)"}},"required":["prompt"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		params := map[string]any{}
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
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_generate_audio",
		Description: "Synthesize AUDIO on the LOCAL GPU for FREE — no cloud. kind=voice (default) is text-to-speech narration via Chatterbox Multilingual (commercial-safe, default Spanish; pass clone=<ref.wav> for zero-shot voice cloning, lang for the language). kind=music is a text-to-music bed via ACE-Step (style-tag prompt; seconds for length; optional lyrics). text is the narration text or the music style prompt. Optional: out (output path; default under the media dir), seed, reserve_vram (music only). It takes the shared single-slot GPU lock, so it serializes with other local gen/inference and may wait before deferring. Returns {audio_path, kind, seed}. On any failure (GPU busy, no route, worker error, timeout) it returns deferred:true — then synthesize it another way.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"narration text (voice) or music style prompt (music)"},"kind":{"type":"string","description":"voice (default, Chatterbox TTS) | music (ACE-Step)"},"clone":{"type":"string","description":"voice: local path to a reference .wav for zero-shot voice cloning"},"lang":{"type":"string","description":"voice: language code (default es)"},"seconds":{"type":"integer","description":"music: clip length in seconds"},"out":{"type":"string","description":"output audio path (optional; default under the media dir)"},"seed":{"type":"integer","description":"RNG seed for reproducibility"},"reserve_vram":{"type":"number","description":"music: VRAM held back for the display (per-workflow override)"}},"required":["text"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Text        string  `json:"text"`
			Kind        string  `json:"kind"`
			Clone       string  `json:"clone"`
			Lang        string  `json:"lang"`
			Seconds     int     `json:"seconds"`
			Out         string  `json:"out"`
			Seed        int     `json:"seed"`
			ReserveVRAM float64 `json:"reserve_vram"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		params := map[string]any{}
		if in.Kind != "" {
			params["kind"] = in.Kind
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
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_nim",
		Description: "Send a prompt to a remote OpenAI-compatible NVIDIA NIM endpoint — NVIDIA's hosted build.nvidia.com catalog (dozens of FREE models: nemotron, llama, gpt-oss, qwen, deepseek, glm, kimi…) by default, or a self-hosted NIM via base. This is the EXPLICIT remote escalation/test tool: it is OPT-IN (the hosted endpoint needs NVIDIA_API_KEY in the server env; a self-hosted NIM via base is keyless), and unlike the local offload tools it calls a cloud model — so use it deliberately for a stronger model than the local cascade, NOT for routine grunt work. The local GBNF grammar path and the savings ledger are untouched (NIM calls are never ledgered). Set list_models=true to browse available model ids. Returns {model, content, reasoning_content, tokens_in, tokens_out, truncated}; on any failure (no key, endpoint down, bad model) it returns deferred:true with a reason and you handle the prompt yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"the user prompt"},"model":{"type":"string","description":"model id (default from config; set list_models=true to browse)"},"system":{"type":"string","description":"optional system prompt"},"base":{"type":"string","description":"override the OpenAI-compatible base URL incl. /v1 (e.g. a self-hosted NIM http://host:8000/v1)"},"max_tokens":{"type":"integer","description":"max completion tokens (default from config; reasoning models need headroom)"},"temperature":{"type":"number","description":"sampling temperature (default 0)"},"list_models":{"type":"boolean","description":"list available model ids instead of generating"}},"required":["prompt"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Prompt      string  `json:"prompt"`
			Model       string  `json:"model"`
			System      string  `json:"system"`
			Base        string  `json:"base"`
			MaxTokens   int     `json:"max_tokens"`
			Temperature float64 `json:"temperature"`
			ListModels  bool    `json:"list_models"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
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
	})

	return srv.Run(ctx, &mcp.StdioTransport{})
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
