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

// TestTranscribeNoSpeechCrashDefersCleanly guards the fix (F-35 regression follow-up,
// 2026-09-23): whisper-server's crash signature (empty-body 5xx) on audio with no
// speech content must surface as the SAME calm "empty transcript" defer the clean
// no-speech case already uses — never the alarming "transcribe call failed: ...
// upstream crashed" wording, and never OK:true with fabricated content. Mutation
// check: reverting the errors.Is branch in runTranscribe (pipeline.go) makes this
// fail because Reason reverts to the "transcribe call failed" text.
func TestTranscribeNoSpeechCrashDefersCleanly(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH; this test needs a real convert to reach the STT call")
	}
	wav := makeSilentWav(t, ffmpeg)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // 502, empty body — the whisper-server crash signature
	}))
	defer srv.Close()

	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	cfg.Endpoint = srv.URL
	p := gatePipeline(t, cfg, gateCache(t))

	res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav})
	if res.OK {
		t.Fatalf("want deferred (no speech found), got OK=true with data=%s", res.Data)
	}
	if !res.Deferred {
		t.Fatal("want Deferred=true on the crash-signature response")
	}
	if res.Reason != "empty transcript (no speech detected)" {
		t.Errorf("reason = %q, want the calm no-speech defer, not the alarming crash wording", res.Reason)
	}
	if strings.Contains(res.Reason, "call failed") || strings.Contains(res.Reason, "crashed") {
		t.Errorf("reason leaked the raw crash wording to the caller: %q", res.Reason)
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
