package tasks

import (
	"testing"

	"github.com/dmmdea/local-offload-pp-cli/internal/core"
)

// TestBuildVQA: a vqa request with a question yields a non-empty system prompt,
// the question verbatim as the user prompt, and NO grammar (free-text VQA).
func TestBuildVQA(t *testing.T) {
	const q = "What number is shown in the image?"
	b, err := Build(core.Request{
		Task:   core.TaskVQA,
		Image:  "/tmp/x.png",
		Params: map[string]any{"question": q},
	})
	if err != nil {
		t.Fatalf("Build(vqa): %v", err)
	}
	if b.System == "" {
		t.Errorf("vqa System must be non-empty")
	}
	if b.User != q {
		t.Errorf("vqa User = %q, want the question %q", b.User, q)
	}
	if b.Grammar != "" {
		t.Errorf("vqa Grammar = %q, want empty (free-text)", b.Grammar)
	}
	if b.MaxTokens <= 0 {
		t.Errorf("vqa MaxTokens = %d, want a positive default", b.MaxTokens)
	}
}

// TestBuildVQAMissingQuestion: a vqa request without a question is an error,
// mirroring triage which requires its question.
func TestBuildVQAMissingQuestion(t *testing.T) {
	if _, err := Build(core.Request{Task: core.TaskVQA, Image: "/tmp/x.png"}); err == nil {
		t.Errorf("expected an error for a vqa request with no question, got nil")
	}
}

// TestBuildOCR: an ocr request needs NO question param, yields a non-empty system
// prompt and a fixed transcription user prompt, NO grammar (free-text OCR), and a
// generous MaxTokens (OCR output can be long).
func TestBuildOCR(t *testing.T) {
	b, err := Build(core.Request{Task: core.TaskOCR, Image: "/tmp/x.png"})
	if err != nil {
		t.Fatalf("Build(ocr): %v", err)
	}
	if b.System == "" {
		t.Errorf("ocr System must be non-empty")
	}
	if b.User == "" {
		t.Errorf("ocr User must be non-empty")
	}
	if b.Grammar != "" {
		t.Errorf("ocr Grammar = %q, want empty (free-text)", b.Grammar)
	}
	if b.MaxTokens < 1024 {
		t.Errorf("ocr MaxTokens = %d, want >= 1024 (OCR output can be long)", b.MaxTokens)
	}
}
