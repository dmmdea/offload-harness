package sttclient

// words_reliable_test.go pins D-38 #169, the flag half: a transcript whose word times drift
// from their segment says so (Result.WordsReliable / WordsDriftSec), computed by the pure
// checkWordDrift and set by Transcribe from the upstream's answer. The cause of the drift
// (where the per-chunk offsets are lost) is not found here; this is the symptom's detector.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// word and seg build the small hand-made transcripts the pure tests run on.
func word(start float64) Word {
	return Word{Word: " w", Start: start, End: start + 0.3, Probability: 0.9}
}

func seg(start float64, words ...Word) Segment {
	return Segment{Start: start, End: start + 4, Text: " t", Words: words}
}

// loadDrifted decodes testdata/drifted_words.json (hand-written, in whisper-server's verbose_json
// shape: segment 0 aligned, segment 1 lagging by 58.8 s, segment 2 by 58.5 s).
func loadDrifted(t *testing.T) (Result, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "drifted_words.json"))
	if err != nil {
		t.Fatal(err)
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("fixture does not decode as a verbose_json answer: %v", err)
	}
	if len(res.Segments) != 3 || len(res.Segments[1].Words) == 0 {
		t.Fatalf("fixture drifted: %d segments, segment 1 words %d", len(res.Segments), len(res.Segments[1].Words))
	}
	return res, raw
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func isFalse(p *bool) bool { return p != nil && !*p }
func isTrue(p *bool) bool  { return p != nil && *p }

func TestCheckWordDriftFlagsAWordArrayThatLagsItsSegment(t *testing.T) {
	res, _ := loadDrifted(t)
	reliable, drift := checkWordDrift(res.Segments, wordDriftThresholdSec)
	if !isFalse(reliable) {
		t.Fatalf("a segment starting at 100.0 whose first word starts at 41.2: reliable = %v, want false", reliable)
	}
	// The worst segment is segment 1 (100.0 - 41.2); segment 2 lags by the smaller 58.5.
	if !near(drift, 58.8) {
		t.Errorf("drift = %v, want 58.8 (the largest |segment.start - words[0].start|, not the first or last segment's)", drift)
	}
}

func TestCheckWordDriftAcceptsAlignedWords(t *testing.T) {
	segs := []Segment{
		seg(0.0, word(0.0), word(0.4)),
		seg(10.0, word(10.1), word(10.5)), // 0.1 s of leading silence, normal
		seg(20.0, word(20.25), word(20.7)),
	}
	reliable, drift := checkWordDrift(segs, wordDriftThresholdSec)
	if !isTrue(reliable) {
		t.Fatalf("aligned words: reliable = %v, want true", reliable)
	}
	if !near(drift, 0.25) {
		t.Errorf("drift = %v, want 0.25 (the largest of 0, 0.1 and 0.25)", drift)
	}
}

func TestCheckWordDriftIsSilentWithoutWords(t *testing.T) {
	for name, segs := range map[string][]Segment{
		"no segments":          nil,
		"segments, nil words":  {{Start: 3, End: 4, Text: " a"}, {Start: 9, End: 10, Text: " b"}},
		"segments, zero words": {{Start: 3, End: 4, Text: " a", Words: []Word{}}},
	} {
		t.Run(name, func(t *testing.T) {
			reliable, drift := checkWordDrift(segs, wordDriftThresholdSec)
			if reliable != nil || drift != 0 {
				t.Errorf("a transcript with no words gets no verdict: got (%v, %v), want (nil, 0)", reliable, drift)
			}
		})
	}
}

// A segment that carries no words is not evidence of drift, and must not stop the others from
// being judged (the check indexes words[0]; an unguarded read would panic here).
func TestCheckWordDriftJudgesOnlyTheSegmentsThatCarryWords(t *testing.T) {
	segs := []Segment{
		{Start: 50, End: 51, Text: " no words here"},
		seg(60.0, word(60.0)),
	}
	reliable, drift := checkWordDrift(segs, wordDriftThresholdSec)
	if !isTrue(reliable) || drift != 0 {
		t.Errorf("one aligned segment and one without words: got (%v, %v), want (true, 0)", reliable, drift)
	}
	segs = append(segs, seg(70.0, word(30.0)))
	if reliable, _ = checkWordDrift(segs, wordDriftThresholdSec); !isFalse(reliable) {
		t.Errorf("a drifted segment behind a segment without words: reliable = %v, want false", reliable)
	}
}

// The boundary. Both numbers are exactly representable, so the difference is exactly 0.5 and
// the rule "at or above the threshold" is what decides, not rounding. (A drift of 58.8 cannot
// tell >= from >, which is why this case is its own test.)
func TestCheckWordDriftFlagsExactlyAtTheThreshold(t *testing.T) {
	at := []Segment{seg(100.0, word(99.5))}
	reliable, drift := checkWordDrift(at, wordDriftThresholdSec)
	if !isFalse(reliable) {
		t.Errorf("a drift of exactly the threshold (%v s): reliable = %v, want false", wordDriftThresholdSec, reliable)
	}
	if drift != 0.5 {
		t.Errorf("drift = %v, want exactly 0.5", drift)
	}
	below := []Segment{seg(100.0, word(99.6))}
	if reliable, _ = checkWordDrift(below, wordDriftThresholdSec); !isTrue(reliable) {
		t.Errorf("a drift just under the threshold (0.4 s): reliable = %v, want true", reliable)
	}
	if wordDriftThresholdSec != 0.5 {
		t.Errorf("wordDriftThresholdSec = %v, want the issue's 0.5", wordDriftThresholdSec)
	}
}

