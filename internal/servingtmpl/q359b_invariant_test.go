package servingtmpl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQ359BSeatKeepsItsMeasuredInvariants guards, on BOTH templates that ship the
// seat, the facts the 2026-08-22 blackwell-8 on-box bake measured (100% extraction
// x2 + 5/5 search+reason x2 for this configuration, vs the E4B fallback's 0%):
//
//  1. No `--reasoning off` and no ${common} (which pins it) — the same measured
//     thinking-family trap the 4B seat guards (28% vs 67% there).
//  2. No thinking kwargs pinned: llama.cpp's default for this template is thinking
//     OFF, the configuration every winning score was measured under. Thinking ON
//     was measured strictly WORSE (89% x2, reasoning prose leaking into content,
//     3.5x the wall) — so `--chat-template-kwargs` appearing on this seat means an
//     unmeasured (or measured-worse) configuration is being shipped.
//  3. ctx is the explicit measured 32768 (6696 MiB on the 8GB reference card —
//     the Gated-DeltaNet hybrid's small KV), not the tier macro: "tidying" it to
//     __CTX__ silently halves the served window on the 16384 tiers that seat it.
func TestQ359BSeatKeepsItsMeasuredInvariants(t *testing.T) {
	for _, tc := range []struct{ name, file string }{
		{"linux-cuda", "llama-swap.linux-cuda.yaml"},
		{"win-cuda", "llama-swap.win-cuda.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			p := params()
			p.IncludeQ359B = true
			// params() serves Ctx 32768, which would make a "tidied" __CTX__ macro
			// render byte-identically to the explicit pin and turn check 3 vacuous
			// (caught by mutation 2026-08-23: the macro edit stayed green until this
			// line). 16384 is the real 8GB tier value — the scenario where the
			// correct and broken configs actually differ.
			p.Ctx = 16384
			out, err := Render(string(b), p)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			block := seatBlockOf(out, "qwen3.5-9b-agent")
			if strings.TrimSpace(block) == "" {
				t.Fatal("no qwen3.5-9b-agent block in the rendered config — the seat vanished, or was renamed without updating this guard")
			}
			if strings.Contains(block, "--reasoning off") {
				t.Errorf("seat pins --reasoning off — the thinking-family collapse trap the 4B seat measured. Block:\n%s", block)
			}
			if strings.Contains(block, "${common}") {
				t.Errorf("seat uses ${common}, which pins --reasoning off. Block:\n%s", block)
			}
			if strings.Contains(block, "--chat-template-kwargs") {
				t.Errorf("seat pins thinking kwargs; the measured configuration is llama.cpp's default (thinking OFF, 100%% extraction) and thinking ON was measured WORSE (89%%, 3.5x wall). Re-bake before pinning. Block:\n%s", block)
			}
			if !strings.Contains(block, "--ctx-size 32768") {
				t.Errorf("seat lost its explicit measured ctx 32768 (6696 MiB on the 8GB reference card). Block:\n%s", block)
			}
		})
	}
}

// TestIncludeQ359BOnAnEntrylessTemplateIsRefused mirrors the Q354B tripwire:
// win-cuda-resident defines neither small agent seat, so the flag must be refused
// by name there rather than becoming a silent no-op while the installer still
// downloads 5.6 GB of weights.
func TestIncludeQ359BOnAnEntrylessTemplateIsRefused(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "llama-swap.win-cuda-resident.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p := params()
	p.IncludeQ359B = true
	_, err = Render(string(b), p)
	if err == nil || !strings.Contains(err.Error(), "qwen3.5-9b-agent") || !strings.Contains(err.Error(), "include_qwen35_9b") {
		t.Fatalf("IncludeQ359B against an entryless template must be refused by name, got %v", err)
	}
	p.IncludeQ359B = false
	if _, err := Render(string(b), p); err != nil {
		t.Fatalf("IncludeQ359B=false must still render an entryless template: %v", err)
	}
}

// TestBothSmallAgentSeatsAreRefused pins the mutual exclusion: the 4B and 9B
// entries share the `agent-seat` alias, so a tier enabling both would render a
// duplicate-alias config llama-swap rejects at startup. validate() must refuse it
// at render time, by name, on any template.
func TestBothSmallAgentSeatsAreRefused(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "llama-swap.win-cuda.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p := params()
	p.IncludeQ354B = true
	p.IncludeQ359B = true
	_, err = Render(string(b), p)
	if err == nil || !strings.Contains(err.Error(), "agent-seat") {
		t.Fatalf("both agent-seat gates set must be refused naming the shared alias, got %v", err)
	}
}
