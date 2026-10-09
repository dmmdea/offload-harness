package servingtmpl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Qwen3.6-35B-A3B RAM-spill agent seat (ADR 0080). These tests pin what the 2026-10-07/08
// lean bake MEASURED (the flags), what the renderer owes the operator's three spill gates
// (the entry is dropped whole when the box cannot hold it), and the alias handoff that keeps
// `agent-seat` unique. Each test states in its name what it pins.

// q3635bTemplates are the two shipped templates that carry the entry: the cuda backend renders
// on both operating systems for ampere-6, and a tier is a hardware class, not a Windows class.
var q3635bTemplates = []struct{ name, file string }{
	{"linux-cuda", "llama-swap.linux-cuda.yaml"},
	{"win-cuda", "llama-swap.win-cuda.yaml"},
}

func q3635bTemplate(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", file))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestQwen3635BSpillSeatKeepsEveryMeasuredFlag: the entry is the exact configuration the bake
// ran, written as literals. Each flag below was part of the measurement; changing one without a
// re-bake ships an unmeasured seat, and two of them (the 0 cache and the load mode) are the
// difference between the seat being a RAM hog and not.
func TestQwen3635BSpillSeatKeepsEveryMeasuredFlag(t *testing.T) {
	for _, tc := range q3635bTemplates {
		t.Run(tc.name, func(t *testing.T) {
			p := params()
			p.IncludeQ3635B = true
			// A window and a KV type DIFFERENT from the seat's literals, and a nonzero tier
			// cache: a block that quietly followed the tier macros would show these instead.
			p.Ctx, p.KVType, p.CacheRAMMiB = 8192, "f16", 16384
			out, err := Render(q3635bTemplate(t, tc.file), p)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			block := seatBlockOf(out, "qwen3.6-35b-a3b-agent")
			if strings.TrimSpace(block) == "" {
				t.Fatal("no qwen3.6-35b-a3b-agent block in the rendered config")
			}
			want := []string{
				"/srv/offload/models/Qwen3.6-35B-A3B/Qwen3.6-35B-A3B-UD-IQ3_XXS.gguf", // the weight, in its subdirectory
				"--n-cpu-moe 40",  // the measured spill: 40 expert layers in host RAM
				"--flash-attn on", // literal, not the tier macro
				"--cache-type-k q8_0", "--cache-type-v q8_0",
				"--ctx-size 32768", // the tier's agent_ctx_tokens
				"--jinja",
				"--threads 8", // the spilled experts run on these
				"--batch-size 2048", "--ubatch-size 512",
				"--parallel 1",     // single slot: fleet_max_concurrent_jobs stays 1
				"--cache-ram 0",    // the host prompt cache grew ~1 GiB per run in the bake
				"--load-mode none", // llama.cpp >= b10964; replaced --no-mmap
				"--host 127.0.0.1",
				"ttl: 300",
				"aliases: [qwen36-35b, agent-seat]",
			}
			for _, w := range want {
				if !strings.Contains(block, w) {
					t.Errorf("the spill seat lost %q. Block:\n%s", w, block)
				}
			}
			if tc.name == "linux-cuda" {
				if !strings.Contains(block, "--n-gpu-layers 99") || !strings.Contains(block, `env: ["${ld}"]`) {
					t.Errorf("the linux entry must offload 99 layers and carry the loader-path macro. Block:\n%s", block)
				}
			} else if !strings.Contains(block, "-ngl 99") {
				t.Errorf("the windows entry must offload 99 layers. Block:\n%s", block)
			}
			// Flags the seat must NOT carry: a THINKING model (the ${common} macro's
			// `--reasoning off` is the 4B's measured collapse trap), the retired mmap flag,
			// and anything that would follow the tier instead of the measurement.
			for _, bad := range []string{"--reasoning", "--no-mmap", "--cpu-moe", "-ngl 0", "--n-gpu-layers 0", "--cache-ram 16384", "--ctx-size 8192", "--cache-type-k f16"} {
				if strings.Contains(block, bad) {
					t.Errorf("the spill seat carries %q, which is not the measured configuration. Block:\n%s", bad, block)
				}
			}
			if strings.Contains(block, "__") {
				t.Errorf("the spill seat still carries a template token. Block:\n%s", block)
			}
		})
	}
}

// TestQwen3635BSpillIsAuditedAgainstTheTiersMeasuredSpill: the rendered entry carries a
// `--n-cpu-moe 40` and the INV-1 spill audit (the write gate's rule) accepts it at a measured
// spill of 40 or more, and refuses it below that or with no measured spill declared. This is the
// audit the tier's n_cpu_moe_max: 40 is there to satisfy.
func TestQwen3635BSpillIsAuditedAgainstTheTiersMeasuredSpill(t *testing.T) {
	for _, tc := range q3635bTemplates {
		t.Run(tc.name, func(t *testing.T) {
			p := params()
			p.IncludeQ3635B = true
			out, err := Render(q3635bTemplate(t, tc.file), p)
			if err != nil {
				t.Fatal(err)
			}
			if vs := AuditSpill(out, 40); len(vs) != 0 {
				t.Errorf("a tier that measured a spill of 40 must be allowed the seat's --n-cpu-moe 40, got %s", Violations(vs))
			}
			if vs := AuditSpill(out, 39); len(vs) == 0 {
				t.Error("a measured spill of 39 must refuse the seat's --n-cpu-moe 40")
			}
			if vs := AuditSpill(out, 0); len(vs) == 0 || !strings.Contains(Violations(vs), "no measured spill") {
				t.Errorf("a tier with no measured spill must refuse the seat, naming the rule, got %s", Violations(vs))
			}
			if vs := Audit(out); len(vs) != 0 {
				t.Errorf("the rendered config with the spill seat breaks a base rule (ttl 300, no -ngl 0, ...): %s", Violations(vs))
			}
		})
	}
}

// TestQwen3635BSpillSeatIsOneMoreHeavyAlternativeNeverAResident: the seat joins the interactive
// set as one more alternative beside the other chat seats (one heavy seat on the card at a time)
// and is never a member of the residents set, which would pin 12 GB of spill in RAM and 2 GB on
// the card beside the memory stack for good.
func TestQwen3635BSpillSeatIsOneMoreHeavyAlternativeNeverAResident(t *testing.T) {
	for _, tc := range q3635bTemplates {
		t.Run(tc.name, func(t *testing.T) {
			p := params()
			p.Include26B = false
			p.IncludeQ3635B = true
			out, err := Render(q3635bTemplate(t, tc.file), p)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "    q36: qwen3.6-35b-a3b-agent\n") {
				t.Errorf("the matrix var for the seat is missing:\n%s", out)
			}
			var interactive, residents string
			for _, ln := range strings.Split(out, "\n") {
				switch {
				case strings.HasPrefix(strings.TrimSpace(ln), "interactive:"):
					interactive = strings.TrimSpace(ln)
				case strings.HasPrefix(strings.TrimSpace(ln), "residents:"):
					residents = strings.TrimSpace(ln)
				}
			}
			if interactive != `interactive: "+residents & (e4b | e2b | q36)"` {
				t.Errorf("the seat must be one more alternative in the interactive set, got %q", interactive)
			}
			if strings.Contains(residents, "q36") {
				t.Errorf("the seat must never be a resident, got %q", residents)
			}
		})
	}
}

