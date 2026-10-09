package servingtmpl

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// embeddinggemma2 (EmbeddingGemma-2 Q8_0 + its multimodal projector) as a second MEMORY-STACK embedder.
// These tests pin what the memory-stack session measured (the flags), what the stack's residency
// owes it (a member of the stack's set, resident among the swappable seats, never swapped by them),
// and that the flag-off render is the render that existed before the entry did. Each says in its name
// what it pins.

// eg2Templates is every shipped template that renders the memory stack, with whether it runs on a GPU.
var eg2Templates = []struct {
	name, file string
	gpu        bool
}{
	{"linux-cuda", "llama-swap.linux-cuda.yaml", true},
	{"linux-vulkan", "llama-swap.linux-vulkan.yaml", true},
	{"linux-cpu", "llama-swap.linux-cpu.yaml", false},
	{"win-cuda", "llama-swap.win-cuda.yaml", true},
	{"win-cuda-resident", "llama-swap.win-cuda-resident.yaml", true},
	{"win-cpu", "llama-swap.win-cpu.yaml", false},
	{"win-vulkan", "llama-swap.win-vulkan.yaml", true},
	{"win-dual-cuda", "llama-swap.win-dual-cuda.yaml", true},
	{"win-dual-blackwell", "llama-swap.win-dual-blackwell.yaml", true},
	{"win-triple-blackwell", "llama-swap.win-triple-blackwell.yaml", true},
}

func eg2Template(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", file))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// eg2Params are render params every one of those templates accepts, with the stack member on or off.
func eg2Params(on bool) Params {
	p := params()
	p.Include26B = false
	p.IncludeEG2 = on
	return p
}

var stackSetRe = regexp.MustCompile(`(?m)^\s{4}(residents?):\s*"([^"]*)"`)

// stackSet returns the residency set that carries the memory stack in a rendered config: the
// `residents` set where the template separates residents from the swappable seats, or the single
// `resident` set of an all-resident template.
func stackSet(t *testing.T, rendered string) string {
	t.Helper()
	m := stackSetRe.FindStringSubmatch(rendered)
	if m == nil {
		t.Fatalf("the rendered config has no residents/resident set:\n%s", rendered)
	}
	return m[2]
}

// TestEmbeddingGemma2IsTheMeasuredStackEntryOnEveryTemplate pins the entry the memory-stack session
// measured on the reference 6 GB node, verbatim, on every template that renders the stack: the two
// GGUFs, the embedding flags, the 4096 / 4096 / 2048 sizing, ttl 300 and NO aliases (the memory stack
// selects the id). It is an embedder, so like embeddinggemma it bypasses ${common}: no --jinja, no
// reasoning flag. The CPU templates run the same entry without -ngl and --flash-attn, as their
// embeddinggemma entry runs without -ngl.
func TestEmbeddingGemma2IsTheMeasuredStackEntryOnEveryTemplate(t *testing.T) {
	for _, tc := range eg2Templates {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Render(eg2Template(t, tc.file), eg2Params(true))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			block := seatBlockOf(out, "embeddinggemma2")
			if strings.TrimSpace(block) == "" {
				t.Fatalf("no embeddinggemma2 block in the rendered config:\n%s", out)
			}
			for _, want := range []string{
				"/srv/offload/models/embeddinggemma-2-Q8_0.gguf",
				"--mmproj /srv/offload/models/mmproj-embeddinggemma-2-Q8_0.gguf",
				"--embeddings --pooling mean",
				"--ctx-size 4096", "--batch-size 4096", "--ubatch-size 2048",
				"--host 127.0.0.1", "--port ${PORT}",
				"ttl: 300",
			} {
				if !strings.Contains(block, want) {
					t.Errorf("the embeddinggemma2 entry lost %q. Block:\n%s", want, block)
				}
			}
			if tc.gpu {
				for _, want := range []string{"--n-gpu-layers 99", "--flash-attn on"} {
					if !strings.Contains(block, want) {
						t.Errorf("the GPU embeddinggemma2 entry lost %q. Block:\n%s", want, block)
					}
				}
			} else {
				for _, bad := range []string{"-ngl", "--n-gpu-layers", "--flash-attn"} {
					if strings.Contains(block, bad) {
						t.Errorf("the CPU embeddinggemma2 entry carries %q, which the CPU build has no use for. Block:\n%s", bad, block)
					}
				}
				if !strings.Contains(block, "--threads 8") {
					t.Errorf("the CPU embeddinggemma2 entry must carry the tier's threads, like its embeddinggemma entry. Block:\n%s", block)
				}
			}
			// An embedder: not via ${common}, no chat flags, and no alias for the memory stack to collide on.
			for _, bad := range []string{"${common}", "--jinja", "--reasoning", "aliases:", "--cache-ram", "-ngl 0", "--n-gpu-layers 0"} {
				if strings.Contains(block, bad) {
					t.Errorf("the embeddinggemma2 entry carries %q, which an alias-less embedder must not. Block:\n%s", bad, block)
				}
			}
			if strings.Contains(block, "__") {
				t.Errorf("the embeddinggemma2 entry still carries a template token. Block:\n%s", block)
			}
			if vs := Audit(out); len(vs) != 0 {
				t.Errorf("the config with embeddinggemma2 breaks a base rule (ttl 300, no -ngl 0, ...): %s", Violations(vs))
			}
		})
	}
}

