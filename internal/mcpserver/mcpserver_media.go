// Media tool handlers: video, transcription, image, graph, SVG, audio and composition doors.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/composeremote"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediaremote"
	"github.com/dmmdea/offload-harness/internal/sttremote"
	"github.com/dmmdea/offload-harness/internal/visionremote"
)

func (s *Server) handleVideoDescribe(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Video    string `json:"video"`
		Question string `json:"question"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskVideoDescribe, Door: "offload_video_describe", Video: in.Video, Params: map[string]any{"question": in.Question}}))
}

func (s *Server) handleVideoWatch(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Video      string  `json:"video"`
		Question   string  `json:"question"`
		WindowSec  float64 `json:"window_sec"`
		FPS        float64 `json:"fps"`
		MaxFrames  int     `json:"max_frames"`
		FrameWidth int     `json:"frame_width"`
		Start      float64 `json:"start"`
		End        float64 `json:"end"`
		Synthesize *bool   `json:"synthesize"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{"question": in.Question, "window_sec": in.WindowSec, "fps": in.FPS, "max_frames": in.MaxFrames, "frame_width": in.FrameWidth, "start": in.Start, "end": in.End}
	if in.Synthesize != nil {
		params["synthesize"] = *in.Synthesize
	}
	return result(s.p.Run(ctx, core.Request{Task: core.TaskVideoWatch, Door: "offload_video_watch", Video: in.Video, Params: params}))
}

func (s *Server) handleTranscribe(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Audio    string   `json:"audio"`
		Language string   `json:"language"`
		HQ       bool     `json:"hq"`
		Engine   string   `json:"engine"`
		Select   []string `json:"select"`
		Route    string   `json:"route"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	// STT ownership mirrors OCR's (2026-08-23): the GPU whisper seat is the
	// quality primary; the NPU's whisper-base is the caller's EXPLICIT fast
	// preview path, never an automatic fallback, and an unrecognized engine
	// defers rather than silently becoming a GPU call.
	switch in.Engine {
	case "", "gpu":
		// fall through to the GPU seat below
	case "npu":
		// The route places the GPU whisper model; the NPU path is this box's own sidecar and never
		// travels, as for OCR. An OMITTED route is the auto default and no request to travel (the
		// handler reads it here as local, not as auto), so only a route the caller NAMED is refused,
		// rather than silently run local under it.
		if r, ok := sttremote.NormalizeRoute(in.Route); !ok || r != sttremote.RouteLocal {
			return jsonResult(map[string]any{"deferred": true, "reason": fmt.Sprintf("route %q applies to engine gpu only; engine npu always runs this box's Hailo sidecar", in.Route)})
		}
		if !s.p.Cfg().HasAccelerator("hailo-8l") {
			return jsonResult(map[string]any{"deferred": true, "reason": "engine:npu requested but this box lists no hailo-8l accelerator"})
		}
		args := map[string]any{"audio_path": in.Audio}
		if in.Language != "" && in.Language != "auto" {
			args["language"] = in.Language
		}
		// hq does not apply here (one NPU model — the schema says so); select
		// DOES: the npu result honors the same top-level projection as the GPU
		// path, so a caller's context-lean contract holds on both engines.
		sc := s.hailoSidecar()
		if err := sc.Ensure(ctx); err != nil {
			return jsonResult(map[string]any{"deferred": true, "reason": "hailo-8l: " + err.Error()})
		}
		out, err := sc.Client().Call(ctx, "transcribe", args)
		if err != nil {
			return jsonResult(map[string]any{"deferred": true, "reason": "hailo-8l: " + err.Error()})
		}
		if len(in.Select) > 0 {
			keep := map[string]any{}
			for _, k := range in.Select {
				if v, ok := out[k]; ok {
					keep[k] = v
				}
			}
			out = keep
		}
		return jsonResult(out)
	default:
		return jsonResult(map[string]any{"deferred": true,
			"reason": fmt.Sprintf("unrecognized engine %q; use \"gpu\" or \"npu\"", in.Engine)})
	}
	params := map[string]any{}
	if in.Language != "" {
		params["language"] = in.Language
	}
	if in.HQ {
		params["hq"] = true
	}
	// A transcription of long audio is silent for minutes, like a delegation. With a progress
	// token the client hears an opening notification and a heartbeat while the call runs;
	// without one nothing is sent (register C-89).
	stopHeartbeat := s.startHeartbeat(ctx, req, "offload_transcribe")
	defer stopHeartbeat()
	// The default route is auto (ADR 0072): this box's whisper when it is free, a fleet node's when it
	// would keep the call waiting. An empty route is the caller not choosing.
	route := strings.TrimSpace(in.Route)
	if route == "" {
		route = sttremote.RouteAuto
	}
	run := s.sttRun
	if run == nil {
		run = sttremote.Run
	}
	res := run(ctx, s.p.Cfg(), s.p, core.Request{Task: core.TaskTranscribe, Door: "offload_transcribe", Audio: in.Audio, Params: params}, route)
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
	return result(s.p.Run(ctx, core.Request{Task: core.TaskExtractImage, Door: "offload_extract_image", Image: in.Image, Params: map[string]any{"schema": in.Schema}}))
}

func (s *Server) handleAssessImage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image string `json:"image"`
		Brief string `json:"brief"`
		Route string `json:"route"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	if in.Brief != "" {
		params["brief"] = in.Brief
	}
	return result(s.visionRun(ctx, core.Request{Task: core.TaskAssessImage, Door: "offload_assess_image", Image: in.Image, Params: params}, in.Route))
}

