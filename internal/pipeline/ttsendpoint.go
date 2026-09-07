package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/tasks"
	"github.com/dmmdea/offload-harness/internal/ttsclient"
)

// VoiceEndpoint is the generate_audio voice value that selects the
// tts_endpoint lane explicitly (beside generalist / finetuned).
const VoiceEndpoint = "endpoint"

// useTTSEndpoint decides whether a voice request goes to the HTTP lane: yes
// when the caller names it (voice=endpoint — a box without the key then
// defers by name inside runVoiceEndpoint, never falls back to a script the
// caller did not ask for), and by default when the box has an endpoint and no
// local voice script (the VoiceStudio-only box). A box with both keeps
// Chatterbox as its default voice, exactly as before the lane existed.
func useTTSEndpoint(cfg config.Config, voice string) bool {
	if voice == VoiceEndpoint {
		return true
	}
	if voice != "" && voice != "generalist" {
		return false
	}
	return cfg.TTSEndpoint != "" && cfg.VoiceGenScript == ""
}

// runVoiceEndpoint renders generate_audio kind=voice through the configured
// OpenAI-compatible speech server (ttsclient). No media lease: the server
// owns its GPU (a remote engine's rule). params: clone is not supported here
// (a server-side voice profile is named by tts_voice / the request's
// tts_voice param), lang is passed as the language hint, out as usual.
func (p *Pipeline) runVoiceEndpoint(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	if p.cfg.TTSEndpoint == "" {
		return p.deferGen(req, meta, start, len(req.Input), "voice=endpoint requested but tts_endpoint is unset on this box")
	}
	text := req.Input
	if text == "" {
		return p.deferGen(req, meta, start, len(req.Input), "empty audio prompt")
	}
	model := p.cfg.TTSModel
	if model == "" {
		model = ttsclient.DefaultModel
	}
	voice := paramStr(req.Params, "tts_voice")
	if voice == "" {
		voice = p.cfg.TTSVoice
	}
	if voice == "" {
		voice = ttsclient.DefaultVoice
	}
	meta.Model = "tts-endpoint:" + model
	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, "voice-"+sha256hex(text + tasks.StableParamsKey(req.Params))[:8]+".wav")
	}
	timeout := time.Duration(p.cfg.AudioGenTimeoutSec) * time.Second
	res, err := ttsclient.Speak(ctx, ttsclient.Request{
		Base:     p.cfg.TTSEndpoint,
		APIKey:   p.cfg.TTSAPIKey,
		Model:    model,
		Voice:    voice,
		Text:     text,
		Language: paramStr(req.Params, "lang"),
		Format:   "wav",
		Out:      out,
		Timeout:  timeout,
	})
	if err != nil {
		meta.ErrClass = "tts_endpoint"
		return p.deferGen(req, meta, start, len(req.Input), "audio generation failed: "+err.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{"audio_path": res.Path, "kind": "voice", "engine": "tts_endpoint",
		"endpoint": p.cfg.TTSEndpoint, "model": res.Model, "voice": res.Voice, "bytes": res.Bytes})
	p.record(req.Task, meta, len(text))
	return core.Result{OK: true, Data: data, Meta: meta}
}
