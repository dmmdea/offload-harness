package gpulease

// F8 follow-up (2026-10-10): the long renders are not launched as `node render/comfy-generate.mjs`; they are
// `cmd /c render-both.cmd` and `bash render-card-v3.sh`, and the helper's path sits inside the script. The
// fixtures below carry exactly the lines those two recipes carry (a batch file's `set "R=..."`, a shell's
// `GEN="..."`), with neutral paths.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHelperBehindABatchFileAndAShellScript(t *testing.T) {
	dir := t.TempDir()
	cmdFile := writeScript(t, dir, "render-both.cmd", "@echo off\r\nrem Runs inside the GPU lease.\r\nset \"R=C:\\tools\\render\\comfy-generate.mjs\"\r\nset \"P=D:\\work\\round2\"\r\nnode \"%R%\" --batch \"%P%\\jobs.jsonl\" --family qwen-image\r\n")
	shFile := writeScript(t, dir, "render-card.sh", "#!/usr/bin/env bash\nset -u\nSHARD=\"${1:?usage}\"\nGEN=\"/opt/tools/render/comfy-generate.mjs\"\nCOMFY_CARD_UUID=\"$pin\" node \"$GEN\" --batch \"$SHARD\" --family qwen-image\n")
	psFile := writeScript(t, dir, "run.ps1", "$gen = Join-Path $root 'render\\comfy-video.mjs'\nnode $gen out.mp4\n")
	pyFile := writeScript(t, dir, "drive.py", "import subprocess\nsubprocess.run([\"node\", \"/x/render/comfy-music.mjs\", \"out.flac\"])\n")
	jsFile := writeScript(t, dir, "drive.mjs", "import { spawn } from 'node:child_process'\nspawn('node', ['/x/render/comfy-upscale.mjs', 'in.png'])\n")
	cases := []struct {
		name       string
		args       []string
		wantHelper string
		wantWhere  string
	}{
		{"cmd /c batch file", []string{"cmd", "/c", cmdFile}, "comfy-generate.mjs", "render-both.cmd"},
		{"cmd /c batch file, quoted whole", []string{"cmd", "/c", "\"" + cmdFile + "\""}, "comfy-generate.mjs", "render-both.cmd"},
		{"bash script with a path in a variable", []string{"bash", shFile, "jobs.jsonl"}, "comfy-generate.mjs", "render-card.sh"},
		{"the script run directly", []string{shFile}, "comfy-generate.mjs", "render-card.sh"},
		{"PowerShell", []string{"pwsh", "-File", psFile}, "comfy-video.mjs", "run.ps1"},
		{"Python driver", []string{"python", pyFile}, "comfy-music.mjs", "drive.py"},
		{"a Node driver of the helpers", []string{"node", jsFile}, "comfy-upscale.mjs", "drive.mjs"},
		{"the helper named directly stays the direct form", []string{"node", "/x/render/comfy-render.mjs", "o.png"}, "comfy-render.mjs", ""},
	}
	for _, c := range cases {
		h, where, ok := HeartbeatHelperBehind(c.args)
		if !ok || h != c.wantHelper || where != c.wantWhere {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q, true)", c.name, h, where, ok, c.wantHelper, c.wantWhere)
		}
	}
}

func TestHelperBehindAnInlineCommandLine(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"cmd /c with a command line", []string{"cmd", "/c", `node C:\x\render\comfy-video.mjs out.mp4`}, "comfy-video.mjs"},
		{"bash -c", []string{"bash", "-c", "cd /w && node /x/render/comfy-render.mjs out.png 'a cat'"}, "comfy-render.mjs"},
		{"pwsh -Command", []string{"pwsh", "-Command", "node D:/x/render/comfy-edit.mjs in.png out.png"}, "comfy-edit.mjs"},
	}
	for _, c := range cases {
		h, where, ok := HeartbeatHelperBehind(c.args)
		if !ok || h != c.want || where != "the inline command" {
			t.Errorf("%s: got (%q, %q, %v), want (%q, the inline command)", c.name, h, where, ok, c.want)
		}
	}
}

