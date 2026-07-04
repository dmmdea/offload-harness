package pipeline

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// minimalPNGDataURI is a valid 1x1 PNG encoded as a data URI, used so the image
// loader resolves without touching disk.
func minimalPNGDataURI() string {
	png := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xDE,
		0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41, 0x54,
		0x08, 0xD7, 0x63, 0xF8, 0xCF, 0xC0, 0x00, 0x00, 0x00, 0x03, 0x00, 0x01,
		0x18, 0xDD, 0x8D, 0xB4,
		0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// baseVisionCfg returns a Default config wired to srv with self-learning paths
// and stores disabled so only the vision branch under test is active.
func baseVisionCfg(srv *httptest.Server, visionModel string) config.Config {
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.VisionModel = visionModel
	cfg.ThresholdsPath = ""
	cfg.RouterWeightsPath = ""
	cfg.TierOverridesPath = ""
	cfg.ConfHeadLabelsPath = ""
	cfg.CachePath = ""
	cfg.LedgerPath = ""
	return cfg
}

// visionServer returns a server that asserts the request is a multimodal call
// (content array with an image_url data-URI part) and then replies with the
// supplied fakeChat.
func visionServer(t *testing.T, reply fakeChat) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		if !strings.Contains(s, `"image_url"`) || !strings.Contains(s, "data:image/png;base64,") {
			t.Errorf("vision request missing image_url data-URI part; body=%s", s)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(reply.marshal())
	}))
}

// TestRunVisionSuccess: a vqa request resolves the image, calls GenerateVision,
// and returns {answer:...} from the model content.
func TestRunVisionSuccess(t *testing.T) {
	srv := visionServer(t, fakeChat{content: "The number shown is 4999.", finishReason: "stop", promptTokens: 50})
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskVQA,
		Image:  minimalPNGDataURI(),
		Params: map[string]any{"question": "What number is shown?"},
	})
	if !res.OK {
		t.Fatalf("expected OK vqa result, got defer: %s", res.Reason)
	}
	var out struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("unmarshal vqa result %s: %v", res.Data, err)
	}
	if out.Answer != "The number shown is 4999." {
		t.Errorf("answer = %q, want the trimmed model content", out.Answer)
	}
	if res.Meta.Model != "fake-vlm" {
		t.Errorf("meta.Model = %q, want the vision model", res.Meta.Model)
	}
}

// TestRunVisionOCRSuccess: an ocr request resolves the image, calls
// GenerateVision, and returns the transcription under the "text" key (NOT
// "answer") — the result key is task-dependent.
func TestRunVisionOCRSuccess(t *testing.T) {
	srv := visionServer(t, fakeChat{content: "Invoice #A-204\nDate: 2026-06-15\nTOTAL 4999", finishReason: "stop", promptTokens: 50})
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)

	res := p.Run(context.Background(), core.Request{
		Task:  core.TaskOCR,
		Image: minimalPNGDataURI(),
	})
	if !res.OK {
		t.Fatalf("expected OK ocr result, got defer: %s", res.Reason)
	}
	var out struct {
		Text   string `json:"text"`
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("unmarshal ocr result %s: %v", res.Data, err)
	}
	if out.Answer != "" {
		t.Errorf("ocr result must use the \"text\" key, not \"answer\"; got answer=%q", out.Answer)
	}
	if !strings.Contains(out.Text, "4999") || !strings.Contains(out.Text, "A-204") {
		t.Errorf("ocr text = %q, want the transcribed lines", out.Text)
	}
	if res.Meta.Model != "fake-vlm" {
		t.Errorf("meta.Model = %q, want the vision model", res.Meta.Model)
	}
}

