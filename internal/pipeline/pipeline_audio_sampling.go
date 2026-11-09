package pipeline

import (
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// voiceSamplingRefusal says why a generate_audio request's sampling knobs (cfg_weight, exaggeration,
// temperature, repetition_penalty, min_p, top_p) cannot be honoured, or "" when they can (or none was
// given). A knob is never dropped silently: it is refused when it is out of range, when the kind is not
// voice, and when the voice lane that would render it (the speech-server endpoint or audio.cpp) takes
// no such knobs. Only the Chatterbox worker (render/tts.mjs) reads them.
func voiceSamplingRefusal(cfg config.Config, kind string, params map[string]any) string {
	if !core.VoiceSamplingGiven(params) {
		return ""
	}
	if err := core.ValidateVoiceSampling(params); err != nil {
		return err.Error()
	}
	names := "voice sampling (" + core.VoiceSamplingNames(params) + ")"
	if kind != "voice" {
		return names + " applies to kind voice only (the " + kind + " lane has no such knob)"
	}
	voice := paramStr(params, "voice")
	if useTTSEndpoint(cfg, voice) {
		return names + " is not carried by the tts_endpoint lane (the speech server picks its own sampling); use voice generalist on a box with the Chatterbox voice script"
	}
	if audiocppServes(cfg, kind, voice) {
		return names + " is not carried by the audio.cpp voice engine; use the Chatterbox voice script"
	}
	return ""
}

// appendVoiceSampling puts the request's sampling knobs on the voice worker argv. A request's value
// wins over the same flag already on the argv (the finetuned engine's per-machine recipe): the old
// value is replaced in place, so the worker never sees a flag twice.
func appendVoiceSampling(args []string, params map[string]any) []string {
	flags := core.VoiceSamplingFlags(params)
	for i := 0; i+1 < len(flags); i += 2 {
		replaced := false
		for j := 0; j+1 < len(args); j++ {
			if args[j] == flags[i] {
				args[j+1] = flags[i+1]
				replaced = true
				break
			}
		}
		if !replaced {
			args = append(args, flags[i], flags[i+1])
		}
	}
	return args
}
