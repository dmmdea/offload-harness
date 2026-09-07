package ttsclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeWAV(n int) []byte {
	b := make([]byte, n)
	copy(b, "RIFF")
	return b
}

func TestSpeakPostsTheOpenAIShapeAndWritesTheBody(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" || r.Method != http.MethodPost {
			http.Error(w, "wrong route", 404)
			return
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(fakeWAV(2000))
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "sub", "a.wav")
	res, err := Speak(context.Background(), Request{Base: srv.URL + "/", APIKey: "k", Text: "hola", Language: "es", Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != DefaultModel || got["voice"] != DefaultVoice || got["input"] != "hola" || got["response_format"] != "wav" || got["language"] != "es" {
		t.Fatalf("request body = %v", got)
	}
	if auth != "Bearer k" {
		t.Fatalf("auth = %q", auth)
	}
	st, serr := os.Stat(out)
	if serr != nil || st.Size() != 2000 || res.Bytes != 2000 || res.Path != out || res.ContentType != "audio/wav" || res.Model != DefaultModel {
		t.Fatalf("output: %v %+v", serr, res)
	}
	if _, err := os.Stat(out + ".part"); err == nil {
		t.Fatal("temp file left behind")
	}
}

func TestSpeakErrorsNameTheServer(t *testing.T) {
	// Error bodies are LONGER than minAudioBytes on purpose: the size check must
	// not be the thing that catches a 400 or a JSON 200 — those have their own
	// checks, and a short body would let a mutation of either pass unnoticed.
	long := strings.Repeat("x", 300)
	cases := map[string]http.HandlerFunc{
		"http 400": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "audio/wav") // even a lying content-type must not rescue a 400
			http.Error(w, `{"detail":"unknown model `+long+`"}`, 400)
		},
		"json 200": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"error":"no audio ` + long + `"}`))
		},
		"tiny body": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write([]byte("RIFF"))
		},
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		out := filepath.Join(t.TempDir(), "a.wav")
		_, err := Speak(context.Background(), Request{Base: srv.URL, Text: "x", Out: out})
		srv.Close()
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if !strings.Contains(err.Error(), srv.URL) {
			t.Errorf("%s: error must name the server: %v", name, err)
		}
		if _, serr := os.Stat(out); serr == nil {
			t.Errorf("%s: an output file must not exist after a failure", name)
		}
	}
	if _, err := Speak(context.Background(), Request{Base: "", Text: "x", Out: "a.wav"}); err == nil {
		t.Fatal("empty endpoint must error")
	}
	if _, err := Speak(context.Background(), Request{Base: "http://127.0.0.1:1", Text: " ", Out: "a.wav"}); err == nil {
		t.Fatal("empty text must error before any network")
	}
}
