package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// No STTModel configured -> transcribe defers without converting/calling.
func TestTranscribeNoModelDefers(t *testing.T) {
	cfg := config.Default()
	cfg.STTModel = ""
	p := New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), nil, nil)
	res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: "x.mp3"})
	if res.OK || !res.Deferred {
		t.Fatalf("want deferred, got OK=%v Deferred=%v reason=%q", res.OK, res.Deferred, res.Reason)
	}
}

// A bad audio path -> ffmpeg convert fails -> defer (no model call).
func TestTranscribeBadAudioDefers(t *testing.T) {
	cfg := config.Default() // STTModel defaults set, but conversion fails first
	p := New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), nil, nil)
	res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: "no-such-file.mp3"})
	if res.OK || !res.Deferred {
		t.Fatalf("want deferred on bad audio, got OK=%v Deferred=%v", res.OK, res.Deferred)
	}
}

// TestTranscribeVanishedUpstreamIsAFailureNotNoSpeech (register C-91; this test pinned
// the opposite from F-35, 2026-09-23, until 2026-10-01). An empty-body 5xx is the bare
// answer of an upstream that vanished mid-request, and a model unloaded from under a
// call looks exactly like whisper-server's exit on audio with no speech content. The F-35
// mapping turned both into the calm "empty transcript (no speech detected)" defer, so a
// recording with speech in it was reported to its caller as silent. Now the defer says the
// call failed — a caller can retry it — and no-speech is reserved for an upstream that
// answered (the next test).
func TestTranscribeVanishedUpstreamIsAFailureNotNoSpeech(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH; this test needs a real convert to reach the STT call")
	}
	wav := makeSilentWav(t, ffmpeg)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // 502, empty body: the upstream vanished
	}))
	defer srv.Close()

	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	cfg.Endpoint = srv.URL
	p := gatePipeline(t, cfg, gateCache(t))

	res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav})
	if res.OK {
		t.Fatalf("want a deferred failure, got OK=true with data=%s", res.Data)
	}
	if !res.Deferred {
		t.Fatal("want Deferred=true on an upstream that vanished")
	}
	if !strings.HasPrefix(res.Reason, "transcribe call failed:") || !strings.Contains(res.Reason, "vanished") {
		t.Errorf("reason = %q, want the call-failed defer that says the upstream vanished", res.Reason)
	}
	if strings.Contains(res.Reason, "(no speech detected)") {
		t.Errorf("reason claims the audio had no speech although nothing answered: %q", res.Reason)
	}
}

// TestTranscribeEmptyAnswerDefersAsNoSpeech: the other half. An upstream that answered 200
// with an empty transcript has heard nothing, and that is the one thing reported as no
// speech: the same calm defer the F-35 fix introduced, with nothing for a caller to retry.
func TestTranscribeEmptyAnswerDefersAsNoSpeech(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH; this test needs a real convert to reach the STT call")
	}
	wav := makeSilentWav(t, ffmpeg)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"language":"en","duration":1,"text":"","segments":[]}`))
	}))
	defer srv.Close()

	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	cfg.Endpoint = srv.URL
	p := gatePipeline(t, cfg, gateCache(t))

	res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav})
	if res.OK || !res.Deferred {
		t.Fatalf("want a deferred no-speech result, got OK=%v Deferred=%v data=%s", res.OK, res.Deferred, res.Data)
	}
	if res.Reason != "empty transcript (no speech detected)" {
		t.Errorf("reason = %q, want the calm no-speech defer", res.Reason)
	}
}

// preview must be rune-safe: a byte-budget cut may land mid-rune on accented
// Spanish (á/ñ), which must never produce invalid UTF-8 in the gist.
func TestPreviewRuneSafe(t *testing.T) {
	s := strings.Repeat("ñáéíóú", 200) // multibyte, no spaces -> forces a byte cut mid-rune
	g := preview(s, 400)
	if !utf8.ValidString(g) {
		t.Errorf("preview produced invalid UTF-8: %q", g)
	}
	if !strings.HasSuffix(g, "…") {
		t.Errorf("expected ellipsis on truncation: %q", g)
	}
	// short input returns unchanged (no ellipsis).
	if got := preview("hola", 400); got != "hola" {
		t.Errorf("short preview = %q, want \"hola\"", got)
	}
}

// Distinct sources that share a basename must NOT collide on disk (the returned
// srt/txt/json pointers would otherwise reference a different audio's transcript).
func TestMediaBaseDisambiguates(t *testing.T) {
	// The ident strings mirror the CONTENT-ADDRESSED shape production now emits
	// (T2-A2): `media:sha256:sz=N:<hex>|model=..|lang=..|proto=..`. They were
	// previously hand-written in the pre-change path+size+mtime form, which passed
	// while exercising a format nothing produces — so the real key shape had no
	// coverage at all.
	identA := "media:sha256:sz=1:" + strings.Repeat("a", 64) + "|model=whisper-stt|lang=es|proto=whisper"
	identB := "media:sha256:sz=9:" + strings.Repeat("b", 64) + "|model=whisper-stt|lang=es|proto=whisper"
	a := mediaBase("/m", "/a/recording.m4a", identA)
	b := mediaBase("/m", "/b/recording.m4a", identB)
	if a == b {
		t.Fatalf("distinct sources with same basename collided: %q", a)
	}
	// Same identity -> stable stem (idempotent overwrite of its own files).
	if again := mediaBase("/m", "/a/recording.m4a", identA); again != a {
		t.Errorf("same ident must yield a stable stem: %q != %q", again, a)
	}
	// IDENTICAL CONTENT under the same filename in a different DIRECTORY now
	// resolves to the same stem — the reuse content-addressing exists to enable,
	// and the case the old path-keyed ident always split. Both runs write the same
	// .srt/.txt, which is correct: they describe the same bytes.
	same := mediaBase("/m", "/elsewhere/recording.m4a", identA)
	if same != a {
		t.Errorf("identical content+params in another directory should yield the same stem: %q vs %q", same, a)
	}
	// Human-readable basename retained.
	if !strings.Contains(a, "recording-") {
		t.Errorf("stem should keep the basename: %q", a)
	}
}

// TestSTTRoute pins the feature's actual switch (v0.22.15): the OpenAI-transcriptions
// protocol is selected ONLY for an hq request with a bound HQ model AND
// stt_hq_api="openai". Everything else keeps the whisper protocol — including the
// non-hq default tier even when the config carries the field.
func TestSTTRoute(t *testing.T) {
	cfg := config.Config{STTModel: "w", STTModelHQ: "q", STTHQAPI: "openai"}
	if m, oai := sttRoute(cfg, true); m != "q" || !oai {
		t.Fatalf("hq + openai: got (%q,%v)", m, oai)
	}
	if m, oai := sttRoute(cfg, false); m != "w" || oai {
		t.Fatalf("non-hq must never take the OAI branch: got (%q,%v)", m, oai)
	}
	cfg.STTHQAPI = ""
	if m, oai := sttRoute(cfg, true); m != "q" || oai {
		t.Fatalf("hq without the field keeps whisper: got (%q,%v)", m, oai)
	}
	cfg.STTHQAPI = "OpenAI" // case-insensitive
	if _, oai := sttRoute(cfg, true); !oai {
		t.Fatal("field must be case-insensitive")
	}
	cfg.STTModelHQ = ""
	if m, oai := sttRoute(cfg, true); m != "w" || oai {
		t.Fatalf("hq with no HQ model falls back to the default tier, whisper protocol: got (%q,%v)", m, oai)
	}
}