func (s *Server) handleOCR(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image  string `json:"image"`
		Engine string `json:"engine"`
		Route  string `json:"route"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	// OCR ownership (2026-08-22): GPU VLM is primary; the NPU's PaddleOCR is the
	// caller's EXPLICIT fast-batch path, never an automatic fallback — the two
	// read stylised text differently and a silent switch would change results.
	switch in.Engine {
	case "", "gpu":
		return result(s.visionRun(ctx, core.Request{Task: core.TaskOCR, Door: "offload_ocr", Image: in.Image}, in.Route))
	case "npu":
		if r, ok := visionremote.NormalizeRoute(in.Route); !ok || r != visionremote.RouteLocal {
			// The route places the GPU vision model; the NPU path is this box's
			// own sidecar and never travels. Refuse rather than silently run
			// local under a route the caller asked for.
			return jsonResult(map[string]any{"deferred": true, "reason": fmt.Sprintf("route %q applies to engine gpu only; engine npu always runs this box's Hailo sidecar", in.Route)})
		}
		if !s.p.Cfg().HasAccelerator("hailo-8l") {
			return jsonResult(map[string]any{"deferred": true, "reason": "engine:npu requested but this box lists no hailo-8l accelerator"})
		}
		return s.hailoCall(ctx, "ocr", map[string]any{"image_path": in.Image})
	default:
		// An unrecognized engine must not silently become a GPU call — the
		// result would be indistinguishable from an intentional one (loop-side
		// twin of this rule lives in agent.offloadTools' ocr tool).
		return jsonResult(map[string]any{"deferred": true,
			"reason": fmt.Sprintf("unrecognized engine %q; use \"gpu\" or \"npu\"", in.Engine)})
	}
}

func (s *Server) handleGenerateImage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Prompt      string   `json:"prompt"`
		Negative    string   `json:"negative"`
		Out         string   `json:"out"`
		Width       int      `json:"width"`
		Height      int      `json:"height"`
		Steps       int      `json:"steps"`
		Seed        int      `json:"seed"`
		Refine      *bool    `json:"refine"`
		Family      string   `json:"family"`
		Transparent bool     `json:"transparent"`
		Route       string   `json:"route"`
		Remotes     []string `json:"remotes"`
		WaitS       *int     `json:"wait_s"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	// Named family (ADR 0058): absent = the default binding, byte-for-byte what a
	// call without it rendered before families existed.
	if in.Family != "" {
		params["family"] = in.Family
	}
	if in.Transparent {
		params["transparent"] = true
	}
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
	// Pointer so absent != false: only an EXPLICIT refine:false reaches the
	// pipeline (the opt-in refiner's only request-level knob turns it off).
	if in.Refine != nil && !*in.Refine {
		params["refine"] = false
	}
	// A busy lane makes the call a ticket the process keeps (ADR 0083): this door answers inside its window (gpu_wait_ms, or
	// wait_s) and the ticket goes on, so the wait is the harness's and the caller is not asked to loop.
	return result(mediaremote.RunWith(ctx, s.mediaCfg(), runTaskAs{s}, core.Request{Task: core.TaskGenerateImage, Door: "offload_generate_image", Input: in.Prompt, Params: withMediaPlace(req.Params.Arguments, params)},
		in.Route, in.Remotes, s.imageWait(in.WaitS)))
}

