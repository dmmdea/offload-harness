package pipeline

import (
	"fmt"
	"os"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/sttclient"
)

// sttOpts are the two per-request switches of a transcription beyond language and hq: words (return
// per-word timing and decode for it deterministically) and vad ("on" or "off", or "" for the seat's own
// setting).
type sttOpts struct {
	words bool
	vad   string
}

// sttOptions reads a transcribe request's words and vad parameters and says why they cannot be honoured,
// or "" when they can. Nothing is dropped silently:
//   - vad is on or off, nothing else;
//   - the hq model served over the OpenAI protocol returns one full-span segment, so it has no word times
//     and no VAD switch to turn;
//   - vad=on needs a seat that has a VAD model (stt_vad), which this box does not say it has otherwise.
//
// vad=off needs no config: whisper-server reads a per-request vad field over its --vad flag.
func sttOptions(cfg config.Config, params map[string]any, useOAI bool) (sttOpts, string) {
	opt := sttOpts{words: paramBool(params, "words")}
	if raw, ok := params["vad"]; ok && raw != nil {
		s, isStr := raw.(string)
		switch v := strings.ToLower(strings.TrimSpace(s)); {
		case isStr && (v == "on" || v == "off"):
			opt.vad = v
		default:
			return opt, fmt.Sprintf("vad must be on or off, got %v", raw)
		}
	}
	if useOAI && opt.words {
		return opt, "words is not available on the hq model: it is served over the OpenAI protocol, which returns one full-span segment with no word times; drop hq, or drop words"
	}
	if useOAI && opt.vad != "" {
		return opt, "vad is not available on the hq model: it is served over the OpenAI protocol, which takes no VAD switch; drop hq, or drop vad"
	}
	if opt.vad == "on" && !cfg.STTVAD {
		return opt, "vad=on needs stt_vad: this box's whisper seat is not declared to have a VAD model (stt_vad is false), so there is no detector to turn on"
	}
	return opt, ""
}

// apply puts the switches on the decode params of one whisper call. vad=off sends vad=false so a seat
// started with --vad skips it for this call; words decodes at temperature 0 with no temperature fallback,
// so the same audio gives the same words and times on a rerun (the greedy decode of a segment never
// depends on which retry fired).
func (o sttOpts) apply(prm *sttclient.Params) {
	if o.vad == "off" {
		prm.VAD = false
		prm.VADOff = true
	}
	if o.words {
		prm.NoFallback = true
	}
}

// stemSuffix names a non-default decode in the on-disk stem, so a words or vad=off transcript never
// overwrites the .srt/.txt/.segments.json of the same audio decoded the usual way. Empty for the default,
// which keeps every existing stem as it was.
func (o sttOpts) stemSuffix() string {
	s := ""
	if o.vad == "off" {
		s += "|vad=off"
	}
	if o.words {
		s += "|words"
	}
	return s
}

// attachWords writes the whisper.cpp-shaped word file next to the transcript and sets the result's word
// fields: the first sttclient.MaxInlineWords inline, the whole list in the file.
func attachWords(out *transcribeResult, base string, words []sttclient.WordTiming) {
	inline, cut := sttclient.CapWords(words)
	out.Words, out.NumWords, out.WordsTruncated = inline, len(words), cut
	if b, err := sttclient.WhisperCppJSON(words); err == nil {
		path := base + ".words.json"
		if os.WriteFile(path, b, 0o644) == nil {
			out.WordsPath = path
		}
	}
}