// Only a script's use of a helper counts, and only a command that RUNS it: prose in a comment, a note passed
// as an argument, a script that cannot be found and a file of another kind all say nothing.
func TestHelperBehindSaysNothingForMentionsAndMissingFiles(t *testing.T) {
	dir := t.TempDir()
	commented := writeScript(t, dir, "bench.sh", "#!/bin/sh\n# renders with comfy-generate.mjs elsewhere\n// comfy-render.mjs too\nrem comfy-video.mjs\n:: comfy-edit.mjs\necho bench\n")
	notes := writeScript(t, dir, "notes.txt", "node comfy-generate.mjs\n")
	// A script that DOES run a helper, reached through a variable that is not set: `${UNSET}/abs/path/recipe.sh`
	// expands to a path with a hole that happens to exist. A reference to an unset variable must make the path
	// unusable, not silently name another file.
	holey := writeScript(t, dir, "recipe.sh", "node /x/render/comfy-render.mjs o.png\n")
	t.Setenv("LO_TEST_SURELY_UNSET", "")
	asDir := filepath.Join(dir, "looks-like-a-script.sh")
	if err := os.MkdirAll(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
	}{
		{"a bench whose comments mention helpers", []string{"bash", commented}},
		{"a note passed to another script", []string{"node", "render/other.mjs", "--note", "comfy-render.mjs is slow"}},
		{"a script that does not exist", []string{"cmd", "/c", filepath.Join(dir, "gone.cmd")}},
		{"a file of another kind is not read", []string{"cat", notes}},
		{"a directory named like a script", []string{"bash", asDir}},
		{"an unset variable in the path", []string{"bash", "$NO_SUCH_DIR_FOR_THE_TEST/render.sh"}},
		{"an unset variable in front of a path that exists", []string{"bash", "${LO_TEST_SURELY_UNSET}" + holey}},
		{"nothing", nil},
	}
	for _, c := range cases {
		if h, where, ok := HeartbeatHelperBehind(c.args); ok {
			t.Errorf("%s: found %q in %q", c.name, h, where)
		}
	}
}

func TestHelperBehindFollowsAScriptThatNamesAScriptBoundedly(t *testing.T) {
	dir := t.TempDir()
	// outer -> inner (found beside the caller by its file name, the way `%~dp0inner.cmd` names it).
	writeScript(t, dir, "outer.cmd", "@echo off\r\ncall \"%~dp0inner.cmd\" %*\r\n")
	writeScript(t, dir, "inner.cmd", "node \"C:\\r\\comfy-run-graph.mjs\" --graph g.json\r\n")
	h, where, ok := HeartbeatHelperBehind([]string{"cmd", "/c", filepath.Join(dir, "outer.cmd")})
	if !ok || h != "comfy-run-graph.mjs" || where != "inner.cmd" {
		t.Errorf("outer -> inner: got (%q, %q, %v)", h, where, ok)
	}

	// Depth: the command's script is read, and the scripts it names; a helper three scripts down is not looked for.
	writeScript(t, dir, "a.sh", "bash b.sh\n")
	writeScript(t, dir, "b.sh", "bash c.sh\n")
	writeScript(t, dir, "c.sh", "bash d.sh\n")
	writeScript(t, dir, "d.sh", "node /x/render/comfy-render.mjs o.png\n")
	if h, where, ok := HeartbeatHelperBehind([]string{"bash", filepath.Join(dir, "a.sh")}); ok {
		t.Errorf("a helper four scripts down was found (%q in %q): the depth bound is gone", h, where)
	}
	writeScript(t, dir, "c.sh", "node /x/render/comfy-render.mjs o.png\n")
	if h, _, ok := HeartbeatHelperBehind([]string{"bash", filepath.Join(dir, "a.sh")}); ok {
		t.Errorf("a helper three scripts down (a -> b -> c) was found (%q): scriptDepthMax = %d", h, scriptDepthMax)
	}
	writeScript(t, dir, "b.sh", "node /x/render/comfy-render.mjs o.png\n")
	if _, _, ok := HeartbeatHelperBehind([]string{"bash", filepath.Join(dir, "a.sh")}); !ok {
		t.Error("a helper two scripts down (a -> b) was not found")
	}

	// A script that names itself, and two that name each other, are each read once and end.
	writeScript(t, dir, "loop1.sh", "bash loop2.sh\nbash loop1.sh\n")
	writeScript(t, dir, "loop2.sh", "bash loop1.sh\n")
	if _, _, ok := HeartbeatHelperBehind([]string{"bash", filepath.Join(dir, "loop1.sh")}); ok {
		t.Error("a loop of scripts with no helper found one")
	}
}

