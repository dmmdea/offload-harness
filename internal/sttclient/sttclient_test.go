package sttclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSRT(t *testing.T) {
	got := SRT([]Segment{
		{ID: 0, Start: 0, End: 1.5, Text: " hello "},
		{ID: 1, Start: 1.5, End: 3661.25, Text: "world"},
	})
	want := "1\n00:00:00,000 --> 00:00:01,500\nhello\n\n2\n00:00:01,500 --> 01:01:01,250\nworld\n\n"
	if got != want {
		t.Errorf("SRT mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestDefaultParamsTuned(t *testing.T) {
	p := DefaultParams()
	if p.BeamSize != 5 || p.BestOf != 5 || p.MaxContext != 64 {
		t.Errorf("decode profile drifted: %+v", p)
	}
	if p.EntropyThold != 2.8 || !p.VAD || !p.SplitOnWord || p.MaxLen != 60 {
		t.Errorf("decode profile drifted: %+v", p)
	}
	if p.ResponseFormat() != "verbose_json" {
		t.Errorf("must request verbose_json, got %q", p.ResponseFormat())
	}
}

// An empty Language must be sent as "auto" (omitting it falls back to the
// server's -l default of "en", which would mis-transcribe Spanish).
func TestBuildMultipartAutoLanguage(t *testing.T) {
	buf, _, err := buildMultipart([]byte("RIFF"), "a.wav", DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "auto") {
		t.Errorf("empty Language should be sent as auto; body:\n%s", buf.String())
	}
}

func TestTranscribeParsesVerboseJSON(t *testing.T) {
	tmp := t.TempDir()
	wav := filepath.Join(tmp, "a.wav")
	if err := os.WriteFile(wav, []byte("RIFFfake-wav-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotPath, gotCT, gotFields string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		_ = r.ParseMultipartForm(1 << 20)
		gotFields = r.FormValue("response_format") + "|" + r.FormValue("language") + "|" + r.FormValue("beam_size")
		f, _, ferr := r.FormFile("file")
		if ferr != nil {
			t.Errorf("missing file field: %v", ferr)
		} else {
			b, _ := io.ReadAll(f)
			if !strings.HasPrefix(string(b), "RIFF") {
				t.Errorf("file field not forwarded verbatim: %q", b)
			}
		}
		_, _ = w.Write([]byte(`{"language":"spanish","duration":3.2,"text":"hola mundo","segments":[{"id":0,"start":0.0,"end":1.6,"text":"hola"},{"id":1,"start":1.6,"end":3.2,"text":"mundo"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, 10*time.Second)
	p := DefaultParams()
	p.Language = "es"
	res, err := c.Transcribe(context.Background(), "whisper-stt", wav, p)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if gotPath != "/upstream/whisper-stt/inference" {
		t.Errorf("path = %q, want /upstream/whisper-stt/inference", gotPath)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Errorf("content-type = %q, want multipart/form-data", gotCT)
	}
	if gotFields != "verbose_json|es|5" {
		t.Errorf("form fields = %q, want verbose_json|es|5", gotFields)
	}
	if res.Language != "spanish" || res.Duration != 3.2 || res.Text != "hola mundo" {
		t.Errorf("parsed top-level wrong: %+v", res)
	}
	if len(res.Segments) != 2 || res.Segments[1].Text != "mundo" || res.Segments[1].End != 3.2 {
		t.Errorf("parsed segments wrong: %+v", res.Segments)
	}
}

func TestTranscribeHTTPErrorIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model loading", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	tmp := t.TempDir()
	wav := filepath.Join(tmp, "a.wav")
	_ = os.WriteFile(wav, []byte("RIFF"), 0o644)
	c := New(srv.URL, 10*time.Second)
	if _, err := c.Transcribe(context.Background(), "whisper-stt", wav, DefaultParams()); err == nil {
		t.Fatal("expected an error on 503")
	}
}

func TestUnloadURL(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
	}))
	defer srv.Close()
	c := New(srv.URL, 5*time.Second)
	_ = c.Unload(context.Background(), "whisper-stt")
	if gotMethod != http.MethodPost || gotPath != "/api/models/unload/whisper-stt" {
		t.Errorf("Unload hit %s %s, want POST /api/models/unload/whisper-stt", gotMethod, gotPath)
	}
}
