package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/tasks"
	"github.com/dmmdea/offload-harness/internal/videoio"
)

// video_watch — watch a video END TO END on the local vision seat.
//
// video_describe samples max_frames at fps from the head of a file, so on the
// default 12 frames @ 2 fps it "sees" the first six seconds of a thirty-second
// short and answers "I cannot tell" about the rest (measured 2026-08-24 on the
// OptiPlex rig). video_watch removes that ceiling without changing what the
// vision seat is good at: it plans fixed-length time windows over the whole
// duration, samples each window at its own fps/frame budget, sends every window
// through the SAME per-call machinery as video_describe (cache, GPU-lock gate,
// breaker, retry, ledger — runVisionGen), labels frames with ABSOLUTE timestamps
// so the notes cite real seconds, then synthesizes the per-window notes into one
// answer on the TEXT seat. A window that defers is reported, not fatal; the
// whole call defers only when every window did (or nothing could be planned).

// Defaults. window_sec × fps ≈ frames per window; 8 s @ 1 fps = 8 frames keeps a
// window well inside an 8k-ctx VLM at 512px, and 1 fps is one frame per second
// of the ENTIRE video — dense enough to catch a 2-second insert.
const (
	videoWatchDefaultWindowSec = 8.0
	videoWatchDefaultFPS       = 1.0
	videoWatchMaxWindows       = 240 // 32 min @ 8 s; a longer file needs a bigger window_sec (reported, never silent)
	videoWatchSynthMaxTokens   = 700
)

