package servingtmpl

import (
	"strings"
	"testing"
)

// A node whose main llama.cpp build is older than b11452 cannot load the gemma-embedding2 architecture
// but must keep that build for every other seat. Params.EG2LlamaBin points ONLY the embeddinggemma2 entry
// at a newer build (retargetEG2Bin). These tests pin what the rewrite touches (that entry and, on a Linux
// template, its loader macro), what it must never touch (every other entry, every unset render), and the
// shapes it refuses loudly. Each says in its name what it pins.

// eg2BinOwn is the entry's own build in these tests; params() carries the main build.
const eg2BinOwn = "/opt/llama-b11490"

var (
	eg2LinuxTemplates   = []string{"linux-cuda", "linux-vulkan", "linux-cpu"}
	eg2WindowsTemplates = []string{"win-cuda", "win-cuda-resident", "win-cpu", "win-vulkan", "win-dual-cuda", "win-dual-blackwell", "win-triple-blackwell"}
)

func eg2TemplateFile(t *testing.T, name string) string {
	t.Helper()
	for _, tc := range eg2Templates {
		if tc.name == name {
			return eg2Template(t, tc.file)
		}
	}
	t.Fatalf("no such template %q", name)
	return ""
}

// eg2BinRender renders one template with the stack member on, the projector kept or stripped, and the
// entry's own build set (empty = unset). goos is the render target; "" is what params() carries.
func eg2BinRender(t *testing.T, name, goos string, textOnly bool, own string) string {
	t.Helper()
	p := eg2Params(true)
	p.EG2TextOnly, p.GOOS, p.EG2LlamaBin = textOnly, goos, own
	out, err := Render(eg2TemplateFile(t, name), p)
	if err != nil {
		t.Fatalf("%s (goos %q, text-only %v, own build %q): %v", name, goos, textOnly, own, err)
	}
	return out
}

// isServerCmd is true for a rendered line that launches a llama-server (a comment that mentions one is not).
func isServerCmd(l string) bool {
	t := strings.TrimSpace(l)
	return !strings.HasPrefix(t, "#") && strings.Contains(t, "/llama-server")
}

func targetOf(name string) string {
	if strings.HasPrefix(name, "win-") {
		return "windows"
	}
	return "linux"
}

// oldEG2Scanner is the block walk dropEG2Projector carried inline before it was shared, kept verbatim as
// the reference the shared scanner is held to.
func oldEG2Scanner(lines []string) []bool {
	mark := make([]bool, len(lines))
	inBlock := false
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "  "+modelEG2+":"):
			inBlock = true
			continue
		case inBlock && l != "" && !strings.HasPrefix(l, " "):
			inBlock = false
		case inBlock && strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.Contains(l, ":"):
			inBlock = false
		}
		mark[i] = inBlock
	}
	return mark
}

// TestTheSharedEG2ScannerKeepsTheBoundariesDropEG2ProjectorHad: the block walk was factored out of
// dropEG2Projector so the projector strip and the build retarget share one definition of where the entry
// ends. This holds the shared walk to the old one, line for line, on every shipped template and on the shapes
// the boundary rules exist for (the entry last in the models map; a column-0 section right after it).
func TestTheSharedEG2ScannerKeepsTheBoundariesDropEG2ProjectorHad(t *testing.T) {
	check := func(name, text string) {
		t.Helper()
		lines := strings.Split(text, "\n")
		got, want := eg2BlockLines(lines), oldEG2Scanner(lines)
		inside := 0
		for i := range lines {
			if got[i] != want[i] {
				t.Errorf("%s line %d (%q): shared scanner says %v, the old walk said %v", name, i+1, lines[i], got[i], want[i])
			}
			if got[i] {
				inside++
			}
		}
		if inside == 0 {
			t.Errorf("%s: no line is inside the embeddinggemma2 block, so the comparison proved nothing", name)
		}
	}
	for _, tc := range eg2Templates {
		check(tc.name, eg2Template(t, tc.file))
	}
	check("entry last, then a column-0 section", "models:\n  a:\n    cmd: x\n  embeddinggemma2:\n    env: [\"${ld}\"]\n    cmd: >-\n      __LLAMA_BIN__/llama-server\n\ngroups:\n  g:\n    members: [a]\n")
	check("entry between two siblings", "models:\n  a:\n    cmd: x\n  embeddinggemma2:\n    cmd: y\n    ttl: 300\n  b:\n    cmd: z\n")
}

