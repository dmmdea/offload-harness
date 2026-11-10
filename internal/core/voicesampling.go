package core

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// VoiceSamplingKnob is one generate() sampling knob of the Chatterbox voice worker, the way a
// generate_audio request names it (Param, a snake_case key in the request's params and in the
// fleet audio-gen payload) and the way the render script takes it (Flag, render/tts.mjs and the
// python workers behind it). Min and Max are the accepted range; a knob whose zero (or whose
// bound) is meaningless excludes that bound, so a nonsense value is refused with a clear error
// before a render takes the card.
type VoiceSamplingKnob struct {
	Param        string
	Flag         string
	Min, Max     float64
	MinExclusive bool
}

// VoiceSamplingKnobs are the six sampling knobs of the voice path, in the order they are sent to
// the worker. The ranges are the ones the engine's own reference interface offers (cfg_weight and
// min_p 0 to 1, exaggeration 0 to 2, temperature above 0 up to 5, repetition_penalty 1 to 2,
// top_p above 0 up to 1); a value outside one makes a take that is noise, not a take.
var VoiceSamplingKnobs = []VoiceSamplingKnob{
	{Param: "cfg_weight", Flag: "--cfg-weight", Min: 0, Max: 1},
	{Param: "exaggeration", Flag: "--exaggeration", Min: 0, Max: 2},
	{Param: "temperature", Flag: "--temperature", Min: 0, Max: 5, MinExclusive: true},
	{Param: "repetition_penalty", Flag: "--repetition-penalty", Min: 1, Max: 2},
	{Param: "min_p", Flag: "--min-p", Min: 0, Max: 1},
	{Param: "top_p", Flag: "--top-p", Min: 0, Max: 1, MinExclusive: true},
}

// VoiceSamplingNumber reads one sampling knob out of a request's params. ok is false when the key is
// absent or null; err is set when it is present and not a finite number (a string, a bool, NaN).
func VoiceSamplingNumber(params map[string]any, param string) (v float64, ok bool, err error) {
	raw, present := params[param]
	if !present || raw == nil {
		return 0, false, nil
	}
	switch n := raw.(type) {
	case float64:
		v = n
	case float32:
		v = float64(n)
	case int:
		v = float64(n)
	case int64:
		v = float64(n)
	case json.Number:
		f, perr := strconv.ParseFloat(n.String(), 64)
		if perr != nil {
			return 0, false, fmt.Errorf("%s must be a number, got %q", param, n.String())
		}
		v = f
	default:
		return 0, false, fmt.Errorf("%s must be a number, got %v", param, raw)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false, fmt.Errorf("%s must be a finite number", param)
	}
	return v, true, nil
}

// VoiceSamplingGiven reports whether a request's params name any sampling knob.
func VoiceSamplingGiven(params map[string]any) bool {
	for _, k := range VoiceSamplingKnobs {
		if raw, ok := params[k.Param]; ok && raw != nil {
			return true
		}
	}
	return false
}

// ValidateVoiceSampling refuses a sampling knob that is not a number or is outside its range, naming
// the knob and the range. A request that names none is fine.
func ValidateVoiceSampling(params map[string]any) error {
	for _, k := range VoiceSamplingKnobs {
		v, ok, err := VoiceSamplingNumber(params, k.Param)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		below := v < k.Min || (k.MinExclusive && v == k.Min)
		if below || v > k.Max {
			lo := "between " + FormatVoiceSampling(k.Min)
			if k.MinExclusive {
				lo = "above " + FormatVoiceSampling(k.Min)
			}
			rng := lo + " and " + FormatVoiceSampling(k.Max)
			if k.MinExclusive {
				rng = lo + ", up to " + FormatVoiceSampling(k.Max)
			}
			return fmt.Errorf("%s must be %s, got %s", k.Param, rng, FormatVoiceSampling(v))
		}
	}
	return nil
}

// FormatVoiceSampling renders a sampling value the way it is sent to the worker: the shortest form
// that reads back as the same number.
func FormatVoiceSampling(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// VoiceSamplingFlags is the worker argv for the sampling knobs a request names: each knob given is a
// flag and a value, in VoiceSamplingKnobs order, and a knob not given is not sent (the model keeps its
// own default). The params are assumed validated.
func VoiceSamplingFlags(params map[string]any) []string {
	var out []string
	for _, k := range VoiceSamplingKnobs {
		if v, ok, err := VoiceSamplingNumber(params, k.Param); err == nil && ok {
			out = append(out, k.Flag, FormatVoiceSampling(v))
		}
	}
	return out
}

// VoiceSamplingNames lists the sampling knobs a request names, for an error message.
func VoiceSamplingNames(params map[string]any) string {
	var names []string
	for _, k := range VoiceSamplingKnobs {
		if raw, ok := params[k.Param]; ok && raw != nil {
			names = append(names, k.Param)
		}
	}
	return strings.Join(names, ", ")
}
