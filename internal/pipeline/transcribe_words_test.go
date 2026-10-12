package pipeline

// transcribe_words_test.go pins D-38 #169 where a caller can see it: the word-time verdict that
// sttclient.Transcribe computes (Result.WordsReliable / WordsDriftSec) has to reach the
// offload_transcribe payload, the cache entry behind it, and the log. Until this file the verdict
// died at the out literal below the Transcribe call: the payload struct had no field for it.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/sttclient"
)

// A verbose_json answer whose second segment starts at 100.0 s but whose first word starts at
// 41.2 s: 58.8 s of drift, the failure issue #169 reports.
const driftedWhisperAnswer = `{"language":"english","duration":104.0,"text":" Welcome. First we cover the schedule.","segments":[` +
	`{"id":0,"start":0.0,"end":3.2,"text":" Welcome.","words":[{"word":" Welcome.","start":0.0,"end":3.2,"probability":0.97}]},` +
	`{"id":1,"start":100.0,"end":104.0,"text":" First we cover the schedule.","words":[{"word":" First","start":41.2,"end":41.7,"probability":0.96},{"word":" we","start":41.7,"end":41.9,"probability":0.99}]}]}`

// The same transcript with its word times where the segments are (0.2 s of leading silence).
const alignedWhisperAnswer = `{"language":"english","duration":104.0,"text":" Welcome. First we cover the schedule.","segments":[` +
	`{"id":0,"start":0.0,"end":3.2,"text":" Welcome.","words":[{"word":" Welcome.","start":0.0,"end":3.2,"probability":0.97}]},` +
	`{"id":1,"start":100.0,"end":104.0,"text":" First we cover the schedule.","words":[{"word":" First","start":100.2,"end":100.7,"probability":0.96},{"word":" we","start":100.7,"end":100.9,"probability":0.99}]}]}`

// No word array at all, as a model that sends none answers.
const noWordsWhisperAnswer = `{"language":"english","duration":4.0,"text":" Welcome.","segments":[{"id":0,"start":0.0,"end":4.0,"text":" Welcome."}]}`

// fakeWhisperAnswering stands in for whisper-server and answers every request with body.
func fakeWhisperAnswering(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// transcribeWords runs one real Run of the transcribe task against a fake whisper that answers
// body, returning the result, its payload as a generic map and the pipeline (for a second call).
func transcribeWords(t *testing.T, body string) (core.Result, map[string]json.RawMessage, func() core.Result) {
	t.Helper()
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH; this test needs a real convert to reach the STT call")
	}
	wav := makeSilentWav(t, ffmpeg)
	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	cfg.Endpoint = fakeWhisperAnswering(t, body)
	p := gatePipeline(t, cfg, gateCache(t))
	run := func() core.Result {
		return p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav})
	}
	res := run()
	if !res.OK {
		t.Fatalf("transcribe deferred: %s", res.Reason)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(res.Data, &data); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	return res, data, run
}

// rawOr renders a payload value, or says the key is absent (a nil RawMessage prints as null).
func rawOr(data map[string]json.RawMessage, key string) string {
	if v, ok := data[key]; ok {
		return string(v)
	}
	return "<absent>"
}

func payloadKeys(data map[string]json.RawMessage) []string {
	var keys []string
	for k := range data {
		keys = append(keys, k)
	}
	return keys
}

// The drifted answer: the payload says words_reliable:false and by how much, the cache entry
// behind it answers the same, the log names the symptom, and the on-disk segment file stays the
// bare array its readers (sttremote's fetch of a truncated list, dataset tooling) parse.
func TestTranscribePublishesTheWordDriftVerdictOfADriftedAnswer(t *testing.T) {
	logged := captureLog(t)
	_, data, run := transcribeWords(t, driftedWhisperAnswer)

	if string(data["words_reliable"]) != "false" {
		t.Fatalf("words_reliable = %s in the offload_transcribe payload, want false (keys: %v)", rawOr(data, "words_reliable"), payloadKeys(data))
	}
	var drift float64
	if err := json.Unmarshal(data["words_drift_sec"], &drift); err != nil || math.Abs(drift-58.8) > 1e-6 {
		t.Errorf("words_drift_sec = %s (%v), want 58.8", rawOr(data, "words_drift_sec"), err)
	}
	if !strings.Contains(logged.String(), "words_reliable=false") || !strings.Contains(logged.String(), "#169") {
		t.Errorf("the drift verdict reached no log line; log = %q", logged.String())
	}

	// The inline segments still carry the words the verdict is about.
	var segs []sttclient.Segment
	if err := json.Unmarshal(data["segments"], &segs); err != nil || len(segs) != 2 || len(segs[1].Words) != 2 {
		t.Errorf("inline segments = %s (err %v), want the 2 segments with their words", data["segments"], err)
	}

	// The segment file is a bare array: wrapping it would break every reader of json_path.
	var jsonPath string
	_ = json.Unmarshal(data["json_path"], &jsonPath)
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("segment file: %v", err)
	}
	var onDisk []sttclient.Segment
	if err := json.Unmarshal(raw, &onDisk); err != nil || len(onDisk) != 2 {
		t.Errorf(".segments.json must stay a bare segment array, got %s (err %v)", raw, err)
	}

	// A second call is served from the cache, and the entry carries the verdict.
	hit := run()
	if !hit.OK || !hit.Meta.CacheHit {
		t.Fatalf("second call: OK=%v CacheHit=%v, want a cache hit", hit.OK, hit.Meta.CacheHit)
	}
	var again map[string]json.RawMessage
	if err := json.Unmarshal(hit.Data, &again); err != nil {
		t.Fatal(err)
	}
	if string(again["words_reliable"]) != "false" || string(again["words_drift_sec"]) != string(data["words_drift_sec"]) {
		t.Errorf("the cached answer lost the verdict: words_reliable=%s words_drift_sec=%s", rawOr(again, "words_reliable"), rawOr(again, "words_drift_sec"))
	}
}

// The aligned answer publishes true (and the small drift behind it) and logs nothing about drift.
func TestTranscribePublishesWordsReliableTrueForAlignedWords(t *testing.T) {
	logged := captureLog(t)
	_, data, _ := transcribeWords(t, alignedWhisperAnswer)

	if string(data["words_reliable"]) != "true" {
		t.Fatalf("words_reliable = %s, want true (keys: %v)", rawOr(data, "words_reliable"), payloadKeys(data))
	}
	var drift float64
	if err := json.Unmarshal(data["words_drift_sec"], &drift); err != nil || math.Abs(drift-0.2) > 1e-6 {
		t.Errorf("words_drift_sec = %s (%v), want 0.2", rawOr(data, "words_drift_sec"), err)
	}
	if strings.Contains(logged.String(), "words_reliable=false") {
		t.Errorf("an aligned answer logged a drift warning: %q", logged.String())
	}
}

// An answer without words gets no verdict: neither key appears, so the payload is byte for byte
// what it was before the check existed.
func TestTranscribeWithoutWordsPublishesNoVerdict(t *testing.T) {
	_, data, _ := transcribeWords(t, noWordsWhisperAnswer)
	for _, k := range []string{"words_reliable", "words_drift_sec"} {
		if v, has := data[k]; has {
			t.Errorf("%s = %s in the payload of an answer with no words, want the key absent", k, v)
		}
	}
}