// imageWait is how the image door (and the graph door, which waits the same way) waits for its ticket: inside its window, with the
// ticket outliving the call, re-reading delegate_remotes from the config file this server runs on.
func (s *Server) imageWait(waitS *int) mediaremote.Wait {
	return mediaremote.Wait{Window: waitWindow(waitS), Detach: true, RosterFile: s.configPath}
}

// maxWaitS caps wait_s: an hour is already longer than any tool call a client will hold open.
const maxWaitS = 3600

// waitWindow turns the door's wait_s into the window a call blocks for its ticket: absent is the default (gpu_wait_ms),
// 0 answers at once (after the ticket's first look), and anything else is that many seconds, capped.
func waitWindow(waitS *int) time.Duration {
	switch {
	case waitS == nil:
		return 0
	case *waitS <= 0:
		return mediaremote.AtOnce
	case *waitS > maxWaitS:
		return maxWaitS * time.Second
	}
	return time.Duration(*waitS) * time.Second
}

func (s *Server) handleEditImageGenerative(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image    string  `json:"image"`
		Prompt   string  `json:"prompt"`
		Negative string  `json:"negative"`
		Preset   string  `json:"preset"`
		Steps    int     `json:"steps"`
		CFG      float64 `json:"cfg"`
		Seed     int     `json:"seed"`
		Out      string  `json:"out"`
		// Named edit family (ADR 0058) and its multi-reference inputs: Images are the
		// references AFTER the target (Image stays image_1).
		Family      string   `json:"family"`
		Images      []string `json:"images"`
		Transparent bool     `json:"transparent"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	if in.Image != "" {
		params["image"] = in.Image
	}
	if in.Family != "" {
		params["family"] = in.Family
	}
	// The pipeline owns the limit (target + references <= 10) and the per-family
	// check, so an over-long list reaches it and defers with the reason there.
	if len(in.Images) > 0 {
		params["images"] = in.Images
	}
	if in.Transparent {
		params["transparent"] = true
	}
	if in.Negative != "" {
		params["negative"] = in.Negative
	}
	if in.Preset != "" {
		params["preset"] = in.Preset
	}
	if in.Steps > 0 {
		params["steps"] = in.Steps
	}
	if in.CFG > 0 {
		params["cfg"] = in.CFG
	}
	if in.Seed > 0 {
		params["seed"] = in.Seed
	}
	if in.Out != "" {
		params["out"] = in.Out
	}
	return result(s.runTask(ctx, core.Request{Task: core.TaskEditImageGenerative, Door: "offload_edit_image_generative", Input: in.Prompt, Params: withMediaPlace(req.Params.Arguments, params)}))
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
	return result(s.runTask(ctx, core.Request{Task: core.TaskInpaintImage, Door: "offload_inpaint_image", Input: in.Prompt, Params: withMediaPlace(req.Params.Arguments, params)}))
}

func (s *Server) handleUpscaleImage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Image  string   `json:"image"`
		Scale  *float64 `json:"scale"` // pointer: an explicit 0 is forwarded and defers, never "unset"
		Width  int      `json:"width"`
		Height int      `json:"height"`
		Method string   `json:"method"`
		Model  string   `json:"model"`
		Out    string   `json:"out"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	if in.Image != "" {
		params["image"] = in.Image
	}
	// Forwarded whenever PRESENT (including 0 and negatives) so the pipeline names the
	// bad value in its defer instead of silently rendering at the model's factor.
	if in.Scale != nil {
		params["scale"] = *in.Scale
	}
	if in.Width != 0 {
		params["width"] = in.Width
	}
	if in.Height != 0 {
		params["height"] = in.Height
	}
	if in.Method != "" {
		params["method"] = in.Method
	}
	if in.Model != "" {
		params["model"] = in.Model
	}
	if in.Out != "" {
		params["out"] = in.Out
	}
	return result(s.runTask(ctx, core.Request{Task: core.TaskUpscaleImage, Door: "offload_upscale_image", Params: withMediaPlace(req.Params.Arguments, params)}))
}

