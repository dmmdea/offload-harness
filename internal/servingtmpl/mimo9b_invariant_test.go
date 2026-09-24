package servingtmpl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mimoTemplates are the three shipped templates that carry the mimo-9b-agent
// entry (the exact set the qwen3.5-9b-agent block ships in today).
var mimoTemplates = []struct{ name, file string }{
	{"win-cuda", "llama-swap.win-cuda.yaml"},
	{"linux-cuda", "llama-swap.linux-cuda.yaml"},
	{"linux-vulkan", "llama-swap.linux-vulkan.yaml"},
}

// TestMimo9BSeatKeepsItsMeasuredInvariants guards, on every template that ships the
// seat, the facts the 2026-09-24 8GB agent-seat bake measured (18/18 shape B x6
// both reference boxes; shape C 5/5 x3 blackwell-8, 4/5 x3 ampere-8):
//
//  1. `--reasoning off` IS pinned directly on the command line — unlike
//     qwen3.5-9b-agent, MiMo measured byte-identical between the CLI flag and the
//     `--chat-template-kwargs {"enable_thinking":false}` env twin on all 12 quality
//     runs, so this seat does not need the env-twin workaround.
//  2. No LLAMA_ARG_CHAT_TEMPLATE_KWARGS env twin — carrying one here would be an
//     unmeasured configuration for this model family.
//  3. ctx is the explicit measured 65536 (fit measured at exactly this window on
//     both 8GB reference boxes), not the tier's __CTX__ macro.
//  4. flash-attn and both cache types are the explicit measured "on"/"q8_0" pins,
//     not the tier's __FLASH_ATTN__/__KV_K__/__KV_V__ macros — the bake measured
//     this exact configuration.
func TestMimo9BSeatKeepsItsMeasuredInvariants(t *testing.T) {
	for _, tc := range mimoTemplates {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			p := params()
			p.IncludeMimo9B = true
			p.Ctx = 16384 // a value the seat's own literal ctx-size must not equal by accident
			out, err := Render(string(b), p)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			block := seatBlockOf(out, "mimo-9b-agent")
			if strings.TrimSpace(block) == "" {
				t.Fatal("no mimo-9b-agent block in the rendered config — the seat vanished, or was renamed without updating this guard")
			}
			if !strings.Contains(block, "--reasoning off") {
				t.Errorf("seat does not pin --reasoning off — measured byte-identical to the chat-template-kwargs env twin on all 12 quality runs, so the plain CLI flag is the intended pin. Block:\n%s", block)
			}
			if strings.Contains(block, "LLAMA_ARG_CHAT_TEMPLATE_KWARGS") {
				t.Errorf("seat carries the qwen3.5-9b-agent env-twin workaround, which MiMo's own measurement showed is unnecessary. Block:\n%s", block)
			}
			if strings.Contains(block, "--chat-template-kwargs") {
				t.Errorf("seat pins thinking kwargs on the COMMAND LINE; llama-swap strips the inner quotes and llama-server rejects it (json parse_error). Block:\n%s", block)
			}
			if !strings.Contains(block, "--ctx-size 65536") {
				t.Errorf("seat lost its explicit measured ctx 65536. Block:\n%s", block)
			}
			if !strings.Contains(block, "--flash-attn on") {
				t.Errorf("seat lost its explicit measured --flash-attn on. Block:\n%s", block)
			}
			if !strings.Contains(block, "--cache-type-k q8_0") || !strings.Contains(block, "--cache-type-v q8_0") {
				t.Errorf("seat lost its explicit measured q8_0 KV cache type. Block:\n%s", block)
			}
			if !strings.Contains(block, "aliases: [mimo-9b, agent-seat]") {
				t.Errorf("seat lost its aliases (mimo-9b, agent-seat). Block:\n%s", block)
			}
		})
	}
}

// TestIncludeMimo9BOnAnEntrylessTemplateIsRefused mirrors the Q359B tripwire:
// win-cuda-resident defines no mimo-9b-agent entry, so the flag must be refused by
// name there rather than becoming a silent no-op while the installer still
// downloads the GGUF.
func TestIncludeMimo9BOnAnEntrylessTemplateIsRefused(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "llama-swap.win-cuda-resident.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p := params()
	p.IncludeMimo9B = true
	_, err = Render(string(b), p)
	if err == nil || !strings.Contains(err.Error(), "mimo-9b-agent") || !strings.Contains(err.Error(), "include_mimo_9b") {
		t.Fatalf("IncludeMimo9B against an entryless template must be refused by name, got %v", err)
	}
	p.IncludeMimo9B = false
	if _, err := Render(string(b), p); err != nil {
		t.Fatalf("IncludeMimo9B=false must still render an entryless template: %v", err)
	}
}