func TestHelperBehindReadsABoundedNumberOfScriptsAndBytes(t *testing.T) {
	dir := t.TempDir()
	// The top script names 9 others; the helper is in the n-th. The command's script plus 7 more are read.
	var top strings.Builder
	for i := 1; i <= 9; i++ {
		top.WriteString("bash s" + strconv.Itoa(i) + ".sh\n")
		body := "echo " + strconv.Itoa(i) + "\n"
		writeScript(t, dir, "s"+strconv.Itoa(i)+".sh", body)
	}
	topPath := writeScript(t, dir, "top.sh", top.String())
	writeScript(t, dir, "s7.sh", "node /x/render/comfy-render.mjs o.png\n")
	if _, _, ok := HeartbeatHelperBehind([]string{"bash", topPath}); !ok {
		t.Errorf("the 7th script named by the command's script is within scriptFilesMax = %d and was not read", scriptFilesMax)
	}
	writeScript(t, dir, "s7.sh", "echo 7\n")
	writeScript(t, dir, "s8.sh", "node /x/render/comfy-render.mjs o.png\n")
	if h, where, ok := HeartbeatHelperBehind([]string{"bash", topPath}); ok {
		t.Errorf("the 8th named script is past scriptFilesMax = %d but %q was found in %q", scriptFilesMax, h, where)
	}

	// Bytes: the helper after the first scriptReadMax bytes is not seen; the same line before them is.
	pad := strings.Repeat("echo padding to push the next line past the read limit\n", scriptReadMax/50)
	big := writeScript(t, dir, "big.sh", pad+"node /x/render/comfy-render.mjs o.png\n")
	if len(pad) <= scriptReadMax {
		t.Fatalf("test padding is %d bytes, not past the limit %d", len(pad), scriptReadMax)
	}
	if h, _, ok := HeartbeatHelperBehind([]string{"bash", big}); ok {
		t.Errorf("a helper past the first %d bytes was found (%q): the read is unbounded", scriptReadMax, h)
	}
	early := writeScript(t, dir, "early.sh", "node /x/render/comfy-render.mjs o.png\n"+pad)
	if _, _, ok := HeartbeatHelperBehind([]string{"bash", early}); !ok {
		t.Error("a helper inside the first bytes of a long script was not found")
	}
}

func TestHelperBehindExpandsTheEnvironmentOfAPath(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "recipe.sh", "node /x/render/comfy-render.mjs o.png\n")
	writeScript(t, dir, "recipe.cmd", "node C:\\x\\render\\comfy-render.mjs o.png\r\n")
	t.Setenv("LO_TEST_SCRIPT_DIR", dir)
	for _, arg := range []string{"$LO_TEST_SCRIPT_DIR/recipe.sh", "${LO_TEST_SCRIPT_DIR}/recipe.sh", "%LO_TEST_SCRIPT_DIR%/recipe.cmd"} {
		if _, _, ok := HeartbeatHelperBehind([]string{"bash", arg}); !ok {
			t.Errorf("%s was not expanded to the script", arg)
		}
	}
	// A variable that expands to itself must end.
	t.Setenv("LO_TEST_SELF", "%LO_TEST_SELF%")
	if _, _, ok := HeartbeatHelperBehind([]string{"cmd", "/c", "%LO_TEST_SELF%.cmd"}); ok {
		t.Error("a self-referencing variable found a helper")
	}
}