func (s *Server) handleRunGraph(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		GraphPath    string `json:"graph_path"`
		GraphJSON    string `json:"graph_json"`
		ManifestPath string `json:"manifest_path"`
		ManifestJSON string `json:"manifest_json"`
		OutDir       string `json:"out_dir"`
		ReserveVram  string `json:"reserve_vram"`
		// Devices is operator-declared: absent = the whole node (see the schema).
		Devices []string `json:"devices"`
		// MinVRAMGB (a number, or a numeral in a string) and ModelFamily steer which fleet node may take the graph; they are
		// forwarded only when given, so a call that never passes them is what it was (mediaremote/graph.go).
		MinVRAMGB   any      `json:"min_vram_gb"`
		ModelFamily string   `json:"model_family"`
		Route       string   `json:"route"`
		Remotes     []string `json:"remotes"`
		WaitS       *int     `json:"wait_s"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	// Inline graph_json/manifest_json are written to a temp file whose path is threaded
	// on (the mjs reads files, not inline). Empty+empty manifest → "" (manifest optional).
	graphPath, cleanGraph, err := materialize(in.GraphPath, in.GraphJSON, "run-graph-*.json")
	if err != nil {
		res, _ := jsonResult(map[string]any{"deferred": true, "reason": "bad arguments: graph: " + err.Error()})
		return res, nil
	}
	// The temp files an inline graph_json or manifest_json became are this call's own, and go when it returns, whichever way it
	// returns (an answer, a deferral, a refusal, a ticket queued, a panic). A ticket this call made is not left holding them: it keeps
	// its own copy of the bytes (internal/mediaremote/ticket.go writeBodies), which is what lets a queued graph run after the call
	// is over. A file the caller named is never one of them.
	defer cleanGraph()
	if graphPath == "" {
		res, _ := jsonResult(map[string]any{"deferred": true, "reason": "bad arguments: graph_path or graph_json required"})
		return res, nil
	}
	manifestPath, cleanManifest, err := materialize(in.ManifestPath, in.ManifestJSON, "run-graph-manifest-*.json")
	if err != nil {
		res, _ := jsonResult(map[string]any{"deferred": true, "reason": "bad arguments: manifest: " + err.Error()})
		return res, nil
	}
	defer cleanManifest()
	params := map[string]any{
		"graph_path":    graphPath,
		"manifest_path": manifestPath,
		"out_dir":       in.OutDir,
		"reserve_vram":  in.ReserveVram,
	}
	if len(in.Devices) > 0 {
		params["devices"] = in.Devices
	}
	if in.MinVRAMGB != nil {
		params["min_vram_gb"] = in.MinVRAMGB
	}
	if f := strings.TrimSpace(in.ModelFamily); f != "" {
		params["model_family"] = f
	}
	// A busy lane makes a whole-node graph a ticket the process keeps (ADR 0083 Amendment 1), as it does an image: this door answers
	// inside its window and the ticket goes on, so the wait is the harness's and the caller is not asked to loop. The graph is
	// keyed by its content, so a graph_json (a temp file of its own on every call) that is sent again is the same ticket.
	return result(mediaremote.RunWith(ctx, s.mediaCfg(), runTaskAs{s}, core.Request{Task: core.TaskRunGraph, Door: "offload_run_graph", Params: withMediaPlace(req.Params.Arguments, params)},
		in.Route, in.Remotes, s.imageWait(in.WaitS)))
}

// materialize returns path if set, else writes inline json to a temp file and returns
// that path (the mjs reads files, not inline). Empty+empty → no path and no error, for the
// optional manifest; a required graph is validated by the caller when this returns "".
//
// The cleanup it returns is never nil and removes only a file THIS call made: it does nothing for a path
// the caller named, nor for an empty one. A write that fails removes its own file before the error returns.
func materialize(path, inline, pattern string) (string, func(), error) {
	noop := func() {}
	if path != "" {
		return path, noop, nil
	}
	if inline == "" {
		return "", noop, nil
	}
	f, err := createTemp("", pattern)
	if err != nil {
		return "", noop, err
	}
	name := f.Name()
	_, werr := f.WriteString(inline)
	cerr := f.Close() // closed before any removal: Windows will not delete a file that is still open
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		removeInlineTemp(name)
		return "", noop, werr
	}
	return name, func() { removeInlineTemp(name) }, nil
}

// createTemp makes the temp file an inline graph or manifest is written to; a seam, so that a test can make the write fail.
var createTemp = os.CreateTemp

// removeFile, removeAttempts and removePause are how the temp files an inline graph or manifest became are deleted; seams, so that a
// test can make a delete fail the way a file held for a moment fails on Windows.
var (
	removeFile     = os.Remove
	removeAttempts = 20
	removePause    = 25 * time.Millisecond
)

// removeInlineTemp deletes a temp file this call made. A scanner that holds the file for a moment, or a runner that has only just
// exited, makes the delete fail with a sharing violation on Windows, so it is tried again for about half a second, as
// pipeline.removeRunGraphEnvelope does for the envelope; a file that is not there is not a failure. One that still cannot be removed is
// named in the log, so that the leak this cleanup exists to stop is never silent.
func removeInlineTemp(name string) {
	var err error
	for i := 0; i < removeAttempts; i++ {
		if i > 0 {
			time.Sleep(removePause)
		}
		if err = removeFile(name); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	log.Printf("offload_run_graph: could not remove the temp file %s (%v); it stays in the OS temp dir until someone deletes it", name, err)
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
	return result(s.p.Run(ctx, core.Request{Task: core.TaskGenerateSVG, Door: "offload_generate_svg", Params: params}))
}

func (s *Server) handleGenerateVideo(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Prompt      string   `json:"prompt"`
		Still       string   `json:"still"`
		Model       string   `json:"model"`
		Negative    string   `json:"negative"`
		Out         string   `json:"out"`
		Frames      int      `json:"frames"`
		Width       int      `json:"width"`
		Height      int      `json:"height"`
		Steps       int      `json:"steps"`
		Seed        int      `json:"seed"`
		ReserveVRAM float64  `json:"reserve_vram"`
		Fast        bool     `json:"fast"`
		Hero        bool     `json:"hero"`
		Upscale     bool     `json:"upscale"`
		Route       string   `json:"route"`
		Remotes     []string `json:"remotes"`
		Node        string   `json:"node"`
		MinVRAMGB   any      `json:"min_vram_gb"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	laneTargeting(params, in.Node, in.MinVRAMGB)
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
	return result(mediaremote.Run(ctx, s.mediaCfg(), runTaskAs{s}, core.Request{Task: core.TaskGenerateVideo, Door: "offload_generate_video", Input: in.Prompt, Image: in.Still, Params: withMediaPlace(req.Params.Arguments, params)}, in.Route, in.Remotes))
}

