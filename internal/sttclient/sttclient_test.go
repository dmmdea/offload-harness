package sttclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTranscribeSerializesConcurrentCalls guards the single-slot invariant: overlapping
// inference requests crash whisper-server, so the client must serialize them. The fake
// server records the peak number of in-flight requests; with the mutex it must be 1.
func TestTranscribeSerializesConcurrentCalls(t *testing.T) {
	var inflight, maxSeen int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inflight, 1)
		for {
			m := atomic.LoadInt32(&maxSeen)
			if n <= m || atomic.CompareAndSwapInt32(&maxSeen, m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond) // hold the slot so a concurrent call would overlap
		atomic.AddInt32(&inflight, -1)
		_, _ = w.Write([]byte(`{"text":"ok","segments":[{"id":0,"start":0,"end":1,"text":"ok"}]}`))
	}))
	defer srv.Close()

	tmp := t.TempDir()
	wav := filepath.Join(tmp, "a.wav")
	_ = os.WriteFile(wav, []byte("RIFF"), 0o644)
	c := New(srv.URL, 10*time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Transcribe(context.Background(), "whisper-stt", wav, DefaultParams())
		}()
	}
	wg.Wait()
	if m := atomic.LoadInt32(&maxSeen); m != 1 {
		t.Fatalf("peak concurrent inference requests = %d, want 1 — the single-slot server must be serialized", m)
	}
}

// TestTranscribeEmptyBody502IsDescriptive guards that a crash-signature 5xx (empty
// body) surfaces an accurate, diagnostic error rather than a bare status code.
func TestTranscribeEmptyBody502IsDescriptive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // 502, empty body — the whisper crash signature
	}))
	defer srv.Close()
	tmp := t.TempDir()
	wav := filepath.Join(tmp, "a.wav")
	_ = os.WriteFile(wav, []byte("RIFF"), 0o644)
	c := New(srv.URL, 10*time.Second)

	_, err := c.Transcribe(context.Background(), "whisper-stt", wav, DefaultParams())
	if err == nil {
		t.Fatal("expected an error on empty-body 502")
	}
	if !strings.Contains(err.Error(), "crashed") || !strings.Contains(err.Error(), "empty body") {
		t.Errorf("empty-body 502 error should be descriptive (crash / no-speech hint); got: %v", err)
	}
}

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

// --- OpenAI-transcriptions path (llama-server mtmd STT, e.g. Qwen3-ASR) ---
// The HQ accuracy tier can be served by llama-server (mtmd) instead of whisper-server;
// llama-server exposes the OpenAI /v1/audio/transcriptions shape (multipart `file`),
// reachable through llama-swap's /upstream/<model>/ passthrough. Verified live on Qube
// 2026-07-22: HTTP 200, {"type":"transcript.text.done","text":"language English<asr_text>..."}.

func TestParseASRText(t *testing.T) {
	lang, text := ParseASRText("language English<asr_text>The quick brown fox.")
	if lang != "english" || text != "The quick brown fox." {
		t.Fatalf("marker form: got lang=%q text=%q", lang, text)
	}
	lang, text = ParseASRText("  plain transcript with no marker ")
	if lang != "" || text != "plain transcript with no marker" {
		t.Fatalf("no-marker form: got lang=%q text=%q", lang, text)
	}
	lang, text = ParseASRText("")
	if lang != "" || text != "" {
		t.Fatalf("empty: got lang=%q text=%q", lang, text)
	}
}

