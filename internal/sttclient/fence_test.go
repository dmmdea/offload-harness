package sttclient

// fence_test.go pins the whisper passthrough behind the GPU-lease fence
// (2026-09-22): /upstream/<whisper>/inference STARTS the whisper model when it
// is not loaded, so a transcription under a render must wait for the card, not
// load onto it.
//
// How long it waits is register C-89 (2026-10-01). The wait used to be the client's
// own timeout, which for a transcription is stt_request_timeout_sec (1,800 s, because
// long audio decodes at 5-8x realtime) and is also where the MCP client aborts an idle
// call, so the two raced. The wait is now its own budget, WithFenceWait, that the
// pipeline sets from gpu_wait_ms like every other GPU door; the client's timeout only
// caps it.

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

// heldCard arms the lease gate at a temp directory and takes a media lease on it, so
// every request for a model that is not resident is fenced until the test ends.
func heldCard(t *testing.T) {
	t.Helper()
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
	t.Cleanup(func() { _ = l.Release() })
}

// fencedWhisper is a llama-swap whose whisper model is NOT resident, counting what
// reaches /upstream: under a fence nothing may.
func fencedWhisper(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
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
	t.Cleanup(srv.Close)
	return srv, &upstream
}

func fenceTestWav(t *testing.T) string {
	t.Helper()
	wav := filepath.Join(t.TempDir(), "a.wav")
	if err := os.WriteFile(wav, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	return wav
}

// bothDoors transcribes through both entry points and returns how long each refusal took.
func bothDoors(t *testing.T, c *Client, wav string) (whisper, oai time.Duration) {
	t.Helper()
	start := time.Now()
	if _, err := c.Transcribe(context.Background(), "whisper", wav, DefaultParams()); !modelaffinity.IsLeaseRefusal(err) {
		t.Fatalf("Transcribe = %v, want the fence's lease refusal", err)
	}
	whisper = time.Since(start)
	start = time.Now()
	if _, err := c.TranscribeOAI(context.Background(), "whisper", wav); !modelaffinity.IsLeaseRefusal(err) {
		t.Fatalf("TranscribeOAI = %v, want the fence's lease refusal", err)
	}
	return whisper, time.Since(start)
}

func TestTranscribeUnderAFenceNeverReachesUpstream(t *testing.T) {
	resetClientState(t)
	heldCard(t)
	srv, upstream := fencedWhisper(t)
	wav := fenceTestWav(t)

	// The client's own timeout is long; the fence wait is what bounds the refusal.
	c := New(srv.URL, 2*time.Second).WithFenceWait(300 * time.Millisecond)
	whisper, oai := bothDoors(t, c, wav)
	for name, el := range map[string]time.Duration{"Transcribe": whisper, "TranscribeOAI": oai} {
		if el < 250*time.Millisecond {
			t.Fatalf("%s refused after %s: a held card is waited for inside the fence-wait budget", name, el)
		}
		if el > 1500*time.Millisecond {
			t.Fatalf("%s refused after %s: it waited out the client's 2s timeout instead of the 300ms fence wait (register C-89)", name, el)
		}
	}
	if got := upstream.Load(); got != 0 {
		t.Fatalf("%d transcription request(s) reached /upstream under a media lease — each one loads whisper", got)
	}
}

// The client's own timeout still caps the wait: a fence wait longer than the HTTP
// timeout cannot outlast the request it is part of.
func TestAFenceWaitLongerThanTheClientTimeoutIsCappedByIt(t *testing.T) {
	resetClientState(t)
	heldCard(t)
	srv, upstream := fencedWhisper(t)

	c := New(srv.URL, 300*time.Millisecond).WithFenceWait(5 * time.Second)
	whisper, oai := bothDoors(t, c, fenceTestWav(t))
	for name, el := range map[string]time.Duration{"Transcribe": whisper, "TranscribeOAI": oai} {
		if el < 250*time.Millisecond || el > 3*time.Second {
			t.Fatalf("%s refused after %s, want about the client's 300ms timeout", name, el)
		}
	}
	if got := upstream.Load(); got != 0 {
		t.Fatalf("%d request(s) reached /upstream under a media lease", got)
	}
}

// gpu_wait_ms: 0 means "a single try" for every GPU door, so a fence wait of ZERO is one
// inspection, never "unset" — an unset client would silently go back to waiting out the
// HTTP timeout on a box configured not to wait.
func TestAFenceWaitOfZeroIsASingleInspection(t *testing.T) {
	resetClientState(t)
	heldCard(t)
	srv, upstream := fencedWhisper(t)

	c := New(srv.URL, 2*time.Second).WithFenceWait(0)
	whisper, oai := bothDoors(t, c, fenceTestWav(t))
	for name, el := range map[string]time.Duration{"Transcribe": whisper, "TranscribeOAI": oai} {
		if el > time.Second {
			t.Fatalf("%s refused after %s with a fence wait of zero, want one inspection", name, el)
		}
	}
	if got := upstream.Load(); got != 0 {
		t.Fatalf("%d request(s) reached /upstream under a media lease", got)
	}
}