// TestEG2BinUnsetRendersByteIdentically is the promise that makes the field free to add: a render that gives
// the entry no build of its own (empty, or the main build spelled again, with or without a trailing slash)
// is the render every earlier release produced, so no node's config changes and no stamp moves. Every
// template, with and without the projector, on the template's own target and on the "" target the other
// tests use.
func TestEG2BinUnsetRendersByteIdentically(t *testing.T) {
	main := params().LlamaBin
	for _, tc := range eg2Templates {
		for _, textOnly := range []bool{false, true} {
			for _, goos := range []string{"", targetOf(tc.name)} {
				base := eg2BinRender(t, tc.name, goos, textOnly, "")
				for _, same := range []string{main, main + "/"} {
					if got := eg2BinRender(t, tc.name, goos, textOnly, same); got != base {
						t.Errorf("%s (goos %q, text-only %v): own build %q (= the main build) changed the render", tc.name, goos, textOnly, same)
					}
				}
				if strings.Contains(base, "ldembed") {
					t.Errorf("%s (goos %q, text-only %v): an unset render mentions ldembed", tc.name, goos, textOnly)
				}
				for _, l := range strings.Split(base, "\n") {
					if isServerCmd(l) && !strings.Contains(l, main+"/llama-server") {
						t.Errorf("%s (goos %q, text-only %v): an unset render runs a llama-server from somewhere else: %q", tc.name, goos, textOnly, l)
					}
				}
			}
		}
	}
}

// TestEG2BinMovesOnlyTheEG2Entry: with an own build set, the lines that differ from the unset render are
// the embeddinggemma2 entry's own (its cmd path and, on a template with a loader macro, its env line), plus
// ONE added macro line on Linux. Exactly one llama-server line carries the new directory and every other
// llama-server line keeps the main build.
func TestEG2BinMovesOnlyTheEG2Entry(t *testing.T) {
	main := params().LlamaBin
	for _, tc := range eg2Templates {
		for _, textOnly := range []bool{false, true} {
			for _, goos := range []string{"", targetOf(tc.name)} {
				unset := strings.Split(eg2BinRender(t, tc.name, goos, textOnly, ""), "\n")
				set := strings.Split(eg2BinRender(t, tc.name, goos, textOnly, eg2BinOwn), "\n")
				linux := targetOf(tc.name) == "linux"
				macros := 0
				var kept []string
				for _, l := range set {
					if strings.HasPrefix(l, "  ldembed: ") {
						macros++
						continue
					}
					kept = append(kept, l)
				}
				wantMacros := 0
				if linux {
					wantMacros = 1
				}
				if macros != wantMacros {
					t.Errorf("%s (goos %q): %d ldembed macro line(s), want %d", tc.name, goos, macros, wantMacros)
				}
				if len(kept) != len(unset) {
					t.Errorf("%s (goos %q): the set render has %d lines besides the macro, the unset one %d", tc.name, goos, len(kept), len(unset))
					continue
				}
				inBlock := eg2BlockLines(unset)
				diffs := 0
				for i := range unset {
					if kept[i] == unset[i] {
						continue
					}
					diffs++
					if !inBlock[i] {
						t.Errorf("%s (goos %q): line %d outside the embeddinggemma2 entry changed:\n  was: %s\n  now: %s", tc.name, goos, i+1, unset[i], kept[i])
					}
				}
				wantDiffs := 1
				if linux {
					wantDiffs = 2 // the cmd path and the env line
				}
				if diffs != wantDiffs {
					t.Errorf("%s (goos %q): %d line(s) differ, want %d", tc.name, goos, diffs, wantDiffs)
				}
				own := 0
				for i, l := range set {
					if !isServerCmd(l) {
						continue
					}
					if strings.Contains(l, eg2BinOwn+"/llama-server") {
						own++
						if strings.Contains(l, main) {
							t.Errorf("%s (goos %q): the entry's llama-server line still names the main build: %s", tc.name, goos, l)
						}
						continue
					}
					if !strings.Contains(l, main+"/llama-server") {
						t.Errorf("%s (goos %q): line %d runs a llama-server from neither build: %s", tc.name, goos, i+1, l)
					}
				}
				if own != 1 {
					t.Errorf("%s (goos %q): %d llama-server line(s) carry the entry's own build, want exactly 1", tc.name, goos, own)
				}
			}
		}
	}
	// A trailing slash on the own build renders as the bare directory, as the main build's does.
	if a, b := eg2BinRender(t, "linux-cuda", "linux", false, eg2BinOwn), eg2BinRender(t, "linux-cuda", "linux", false, eg2BinOwn+"/"); a != b {
		t.Error("a trailing slash on the entry's own build changed the render")
	}
}

