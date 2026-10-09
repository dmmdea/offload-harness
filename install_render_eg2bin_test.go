package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/servingtmpl"
)

// `install render --llama-bin-eg2 DIR` points ONLY the embeddinggemma2 entry at a second llama.cpp build,
// for a node whose main build is older than b11452 (it cannot load the gemma-embedding2 architecture) but
// must keep that build for its other seats. These tests pin the flag's plumbing, what an audit can and
// cannot see of it, and the write-time floor check on the build. Each says in its name what it pins.

const (
	eg2OldBuild = "/opt/llamacpp-b10964" // the node's main build: below the floor
	eg2NewBuild = "/opt/llamacpp-b11490" // the entry's own build: the one the entry was proven on
)

func eg2BinReq(tier, goos string) renderRequest {
	return renderRequest{
		TierID: tier, RAMTier: "min", GOOS: goos,
		LlamaBin: eg2OldBuild, ModelsDir: "/opt/models", Listen: "127.0.0.1:11436", Home: "/opt/offload", Threads: 4,
		PinnedVLLM: &pinnedVLLM{},
	}
}

// cmdLines are the rendered lines that launch a llama-server (a comment that mentions one is not).
func cmdLines(cfg string) []string {
	var out []string
	for _, l := range strings.Split(cfg, "\n") {
		if t := strings.TrimSpace(l); !strings.HasPrefix(t, "#") && strings.Contains(t, "/llama-server") {
			out = append(out, t)
		}
	}
	return out
}

// eg2Entry is the rendered embeddinggemma2 entry's body.
func eg2Entry(t *testing.T, cfg string) string {
	t.Helper()
	lines := strings.Split(cfg, "\n")
	var body []string
	in := false
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "  embeddinggemma2:"):
			in = true
			continue
		case in && l != "" && !strings.HasPrefix(l, " "):
			in = false
		case in && strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.Contains(l, ":"):
			in = false
		}
		if in {
			body = append(body, l)
		}
	}
	if len(body) == 0 {
		t.Fatalf("no embeddinggemma2 entry in the rendered config:\n%s", cfg)
	}
	return strings.Join(body, "\n")
}

// TestEG2BinMovesOnlyTheEntryAndIsRecorded: the flag reaches the render through deriveRender on all three
// tiers that carry the entry (the projector tier, a text-only replica on a Linux and a Windows template),
// the entry runs from its own build while every other llama-server keeps the main one, and the stamp's basis
// records the directory.
func TestEG2BinMovesOnlyTheEntryAndIsRecorded(t *testing.T) {
	for _, tc := range []struct{ tier, goos string }{
		{"ampere-6", "linux"}, {"ampere-6", "windows"}, {"ampere-8", "linux"}, {"ampere-8", "windows"}, {"blackwell-3x16", "windows"},
	} {
		req := eg2BinReq(tc.tier, tc.goos)
		req.EG2LlamaBin = eg2NewBuild
		res, err := deriveRender(embeddedProfiles, req)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.tier, tc.goos, err)
		}
		if res.Params.EG2LlamaBin != eg2NewBuild || res.Basis.Params.EG2LlamaBin != eg2NewBuild {
			t.Errorf("%s/%s: the params (%q) and the basis (%q) must both carry the entry's build", tc.tier, tc.goos, res.Params.EG2LlamaBin, res.Basis.Params.EG2LlamaBin)
		}
		entry := eg2Entry(t, res.Config)
		if !strings.Contains(entry, eg2NewBuild+"/llama-server") || strings.Contains(entry, eg2OldBuild) {
			t.Errorf("%s/%s: the entry does not run from its own build:\n%s", tc.tier, tc.goos, entry)
		}
		own, other := 0, 0
		for _, l := range cmdLines(res.Config) {
			switch {
			case strings.Contains(l, eg2NewBuild+"/llama-server"):
				own++
			case strings.Contains(l, eg2OldBuild+"/llama-server"):
				other++
			default:
				t.Errorf("%s/%s: a llama-server runs from neither build: %s", tc.tier, tc.goos, l)
			}
		}
		if own != 1 || other == 0 {
			t.Errorf("%s/%s: %d server(s) on the entry's build, %d on the main one; want exactly 1 and at least 1", tc.tier, tc.goos, own, other)
		}
	}
}