// TestEmbeddingGemma2JoinsTheStacksResidencySetBesideEmbeddinggemma: it is a member of the SAME set
// embeddinggemma sits in (the residents set, or the single resident set of an all-resident template),
// declared by a matrix var of its own with the stack's evict cost, and never an alternative in the
// swappable set. The 300M entry stays in the set and in the roster.
func TestEmbeddingGemma2JoinsTheStacksResidencySetBesideEmbeddinggemma(t *testing.T) {
	for _, tc := range eg2Templates {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Render(eg2Template(t, tc.file), eg2Params(true))
			if err != nil {
				t.Fatal(err)
			}
			set := stackSet(t, out)
			if !strings.Contains(set, "eg2") || !strings.Contains(set, "emb") {
				t.Errorf("the stack's residency set must carry emb and eg2, got %q", set)
			}
			if !regexp.MustCompile(`\beg2\b`).MatchString(set) {
				t.Errorf("eg2 must be a set member of its own, got %q", set)
			}
			if !strings.Contains(out, "\n    eg2: embeddinggemma2\n") {
				t.Errorf("the matrix var for the entry is missing:\n%s", out)
			}
			if !strings.Contains(out, "\n    eg2: 1000\n") {
				t.Errorf("the entry is a stack member and carries the stack's evict cost:\n%s", out)
			}
			// the swappable/interactive set never offers it as an alternative to the chat seats
			for _, ln := range strings.Split(out, "\n") {
				t2 := strings.TrimSpace(ln)
				if (strings.HasPrefix(t2, "interactive:") || strings.HasPrefix(t2, "text:")) && regexp.MustCompile(`\beg2\b`).MatchString(t2) {
					t.Errorf("the entry must be resident, never a swappable alternative, got %q", t2)
				}
			}
			// the 300M embedder is untouched
			if !strings.Contains(seatBlockOf(out, "embeddinggemma"), "embeddinggemma-300") {
				t.Errorf("the embeddinggemma (300M) entry must stay on every template:\n%s", out)
			}
			if !strings.Contains(out, "\n    emb: embeddinggemma\n") {
				t.Errorf("the emb var must stay:\n%s", out)
			}
		})
	}
}

