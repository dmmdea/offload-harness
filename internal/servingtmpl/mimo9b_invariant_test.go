package servingtmpl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mimoTemplates are the four shipped templates that carry the mimo-9b-agent entry.
// win-vulkan joined 2026-09-24 (the amd-gcn onboarding): TestVulkanAndCPUTiersRenderOnLinux
// and TestEveryBoundAliasIsServed both require every "vulkan"-backend tier to render
// on BOTH operating systems, seats included — win-cuda/linux-cuda/linux-vulkan alone
// left amd-gcn (backend vulkan) refusing to render on Windows once it set
// include_mimo_9b, which is exactly the OS-decides-capability failure those two gates
// exist to catch.
var mimoTemplates = []struct{ name, file string }{
	{"win-cuda", "llama-swap.win-cuda.yaml"},
	{"linux-cuda", "llama-swap.linux-cuda.yaml"},
	{"linux-vulkan", "llama-swap.linux-vulkan.yaml"},
	{"win-vulkan", "llama-swap.win-vulkan.yaml"},
}

// mimoAndQ359BTemplates is the narrower set that ALSO carries qwen3.5-9b-agent —
// win-vulkan does not (it never grew a 9B-class entry, only 4B and now mimo), so
// tests that exercise the mimo+9B pairing specifically use this list instead of the
// full mimoTemplates.
var mimoAndQ359BTemplates = []struct{ name, file string }{
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
//
// win-cuda and linux-cuda additionally pin the CUDA 8GB reference boxes' explicit
// measured LITERALS — ctx 65536, flash-attn on, q8_0 KV — because those boxes have
// a fixed 8GB VRAM budget the fit was measured against. linux-vulkan is the
// exception (2026-09-24, the amd-gcn onboarding): a Vulkan UMA box has no such
// fixed fit, so its entry instead follows the TIER's own window/KV/flash-attn,
// exactly like that template's qwen3.5-4b-agent block — asserted here with values
// DELIBERATELY DIFFERENT from the CUDA literals (amd-gcn's own measured geometry:
// 32768 / f16 / cache-ram 8192) so a block that silently kept the CUDA pin, or one
// that silently dropped the tier substitution, is caught either way.
func TestMimo9BSeatKeepsItsMeasuredInvariants(t *testing.T) {
	for _, tc := range mimoTemplates {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			p := params()
			p.IncludeMimo9B = true
			p.Ctx = 32768 // amd-gcn's own measured window — must not equal the CUDA literal 65536
			p.KVType = "f16" // amd-gcn's own measured KV type — must not equal the CUDA literal q8_0
			p.CacheRAMMiB = 8192
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
			if !strings.Contains(block, "aliases: [mimo-9b, agent-seat]") {
				t.Errorf("seat lost its aliases (mimo-9b, agent-seat). Block:\n%s", block)
			}
			if tc.name == "linux-vulkan" || tc.name == "win-vulkan" {
				if !strings.Contains(block, "--ctx-size 32768") {
					t.Errorf("%s's mimo-9b-agent must serve the TIER's window (__CTX__) — a Vulkan UMA box has no fixed 8GB-class fit to pin to. Block:\n%s", tc.name, block)
				}
				if strings.Contains(block, "--ctx-size 65536") {
					t.Errorf("%s's mimo-9b-agent still carries the CUDA 8GB tiers' literal window. Block:\n%s", tc.name, block)
				}
				if !strings.Contains(block, "--cache-type-k f16") || !strings.Contains(block, "--cache-type-v f16") {
					t.Errorf("%s's mimo-9b-agent must serve the TIER's KV type (__KV_K__/__KV_V__), not the CUDA literal q8_0. Block:\n%s", tc.name, block)
				}
				if !strings.Contains(block, "--cache-ram 8192") {
					t.Errorf("%s's mimo-9b-agent must carry the tier's --cache-ram, like every other tier-driven seat on this template. Block:\n%s", tc.name, block)
				}
			} else {
				if !strings.Contains(block, "--ctx-size 65536") {
					t.Errorf("seat lost its explicit measured ctx 65536. Block:\n%s", block)
				}
				if !strings.Contains(block, "--flash-attn on") {
					t.Errorf("seat lost its explicit measured --flash-attn on. Block:\n%s", block)
				}
				if !strings.Contains(block, "--cache-type-k q8_0") || !strings.Contains(block, "--cache-type-v q8_0") {
					t.Errorf("seat lost its explicit measured q8_0 KV cache type. Block:\n%s", block)
				}
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

// TestMimoAndQwen354BRenderTogetherQwenLosesTheAlias is the acceptance case the
// amd-gcn onboarding calls out explicitly (2026-09-24, superseding the render
// refusal this seat shipped with): mimo-9b-agent and qwen3.5-4b-agent are ALLOWED
// together — qwen3.5-4b-agent stays as the tier's ROLLBACK seat but must lose the
// `agent-seat` alias so the two entries never collide. Exactly the mirror of
// TestMimoAndQwen359BRenderTogetherQwenLosesTheAlias below, one weight class down.
func TestMimoAndQwen354BRenderTogetherQwenLosesTheAlias(t *testing.T) {
	for _, tc := range mimoTemplates {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			p := params()
			p.IncludeMimo9B = true
			p.IncludeQ354B = true
			out, err := Render(string(b), p)
			if err != nil {
				t.Fatalf("include_mimo_9b + include_qwen35_4b must render together: %v", err)
			}
			mimoBlock := seatBlockOf(out, "mimo-9b-agent")
			if strings.TrimSpace(mimoBlock) == "" {
				t.Fatal("mimo-9b-agent block is missing")
			}
			if !strings.Contains(mimoBlock, "agent-seat") {
				t.Errorf("mimo-9b-agent must hold the agent-seat alias when both seats render. Block:\n%s", mimoBlock)
			}
			q354Block := seatBlockOf(out, "qwen3.5-4b-agent")
			if strings.TrimSpace(q354Block) == "" {
				t.Fatal("qwen3.5-4b-agent block is missing — it must stay rendered as the rollback seat")
			}
			if strings.Contains(q354Block, "agent-seat") {
				t.Errorf("qwen3.5-4b-agent must lose the agent-seat alias once mimo-9b-agent renders (duplicate alias otherwise). Block:\n%s", q354Block)
			}
			if !strings.Contains(q354Block, "qwen35-4b") {
				t.Errorf("qwen3.5-4b-agent must keep its own qwen35-4b alias. Block:\n%s", q354Block)
			}
			// Every alias must be unique across the whole rendered config: llama-swap
			// rejects a config with a duplicate at startup.
			assertNoDuplicateAliases(t, out)
		})
	}
}

// TestMimoWithBothSmallerSeatsStillRefusedOnTheirOwnRule pins that adding the new
// mimo/4B pairing did NOT loosen the pre-existing, UNRELATED 4B/9B mutual
// exclusion (they share `agent-seat` with each other too, independent of mimo):
// include_mimo_9b does not make include_qwen35_4b + include_qwen35_9b together any
// more legal than it already wasn't.
func TestMimoWithBothSmallerSeatsStillRefusedOnTheirOwnRule(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "llama-swap.win-cuda.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p := params()
	p.IncludeMimo9B = true
	p.IncludeQ354B = true
	p.IncludeQ359B = true
	_, err = Render(string(b), p)
	if err == nil || !strings.Contains(err.Error(), "agent-seat") {
		t.Fatalf("include_qwen35_4b + include_qwen35_9b must still be refused naming the shared alias (independent of include_mimo_9b), got %v", err)
	}
}

// TestMimoAndQwen359BRenderTogetherQwenLosesTheAlias is the acceptance case the
// spec calls out explicitly: unlike the 4B/9B pair, mimo-9b-agent and
// qwen3.5-9b-agent are ALLOWED together — qwen3.5-9b-agent stays as the rollback
// seat but must lose the `agent-seat` alias so the two entries never collide.
func TestMimoAndQwen359BRenderTogetherQwenLosesTheAlias(t *testing.T) {
	for _, tc := range mimoAndQ359BTemplates {
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
