package sttclient

// fence_test.go pins the whisper passthrough behind the GPU-lease fence
// (2026-09-22): /upstream/<whisper>/inference STARTS the whisper model when it
// is not loaded, so a transcription under a render must wait for the card, not
// load onto it.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

func TestTranscribeUnderAFenceNeverReachesUpstream(t *testing.T) {
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(modelaffinity.DisarmGPULease)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "video render", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	var upstream atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"whisper"}]}`))
		case r.URL.Path == "/running":
			_, _ = w.Write([]byte(`{"running":[]}`))
		case strings.HasPrefix(r.URL.Path, "/upstream/"):
			upstream.Add(1)
			_, _ = w.Write([]byte(`{"text":"hi","segments":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	wav := filepath.Join(t.TempDir(), "a.wav")
	if err := os.WriteFile(wav, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}

	c := New(srv.URL, 300*time.Millisecond)
	start := time.Now()
	_, terr := c.Transcribe(context.Background(), "whisper", wav, DefaultParams())
	if !modelaffinity.IsLeaseRefusal(terr) {
		t.Fatalf("Transcribe = %v, want the fence's lease refusal", terr)
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("refused after %s: a held card is waited for inside the client's budget", el)
	}
	if _, oerr := c.TranscribeOAI(context.Background(), "whisper", wav); !modelaffinity.IsLeaseRefusal(oerr) {
		t.Fatalf("TranscribeOAI = %v, want the fence's lease refusal", oerr)
	}
	if got := upstream.Load(); got != 0 {
		t.Fatalf("%d transcription request(s) reached /upstream under a media lease — each one loads whisper", got)
	}
}