// Words that run AHEAD of their segment are as wrong as words that lag it.
func TestCheckWordDriftCountsWordsThatRunAheadOfTheirSegment(t *testing.T) {
	reliable, drift := checkWordDrift([]Segment{seg(10.0, word(15.0))}, wordDriftThresholdSec)
	if !isFalse(reliable) || !near(drift, 5.0) {
		t.Errorf("words 5 s ahead of the segment: got (%v, %v), want (false, 5)", reliable, drift)
	}
}

// The check reads the FIRST word of each segment, the issue's own formula. Pin it from both
// sides: a late word cannot condemn a segment whose first word is on time, and an early one
// cannot save it.
func TestCheckWordDriftReadsTheFirstWordOfEachSegment(t *testing.T) {
	lateWordOnly := []Segment{seg(10.0, word(10.0), word(90.0))}
	if reliable, _ := checkWordDrift(lateWordOnly, wordDriftThresholdSec); !isTrue(reliable) {
		t.Errorf("first word on time, a later word far away: reliable = %v, want true (the check is the first word's start)", reliable)
	}
	lateFirstWord := []Segment{seg(10.0, word(60.0), word(10.1))}
	if reliable, _ := checkWordDrift(lateFirstWord, wordDriftThresholdSec); !isFalse(reliable) {
		t.Errorf("first word far away, a later word on time: reliable = %v, want false", reliable)
	}
}

// Transcribe sets the fields from the upstream's answer: the drifted fixture publishes false,
// an aligned answer true, an answer with no words neither.
func TestTranscribePublishesTheWordDriftOfTheUpstreamsAnswer(t *testing.T) {
	_, drifted := loadDrifted(t)
	aligned, _ := json.Marshal(Result{Language: "english", Duration: 8, Text: " ok", Segments: []Segment{
		seg(0.0, word(0.05)),
		seg(4.0, word(4.2)),
	}})
	noWords := []byte(`{"language":"english","duration":4,"text":" ok","segments":[{"id":0,"start":0,"end":4,"text":" ok"}]}`)

	for _, c := range []struct {
		name      string
		body      []byte
		want      string // "false", "true" or "nil"
		wantDrift float64
	}{
		{"drifted fixture", drifted, "false", 58.8},
		{"aligned words", aligned, "true", 0.2},
		{"no words", noWords, "nil", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetClientState(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(c.body)
			}))
			defer srv.Close()
			wav := filepath.Join(t.TempDir(), "a.wav")
			_ = os.WriteFile(wav, []byte("RIFF"), 0o644)

			got, err := New(srv.URL, 10*time.Second).Transcribe(context.Background(), "whisper-stt", wav, DefaultParams())
			if err != nil {
				t.Fatal(err)
			}
			have := "nil"
			if got.WordsReliable != nil {
				have = map[bool]string{true: "true", false: "false"}[*got.WordsReliable]
			}
			if have != c.want {
				t.Errorf("WordsReliable = %s, want %s", have, c.want)
			}
			if !near(got.WordsDriftSec, c.wantDrift) {
				t.Errorf("WordsDriftSec = %v, want %v", got.WordsDriftSec, c.wantDrift)
			}
		})
	}
}

// The OpenAI-transcriptions path synthesizes one segment with no words, so it never gets a verdict.
func TestTranscribeOAIGivesNoWordVerdict(t *testing.T) {
	resetClientState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"text":"language English<asr_text>hello there"}`))
	}))
	defer srv.Close()
	wav := filepath.Join(t.TempDir(), "a.wav")
	_ = os.WriteFile(wav, []byte("RIFF"), 0o644)

	got, err := New(srv.URL, 10*time.Second).TranscribeOAI(context.Background(), "asr", wav)
	if err != nil {
		t.Fatal(err)
	}
	if got.WordsReliable != nil || got.WordsDriftSec != 0 {
		t.Errorf("the OAI path has no word times: got (%v, %v), want (nil, 0)", got.WordsReliable, got.WordsDriftSec)
	}
}

// A result without words marshals to the bytes it marshalled to before the flag existed; one with
// a verdict carries both keys, and a "false" verdict is not dropped as a zero value.
func TestResultJSONCarriesTheFlagOnlyWhenSet(t *testing.T) {
	plain := Result{Language: "en", Duration: 1.5, Text: "hi", Segments: []Segment{{ID: 0, Start: 0, End: 1.5, Text: "hi"}}}
	b, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	const before = `{"language":"en","duration":1.5,"text":"hi","segments":[{"id":0,"start":0,"end":1.5,"text":"hi","words":null,"avg_logprob":0,"no_speech_prob":0}]}`
	if string(b) != before {
		t.Errorf("a result without a verdict changed shape:\n got %s\nwant %s", b, before)
	}

	no := false
	flagged := plain
	flagged.WordsReliable, flagged.WordsDriftSec = &no, 58.8
	b, err = json.Marshal(flagged)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	if string(keys["words_reliable"]) != "false" || string(keys["words_drift_sec"]) != "58.8" {
		t.Errorf("a verdict of false must publish both keys: words_reliable=%s words_drift_sec=%s (in %s)", keys["words_reliable"], keys["words_drift_sec"], b)
	}

	yes := true
	ok := plain
	ok.WordsReliable = &yes
	b, _ = json.Marshal(ok)
	keys = nil
	_ = json.Unmarshal(b, &keys)
	if string(keys["words_reliable"]) != "true" {
		t.Errorf("a verdict of true must publish words_reliable: %s", b)
	}
	if _, has := keys["words_drift_sec"]; has {
		t.Errorf("a zero drift is omitted, not published as 0: %s", b)
	}
}