// TestEG2BinIsNormalisedBeforeItIsRecorded: backslashes become slashes and a trailing slash goes, so the
// stamp records, and a replay re-derives from, one spelling; a value that spells the main build again is no
// build of its own at all, so the render is the unset render and the stamp carries no key.
func TestEG2BinIsNormalisedBeforeItIsRecorded(t *testing.T) {
	unsetReq := eg2BinReq("ampere-6", "linux")
	unset, err := deriveRender(embeddedProfiles, unsetReq)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`\opt\llamacpp-b11490\`, eg2NewBuild + "/", eg2NewBuild + `\`} {
		req := eg2BinReq("ampere-6", "linux")
		req.EG2LlamaBin = raw
		res, err := deriveRender(embeddedProfiles, req)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if res.Params.EG2LlamaBin != eg2NewBuild {
			t.Errorf("%q recorded as %q, want %q", raw, res.Params.EG2LlamaBin, eg2NewBuild)
		}
	}
	for _, same := range []string{eg2OldBuild, eg2OldBuild + "/", strings.ReplaceAll(eg2OldBuild, "/", `\`)} {
		req := eg2BinReq("ampere-6", "linux")
		req.EG2LlamaBin = same
		res, err := deriveRender(embeddedProfiles, req)
		if err != nil {
			t.Fatalf("%q: %v", same, err)
		}
		if res.Params.EG2LlamaBin != "" || res.Config != unset.Config {
			t.Errorf("%q spells the main build: want the unset render with no recorded build, got params %q", same, res.Params.EG2LlamaBin)
		}
		_, canon, err := servingtmpl.SpecHash(res.Basis)
		if err != nil || strings.Contains(string(canon), "eg2_llama_bin") {
			t.Errorf("%q: the canonical basis must not mention the key (err %v):\n%s", same, err, canon)
		}
		if h1, _, _ := servingtmpl.SpecHash(res.Basis); h1 != mustSpec(t, unset.Basis) {
			t.Errorf("%q: the spec hash moved for a render that is the unset render", same)
		}
	}
}

func mustSpec(t *testing.T, b servingtmpl.SpecBasis) string {
	t.Helper()
	h, _, err := servingtmpl.SpecHash(b)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestEG2BinRefusesAValueThatCannotBeADirectory: the directory lands inside a quoted YAML macro that
// llama-swap expands, and a value that is only separators names no directory at all.
func TestEG2BinRefusesAValueThatCannotBeADirectory(t *testing.T) {
	for _, bad := range []string{`/opt/a"b`, "/opt/a\nb", "/opt/a$b", "/opt/${HOME}/b", "/", `\`, "//"} {
		req := eg2BinReq("ampere-6", "linux")
		req.EG2LlamaBin = bad
		_, err := deriveRender(embeddedProfiles, req)
		if err == nil || !strings.Contains(err.Error(), "--llama-bin-eg2") {
			t.Errorf("%q: want a refusal naming --llama-bin-eg2, got %v", bad, err)
		}
	}
}