// TestDroppingTheQwen3635BSpillSeatLeavesNoTraceOfItAnywhere: a box the RAM gate keeps the seat off
// renders exactly the tier it rendered before the seat existed. Not the entry, not its matrix var,
// not its set member, not one `--n-cpu-moe`, not its weight's name.
func TestDroppingTheQwen3635BSpillSeatLeavesNoTraceOfItAnywhere(t *testing.T) {
	for _, tc := range q3635bTemplates {
		t.Run(tc.name, func(t *testing.T) {
			p := params()
			p.Include26B = false
			p.IncludeQ354B = true
			p.IncludeQ3635B = false
			out, err := Render(q3635bTemplate(t, tc.file), p)
			if err != nil {
				t.Fatal(err)
			}
			// Functional lines only: the comment above the entry stays in a render, as every
			// dropped entry's comment does, and may name the model.
			functional := nonCommentLines(out)
			for _, gone := range []string{"qwen3.6-35b-a3b-agent", "q36", "qwen36-35b", "--n-cpu-moe", "Qwen3.6-35B-A3B", "--load-mode", "__Q3635B"} {
				if strings.Contains(functional, gone) {
					t.Errorf("a render without the spill seat still mentions %q in a functional line:\n%s", gone, out)
				}
			}
			if !strings.Contains(out, `interactive: "+residents & (e4b | e2b | q354)"`) {
				t.Errorf("the interactive set must be exactly the pre-seat one:\n%s", out)
			}
			// ...and the 4B, which the seat would have unaliased, keeps `agent-seat`.
			if !strings.Contains(seatBlockOf(out, "qwen3.5-4b-agent"), "agent-seat") {
				t.Errorf("without the spill seat the 4B is the agent seat and must hold the agent-seat alias:\n%s", out)
			}
			assertNoDuplicateAliases(t, out)
		})
	}
}