// videoWatchWindow is one planned time window (absolute seconds).
type videoWatchWindow struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// planVideoWatchWindows splits [start, end) of a duration-long video into
// windowSec-long windows (the last one may be shorter, never < 0.5 s). Pure so
// it is unit-testable without ffmpeg. end <= 0 means "to the end".
func planVideoWatchWindows(duration, start, end, windowSec float64, maxWindows int) ([]videoWatchWindow, error) {
	if duration <= 0 {
		return nil, fmt.Errorf("video_watch: no duration")
	}
	if windowSec <= 0 {
		windowSec = videoWatchDefaultWindowSec
	}
	if start < 0 {
		start = 0
	}
	if end <= 0 || end > duration {
		end = duration
	}
	if start >= end {
		return nil, fmt.Errorf("video_watch: start %.2f is not before end %.2f", start, end)
	}
	if maxWindows <= 0 {
		maxWindows = videoWatchMaxWindows
	}
	var out []videoWatchWindow
	for t := start; t < end; t += windowSec {
		e := math.Min(t+windowSec, end)
		if e-t < 0.5 && len(out) > 0 {
			out[len(out)-1].End = e // fold a sliver into the previous window
			break
		}
		out = append(out, videoWatchWindow{Start: round3(t), End: round3(e)})
		if len(out) > maxWindows {
			return nil, fmt.Errorf("video_watch: %d windows of %.1fs exceed the %d-window cap — raise window_sec or narrow start/end", len(out), windowSec, maxWindows)
		}
	}
	return out, nil
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// videoWatchNote is the per-window output surfaced to the caller.
type videoWatchNote struct {
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
	Frames   int     `json:"frames"`
	Notes     string  `json:"notes,omitempty"`
	Truncated bool    `json:"truncated,omitempty"` // notes are partial (hit max_tokens); still evidence
	Deferred  bool    `json:"deferred,omitempty"`
	Reason    string  `json:"reason,omitempty"`
}

// runVideoWatch implements TaskVideoWatch. Params: question (required),
// window_sec, fps, max_frames, frame_width, start, end, synthesize (default
// true), max_windows.
func (p *Pipeline) runVideoWatch(ctx context.Context, req core.Request, built tasks.Built, meta core.Meta, start time.Time) core.Result {
	if p.cfg.VisionModel == "" {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "no vision model configured")
		return core.Deferf("no vision model configured", "", meta)
	}
	meta.Model = p.cfg.VisionModel

	windowSec := paramFloat(req.Params, "window_sec")
	if windowSec <= 0 {
		windowSec = videoWatchDefaultWindowSec
	}
	fps := paramFloat(req.Params, "fps")
	if fps <= 0 {
		fps = videoWatchDefaultFPS
	}
	maxFrames := paramIntOr(req.Params, "max_frames", p.cfg.VideoMaxFrames)
	if maxFrames <= 0 {
		maxFrames = 12
	}
	width := paramIntOr(req.Params, "frame_width", p.cfg.VideoFrameWidth)
	if width <= 0 {
		width = 512
	}
	synthesize := paramBoolOr(req.Params, "synthesize", true)

	duration, err := videoio.Duration(req.Video, p.cfg.FFmpegPath)
	if err != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "probe: "+err.Error())
		return core.Deferf("probe: "+err.Error(), "", meta)
	}
	windows, err := planVideoWatchWindows(duration, paramFloat(req.Params, "start"), paramFloat(req.Params, "end"), windowSec, paramIntOr(req.Params, "max_windows", 0))
	if err != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), err.Error())
		return core.Deferf(err.Error(), "", meta)
	}

	// Identity once, up front (same reasoning as video_describe: loop-invariant,
	// and a mid-sweep rotation must not store a hybrid under one digest).
	vidID, vdErr := p.digestMedia(req.Video)

	notes := make([]videoWatchNote, 0, len(windows))
	var tokIn, tokOut, framesTotal, deferred int
	for _, w := range windows {
		if ctx.Err() != nil {
			break
		}
		wdur := w.End - w.Start
		wframes := int(math.Ceil(wdur * fps))
		if wframes > maxFrames {
			wframes = maxFrames
		}
		if wframes < 1 {
			wframes = 1
		}
		note := videoWatchNote{Start: w.Start, End: w.End}
		wwidth := width
		for {
			frames, serr := videoio.SampleFramesWindow(req.Video, p.cfg.FFmpegPath, w.Start, wdur, fps, wframes, wwidth, p.cfg.VisionMaxImageBytes)
			if serr != nil {
				note.Deferred, note.Reason = true, "frame sampling: "+serr.Error()
				break
			}
			labels := make([]string, len(frames))
			for i := range frames {
				labels[i] = fmt.Sprintf("<%.1f seconds>", w.Start+float64(i)/fps)
			}
			cacheable := vdErr == nil && p.mediaStillMatches(vidID, req.Video)
			extra := ""
			if cacheable {
				extra = fmt.Sprintf("vidw:%s|s=%.3f|d=%.3f|fps=%g|n=%d|w=%d|frames=%d", vidID.Digest, w.Start, wdur, fps, wframes, wwidth, len(frames))
			}
			wmeta := core.Meta{Model: p.cfg.VisionModel}
			res := p.runVisionGen(ctx, req, built, wmeta, time.Now(), extra, cacheable, func(gctx context.Context) (llamaclient.GenResult, error) {
				return p.client.GenerateVisionInterleaved(gctx, p.cfg.VisionModel, built.System, labels, frames, built.User, built.Grammar, built.MaxTokens, p.cfg.Temperature, 0, llamaclient.WithoutThinking())
			})
			tokIn += res.Meta.TokensIn
			tokOut += res.Meta.TokensOut
			if res.OK {
				var got map[string]string
				_ = json.Unmarshal(res.Data, &got)
				note.Notes = strings.TrimSpace(got["answer"])
				note.Frames = len(frames)
				framesTotal += len(frames)
				break
			}
			if wwidth > 256 && isContextOverflow(res.Reason) {
				wwidth /= 2 // same halve-and-retry as video_describe: keep coverage, shrink pixels
				continue
			}
			// A window whose notes hit max_tokens is still EVIDENCE for the seconds it
			// did cover — video_describe rightly defers a truncated single answer, but
			// dropping a whole window here would leave a hole in the sweep (measured on
			// the first live run: 2 of 4 windows lost to "vision output truncated").
			// Keep the partial notes, mark them, and let the synthesis treat the tail
			// of the window as unverified.
			if strings.HasPrefix(res.Reason, "vision output truncated") && strings.TrimSpace(res.Partial) != "" {
				note.Notes = strings.TrimSpace(res.Partial) + "\n(notes truncated at the model's output limit — the rest of this window is unverified)"
				note.Frames = len(frames)
				note.Truncated = true
				framesTotal += len(frames)
				break
			}
			note.Deferred, note.Reason = true, res.Reason
			break
		}
		if note.Deferred {
			deferred++
			log.Printf("video_watch: window %.1f-%.1fs deferred: %s", w.Start, w.End, note.Reason)
		}
		notes = append(notes, note)
	}
	meta.TokensIn, meta.TokensOut = tokIn, tokOut
	if deferred == len(notes) {
		meta.LatencyMs = time.Since(start).Milliseconds()
		reason := "every window deferred"
		if len(notes) > 0 {
			reason += ": " + notes[0].Reason
		}
		p.recordDefer(req.Task, meta, len(req.Input), reason)
		return core.Deferf(reason, "", meta)
	}

	out := map[string]any{
		"duration_sec":     round3(duration),
		"window_sec":       windowSec,
		"fps":              fps,
		"frame_width":      width,
		"windows_total":    len(notes),
		"windows_deferred": deferred,
		"frames_total":     framesTotal,
		"windows":          notes,
	}
	// Synthesis on the TEXT seat: the per-window notes are the evidence; the
	// answer must cite seconds from them and never invent a window it lacks.
	if synthesize {
		var sb strings.Builder
		for _, n := range notes {
			if n.Deferred {
				fmt.Fprintf(&sb, "[%.1f-%.1fs] (no notes — %s)\n", n.Start, n.End, n.Reason)
				continue
			}
			fmt.Fprintf(&sb, "[%.1f-%.1fs] %s\n", n.Start, n.End, n.Notes)
		}
		system := "You are answering a question about a whole video from timestamped notes written by a vision model that watched it window by window. Answer using ONLY the notes. Cite seconds (e.g. 'at 13-16s') for every claim, flag any window that has no notes as unverified, and keep it concise and concrete. If the notes do not settle the question, say what is missing."
		user := "Question: " + paramStr(req.Params, "question") + "\n\nNotes (" + fmt.Sprintf("%d windows, %.1fs total", len(notes), duration) + "):\n" + sb.String() + "\nAnswer:"
		gres, gerr := p.client.Generate(ctx, p.cfg.Model, system, user, "", videoWatchSynthMaxTokens, p.cfg.Temperature, 0, llamaclient.WithoutThinking())
		if gerr != nil || strings.TrimSpace(gres.Content) == "" {
			why := "empty"
			if gerr != nil {
				why = gerr.Error()
			}
			out["synthesis_error"] = why // the notes are still the deliverable
			log.Printf("video_watch: synthesis on %s failed (%s); returning notes only", p.cfg.Model, why)
		} else {
			out["answer"] = strings.TrimSpace(gres.Content)
			meta.TokensIn += gres.TokensIn
			meta.TokensOut += gres.TokensOut
		}
	}
	data, _ := json.Marshal(out)
	meta.LatencyMs = time.Since(start).Milliseconds()
	p.record(req.Task, meta, len(req.Input))
	return core.Result{OK: true, Data: data, Meta: meta}
}