func TestTranscribeOAIPostsMultipartAndParses(t *testing.T) {
	var gotPath, gotCT string
	var sawFile bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			_, _, ferr := r.FormFile("file")
			sawFile = ferr == nil
		}
		_, _ = w.Write([]byte(`{"type":"transcript.text.done","text":"language English<asr_text>Hello world.","usage":{"total_tokens":10}}`))
	}))
	defer srv.Close()

	tmp := t.TempDir()
	wav := filepath.Join(tmp, "a.wav")
	// ffmpeg-shaped RIFF: fmt + LIST/INFO (the real producer writes one — review-verified
	// 78-byte header) + a data chunk of exactly 1.0s of 16kHz mono s16. Duration must come
	// from the DATA CHUNK size, not a naive size-44.
	_ = os.WriteFile(wav, buildRIFF(t, 32000), 0o644)

	c := New(srv.URL, 10*time.Second)
	r, err := c.TranscribeOAI(context.Background(), "qwen3-asr", wav)
	if err != nil {
		t.Fatalf("TranscribeOAI: %v", err)
	}
	if gotPath != "/upstream/qwen3-asr/v1/audio/transcriptions" {
		t.Fatalf("posted to %q", gotPath)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") || !sawFile {
		t.Fatalf("must upload multipart with a `file` field (ct=%q sawFile=%v)", gotCT, sawFile)
	}
	if r.Language != "english" || r.Text != "Hello world." {
		t.Fatalf("parse: lang=%q text=%q", r.Language, r.Text)
	}
	if r.Duration < 0.99 || r.Duration > 1.01 {
		t.Fatalf("duration from wav size: got %v want ~1.0", r.Duration)
	}
	if len(r.Segments) != 1 || r.Segments[0].Text != "Hello world." || r.Segments[0].End != r.Duration {
		t.Fatalf("must synthesize ONE full-span segment (SRT/consumers rely on segments); got %+v", r.Segments)
	}
}

func TestTranscribeOAIErrorsAreErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"File Not Found"}}`, 404)
	}))
	defer srv.Close()
	tmp := t.TempDir()
	wav := filepath.Join(tmp, "a.wav")
	_ = os.WriteFile(wav, make([]byte, 100), 0o644)
	c := New(srv.URL, 10*time.Second)
	if _, err := c.TranscribeOAI(context.Background(), "m", wav); err == nil {
		t.Fatal("non-200 must be an error, not an empty result")
	}
}

// buildRIFF fabricates a WAV like the real ConvertToWav16k producer: RIFF/WAVE with a
// fmt chunk, a LIST/INFO chunk (ffmpeg writes one — the naive 44-byte-header assumption
// broke on it), then a data chunk of dataLen zero bytes.
func buildRIFF(t *testing.T, dataLen int) []byte {
	t.Helper()
	le32 := func(v int) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	var b []byte
	fmtChunk := append([]byte("fmt "), le32(16)...)
	fmtChunk = append(fmtChunk, make([]byte, 16)...)
	info := []byte("INFOISFT")
	info = append(info, le32(13)...)
	info = append(info, []byte("Lavf62.12.102")...) // 13 bytes (ffmpeg pads to even with a NUL; the walk only reads top-level chunks)
	list := append([]byte("LIST"), le32(len(info))...)
	list = append(list, info...)
	data := append([]byte("data"), le32(dataLen)...)
	data = append(data, make([]byte, dataLen)...)
	body := append(append(append([]byte("WAVE"), fmtChunk...), list...), data...)
	b = append([]byte("RIFF"), le32(len(body))...)
	return append(b, body...)
}

// Guarded prefix parsing (review finding: an over-eager parse dropped leading content).
func TestParseASRTextGuards(t *testing.T) {
	// A transcript that legitimately CONTAINS the marker must not lose its lead.
	lang, text := ParseASRText("The model emits <asr_text>as a delimiter.")
	if lang != "" || text != "The model emits <asr_text>as a delimiter." {
		t.Fatalf("non-language prefix must be kept verbatim: lang=%q text=%q", lang, text)
	}
	// Case-insensitive language keyword.
	lang, text = ParseASRText("Language Spanish<asr_text>Hola mundo.")
	if lang != "spanish" || text != "Hola mundo." {
		t.Fatalf("case-insensitive prefix: lang=%q text=%q", lang, text)
	}
	// A second marker later in the transcript stays verbatim.
	lang, text = ParseASRText("language English<asr_text>one <asr_text> two")
	if lang != "english" || text != "one <asr_text> two" {
		t.Fatalf("later markers stay: lang=%q text=%q", lang, text)
	}
}