// TestDroppingEmbeddingGemma2LeavesTheRenderThatExistedBeforeIt: with the flag off the entry, its
// matrix var, its evict row and its set member are all gone, so a tier that does not carry the
// entry renders as it did before: no `eg2` in any functional line, the stack set as it was. The
// evict row is the one dropModel cannot see (it matches the var's VALUE), so it is pinned by name.
func TestDroppingEmbeddingGemma2LeavesTheRenderThatExistedBeforeIt(t *testing.T) {
	for _, tc := range eg2Templates {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Render(eg2Template(t, tc.file), eg2Params(false))
			if err != nil {
				t.Fatal(err)
			}
			functional := nonCommentLines(out)
			for _, gone := range []string{"embeddinggemma2", "embeddinggemma-2", "mmproj-embeddinggemma", "__EG2"} {
				if strings.Contains(functional, gone) {
					t.Errorf("a render without the entry still mentions %q in a functional line:\n%s", gone, out)
				}
			}
			if regexp.MustCompile(`(?m)^\s+eg2:`).MatchString(functional) {
				t.Errorf("a render without the entry still declares an eg2 var or evict row (a dangling evict cost):\n%s", out)
			}
			if regexp.MustCompile(`\beg2\b`).MatchString(stackSet(t, out)) {
				t.Errorf("the stack set still names eg2: %q", stackSet(t, out))
			}
			// the stack as it was
			for _, want := range []string{"\n    emb: embeddinggemma\n", "\n    emb: 1000\n"} {
				if !strings.Contains(out, want) {
					t.Errorf("the pre-existing stack lost %q:\n%s", want, out)
				}
			}
		})
	}
	// and the exact stack sets the existing memory-stack tests pin
	out, err := Render(eg2Template(t, "llama-swap.linux-cuda.yaml"), eg2Params(false))
	if err != nil {
		t.Fatal(err)
	}
	if got := stackSet(t, out); got != "emb & rer" {
		t.Errorf("linux-cuda stack set = %q, want %q", got, "emb & rer")
	}
	out, err = Render(eg2Template(t, "llama-swap.win-triple-blackwell.yaml"), eg2Params(false))
	if err != nil {
		t.Fatal(err)
	}
	if got := stackSet(t, out); got != "emb & rer" {
		t.Errorf("win-triple-blackwell stack set = %q, want %q", got, "emb & rer")
	}
}

// TestEmbeddingGemma2StackSetsAreExactlyTheMemoryStackPlusIt pins the rendered set strings, so a
// token placed in the wrong spot (inside a swappable alternation, or after the seat fragment) cannot
// pass on a regex that only looks for the word.
func TestEmbeddingGemma2StackSetsAreExactlyTheMemoryStackPlusIt(t *testing.T) {
	want := map[string]string{
		"linux-cuda":           "emb & rer & eg2",
		"linux-vulkan":         "emb & rer & eg2",
		"linux-cpu":            "emb & eg2",
		"win-cuda":             "emb & eg2",
		"win-vulkan":           "emb & eg2",
		"win-cpu":              "emb & eg2",
		"win-dual-blackwell":   "emb & rer & eg2",
		"win-triple-blackwell": "emb & rer & eg2",
		// the all-resident templates: one resident set, every member co-resident
		"win-cuda-resident": "emb & eg2 & e4b & e2b",
		"win-dual-cuda":     "emb & eg2 & e4b & e2b",
	}
	for _, tc := range eg2Templates {
		out, err := Render(eg2Template(t, tc.file), eg2Params(true))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := stackSet(t, out); got != want[tc.name] {
			t.Errorf("%s: the stack set = %q, want %q", tc.name, got, want[tc.name])
		}
	}
}

// TestIncludeEmbeddingGemma2OnAnEntrylessTemplateIsRefused: rk3588 serves no embedder, so a tier
// that asked for the stack member against it must be refused by name rather than rendered without it.
func TestIncludeEmbeddingGemma2OnAnEntrylessTemplateIsRefused(t *testing.T) {
	tmpl := eg2Template(t, "llama-swap.linux-rk3588.yaml")
	p := eg2Params(true)
	_, err := Render(tmpl, p)
	if err == nil || !strings.Contains(err.Error(), "embeddinggemma2") || !strings.Contains(err.Error(), "include_embeddinggemma2") {
		t.Fatalf("IncludeEG2 against a template with no entry must be refused by name, got %v", err)
	}
	// (With the flag off, rk3588 renders only with a tier seat, which has nothing to do with this
	// entry; the flag-off render of every template that has the entry is pinned above.)
}

