package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediaops"
)

// edit_image + media handlers. Deterministic CPU work (PIL/GIMP/ffmpeg via
// internal/mediaops): NO GPU lock, no llama-swap eviction — these run in
// parallel with renders (spec 2026-07-16-edit-media-tools-design.md). Every
// failure class defers cleanly (bad args, engine absent, tool failure); the
// savings ledger records like generate_svg does.

func (p *Pipeline) editConfig() mediaops.EditConfig {
	timeout := time.Duration(p.cfg.EditTimeoutSec) * time.Second
	worker := ""
	if p.cfg.ImageGenScript != "" {
		// render/edit_image.py lives beside the render scripts this machine already
		// resolved (imagegen_script is an absolute path on configured boxes).
		worker = filepath.Join(filepath.Dir(p.cfg.ImageGenScript), "edit_image.py")
	}
	return mediaops.EditConfig{
		Python:      mediaops.ResolveEditPython(p.cfg.EditPython, p.cfg.ComfyDir),
		GimpConsole: p.cfg.GimpConsolePath,
		Worker:      worker,
		Timeout:     timeout,
	}
}

// runEditImage: params image (path), ops ([]op objects), out (optional).
func (p *Pipeline) runEditImage(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	meta.Model = "pil"
	deferWith := func(reason string) core.Result {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Image), reason)
		return core.Deferf(reason, "", meta)
	}
	var ops []mediaops.EditOp
	if raw, ok := req.Params["ops"]; ok {
		b, err := json.Marshal(raw)
		if err != nil {
			return deferWith("edit_image: bad ops: " + err.Error())
		}
		if err := json.Unmarshal(b, &ops); err != nil {
			return deferWith("edit_image: bad ops: " + err.Error())
		}
	}
	cfg := p.editConfig()
	if cfg.Worker == "" {
		return deferWith("edit_image: imagegen_script unset so render/edit_image.py is unlocatable — set imagegen_script (or install the render scripts)")
	}
	if mediaops.UsesGimp(ops) {
		meta.Model = "gimp+pil"
	}
	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, "edit-"+sha256hex(req.Image+fmt.Sprint(req.Params["ops"]))[:8]+".png")
	}
	res, err := mediaops.RunEditImage(ctx, cfg, mediaops.EditRequest{Image: req.Image, Ops: ops, Out: out})
	if err != nil {
		return deferWith("edit_image: " + err.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(res)
	p.record(req.Task, meta, len(req.Image))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// runMedia: params op (string), in/inputs, out, and op-specific args.
func (p *Pipeline) runMedia(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	meta.Model = "ffmpeg"
	deferWith := func(reason string) core.Result {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, 0, reason)
		return core.Deferf(reason, "", meta)
	}
	mreq := mediaops.MediaRequest{
		Op:        paramStr(req.Params, "op"),
		In:        paramStr(req.Params, "in"),
		Out:       paramStr(req.Params, "out"),
		Start:     paramStr(req.Params, "start"),
		End:       paramStr(req.Params, "end"),
		Duration:  paramStr(req.Params, "duration"),
		Audio:     paramStr(req.Params, "audio"),
		Shortest:  paramBoolOr(req.Params, "shortest", true),
		Reencode:  paramBool(req.Params, "reencode"),
		AudioOnly: paramBool(req.Params, "audio_only"),
		VideoOnly: paramBool(req.Params, "video_only"),
		FPS:       paramFloat(req.Params, "fps"),
		Count:     paramIntOr(req.Params, "count", 0),
	}
	if raw, ok := req.Params["inputs"]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &mreq.Inputs)
	}
	if mreq.Op == "" {
		return deferWith("media: missing op (trim|concat|extract_frames|convert|mux_audio|probe)")
	}
	if mreq.Out == "" && mreq.Op != "probe" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		ext := defaultMediaExt(mreq)
		mreq.Out = filepath.Join(p.cfg.MediaDir, "media-"+sha256hex(mreq.Op+mreq.In+fmt.Sprint(mreq.Inputs))[:8]+ext)
	}
	cfg := mediaops.MediaConfig{FFmpeg: p.cfg.FFmpegPath, Timeout: time.Duration(p.cfg.EditTimeoutSec) * time.Second}
	res, err := mediaops.RunMedia(ctx, cfg, mreq)
	if err != nil {
		return deferWith("media " + mreq.Op + ": " + err.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(res)
	p.record(req.Task, meta, 0)
	return core.Result{OK: true, Data: data, Meta: meta}
}

// defaultMediaExt picks a sensible default output name per op when out is unset:
// extract_frames wants a DIRECTORY (RunMedia appends the frame pattern); container
// ops inherit the input's extension (concat: the first input's).
func defaultMediaExt(r mediaops.MediaRequest) string {
	switch r.Op {
	case "extract_frames":
		return "" // directory
	case "concat":
		if len(r.Inputs) > 0 {
			return filepath.Ext(r.Inputs[0])
		}
		return ".mp4"
	default:
		if e := filepath.Ext(r.In); e != "" {
			return e
		}
		return ".mp4"
	}
}

func paramFloat(p map[string]any, k string) float64 {
	switch v := p[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func paramBoolOr(p map[string]any, k string, def bool) bool {
	if _, ok := p[k]; !ok {
		return def
	}
	return paramBool(p, k)
}
