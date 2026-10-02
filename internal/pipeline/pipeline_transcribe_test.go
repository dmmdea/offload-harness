package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/sttclient"
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

// TestTranscribeACrashOfACallThatRanAloneDefersAsNoSpeech (the F-35 case, 2026-09-23;
// narrowed by register C-91, whose first cut read every empty-body 5xx as a vanished upstream,
// and put back in its review for the case that can be told apart). whisper.cpp exits on audio
// with no speech content, and llama-swap answers that with a bare 5xx. A call that ran alone,
// with no unload sent by this process, has nothing of its own to blame, so it defers calmly as
// no speech: a retry would only crash the server again and cost the restart.
func TestTranscribeACrashOfACallThatRanAloneDefersAsNoSpeech(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH; this test needs a real convert to reach the STT call")
	}
	wav := makeSilentWav(t, ffmpeg)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // 502, empty body: the crash signature
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
		t.Errorf("reason = %q, want the calm no-speech defer for the F-35 crash of a call that ran alone", res.Reason)
	}
}

// TestTranscribeCallsThatLoseTheUpstreamTogetherFailInsteadOfClaimingNoSpeech (register C-91):
// two calls overlap and the upstream answers both with the same bare 5xx. Either may have been
// hurt by the other (an unload, a swap, a restart), so neither is told its audio was silent:
// the defer says the call failed and the upstream vanished, which a caller can retry.
func TestTranscribeCallsThatLoseTheUpstreamTogetherFailInsteadOfClaimingNoSpeech(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH; this test needs a real convert to reach the STT call")
	}
	isolateSwapKeepSet(t)
	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	fake := newSwapStandIn(t, cfg.STTModel, time.Millisecond)
	fake.inferStatus = http.StatusBadGateway
	cfg.Endpoint = fake.srv.URL
	p := gatePipeline(t, cfg, nil)

	var wg sync.WaitGroup
	var a, b core.Result
	run := func(out *core.Result) {
		wav := makeSilentWav(t, ffmpeg)
		wg.Add(1)
		go func() {
			defer wg.Done()
			*out = p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav})
		}()
	}
	run(&a)
	select {
	case <-fake.firstN:
	case <-time.After(30 * time.Second):
		close(fake.hold)
		t.Fatal("call A never reached the upstream")
	}
	run(&b)
	for deadline := time.Now().Add(30 * time.Second); sttclient.Pending() < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			close(fake.hold)
			wg.Wait()
			t.Fatalf("call B never got in line behind A (pending = %d)", sttclient.Pending())
		}
	}
	close(fake.hold)
	wg.Wait()

	for name, res := range map[string]core.Result{"A": a, "B": b} {
		if res.OK || !res.Deferred {
			t.Errorf("call %s: want a deferred failure, got OK=%v Deferred=%v", name, res.OK, res.Deferred)
			continue
		}
		if !strings.HasPrefix(res.Reason, "transcribe call failed:") || !strings.Contains(res.Reason, "vanished") {
			t.Errorf("call %s: reason = %q, want the call-failed defer that says the upstream vanished", name, res.Reason)
		}
		if strings.Contains(res.Reason, "(no speech detected)") {
			t.Errorf("call %s: reason claims the audio had no speech although another call overlapped it: %q", name, res.Reason)
		}
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