// TestEG2BinLinuxLoaderMacro: on a template with a loader macro the entry gets a macro of its own, placed
// right after `ld:`, and its env swaps the one `${ld}` list item for it (the Vulkan template keeps its
// `${vk}` item). Nothing else references the new macro, a tier gpu_env still lands on the entry, and with a
// CPU family too the macros read ld, ldcpu, ldembed. The macro path keys on the template carrying `ld:`,
// not on the render target, so the "" target the other tests use takes it too.
func TestEG2BinLinuxLoaderMacro(t *testing.T) {
	macro := `  ldembed: "LD_LIBRARY_PATH=` + eg2BinOwn + `:${LD_LIBRARY_PATH:-}"`
	for _, name := range eg2LinuxTemplates {
		for _, goos := range []string{"", "linux"} {
			out := eg2BinRender(t, name, goos, false, eg2BinOwn)
			lines := strings.Split(out, "\n")
			ld, emb := -1, -1
			for i, l := range lines {
				switch {
				case strings.HasPrefix(l, "  ld: "):
					ld = i
				case l == macro:
					if emb >= 0 {
						t.Errorf("%s (goos %q): the ldembed macro is defined twice", name, goos)
					}
					emb = i
				}
			}
			if ld < 0 || emb != ld+1 {
				t.Errorf("%s (goos %q): the ldembed macro is at line %d, want right after ld: (line %d)\n%s", name, goos, emb+1, ld+1, out)
			}
			if !strings.Contains(lines[ld], "LD_LIBRARY_PATH="+params().LlamaBin+":") {
				t.Errorf("%s (goos %q): the shared ld macro no longer names the main build: %s", name, goos, lines[ld])
			}
			if n := strings.Count(out, "${ldembed}"); n != 1 {
				t.Errorf("%s (goos %q): ${ldembed} is referenced %d times, want exactly once (by the entry)", name, goos, n)
			}
			block := seatBlockOf(out, "embeddinggemma2")
			want := `env: ["${ldembed}"]`
			if name == "linux-vulkan" {
				want = `env: ["${ldembed}", "${vk}"]`
			}
			if !strings.Contains(block, want) || strings.Contains(block, `"${ld}"`) {
				t.Errorf("%s (goos %q): the entry's env is not %s. Block:\n%s", name, goos, want, block)
			}
			if vs := Audit(out); len(vs) != 0 {
				t.Errorf("%s (goos %q): the retargeted config breaks a rule: %s", name, goos, Violations(vs))
			}
		}
	}

	// A tier gpu_env is merged into the entry's env line after the swap.
	p := eg2Params(true)
	p.GOOS, p.EG2LlamaBin, p.GPUEnv = "linux", eg2BinOwn, []string{"CUDA_VISIBLE_DEVICES=0"}
	out, err := Render(eg2TemplateFile(t, "linux-cuda"), p)
	if err != nil {
		t.Fatal(err)
	}
	if block := seatBlockOf(out, "embeddinggemma2"); !strings.Contains(block, `"${ldembed}"`) || !strings.Contains(block, "CUDA_VISIBLE_DEVICES=0") {
		t.Errorf("the tier's gpu_env must join the retargeted entry's env. Block:\n%s", block)
	}

	// With a CPU family as well, both macros sit after ld in a fixed order: ld, ldcpu, ldembed.
	for _, name := range []string{"linux-cuda", "linux-vulkan"} {
		p := altCPUParams("linux")
		p.ModelsDir = "/opt/models"
		p.IncludeEG2, p.EG2LlamaBin = true, eg2BinOwn
		out, err := Render(eg2TemplateFile(t, name), p)
		if err != nil {
			t.Fatalf("%s with a CPU family and an own build: %v", name, err)
		}
		lines := strings.Split(out, "\n")
		var order []string
		for _, l := range lines {
			for _, m := range []string{"ld", "ldcpu", "ldembed"} {
				if strings.HasPrefix(l, "  "+m+": ") {
					order = append(order, m)
				}
			}
		}
		if got := strings.Join(order, ","); got != "ld,ldcpu,ldembed" {
			t.Errorf("%s: the loader macros read %s, want ld,ldcpu,ldembed", name, got)
		}
		if !strings.Contains(seatBlockOf(out, "embeddinggemma2"), `"${ldembed}"`) || strings.Contains(seatBlockOf(out, "offload-e4b-cpu"), "ldembed") {
			t.Errorf("%s: ldembed must serve the embeddinggemma2 entry alone", name)
		}
	}
}

