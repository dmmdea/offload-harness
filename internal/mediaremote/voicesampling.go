package mediaremote

import (
	"fmt"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// planVoiceSampling puts the voice sampling knobs of a generate_audio call (cfg_weight, exaggeration, temperature,
// repetition_penalty, min_p, top_p) into the audio-gen payload under the names the request has, and refuses by name
// what the node could not honour: a value out of range, a knob on a music call, or on a voice the node's speech server
// picks the sampling of. A knob the caller did not name is not sent, so the node's engine keeps its own default.
func planVoiceSampling(p, out map[string]any) error {
	if !core.VoiceSamplingGiven(p) {
		return nil
	}
	if err := core.ValidateVoiceSampling(p); err != nil {
		return &contractError{"generate_audio: " + err.Error()}
	}
	names := "voice sampling (" + core.VoiceSamplingNames(p) + ")"
	if kind := str(p, "kind"); kind != "" && kind != "voice" {
		return &contractError{fmt.Sprintf("generate_audio: %s applies to kind voice only (the %s lane has no such knob)", names, kind)}
	}
	if str(p, "voice") == "endpoint" {
		return &contractError{fmt.Sprintf("generate_audio: %s is not carried by the speech-server (endpoint) voice, which picks its own sampling; use voice generalist", names)}
	}
	for _, k := range core.VoiceSamplingKnobs {
		if v, ok, err := core.VoiceSamplingNumber(p, k.Param); err == nil && ok {
			out[k.Param] = v
		}
	}
	return nil
}

// voiceSamplingRefuses says why node v cannot take an audio call that names a sampling knob, "" when it can or the call
// names none: a node that predates the knobs drops them from the payload and renders with its engine defaults, which is a
// different take than the one asked for.
func (n need) voiceSamplingRefuses(v delegate.NodeView) string {
	if n.voiceSampling && !v.VoiceSamplingHonoured {
		return "does not carry the voice sampling knobs (cfg_weight, exaggeration, temperature, repetition_penalty, min_p, top_p; an older harness would render the take with its engine defaults); send without them, or render it on a node that does"
	}
	return ""
}