// TestEG2BinIsRefusedByNameWhereTheTierRendersNoEntry: a tier that does not carry include_embeddinggemma2
// renders no entry to point anywhere, and the flag recorded in a stamp would mean nothing. The refusal names
// the tier and the flag, for the other CUDA tiers, the Rockchip board and an off-matrix box alike.
func TestEG2BinIsRefusedByNameWhereTheTierRendersNoEntry(t *testing.T) {
	for _, tc := range []struct {
		tier, fallback, want string
	}{
		{"blackwell-16", "", "blackwell-16"},
		{"volta-16", "", "volta-16"},
		{"rockchip-rk3588", "", "rockchip-rk3588"},
		{"", "cuda", "off-matrix"},
	} {
		req := eg2BinReq(tc.tier, "linux")
		req.Fallback = tc.fallback
		req.EG2LlamaBin = eg2NewBuild
		_, err := deriveRender(embeddedProfiles, req)
		if err == nil || !strings.Contains(err.Error(), "--llama-bin-eg2") || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "include_embeddinggemma2") {
			t.Errorf("%s%s: want a refusal naming the tier, --llama-bin-eg2 and include_embeddinggemma2, got %v", tc.tier, tc.fallback, err)
		}
		req.EG2LlamaBin = eg2OldBuild // even the main build spelled again is a flag given where it means nothing
		if _, err := deriveRender(embeddedProfiles, req); err == nil {
			t.Errorf("%s%s: the flag naming the main build was accepted on a tier that renders no entry", tc.tier, tc.fallback)
		}
	}
}

// TestEG2BinReplayMatches: a node rendered with the flag audits MATCH, because the replay carries the
// recorded build. Before replayRequest carried it, the replay re-rendered the entry on the main build and the
// node read STALE naming params.eg2_llama_bin forever.
func TestEG2BinReplayMatches(t *testing.T) {
	for _, tc := range []struct{ tier, goos string }{{"ampere-6", "linux"}, {"ampere-8", "windows"}} {
		req := eg2BinReq(tc.tier, tc.goos)
		req.EG2LlamaBin = eg2NewBuild
		res, err := deriveRender(embeddedProfiles, req)
		if err != nil {
			t.Fatal(err)
		}
		stamped, err := servingtmpl.Stamp(res.Config, res.Basis, stampedAtFixed())
		if err != nil {
			t.Fatal(err)
		}
		if rep := provenanceOf(stamped); rep.State != servingtmpl.StateMatch {
			t.Errorf("%s/%s: a config rendered with --llama-bin-eg2 reports %s: %s (keys %v)", tc.tier, tc.goos, rep.State, rep.Detail, rep.Keys)
		}
	}
}

// TestAHandEditedEG2PathReadsHandEditedAndAReRenderWithTheFlagAdoptsIt pins what an audit can and cannot see
// per entry: a stamped file whose entry was pointed at another build by hand reads HAND-EDITED, and the same
// change made by the renderer (on a template without a loader macro it is exactly the path edit) reads MATCH.
func TestAHandEditedEG2PathReadsHandEditedAndAReRenderWithTheFlagAdoptsIt(t *testing.T) {
	base, err := deriveRender(embeddedProfiles, eg2BinReq("ampere-8", "windows"))
	if err != nil {
		t.Fatal(err)
	}
	stampedBase, err := servingtmpl.Stamp(base.Config, base.Basis, stampedAtFixed())
	if err != nil {
		t.Fatal(err)
	}
	if rep := provenanceOf(stampedBase); rep.State != servingtmpl.StateMatch {
		t.Fatalf("the control render reports %s: %s", rep.State, rep.Detail)
	}
	hand := func(cfg string) string {
		var out []string
		done := 0
		for _, l := range strings.Split(cfg, "\n") {
			if strings.Contains(l, eg2OldBuild+"/llama-server") && strings.Contains(l, "embeddinggemma-2-Q8_0.gguf") {
				l = strings.Replace(l, eg2OldBuild, eg2NewBuild, 1)
				done++
			}
			out = append(out, l)
		}
		if done != 1 {
			t.Fatalf("the hand edit touched %d lines, want 1", done)
		}
		return strings.Join(out, "\n")
	}
	if rep := provenanceOf(hand(stampedBase)); rep.State != servingtmpl.StateHandEdited {
		t.Errorf("a stamped file with the entry's path edited by hand reports %s, want HAND-EDITED", rep.State)
	}

	req := eg2BinReq("ampere-8", "windows")
	req.EG2LlamaBin = eg2NewBuild
	adopted, err := deriveRender(embeddedProfiles, req)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Config != hand(base.Config) {
		t.Error("on a template without a loader macro the flag renders exactly the hand edit of the entry's path; it does not")
	}
	stampedAdopted, err := servingtmpl.Stamp(adopted.Config, adopted.Basis, stampedAtFixed())
	if err != nil {
		t.Fatal(err)
	}
	if rep := provenanceOf(stampedAdopted); rep.State != servingtmpl.StateMatch {
		t.Errorf("the re-render with the flag reports %s: %s (keys %v)", rep.State, rep.Detail, rep.Keys)
	}
}