// TestEmbeddingGemma2UnloadsAfterFiveIdleMinutesLikeEveryEntry: ttl 300 on the entry, in the shipped
// template text (not only in a render), so the operator's idle-unload rule holds for the new entry too.
func TestEmbeddingGemma2UnloadsAfterFiveIdleMinutesLikeEveryEntry(t *testing.T) {
	for _, tc := range eg2Templates {
		raw := eg2Template(t, tc.file)
		block := seatBlockOf(raw, "embeddinggemma2")
		if strings.TrimSpace(block) == "" {
			t.Errorf("%s: the shipped template defines no embeddinggemma2 entry", tc.name)
			continue
		}
		if !regexp.MustCompile(`(?m)^\s+ttl:\s*300\b`).MatchString(block) {
			t.Errorf("%s: the embeddinggemma2 entry has no ttl 300:\n%s", tc.name, block)
		}
	}
}

// eg2TextParams are the render params of a TEXT-ONLY replica: the entry on, its projector off.
func eg2TextParams() Params {
	p := eg2Params(true)
	p.EG2TextOnly = true
	return p
}

// eg2ProjectorArg is the projector argument as the shipped templates carry it once __MODELS__ is
// resolved against eg2Params' models directory, with the trailing space the strip removes.
const eg2ProjectorArg = "--mmproj /srv/offload/models/mmproj-embeddinggemma-2-Q8_0.gguf "

