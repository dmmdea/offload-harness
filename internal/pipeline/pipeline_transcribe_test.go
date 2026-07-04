package pipeline

import (
	"context"
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
	a := mediaBase("/m", "/a/recording.m4a", "/a/recording.m4a|sz=1|mt=2|model=whisper-stt|lang=es")
	b := mediaBase("/m", "/b/recording.m4a", "/b/recording.m4a|sz=9|mt=3|model=whisper-stt|lang=es")
	if a == b {
		t.Fatalf("distinct sources with same basename collided: %q", a)
	}
	// Same identity -> stable stem (idempotent overwrite of its own files).
	if again := mediaBase("/m", "/a/recording.m4a", "/a/recording.m4a|sz=1|mt=2|model=whisper-stt|lang=es"); again != a {
		t.Errorf("same ident must yield a stable stem: %q != %q", again, a)
	}
	// Human-readable basename retained.
	if !strings.Contains(a, "recording-") {
		t.Errorf("stem should keep the basename: %q", a)
	}
}