// TestAltCPUBinReplayMatches: --llama-bin-cpu is a render input too, and replayRequest used to drop it, so a
// node rendered with a CPU family replayed without one and read STALE forever (dormant: no tier declares
// alt_backends since 2026-09-24). No shipped tier declares it, so the case is a copy of a Vulkan tier with
// the declaration added, driven through deriveRender, Stamp and AgainstRender directly (provenanceOf replays
// against the embedded table, which cannot carry a test tier).
func TestAltCPUBinReplayMatches(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	profiles := doc["profiles"].(map[string]any)
	var entry map[string]any
	for _, v := range profiles {
		if m, ok := v.(map[string]any); ok && m["backend"] == "vulkan" {
			entry = m
			break
		}
	}
	if entry == nil {
		t.Fatal("no Vulkan tier in the shipped table to copy")
	}
	entry["alt_backends"] = []any{"cpu"}
	delete(entry, "media_seats")
	profiles["test-dual-route"] = entry
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	req := eg2BinReq("test-dual-route", "linux")
	req.AltLlamaBinCPU = "/opt/llama-cpu"
	res, err := deriveRender(raw, req)
	if err != nil {
		t.Fatalf("the dual-route copy does not render: %v", err)
	}
	if !strings.Contains(res.Config, "offload-e4b-cpu") {
		t.Fatal("the control render carries no CPU family, so the case under test did not happen")
	}
	stamped, err := servingtmpl.Stamp(res.Config, res.Basis, stampedAtFixed())
	if err != nil {
		t.Fatal(err)
	}
	st, ok := servingtmpl.ParseStamp(stamped)
	if !ok {
		t.Fatal("the stamp does not parse back")
	}
	basis, err := st.Basis()
	if err != nil {
		t.Fatal(err)
	}
	replay, ok := replayRequest(basis)
	if !ok {
		t.Fatal("no replay request")
	}
	again, err := deriveRender(raw, replay)
	if err != nil {
		t.Fatal(err)
	}
	if rep := servingtmpl.AgainstRender(stamped, again.Basis, again.Config); rep.State != servingtmpl.StateMatch {
		t.Errorf("a config rendered with --llama-bin-cpu reports %s: %s (keys %v)", rep.State, rep.Detail, rep.Keys)
	}
}

// bothStreams runs `install render` with the given arguments and returns what it wrote to stdout and to
// stderr (renderCommand keeps only the second, and always passes -out).
func bothStreams(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	stderr = captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = runInstallRender(args) })
	})
	return stdout, stderr, err
}

func eg2RenderArgs(t *testing.T, tier, goos string, extra ...string) (args []string, out string) {
	t.Helper()
	out = filepath.Join(t.TempDir(), "llama-swap.yaml")
	args = append([]string{"-profile", tier, "-os", goos, "-root", ".", "-home", t.TempDir(), "-models", t.TempDir()}, extra...)
	return args, out
}