// eg2CmdScalar returns the lines of an entry's `cmd: >-` folded scalar (between `cmd: >-` and the next
// key of the entry), and the folded command line they read as.
func eg2CmdScalar(t *testing.T, block string) (lines []string, folded string) {
	t.Helper()
	in := false
	for _, ln := range strings.Split(block, "\n") {
		switch {
		case strings.TrimSpace(ln) == "cmd: >-":
			in = true
		case in && strings.HasPrefix(ln, "      "):
			lines = append(lines, ln)
		case in:
			in = false
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no cmd scalar in the entry:\n%s", block)
	}
	return lines, strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// TestEmbeddingGemma2TextOnlyIsTheAuthoritysEntryMinusTheProjectorOnEveryTemplate: a replica embeds text
// only and its card cannot hold the projector, so its entry is the authority's entry with the one
// `--mmproj <path>` argument removed and NOTHING else changed. Checked three ways on every template
// that renders the stack: the whole rendered config differs from the with-projector render by exactly
// that argument (so no set, var, evict row or neighbouring entry moved), the entry's folded command
// line differs by exactly that argument (so the YAML fold did not turn the stripped line into literal
// text), and the load-bearing flags are all there (ubatch 2048 is the one a smaller default breaks:
// the stack's hot budget is 1,900 tokens and a smaller ubatch returns HTTP 500 on long memories).
func TestEmbeddingGemma2TextOnlyIsTheAuthoritysEntryMinusTheProjectorOnEveryTemplate(t *testing.T) {
	for _, tc := range eg2Templates {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := eg2Template(t, tc.file)
			with, err := Render(tmpl, eg2Params(true))
			if err != nil {
				t.Fatalf("render with the projector: %v", err)
			}
			text, err := Render(tmpl, eg2TextParams())
			if err != nil {
				t.Fatalf("render text-only: %v", err)
			}
			withBlock, textBlock := seatBlockOf(with, "embeddinggemma2"), seatBlockOf(text, "embeddinggemma2")
			if strings.TrimSpace(textBlock) == "" {
				t.Fatalf("a text-only tier still renders the entry; none found:\n%s", text)
			}
			if got := strings.Count(withBlock, eg2ProjectorArg); got != 1 {
				t.Fatalf("the authority's entry carries the projector argument %d times, want 1:\n%s", got, withBlock)
			}
			if strings.Contains(textBlock, "mmproj") {
				t.Errorf("the text-only entry still names a projector:\n%s", textBlock)
			}
			if want := strings.Replace(withBlock, eg2ProjectorArg, "", 1); textBlock != want {
				t.Errorf("the text-only entry is not the authority's entry minus the projector argument.\n got:\n%s\nwant:\n%s", textBlock, want)
			}
			if want := strings.Replace(with, eg2ProjectorArg, "", 1); text != want {
				t.Errorf("the text-only render differs from the with-projector render by more than the projector argument")
			}
			// the folded command line: the same words minus `--mmproj <path>`, in one scalar at one indent
			withLines, withCmd := eg2CmdScalar(t, withBlock)
			textLines, textCmd := eg2CmdScalar(t, textBlock)
			if want := strings.Replace(withCmd, strings.TrimSpace(eg2ProjectorArg)+" ", "", 1); textCmd != want {
				t.Errorf("folded command lines differ by more than the projector argument.\n got: %s\nwant: %s", textCmd, want)
			}
			if len(textLines) != len(withLines) {
				t.Errorf("the strip changed the number of lines in the cmd scalar: %d -> %d", len(withLines), len(textLines))
			}
			indent := len(textLines[0]) - len(strings.TrimLeft(textLines[0], " "))
			for _, ln := range textLines {
				if got := len(ln) - len(strings.TrimLeft(ln, " ")); got != indent {
					t.Errorf("a cmd line of the text-only entry is indented %d, the scalar's first line %d: a more-indented line in a folded scalar is literal text and keeps its newline:\n%s", got, indent, strings.Join(textLines, "\n"))
				}
			}
			for _, want := range []string{
				"/srv/offload/models/embeddinggemma-2-Q8_0.gguf",
				"--embeddings --pooling mean",
				"--ctx-size 4096", "--batch-size 4096", "--ubatch-size 2048",
				"--host 127.0.0.1", "--port ${PORT}", "ttl: 300",
			} {
				if !strings.Contains(textBlock, want) {
					t.Errorf("the text-only entry lost %q:\n%s", want, textBlock)
				}
			}
			if tc.gpu {
				for _, want := range []string{"--n-gpu-layers 99", "--flash-attn on"} {
					if !strings.Contains(textBlock, want) {
						t.Errorf("the GPU text-only entry lost %q:\n%s", want, textBlock)
					}
				}
			}
			// it is still a stack member: same set, var and evict cost as the authority's render
			if stackSet(t, text) != stackSet(t, with) {
				t.Errorf("the text-only entry's stack set %q differs from the authority's %q", stackSet(t, text), stackSet(t, with))
			}
			if vs := Audit(text); len(vs) != 0 {
				t.Errorf("the text-only render breaks a base rule: %s", Violations(vs))
			}
		})
	}
}

// TestEmbeddingGemma2TextOnlyLeavesTheOtherProjectorsAlone: win-triple-blackwell and win-dual-blackwell
// carry a --mmproj on their Qwen3.8-27B entries. The text-only strip is scoped to the embeddinggemma2
// entry; every other projector in the config survives it, byte for byte.
func TestEmbeddingGemma2TextOnlyLeavesTheOtherProjectorsAlone(t *testing.T) {
	for _, file := range []string{"llama-swap.win-triple-blackwell.yaml", "llama-swap.win-dual-blackwell.yaml"} {
		tmpl := eg2Template(t, file)
		p := eg2TextParams()
		p.IncludeQ38 = true
		text, err := Render(tmpl, p)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if !strings.Contains(text, "--mmproj /srv/offload/models/mmproj-Qwen3.8-27B-F16.gguf") {
			t.Errorf("%s: the 27B's projector was stripped along with the embedder's:\n%s", file, text)
		}
		if strings.Contains(seatBlockOf(text, "embeddinggemma2"), "mmproj") {
			t.Errorf("%s: the embedder's projector survived", file)
		}
	}
}