func (s *Server) handleAnimateCharacter(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Ref          string   `json:"ref"`
		Driver       string   `json:"driver"`
		Prompt       string   `json:"prompt"`
		MotionPrompt string   `json:"motion_prompt"`
		Negative     string   `json:"negative"`
		Width        int      `json:"width"`
		Height       int      `json:"height"`
		Frames       int      `json:"frames"`
		Steps        int      `json:"steps"`
		Seed         int      `json:"seed"`
		PoseStrength string   `json:"pose_strength"`
		RefStrength  string   `json:"ref_strength"`
		ReserveVRAM  float64  `json:"reserve_vram"`
		Out          string   `json:"out"`
		Route        string   `json:"route"`
		Remotes      []string `json:"remotes"`
		Node         string   `json:"node"`
		MinVRAMGB    any      `json:"min_vram_gb"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	laneTargeting(params, in.Node, in.MinVRAMGB)
	if in.Ref != "" {
		params["ref"] = in.Ref
	}
	if in.Driver != "" {
		params["driver"] = in.Driver
	}
	if in.MotionPrompt != "" {
		params["motion_prompt"] = in.MotionPrompt
	}
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
	if in.Frames > 0 {
		params["frames"] = in.Frames
	}
	if in.Steps > 0 {
		params["steps"] = in.Steps
	}
	if in.Seed > 0 {
		params["seed"] = in.Seed
	}
	if in.PoseStrength != "" {
		params["pose_strength"] = in.PoseStrength
	}
	if in.RefStrength != "" {
		params["ref_strength"] = in.RefStrength
	}
	if in.ReserveVRAM > 0 {
		params["reserve_vram"] = strconv.FormatFloat(in.ReserveVRAM, 'f', -1, 64)
	}
	return result(mediaremote.Run(ctx, s.mediaCfg(), runTaskAs{s}, core.Request{Task: core.TaskAnimateCharacter, Door: "offload_animate_character", Input: in.Prompt, Image: in.Ref, Video: in.Driver, Params: withMediaPlace(req.Params.Arguments, params)}, in.Route, in.Remotes))
}

func (s *Server) handleGenerateAudio(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Text        string   `json:"text"`
		Kind        string   `json:"kind"`
		Voice       string   `json:"voice"`
		TTSVoice    string   `json:"tts_voice"` // voice=endpoint: the server-side voice name
		Clone       string   `json:"clone"`
		Lang        string   `json:"lang"`
		Seconds     int      `json:"seconds"`
		Out         string   `json:"out"`
		Seed        int      `json:"seed"`
		ReserveVRAM float64  `json:"reserve_vram"`
		Route       string   `json:"route"`
		Remotes     []string `json:"remotes"`
		Node        string   `json:"node"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	laneTargeting(params, in.Node, nil)
	if in.Kind != "" {
		params["kind"] = in.Kind
	}
	if in.Voice != "" {
		params["voice"] = in.Voice
	}
	if in.TTSVoice != "" {
		params["tts_voice"] = in.TTSVoice
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
	return result(mediaremote.Run(ctx, s.mediaCfg(), runTaskAs{s}, core.Request{Task: core.TaskGenerateAudio, Door: "offload_generate_audio", Input: in.Text, Params: withMediaPlace(req.Params.Arguments, params)}, in.Route, in.Remotes))
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
	return result(s.p.Run(ctx, core.Request{Task: core.TaskEditImage, Door: "offload_edit_image", Image: in.Image, Params: params}))
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
	return result(s.p.Run(ctx, core.Request{Task: core.TaskMedia, Door: "offload_media", Params: params}))
}

// handleComposeVideo — offload_compose_video (ADR 0059). The pipeline validates the
// one-input rule and every enum; this handler only decodes and forwards what was set.
func (s *Server) handleComposeVideo(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Template    string         `json:"template"`
		Variables   map[string]any `json:"variables"`
		HTML        string         `json:"html"`
		ProjectDir  string         `json:"project_dir"`
		Composition string         `json:"composition"`
		Out         string         `json:"out"`
		Format      string         `json:"format"`
		FPS         float64        `json:"fps"`
		Quality     string         `json:"quality"`
		Resolution  string         `json:"resolution"`
		Workers     float64        `json:"workers"`
		Strict      *bool          `json:"strict"`
		Snapshots   []float64      `json:"snapshots"`
		Route       string         `json:"route"`
	}
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	params := map[string]any{}
	for k, v := range map[string]string{"template": in.Template, "html": in.HTML, "project_dir": in.ProjectDir,
		"composition": in.Composition, "out": in.Out, "format": in.Format, "quality": in.Quality, "resolution": in.Resolution} {
		if v != "" {
			params[k] = v
		}
	}
	if in.Variables != nil {
		params["variables"] = in.Variables
	}
	if in.FPS != 0 {
		params["fps"] = in.FPS
	}
	if in.Workers != 0 {
		params["workers"] = in.Workers
	}
	if in.Strict != nil {
		params["strict"] = *in.Strict
	}
	if len(in.Snapshots) > 0 {
		params["snapshots"] = in.Snapshots
	}
	return result(composeremote.Run(ctx, s.p.Cfg(), s.p, core.Request{Task: core.TaskComposeVideo, Door: "offload_compose_video", Params: params}, in.Route))
}