// TestEG2BinWindowsHasNoLoaderMacro: a Windows build resolves its DLLs beside the executable, so the
// entry's own build is only its cmd path (llama-server.exe in that directory) and no macro appears.
func TestEG2BinWindowsHasNoLoaderMacro(t *testing.T) {
	for _, name := range eg2WindowsTemplates {
		out := eg2BinRender(t, name, "windows", false, eg2BinOwn)
		if strings.Contains(out, "ldembed") || strings.Contains(out, "LD_LIBRARY_PATH") {
			t.Errorf("%s: a Windows render carries a loader macro", name)
		}
		if block := seatBlockOf(out, "embeddinggemma2"); !strings.Contains(block, eg2BinOwn+"/llama-server.exe --model") {
			t.Errorf("%s: the entry does not run llama-server.exe from its own build. Block:\n%s", name, block)
		}
	}
}

// TestEG2BinWithTextOnlyProjectorStrip: the text-only strip and the retarget are two rewrites of the same
// entry; either order must leave a block with no projector argument AND the own build.
func TestEG2BinWithTextOnlyProjectorStrip(t *testing.T) {
	for _, tc := range eg2Templates {
		out := eg2BinRender(t, tc.name, targetOf(tc.name), true, eg2BinOwn)
		block := seatBlockOf(out, "embeddinggemma2")
		if strings.Contains(block, "--mmproj") {
			t.Errorf("%s: a text-only entry with an own build still names a projector. Block:\n%s", tc.name, block)
		}
		if !strings.Contains(block, eg2BinOwn+"/llama-server") {
			t.Errorf("%s: a text-only entry lost its own build. Block:\n%s", tc.name, block)
		}
	}
}