// nonCommentLines is the rendered config without its `#` comment lines.
func nonCommentLines(rendered string) string {
	var b strings.Builder
	for _, ln := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "#") {
			continue
		}
		b.WriteString(ln)
		b.WriteString("\n")
	}
	return b.String()
}

// TestQwen3635BSpillSeatIsTheOnlyEntryWithoutTheTierCacheRAM: the seat's zero host prompt cache is
// a measured exception (the cache grew ~1 GiB per run on top of 11.6 GB of experts), not a license:
// beside it, every other llama.cpp entry that states a cache still follows the tier-resolved figure
// (ADR 0056), and the seat is the only one at zero.
func TestQwen3635BSpillSeatIsTheOnlyEntryWithoutTheTierCacheRAM(t *testing.T) {
	for _, tc := range q3635bTemplates {
		t.Run(tc.name, func(t *testing.T) {
			p := params()
			p.IncludeQ354B = true
			p.IncludeQ3635B = true
			p.CacheRAMMiB = 16384
			out, err := Render(q3635bTemplate(t, tc.file), p)
			if err != nil {
				t.Fatal(err)
			}
			// The entries that write --cache-ram explicitly (the cascade seats take it from the
			// ${common} macro llama-swap expands).
			for _, model := range []string{"qwen3.6-35b-a3b-agent", "qwen3.5-4b-agent", "gemma-4-26b-agent"} {
				block := seatBlockOf(out, model)
				if strings.TrimSpace(block) == "" {
					t.Fatalf("no %s block in the render", model)
				}
				want := "--cache-ram 16384"
				if model == "qwen3.6-35b-a3b-agent" {
					want = "--cache-ram 0"
				}
				if !strings.Contains(block, want) {
					t.Errorf("%s must carry %q. Block:\n%s", model, want, block)
				}
			}
		})
	}
}

// TestQwen3635BSpillSeatTakesTheAgentSeatAliasFromTheSmallSeats: the live entry claims `agent-seat`,
// so the 4B (and the 9B) stay rendered as un-aliased opt-in rollbacks beside it, and no alias is
// ever claimed twice (llama-swap refuses a duplicate at startup).
func TestQwen3635BSpillSeatTakesTheAgentSeatAliasFromTheSmallSeats(t *testing.T) {
	for _, tc := range q3635bTemplates {
		t.Run(tc.name, func(t *testing.T) {
			p := params()
			p.Include26B = false
			p.IncludeQ354B = true
			p.IncludeQ3635B = true
			out, err := Render(q3635bTemplate(t, tc.file), p)
			if err != nil {
				t.Fatalf("include_qwen36_35b + include_qwen35_4b must render together: %v", err)
			}
			if !strings.Contains(seatBlockOf(out, "qwen3.6-35b-a3b-agent"), "agent-seat") {
				t.Error("the spill seat must hold the agent-seat alias")
			}
			small := seatBlockOf(out, "qwen3.5-4b-agent")
			if strings.TrimSpace(small) == "" {
				t.Fatal("qwen3.5-4b-agent must stay rendered as the opt-in rollback seat")
			}
			if strings.Contains(small, "agent-seat") {
				t.Errorf("the 4B must hand agent-seat to the spill seat, got:\n%s", small)
			}
			if !strings.Contains(small, "qwen35-4b") {
				t.Errorf("the 4B keeps its own alias, got:\n%s", small)
			}
			assertNoDuplicateAliases(t, out)

			// The 9B hands the alias off the same way.
			p.IncludeQ354B, p.IncludeQ359B = false, true
			out, err = Render(q3635bTemplate(t, tc.file), p)
			if err != nil {
				t.Fatalf("include_qwen36_35b + include_qwen35_9b must render together: %v", err)
			}
			if strings.Contains(seatBlockOf(out, "qwen3.5-9b-agent"), "agent-seat") {
				t.Error("the 9B must hand agent-seat to the spill seat")
			}
			assertNoDuplicateAliases(t, out)
		})
	}
}

