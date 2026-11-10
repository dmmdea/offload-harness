package sttclient

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// WordOffsets is a word's span in whole milliseconds, under the same keys whisper.cpp's JSON output uses
// (transcription[].offsets.from and .to), so a reader written against that file reads these.
type WordOffsets struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// WordTiming is one word of a transcript with its span: Start and End in seconds (the verbose_json
// reading), Offsets the same span in milliseconds (the whisper.cpp reading), and the word's decode
// probability.
type WordTiming struct {
	Word        string      `json:"word"`
	Start       float64     `json:"start"`
	End         float64     `json:"end"`
	Offsets     WordOffsets `json:"offsets"`
	Probability float64     `json:"probability"`
}

func millis(sec float64) int64 { return int64(math.Round(sec * 1000)) }

// FlatWords lists every word of every segment in order, trimmed of the leading space whisper puts on a
// word, with the empty ones (a bare space token) dropped. nil when no segment carries word times.
func FlatWords(segs []Segment) []WordTiming {
	var out []WordTiming
	for _, s := range segs {
		for _, w := range s.Words {
			text := strings.TrimSpace(w.Word)
			if text == "" {
				continue
			}
			out = append(out, WordTiming{
				Word: text, Start: w.Start, End: w.End,
				Offsets:     WordOffsets{From: millis(w.Start), To: millis(w.End)},
				Probability: w.Probability,
			})
		}
	}
	return out
}

// clock renders milliseconds as whisper.cpp's HH:MM:SS,mmm.
func clock(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// WhisperCppJSON is the word list in the shape whisper-cli's -oj file has when it is run one word to a
// segment (-ml 1 -sow): {"transcription":[{"timestamps":{"from","to"},"offsets":{"from","to"},"text":" word"}]},
// the text with the leading space whisper.cpp writes. A reader of that file (offsets in milliseconds under
// transcription[]) reads this one unchanged.
func WhisperCppJSON(words []WordTiming) ([]byte, error) {
	type span struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	type entry struct {
		Timestamps span        `json:"timestamps"`
		Offsets    WordOffsets `json:"offsets"`
		Text       string      `json:"text"`
	}
	rows := make([]entry, 0, len(words))
	for _, w := range words {
		rows = append(rows, entry{
			Timestamps: span{From: clock(w.Offsets.From), To: clock(w.Offsets.To)},
			Offsets:    w.Offsets,
			Text:       " " + w.Word,
		})
	}
	return json.MarshalIndent(map[string]any{"transcription": rows}, "", "  ")
}

// MaxInlineWords caps the words a result carries inline; the whole list is in the words file.
const MaxInlineWords = 2000

// CapWords is the inline part of a word list and whether it was cut.
func CapWords(words []WordTiming) (inline []WordTiming, truncated bool) {
	if len(words) > MaxInlineWords {
		return words[:MaxInlineWords], true
	}
	return words, false
}
