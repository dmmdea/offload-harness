package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// etestSchema is the {properties:{...}} JSON-schema param the extract task expects.
func etestSchema() map[string]any {
	return map[string]any{
		"properties": map[string]any{
			"invoice_no": map[string]any{"type": "string"},
			"total":      map[string]any{"type": "number"},
		},
	}
}

// TestRunExtractImageComposesOCRThenExtract drives runExtractImage end to end
// through the fake-client harness: the OCR sub-call (a multimodal request with an
// image_url part) returns invoice text; the extract sub-call (a text request, NO
// image part) returns a grounded object. The final result must be the extract
// result — proving extract_image is OCR -> existing text-extract composition.
func TestRunExtractImageComposesOCRThenExtract(t *testing.T) {
	const ocrText = "Invoice #A-204\nTOTAL 4999"
	var sawOCR, sawExtract bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(s, `"image_url"`) {
			// OCR sub-call: transcribe the image.
			sawOCR = true
			_, _ = w.Write(fakeChat{content: ocrText, finishReason: "stop", promptTokens: 50}.marshal())
			return
		}
		// Text extract sub-call: emit a grounded object (values appear in ocrText).
		sawExtract = true
		_, _ = w.Write(fakeChat{content: `{"invoice_no":"A-204","total":4999}`, finishReason: "stop", promptTokens: 60}.marshal())
	}))
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.Model = "fake-e4b" // text extract tier
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskExtractImage,
		Image:  minimalPNGDataURI(),
		Params: map[string]any{"schema": etestSchema()},
	})
	if !res.OK {
		t.Fatalf("expected OK extract_image result, got defer: %s", res.Reason)
	}
	if !sawOCR {
		t.Errorf("OCR sub-call was never made")
	}
	if !sawExtract {
		t.Errorf("extract sub-call was never made")
	}
	var out struct {
		InvoiceNo string  `json:"invoice_no"`
		Total     float64 `json:"total"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("unmarshal extract_image result %s: %v", res.Data, err)
	}
	if out.InvoiceNo != "A-204" {
		t.Errorf("invoice_no = %q, want the grounded extract value", out.InvoiceNo)
	}
	if out.Total != 4999 {
		t.Errorf("total = %v, want 4999", out.Total)
	}
}

// TestRunExtractImageOCRDeferShortCircuits: when the OCR sub-call defers (here the
// model returns empty content), runExtractImage propagates that defer and NEVER
// makes the text extract sub-call.
func TestRunExtractImageOCRDeferShortCircuits(t *testing.T) {
	var sawExtract bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(s, `"image_url"`) {
			// OCR sub-call: empty output -> the ocr task defers.
			_, _ = w.Write(fakeChat{content: "   ", finishReason: "stop", promptTokens: 50}.marshal())
			return
		}
		sawExtract = true
		_, _ = w.Write(fakeChat{content: `{"invoice_no":"A-204","total":4999}`, finishReason: "stop", promptTokens: 60}.marshal())
	}))
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.Model = "fake-e4b"
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskExtractImage,
		Image:  minimalPNGDataURI(),
		Params: map[string]any{"schema": etestSchema()},
	})
	if res.OK || !res.Deferred {
		t.Fatalf("expected the OCR defer to propagate, got OK=%v", res.OK)
	}
	if sawExtract {
		t.Errorf("extract sub-call must NOT run after an OCR defer")
	}
}