// TestEG2BinIsRefusedWhereNoEntryRenders: an own build for an entry the render does not carry would be
// recorded in the stamp and mean nothing, so Render refuses it by name, whatever its value.
func TestEG2BinIsRefusedWhereNoEntryRenders(t *testing.T) {
	for _, own := range []string{eg2BinOwn, params().LlamaBin} {
		p := eg2Params(false)
		p.EG2LlamaBin = own
		_, err := Render(eg2TemplateFile(t, "linux-cuda"), p)
		if err == nil || !strings.Contains(err.Error(), "EG2LlamaBin") || !strings.Contains(err.Error(), modelEG2) {
			t.Errorf("own build %q without the entry: want a refusal naming EG2LlamaBin and the entry, got %v", own, err)
		}
	}
}

// TestEG2BinRefusesAValueThatWouldBreakTheMacro: the directory lands inside a double-quoted YAML scalar that
// llama-swap then expands, so a quote or a line break ends the scalar and a `$` starts a substitution.
func TestEG2BinRefusesAValueThatWouldBreakTheMacro(t *testing.T) {
	for _, bad := range []string{`/opt/a"b`, "/opt/a\nb", "/opt/a\rb", "/opt/${HOME}", "/opt/$x"} {
		p := eg2Params(true)
		p.GOOS, p.EG2LlamaBin = "linux", bad
		_, err := Render(eg2TemplateFile(t, "linux-cuda"), p)
		if err == nil || !strings.Contains(err.Error(), "EG2LlamaBin") {
			t.Errorf("own build %q: want a refusal naming EG2LlamaBin, got %v", bad, err)
		}
	}
}

// TestRetargetEG2BinRefusesAChangedTemplateShape: the rewrite is exact in both directions, like the
// projector strip. An entry whose cmd names the build zero or two times, a Linux entry whose env is not the
// one `${ld}` item, or a template that already defines the macro is a shape this renderer does not know, and
// it fails by naming the entry instead of shipping an entry on the wrong build. A template with no entry is
// returned untouched, as the projector strip does: Render's refusal-by-name reports that case.
func TestRetargetEG2BinRefusesAChangedTemplateShape(t *testing.T) {
	linux := eg2TemplateFile(t, "linux-cuda")
	win := eg2TemplateFile(t, "win-cuda")
	const entryCmd = "      __LLAMA_BIN__/llama-server.exe --model __MODELS__/embeddinggemma-2-Q8_0.gguf"
	for _, tc := range []struct {
		name, tmpl, wantIn string
	}{
		{"two builds named in the entry", strings.Replace(win, entryCmd, entryCmd+" --alias __LLAMA_BIN__/x", 1), "2 times"},
		{"no build named in the entry", strings.Replace(win, entryCmd, strings.Replace(entryCmd, "__LLAMA_BIN__", "/fixed/dir", 1), 1), "0 times"},
		{"a Linux entry without the loader item", strings.Replace(linux, "  embeddinggemma2:\n    env: [\"${ld}\"]", "  embeddinggemma2:\n    env: []", 1), "${ld}"},
		{"a Linux entry with the loader item twice", strings.Replace(linux, "  embeddinggemma2:\n    env: [\"${ld}\"]", "  embeddinggemma2:\n    env: [\"${ld}\", \"${ld}\"]", 1), "${ld}"},
		{"a template that already defines the macro", strings.Replace(linux, "\n\nmodels:", "\n  ldembed: \"x\"\n\nmodels:", 1), "ldembed"},
	} {
		if tc.tmpl == linux || tc.tmpl == win {
			t.Fatalf("%s: the test mutation did not apply", tc.name)
		}
		_, err := retargetEG2Bin(tc.tmpl, eg2BinOwn)
		if err == nil || !strings.Contains(err.Error(), modelEG2) || !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s: want a refusal naming %q and %q, got %v", tc.name, modelEG2, tc.wantIn, err)
		}
	}
	bare := "models:\n  other:\n    cmd: >-\n      __LLAMA_BIN__/llama-server --model m\n"
	if got, err := retargetEG2Bin(bare, eg2BinOwn); err != nil || got != bare {
		t.Errorf("a template with no entry must come back untouched, got err %v", err)
	}
}