// TestEmbeddingGemma2TextOnlyWithoutTheStackMemberChangesNothing: the field means something only with
// the entry; on a tier that does not carry it the render is the flag-off render, not an error.
func TestEmbeddingGemma2TextOnlyWithoutTheStackMemberChangesNothing(t *testing.T) {
	for _, tc := range eg2Templates {
		tmpl := eg2Template(t, tc.file)
		off, err := Render(tmpl, eg2Params(false))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		p := eg2Params(false)
		p.EG2TextOnly = true
		got, err := Render(tmpl, p)
		if err != nil {
			t.Fatalf("%s: text-only without the entry must render the flag-off config, got %v", tc.name, err)
		}
		if got != off {
			t.Errorf("%s: EG2TextOnly changed a render that carries no entry", tc.name)
		}
	}
}

// TestTextOnlyOnAnEntrylessTemplateIsStillRefusedByName: the text-only strip must not pre-empt the
// refusal-by-name for a template with no entry (rk3588 serves no embedder).
func TestTextOnlyOnAnEntrylessTemplateIsStillRefusedByName(t *testing.T) {
	_, err := Render(eg2Template(t, "llama-swap.linux-rk3588.yaml"), eg2TextParams())
	if err == nil || !strings.Contains(err.Error(), "include_embeddinggemma2") {
		t.Fatalf("a text-only request against a template with no entry must be refused by name, got %v", err)
	}
}

// TestDropEG2ProjectorIsExactAndScopedToTheEntry pins the strip's own refusals on synthetic
// templates, so a template edit that moves the projector argument fails the render by name
// instead of shipping an entry that still loads the projector, or a bare flag with no path.
func TestDropEG2ProjectorIsExactAndScopedToTheEntry(t *testing.T) {
	entry := func(cmd string) string {
		return "models:\n" +
			"  other:\n" +
			"    cmd: >-\n" +
			"      llama-server --mmproj __MODELS__/mmproj-embeddinggemma-2-Q8_0.gguf --x\n" +
			"  embeddinggemma2:\n" +
			"    cmd: >-\n" + cmd +
			"    ttl: 300\n" +
			"  after:\n" +
			"    cmd: >-\n" +
			"      llama-server --mmproj __MODELS__/mmproj-embeddinggemma-2-Q8_0.gguf --y\n"
	}
	ok := entry("      llama-server --model m.gguf\n      --mmproj __MODELS__/mmproj-embeddinggemma-2-Q8_0.gguf --embeddings\n")
	got, err := dropEG2Projector(ok)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(ok, "--mmproj __MODELS__/mmproj-embeddinggemma-2-Q8_0.gguf --embeddings", "--embeddings", 1)
	if got != want {
		t.Errorf("only the embeddinggemma2 entry's projector argument may go.\n got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Count(got, "--mmproj") != 2 {
		t.Errorf("the neighbours' projector arguments must survive, found %d --mmproj in:\n%s", strings.Count(got, "--mmproj"), got)
	}
	for name, bad := range map[string]string{
		"twice":   entry("      llama-server --mmproj __MODELS__/mmproj-embeddinggemma-2-Q8_0.gguf --embeddings --mmproj __MODELS__/mmproj-embeddinggemma-2-Q8_0.gguf --z\n"),
		"absent":  entry("      llama-server --model m.gguf --embeddings\n"),
		"renamed": entry("      llama-server --mmproj __MODELS__/mmproj-embeddinggemma-3-Q8_0.gguf --embeddings\n"),
		"eol":     entry("      llama-server --model m.gguf --mmproj __MODELS__/mmproj-embeddinggemma-2-Q8_0.gguf\n      --embeddings\n"),
		"extra":   entry("      llama-server --model m.gguf --mmproj __MODELS__/mmproj-embeddinggemma-2-Q8_0.gguf --embeddings\n      --mmproj /other/projector.gguf\n"),
	} {
		if out, err := dropEG2Projector(bad); err == nil {
			t.Errorf("%s: a template whose projector argument is not the one the renderer removes must be refused, got:\n%s", name, out)
		}
	}
}