// TestQwen3635BSpillSeatBesideMimoIsRefusedByName: mimo-9b-agent keeps its own claim on
// agent-seat, so the pair would be a duplicate alias at llama-swap startup. No tier sets both.
func TestQwen3635BSpillSeatBesideMimoIsRefusedByName(t *testing.T) {
	p := params()
	p.IncludeQ3635B = true
	p.IncludeMimo9B = true
	_, err := Render(q3635bTemplate(t, "llama-swap.linux-cuda.yaml"), p)
	if err == nil || !strings.Contains(err.Error(), "include_qwen36_35b") || !strings.Contains(err.Error(), "agent-seat") {
		t.Fatalf("include_qwen36_35b + include_mimo_9b must be refused naming the shared alias, got %v", err)
	}
}

// TestIncludeQwen3635BOnAnEntrylessTemplateIsRefused is the tripwire every gated seat has: a tier
// that asks for the seat against a template that defines none must be refused by name, not
// rendered without it while the installer downloads 12 GB for it.
func TestIncludeQwen3635BOnAnEntrylessTemplateIsRefused(t *testing.T) {
	for _, file := range []string{"llama-swap.win-cuda-resident.yaml", "llama-swap.linux-vulkan.yaml", "llama-swap.linux-cpu.yaml"} {
		t.Run(file, func(t *testing.T) {
			p := params()
			p.IncludeQ3635B = true
			_, err := Render(q3635bTemplate(t, file), p)
			if err == nil || !strings.Contains(err.Error(), "qwen3.6-35b-a3b-agent") || !strings.Contains(err.Error(), "include_qwen36_35b") {
				t.Fatalf("IncludeQ3635B against an entryless template must be refused by name, got %v", err)
			}
			p.IncludeQ3635B = false
			if _, err := Render(q3635bTemplate(t, file), p); err != nil {
				t.Fatalf("IncludeQ3635B=false must still render an entryless template: %v", err)
			}
		})
	}
}

// TestDroppingTheQwen3827BAgentEntryKeepsTheFirstLineOfTheSpillSeatsComment pins a rendering
// quirk the spill seat's comment block must be written around: dropModel ends a skipped block at
// the next two-space line that contains a colon, so a comment block directly after a dropped entry
// loses every leading line without one. The 6GB tier drops qwen38-27b-agent, and the spill seat's
// block (which follows it) began mid-sentence in the rendered config. Each template's block must
// therefore open with a line that contains a colon, which this test checks by rendering the
// templates with the 27B entry dropped and looking for the block's opening line.
func TestDroppingTheQwen3827BAgentEntryKeepsTheFirstLineOfTheSpillSeatsComment(t *testing.T) {
	for _, tc := range q3635bTemplates {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := q3635bTemplate(t, tc.file)
			lines := strings.Split(strings.ReplaceAll(tmpl, "\r\n", "\n"), "\n")
			key := -1
			for i, ln := range lines {
				if strings.HasPrefix(ln, "  qwen3.6-35b-a3b-agent:") {
					key = i
					break
				}
			}
			if key < 0 {
				t.Fatal("the template has no qwen3.6-35b-a3b-agent entry")
			}
			first := key
			for first > 0 && strings.HasPrefix(lines[first-1], "  #") {
				first--
			}
			if first == key {
				t.Fatal("the spill seat's entry has no comment block above it: this check went blind")
			}
			opening := lines[first]
			p := params()
			p.Include26B = false
			p.IncludeQ3635B = true
			p.IncludeQ3827B = false
			out, err := Render(tmpl, p)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.Contains(out, opening) {
				t.Errorf("dropping the 27B agent entry swallowed the opening line of the spill seat's comment block (%q): give that line a colon, because dropModel ends a skipped block at the next two-space line that has one", strings.TrimSpace(opening))
			}
		})
	}
}
