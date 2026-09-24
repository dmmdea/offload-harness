package pipeline

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// findEnv returns the value of key=... in env, or ("", false).
func findEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix), true
		}
	}
	return "", false
}

// TestGenEnv_FFmpegPath is the regression test for F-38: config.Default()'s
// FFmpegPath is the bare name "ffmpeg" (a box that never set ffmpeg_path), and
// the old genEnv() only checked `!= ""` — which a bare name always satisfies —
// so it always threaded FFMPEG_PATH=ffmpeg into every GPU-gen child, including
// on boxes where nothing on PATH resolves that name. render/audio-qa.mjs's
// resolveFfmpeg() treats a set FFMPEG_PATH as an exact file path (existsSync),
// which cannot see PATH resolution, so a bare "ffmpeg" always read as missing —
// the music route's over-render/trim/dead-air QA gate silently skipped itself
// on every fleet node that had not set an explicit ffmpeg_path (reproduced on
// the Lenovo and the Aorus, 2026-09-23/24).
func TestGenEnv_FFmpegPath(t *testing.T) {
	t.Run("default config: bare name resolvable on PATH resolves to an absolute path, never the bare name", func(t *testing.T) {
		name := "go"
		if runtime.GOOS == "windows" {
			name = "go.exe"
		}
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("no %s on PATH in this environment", name)
		}
		cfg := config.Default()
		cfg.FFmpegPath = "go" // stands in for the shipped bare "ffmpeg" default
		p := &Pipeline{cfg: cfg}
		env := p.genEnv()
		got, ok := findEnv(env, "FFMPEG_PATH")
		if !ok {
			t.Fatal("FFMPEG_PATH must be set when the bare name resolves on PATH")
		}
		if got == "go" {
			t.Fatal("FFMPEG_PATH must be the PATH-resolved absolute path, not the bare name (F-38: a bare name defeats audio-qa.mjs's existsSync check)")
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("FFMPEG_PATH = %q, want an absolute resolved path", got)
		}
	})

	t.Run("bare name that resolves nowhere: FFMPEG_PATH is omitted, never sent as a broken bare name", func(t *testing.T) {
		cfg := config.Default()
		cfg.FFmpegPath = "definitely-not-a-real-binary-genenv-f38"
		p := &Pipeline{cfg: cfg}
		env := p.genEnv()
		if got, ok := findEnv(env, "FFMPEG_PATH"); ok {
			t.Fatalf("FFMPEG_PATH must be omitted when unresolvable, got %q", got)
		}
	})

	t.Run("explicit configured path: threaded through unchanged when it resolves", func(t *testing.T) {
		dir := t.TempDir()
		name := "ffmpeg"
		if runtime.GOOS == "windows" {
			name = "ffmpeg.exe"
		}
		abs := filepath.Join(dir, name)
		if err := os.WriteFile(abs, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.FFmpegPath = abs
		p := &Pipeline{cfg: cfg}
		env := p.genEnv()
		if got, ok := findEnv(env, "FFMPEG_PATH"); !ok || got != abs {
			t.Fatalf("FFMPEG_PATH = (%q, %v), want (%q, true)", got, ok, abs)
		}
	})

	t.Run("unset ffmpeg_path (empty string): FFMPEG_PATH is omitted", func(t *testing.T) {
		cfg := config.Default()
		cfg.FFmpegPath = ""
		p := &Pipeline{cfg: cfg}
		env := p.genEnv()
		if got, ok := findEnv(env, "FFMPEG_PATH"); ok {
			t.Fatalf("FFMPEG_PATH must be omitted for an unset binding, got %q", got)
		}
	})
}