// TestRunInstallRenderEG2BinFlag drives the command: the file it writes carries the entry on its own build
// and the stamp's basis records the directory, with the main build below the floor and the entry's above it
// (the node the flag exists for).
func TestRunInstallRenderEG2BinFlag(t *testing.T) {
	args, out := eg2RenderArgs(t, "ampere-6", "linux", "-llama-bin", eg2OldBuild, "-llama-bin-eg2", eg2NewBuild)
	stdout, stderr, err := bothStreams(t, append(args, "-out", out)...)
	if err != nil {
		t.Fatalf("install render: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	st, ok := servingtmpl.ParseStamp(text)
	if !ok {
		t.Fatal("the written file carries no stamp")
	}
	basis, err := st.Basis()
	if err != nil {
		t.Fatal(err)
	}
	if basis.Params.EG2LlamaBin != eg2NewBuild || basis.Params.LlamaBin != eg2OldBuild {
		t.Errorf("the stamp records builds %q / %q, want %q / %q", basis.Params.LlamaBin, basis.Params.EG2LlamaBin, eg2OldBuild, eg2NewBuild)
	}
	if entry := eg2Entry(t, text); !strings.Contains(entry, eg2NewBuild+"/llama-server") || !strings.Contains(entry, "${ldembed}") {
		t.Errorf("the written entry is not on its own build:\n%s", entry)
	}
	if strings.Contains(stdout+stderr, "could not be read") {
		t.Errorf("a build the name states (b11490) was reported unreadable:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if rep := provenanceOf(text); rep.State != servingtmpl.StateMatch {
		t.Errorf("the written file reports %s: %s (keys %v)", rep.State, rep.Detail, rep.Keys)
	}

	// On a tier that renders no entry the command refuses by name and writes nothing.
	args, out = eg2RenderArgs(t, "blackwell-16", "linux", "-llama-bin", eg2NewBuild, "-llama-bin-eg2", eg2NewBuild)
	_, _, err = bothStreams(t, append(args, "-out", out)...)
	if err == nil || !strings.Contains(err.Error(), "blackwell-16") || !strings.Contains(err.Error(), "--llama-bin-eg2") {
		t.Errorf("want a refusal naming the tier and the flag, got %v", err)
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Error("a refused render wrote a file")
	}
}

// TestLlamaBuildIsReadFromTheDirectoryName locks the one source the floor check trusts: a b<digits> token
// of 4 to 6 digits in the directory's OWN last path element, as the release archives and the usual build
// directories are named. Anything the name does not state plainly is unknown, never guessed.
func TestLlamaBuildIsReadFromTheDirectoryName(t *testing.T) {
	for _, tc := range []struct {
		dir   string
		build int
		known bool
	}{
		{"/opt/llamacpp-b10964", 10964, true},
		{"/opt/llama.cpp-b11490", 11490, true},
		{"/opt/llama-b11490/", 11490, true},
		{`\opt\llama-b11490`, 11490, true},
		{"/opt/b11452", 11452, true},
		{"b11452", 11452, true},
		{"/opt/llama-b11490-bin-win-cuda-12.4-x64", 11490, true},
		{"/opt/llama-b11490-b11490", 11490, true},
		{"/opt/llama-b9934", 9934, true},
		{"/opt/llama-b123456", 123456, true},
		{"/opt/llama", 0, false},
		{"/opt/llama-b123", 0, false},          // 3 digits: not a build token
		{"/opt/llama-b1234567", 0, false},      // 7 digits
		{"/opt/cub11490", 0, false},            // not a standalone token
		{"/opt/llama-b11490x", 0, false},       // trailing letters: not a token
		{"/opt/llama-b11452-b11490", 0, false}, // two builds: ambiguous
		{"/opt/llama-b11490/bin", 0, false},    // only the directory's own name is read
		{"", 0, false},
		{"/", 0, false},
	} {
		got, known := llamaBuildOf(tc.dir)
		if got != tc.build || known != tc.known {
			t.Errorf("llamaBuildOf(%q) = %d, %v; want %d, %v", tc.dir, got, known, tc.build, tc.known)
		}
	}
}

// floorMsg is the refusal the check owes, word for word.
func floorMsg(tier, dir string, build int) string {
	return "tier " + tier + " renders embeddinggemma2, which needs llama.cpp b" + strconv.Itoa(eg2MinLlamaBuild) +
		" or newer (gemma-embedding2), but " + dir + " is b" + strconv.Itoa(build) +
		": pass --llama-bin-eg2 <dir of a b" + strconv.Itoa(eg2MinLlamaBuild) + "+ build> - not written"
}

// TestTheFloorCheckRefusesABuildBelowB11452AtWriteTime: exactly the floor passes and one below refuses, for
// the main build (no flag) and for the entry's own; the refusal names the tier, the directory, its build and
// the flag, and nothing is written.
func TestTheFloorCheckRefusesABuildBelowB11452AtWriteTime(t *testing.T) {
	run := func(tier, goos string, extra ...string) (string, error) {
		args, out := eg2RenderArgs(t, tier, goos, extra...)
		_, _, err := bothStreams(t, append(args, "-out", out)...)
		if err != nil {
			if _, serr := os.Stat(out); serr == nil {
				t.Errorf("%s: a refused render wrote a file", tier)
			}
		}
		return out, err
	}
	if eg2MinLlamaBuild != 11452 {
		t.Fatalf("the floor is %d, want 11452 (the gemma-embedding2 architecture, llama.cpp PR 30054)", eg2MinLlamaBuild)
	}
	// The main build is the one that serves the entry when no flag is given.
	for _, tc := range []struct {
		tier, goos, dir string
		refuse          bool
	}{
		{"ampere-6", "linux", "/opt/llamacpp-b11452", false},
		{"ampere-6", "linux", "/opt/llamacpp-b11451", true},
		{"ampere-6", "linux", eg2OldBuild, true},
		{"ampere-8", "windows", "/opt/llama-b11452", false}, // a text-only replica renders the entry too
		{"ampere-8", "windows", "/opt/llama-b11451", true},
		{"blackwell-3x16", "windows", "/opt/llama-b10000", true},
		{"blackwell-16", "linux", "/opt/llamacpp-b10000", false}, // a tier with no entry has no floor
	} {
		_, err := run(tc.tier, tc.goos, "-llama-bin", tc.dir)
		switch {
		case tc.refuse && (err == nil || !strings.Contains(err.Error(), floorMsg(tc.tier, tc.dir, mustBuild(t, tc.dir)))):
			t.Errorf("%s %s: want the floor refusal %q, got %v", tc.tier, tc.dir, floorMsg(tc.tier, tc.dir, mustBuild(t, tc.dir)), err)
		case !tc.refuse && err != nil:
			t.Errorf("%s %s: refused: %v", tc.tier, tc.dir, err)
		}
	}
	// With a build of its own the entry is checked on THAT build; the main one may be older.
	if _, err := run("ampere-6", "linux", "-llama-bin", eg2OldBuild, "-llama-bin-eg2", "/opt/llamacpp-b11452"); err != nil {
		t.Errorf("an old main build with an entry build exactly at the floor was refused: %v", err)
	}
	if _, err := run("ampere-6", "linux", "-llama-bin", eg2NewBuild, "-llama-bin-eg2", "/opt/llamacpp-b11451"); err == nil ||
		!strings.Contains(err.Error(), floorMsg("ampere-6", "/opt/llamacpp-b11451", 11451)) {
		t.Errorf("an entry build one below the floor beside a current main build: want the refusal naming the entry's build, got %v", err)
	}
}

func mustBuild(t *testing.T, dir string) int {
	t.Helper()
	n, ok := llamaBuildOf(dir)
	if !ok {
		t.Fatalf("test directory %q names no build", dir)
	}
	return n
}

// TestAnUnreadableBuildIsANoteOnTheStreamThatCannotCorruptTheOutput: a directory whose name states no build
// cannot be checked without running it, which the renderer does not do. The render proceeds and says so on
// stdout when the config goes to --out (install.ps1 reads that stream), and on stderr when the config itself
// is stdout, where a note would sit in front of the stamp and break the file.
func TestAnUnreadableBuildIsANoteOnTheStreamThatCannotCorruptTheOutput(t *testing.T) {
	const marker = "could not be read from its directory name"
	args, out := eg2RenderArgs(t, "ampere-6", "linux", "-llama-bin", "/opt/llama")
	stdout, stderr, err := bothStreams(t, append(args, "-out", out)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, marker) || !strings.Contains(stdout, "note:") || strings.Contains(stderr, marker) {
		t.Errorf("with --out the note belongs on stdout.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	args, _ = eg2RenderArgs(t, "ampere-6", "linux", "-llama-bin", "/opt/llama")
	stdout, stderr, err = bothStreams(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := servingtmpl.ParseStamp(stdout); !ok || strings.Contains(stdout, marker) {
		t.Errorf("without --out stdout must be the stamped config and nothing else.\nstdout:\n%.400s", stdout)
	}
	if !strings.Contains(stderr, marker) {
		t.Errorf("without --out the note belongs on stderr.\nstderr:\n%s", stderr)
	}

	// A state the name does state, at or above the floor, is silent on both streams.
	args, out = eg2RenderArgs(t, "ampere-6", "linux", "-llama-bin", eg2NewBuild)
	stdout, stderr, err = bothStreams(t, append(args, "-out", out)...)
	if err != nil || strings.Contains(stdout+stderr, marker) {
		t.Errorf("a build at b11490 must pass silently, err %v.\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	// And a tier with no entry has nothing to check, whatever its directory is called.
	args, out = eg2RenderArgs(t, "blackwell-16", "linux", "-llama-bin", "/opt/llama")
	stdout, stderr, err = bothStreams(t, append(args, "-out", out)...)
	if err != nil || strings.Contains(stdout+stderr, marker) {
		t.Errorf("a tier without the entry must not be checked, err %v.\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
}

// TestTheFloorIsAWriteTimeGateOnly: a replay runs on the auditing machine with the audited node's recorded
// paths and must never judge them, so a config rendered on a build below the floor (before the check existed,
// or with the check's note) still audits MATCH, and deriveRender itself never refuses on a build.
func TestTheFloorIsAWriteTimeGateOnly(t *testing.T) {
	res, err := deriveRender(embeddedProfiles, eg2BinReq("ampere-6", "linux"))
	if err != nil {
		t.Fatalf("deriveRender refused a main build below the floor: %v", err)
	}
	stamped, err := servingtmpl.Stamp(res.Config, res.Basis, stampedAtFixed())
	if err != nil {
		t.Fatal(err)
	}
	if rep := provenanceOf(stamped); rep.State != servingtmpl.StateMatch {
		t.Errorf("a stamped config on a build below the floor audits %s: %s", rep.State, rep.Detail)
	}
}

// TestInstallPs1PinsABuildAtOrAboveTheEG2Floor ties the two files that each state the floor: the Windows
// installer downloads one pinned tag for every node it installs, so that tag must be at or above the build
// the renderer refuses below.
func TestInstallPs1PinsABuildAtOrAboveTheEG2Floor(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("setup", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\$LLAMA_TAG\s*=\s*'b(\d+)'`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("setup/install.ps1 states no $LLAMA_TAG = 'bNNNNN'")
	}
	tag, _ := strconv.Atoi(m[1])
	if tag < eg2MinLlamaBuild {
		t.Errorf("install.ps1 pins llama.cpp b%d, below the b%d floor the renderer refuses under: every Windows install it makes would render an entry that cannot load", tag, eg2MinLlamaBuild)
	}
}

// TestInstallRenderUsageNamesBothSecondBuildFlags: the usage text is the CLI's own doc, and it listed
// neither second-build flag.
func TestInstallRenderUsageNamesBothSecondBuildFlags(t *testing.T) {
	text := captureStderr(t, usage)
	var line string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "local-offload install render") {
			line = l
		}
	}
	for _, flag := range []string{"--llama-bin-cpu", "--llama-bin-eg2"} {
		if !strings.Contains(line, flag) {
			t.Errorf("the `install render` usage line does not name %s:\n%s", flag, line)
		}
	}
}