// TestMimoAndQwen354BAreRefused pins the mutual exclusion: mimo-9b-agent and
// qwen3.5-4b-agent share the `agent-seat` alias, so a tier enabling both would
// render a duplicate-alias config llama-swap rejects at startup.
func TestMimoAndQwen354BAreRefused(t *testing.T) {
	for _, tc := range mimoTemplates {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			p := params()
			p.IncludeMimo9B = true
			p.IncludeQ354B = true
			_, err = Render(string(b), p)
			if err == nil || !strings.Contains(err.Error(), "agent-seat") {
				t.Fatalf("include_mimo_9b + include_qwen35_4b must be refused naming the shared alias, got %v", err)
			}
		})
	}
}

// TestMimoAndQwen359BRenderTogetherQwenLosesTheAlias is the acceptance case the
// spec calls out explicitly: unlike the 4B/9B pair, mimo-9b-agent and
// qwen3.5-9b-agent are ALLOWED together — qwen3.5-9b-agent stays as the rollback
// seat but must lose the `agent-seat` alias so the two entries never collide.
func TestMimoAndQwen359BRenderTogetherQwenLosesTheAlias(t *testing.T) {
	for _, tc := range mimoTemplates {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			p := params()
			p.IncludeMimo9B = true
			p.IncludeQ359B = true
			out, err := Render(string(b), p)
			if err != nil {
				t.Fatalf("include_mimo_9b + include_qwen35_9b must render together: %v", err)
			}
			mimoBlock := seatBlockOf(out, "mimo-9b-agent")
			if strings.TrimSpace(mimoBlock) == "" {
				t.Fatal("mimo-9b-agent block is missing")
			}
			if !strings.Contains(mimoBlock, "agent-seat") {
				t.Errorf("mimo-9b-agent must hold the agent-seat alias when both seats render. Block:\n%s", mimoBlock)
			}
			qwenBlock := seatBlockOf(out, "qwen3.5-9b-agent")
			if strings.TrimSpace(qwenBlock) == "" {
				t.Fatal("qwen3.5-9b-agent block is missing — it must stay rendered as the rollback seat")
			}
			if strings.Contains(qwenBlock, "agent-seat") {
				t.Errorf("qwen3.5-9b-agent must lose the agent-seat alias once mimo-9b-agent renders (duplicate alias otherwise). Block:\n%s", qwenBlock)
			}
			if !strings.Contains(qwenBlock, "qwen35-9b") {
				t.Errorf("qwen3.5-9b-agent must keep its own qwen35-9b alias. Block:\n%s", qwenBlock)
			}
			// Every alias must be unique across the whole rendered config: llama-swap
			// rejects a config with a duplicate at startup.
			assertNoDuplicateAliases(t, out)
		})
	}
}

// TestMimoAloneKeepsTheAgentSeatAlias is the simplest accepted shape: mimo-9b-agent
// with qwen3.5-9b-agent OFF still renders and still carries the shared alias
// itself (nothing else can be claiming it).
func TestMimoAloneKeepsTheAgentSeatAlias(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "llama-swap.win-cuda.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p := params()
	p.IncludeMimo9B = true
	p.IncludeQ359B = false
	out, err := Render(string(b), p)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.TrimSpace(seatBlockOf(out, "qwen3.5-9b-agent")) != "" {
		t.Error("qwen3.5-9b-agent must not render when include_qwen35_9b is false")
	}
	mimoBlock := seatBlockOf(out, "mimo-9b-agent")
	if !strings.Contains(mimoBlock, "agent-seat") {
		t.Errorf("mimo-9b-agent alone must still carry the agent-seat alias. Block:\n%s", mimoBlock)
	}
	assertNoDuplicateAliases(t, out)
}

// assertNoDuplicateAliases parses every `aliases: [...]` line in a rendered
// config and fails if any bare alias token repeats — the shape llama-swap
// itself refuses to start on.
func assertNoDuplicateAliases(t *testing.T, rendered string) {
	t.Helper()
	seen := map[string]bool{}
	for _, line := range strings.Split(rendered, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "aliases:") {
			continue
		}
		open, close := strings.Index(trimmed, "["), strings.LastIndex(trimmed, "]")
		if open < 0 || close < open {
			continue
		}
		for _, a := range strings.Split(trimmed[open+1:close], ",") {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			if seen[a] {
				t.Errorf("alias %q appears more than once in the rendered config — llama-swap refuses a duplicate alias at startup", a)
			}
			seen[a] = true
		}
	}
}