// TestRunVisionAssessImageUnwrapped: for a grammar vision task (assess_image) the
// model output is ALREADY a JSON object, so Data must be that raw JSON verbatim —
// NOT wrapped in {key: content}. vqa/ocr (no grammar) keep wrapping (above).
func TestRunVisionAssessImageUnwrapped(t *testing.T) {
	out := `{"has_people":false,"has_text":true,"matches_brief":true,"notes":"red square, black text"}`
	srv := visionServer(t, fakeChat{content: out, finishReason: "stop", promptTokens: 50})
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)

	res := p.Run(context.Background(), core.Request{
		Task:  core.TaskAssessImage,
		Image: minimalPNGDataURI(),
	})
	if !res.OK {
		t.Fatalf("expected OK assess_image result, got defer: %s", res.Reason)
	}
	// Data must be the raw object — NOT {"answer":...}/{"text":...}.
	var obj map[string]any
	if err := json.Unmarshal(res.Data, &obj); err != nil {
		t.Fatalf("assess_image Data is not valid JSON %s: %v", res.Data, err)
	}
	if _, wrapped := obj["answer"]; wrapped {
		t.Fatalf("grammar task must NOT wrap in {answer:...}; got %s", res.Data)
	}
	if _, wrapped := obj["text"]; wrapped {
		t.Fatalf("grammar task must NOT wrap in {text:...}; got %s", res.Data)
	}
	if v, ok := obj["has_text"].(bool); !ok || !v {
		t.Errorf("has_text not surfaced as a top-level bool: %s", res.Data)
	}
	if v, ok := obj["has_people"].(bool); !ok || v {
		t.Errorf("has_people not surfaced as a top-level bool=false: %s", res.Data)
	}
	if n, ok := obj["notes"].(string); !ok || n == "" {
		t.Errorf("notes not surfaced as a string: %s", res.Data)
	}
}

// TestRunVisionAssessImageBadJSONDefers: if a grammar vision task somehow returns
// non-JSON (shouldn't happen with a grammar), the pipeline defers instead of
// emitting garbage Data.
func TestRunVisionAssessImageBadJSONDefers(t *testing.T) {
	srv := visionServer(t, fakeChat{content: "not json at all", finishReason: "stop", promptTokens: 50})
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)

	res := p.Run(context.Background(), core.Request{
		Task:  core.TaskAssessImage,
		Image: minimalPNGDataURI(),
	})
	if res.OK || !res.Deferred {
		t.Fatalf("expected defer on non-JSON grammar output, got OK=%v data=%s", res.OK, res.Data)
	}
}

// TestVisionResultKey: the success output key is task-dependent — vqa -> "answer"
// (unchanged), ocr -> "text". Guards the byte-identical vqa behavior.
func TestVisionResultKey(t *testing.T) {
	if k := visionResultKey(core.TaskVQA); k != "answer" {
		t.Errorf("visionResultKey(vqa) = %q, want \"answer\"", k)
	}
	if k := visionResultKey(core.TaskOCR); k != "text" {
		t.Errorf("visionResultKey(ocr) = %q, want \"text\"", k)
	}
}

// TestRunVisionEmptyDefers: empty model content -> defer (no answer to give).
func TestRunVisionEmptyDefers(t *testing.T) {
	srv := visionServer(t, fakeChat{content: "   ", finishReason: "stop", promptTokens: 50})
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskVQA,
		Image:  minimalPNGDataURI(),
		Params: map[string]any{"question": "What number is shown?"},
	})
	if res.OK || !res.Deferred {
		t.Fatalf("expected defer on empty vision output, got OK=%v", res.OK)
	}
}

// TestRunVisionTruncatedDefers: a length-truncated answer defers.
func TestRunVisionTruncatedDefers(t *testing.T) {
	srv := visionServer(t, fakeChat{content: "The number is", finishReason: "length", promptTokens: 50})
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskVQA,
		Image:  minimalPNGDataURI(),
		Params: map[string]any{"question": "What number is shown?"},
	})
	if res.OK || !res.Deferred {
		t.Fatalf("expected defer on truncated vision output, got OK=%v", res.OK)
	}
}

// TestRunVisionEmptyModelDefers: an empty VisionModel ("" = no vision route) must
// defer WITHOUT calling the model — never fall back to the text model (misroute).
func TestRunVisionEmptyModelDefers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("model must not be called when VisionModel is empty")
	}))
	defer srv.Close()

	cfg := baseVisionCfg(srv, "") // empty vision model
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskVQA,
		Image:  minimalPNGDataURI(),
		Params: map[string]any{"question": "What number is shown?"},
	})
	if res.OK || !res.Deferred {
		t.Fatalf("expected defer when VisionModel is empty, got OK=%v", res.OK)
	}
	if !strings.Contains(res.Reason, "no vision model") {
		t.Errorf("defer reason = %q, want it to mention no vision model configured", res.Reason)
	}
}

// TestRunVisionBadImageDefers: an unresolvable image input defers without ever
// calling the model.
func TestRunVisionBadImageDefers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("model must not be called when the image fails to load")
	}))
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskVQA,
		Image:  "/no/such/path/does-not-exist.png",
		Params: map[string]any{"question": "What number is shown?"},
	})
	if res.OK || !res.Deferred {
		t.Fatalf("expected defer on image load failure, got OK=%v", res.OK)
	}
}
