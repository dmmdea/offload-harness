package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func ttsServer(t *testing.T, seen *map[string]any, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			http.Error(w, "wrong route "+r.URL.Path, 404)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if seen != nil {
			*seen = body
		}
		if status != 200 {
			http.Error(w, `{"detail":"engine not loaded"}`, status)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		b := make([]byte, 4096)
		copy(b, "RIFF")
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A box with an endpoint and NO local voice script renders through the
// endpoint by default: the OpenAI-shaped body, a WAV on disk, the engine
// named on the result.
func TestRunGenerateAudio_EndpointIsTheDefaultWithoutAScript(t *testing.T) {
	var seen map[string]any
	srv := ttsServer(t, &seen, 200)
	cfg := config.Default()
	cfg.VoiceGenScript = ""
	cfg.TTSEndpoint = srv.URL
	cfg.TTSVoice = "narrator"
	cfg.MediaDir = t.TempDir()
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateAudio, Input: "hola mundo", Params: map[string]any{"kind": "voice", "lang": "es"}})
	if !res.OK {
		t.Fatalf("expected ok via the endpoint, got defer: %s", res.Reason)
	}
	if seen["model"] != "tts-1" || seen["voice"] != "narrator" || seen["input"] != "hola mundo" || seen["language"] != "es" || seen["response_format"] != "wav" {
		t.Fatalf("request body = %v", seen)
	}
	var out struct {
		AudioPath string `json:"audio_path"`
		Kind      string `json:"kind"`
		Engine    string `json:"engine"`
		Bytes     int64  `json:"bytes"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != "voice" || out.Engine != "tts_endpoint" || out.Bytes != 4096 || !strings.HasSuffix(out.AudioPath, ".wav") {
		t.Fatalf("result = %+v", out)
	}
	if st, err := os.Stat(out.AudioPath); err != nil || st.Size() != 4096 {
		t.Fatalf("audio file: %v", err)
	}
	if res.Meta.Model != "tts-endpoint:tts-1" {
		t.Fatalf("meta.Model = %q", res.Meta.Model)
	}
}

// With BOTH a script and an endpoint the default stays the script (nothing
// changes for a box that had Chatterbox); voice=endpoint selects the lane.
func TestRunGenerateAudio_EndpointOnlyWhenAskedIfAScriptExists(t *testing.T) {
	var seen map[string]any
	srv := ttsServer(t, &seen, 200)
	cfg := config.Default()
	cfg.VoiceGenScript = "render/does-not-exist.mjs" // present in config: the script lane is the default
	cfg.TTSEndpoint = srv.URL
	cfg.MediaDir = t.TempDir()
	p := &Pipeline{cfg: cfg}
	// default voice → the script lane, which defers because the file is missing (never the endpoint)
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateAudio, Input: "x", Params: map[string]any{"kind": "voice"}})
	if res.OK || seen != nil {
		t.Fatalf("default voice must take the script lane when a script is configured: ok=%v seen=%v", res.OK, seen)
	}
	res = p.Run(context.Background(), core.Request{Task: core.TaskGenerateAudio, Input: "x", Params: map[string]any{"kind": "voice", "voice": VoiceEndpoint}})
	if !res.OK || seen == nil {
		t.Fatalf("voice=endpoint must take the endpoint lane: %s", res.Reason)
	}
}

// Failures are defers that name the server and its words; a box asked for
// voice=endpoint with no endpoint says so by key; music is untouched.
func TestRunGenerateAudio_EndpointDefersByName(t *testing.T) {
	srv := ttsServer(t, nil, 503)
	cfg := config.Default()
	cfg.VoiceGenScript = ""
	cfg.TTSEndpoint = srv.URL
	cfg.MediaDir = t.TempDir()
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateAudio, Input: "x", Params: map[string]any{"kind": "voice"}})
	if res.OK || !res.Deferred || !strings.Contains(res.Reason, "503") || !strings.Contains(res.Reason, "engine not loaded") {
		t.Fatalf("want a defer naming the server's answer, got ok=%v reason=%q", res.OK, res.Reason)
	}
	if res.Meta.ErrClass != "tts_endpoint" {
		t.Fatalf("err_class = %q", res.Meta.ErrClass)
	}
	cfg.TTSEndpoint = ""
	p = &Pipeline{cfg: cfg}
	res = p.Run(context.Background(), core.Request{Task: core.TaskGenerateAudio, Input: "x", Params: map[string]any{"kind": "voice", "voice": VoiceEndpoint}})
	if res.OK || !strings.Contains(res.Reason, "tts_endpoint is unset") {
		t.Fatalf("voice=endpoint without the key must defer by key, got %q", res.Reason)
	}
	cfg.TTSEndpoint = srv.URL
	cfg.MusicGenScript = ""
	p = &Pipeline{cfg: cfg}
	res = p.Run(context.Background(), core.Request{Task: core.TaskGenerateAudio, Input: "x", Params: map[string]any{"kind": "music"}})
	if res.OK || !strings.Contains(res.Reason, "kind music") {
		t.Fatalf("music must never route to the speech endpoint: %q", res.Reason)
	}
}

func TestUseTTSEndpointDecision(t *testing.T) {
	both := config.Config{TTSEndpoint: "http://x", VoiceGenScript: "s.mjs"}
	only := config.Config{TTSEndpoint: "http://x"}
	none := config.Config{}
	for _, c := range []struct {
		cfg   config.Config
		voice string
		want  bool
	}{
		{both, "", false}, {both, "generalist", false}, {both, "finetuned", false}, {both, VoiceEndpoint, true},
		{only, "", true}, {only, "generalist", true}, {only, "finetuned", false}, {only, VoiceEndpoint, true},
		{none, "", false}, {none, VoiceEndpoint, true}, // true so the lane defers BY KEY, never "unknown voice"
	} {
		if got := useTTSEndpoint(c.cfg, c.voice); got != c.want {
			t.Errorf("useTTSEndpoint(endpoint=%q script=%q voice=%q) = %v, want %v", c.cfg.TTSEndpoint, c.cfg.VoiceGenScript, c.voice, got, c.want)
		}
	}
}
