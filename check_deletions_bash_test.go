package main

// F63: TestDeletionGuardRefusesAnUndeclaredDeletion ran `bash scripts/check-deletions.sh` by the bare name `bash`. On a
// plain PowerShell PATH on Windows that name is the WSL launcher (WindowsApps\bash.exe, or System32\bash.exe), which hands
// the script path to a Linux shell with its backslashes eaten ("No such file or directory"), so the
// test failed whenever `go test` was started from PowerShell and passed from Git Bash, which has Git's bash first on PATH.
// The test now resolves Git's own bash from `git --exec-path` (which does not depend on the PATH order), passes the
// script as a forward-slash path, and skips with a reason when the only bash on the machine is the WSL launcher.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// resolveGuardBash picks the bash that runs the guard. bash is its path; skip, when not empty, says why the guard cannot
// run here (the caller skips the test with it). The inputs are the host's facts so a test can state any host.
func resolveGuardBash(goos string, lookPath func(string) (string, error), gitExecPath func() (string, error), exists func(string) bool) (bash, skip string) {
	if goos != "windows" {
		p, err := lookPath("bash")
		if err != nil {
			return "", "no bash on PATH; the guard runs in CI's ubuntu build job"
		}
		return p, ""
	}
	// Git for Windows ships its own bash next to git: <root>/usr/bin/bash.exe, and git --exec-path is
	// <root>/mingw64/libexec/git-core (or mingw32, clangarm64), so the root is found by walking up from it.
	if ep, err := gitExecPath(); err == nil && strings.TrimSpace(ep) != "" {
		dir := filepath.ToSlash(strings.TrimSpace(ep))
		for i := 0; i < 5 && dir != "" && dir != "." && dir != "/"; i++ {
			if cand := dir + "/usr/bin/bash.exe"; exists(cand) {
				return cand, ""
			}
			next := filepath.ToSlash(filepath.Dir(dir))
			if next == dir {
				break
			}
			dir = next
		}
	}
	p, err := lookPath("bash")
	if err != nil {
		return "", "no bash on this machine; the guard runs in CI's ubuntu build job"
	}
	if isWSLLauncher(p) {
		return "", "the only bash on PATH is the WSL launcher (" + p + "), which drops the backslashes of a Windows script path, and Git's own bash was not found next to git; run the test from Git Bash or install Git for Windows"
	}
	return p, ""
}

// isWSLLauncher reports whether a Windows path to bash.exe is the WSL launcher: the App Execution Alias under WindowsApps,
// or the System32/Sysnative one. Git's bash, MSYS2's and Cygwin's live under their own roots.
func isWSLLauncher(p string) bool {
	l := strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	return strings.Contains(l, "/windowsapps/") || strings.HasSuffix(l, "/system32/bash.exe") || strings.HasSuffix(l, "/sysnative/bash.exe")
}

// withBashOnPath puts the directory of an absolute bash first on the PATH of env. Git's bash started by its full path from a
// shell that has only git's cmd directory on PATH (a plain PowerShell) finds none of the tools the guard script calls
// (cut, grep, sed: "cut: command not found"), because they live beside it in <root>/usr/bin; Git Bash puts that
// directory on PATH itself, which is why the test used to pass there. A bare "bash" (nothing to add) leaves env as it is.
func withBashOnPath(env []string, bash string) []string {
	if !filepath.IsAbs(filepath.FromSlash(bash)) {
		return env
	}
	dir := filepath.Dir(filepath.FromSlash(bash))
	out := make([]string, 0, len(env)+1)
	cur := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
			cur = v
			continue
		}
		out = append(out, kv)
	}
	if cur == "" {
		return append(out, "PATH="+dir)
	}
	return append(out, "PATH="+dir+string(os.PathListSeparator)+cur)
}

// guardBash is resolveGuardBash on this host; the test is skipped when the guard cannot run here.
func guardBash(t *testing.T) string {
	t.Helper()
	gitExec := func() (string, error) {
		out, err := exec.Command("git", "--exec-path").Output()
		return string(out), err
	}
	exists := func(p string) bool {
		fi, err := os.Stat(filepath.FromSlash(p))
		return err == nil && !fi.IsDir()
	}
	bash, skip := resolveGuardBash(runtime.GOOS, exec.LookPath, gitExec, exists)
	if skip != "" {
		t.Skip(skip)
	}
	return bash
}

func TestGuardBashIsGitsBashNotTheWSLLauncher(t *testing.T) {
	const wsl = `C:\Users\u\AppData\Local\Microsoft\WindowsApps\bash.exe`
	const gitRoot = "C:/Program Files/Git"
	gitOK := func() (string, error) { return gitRoot + "/mingw64/libexec/git-core\n", nil }
	gitGone := func() (string, error) { return "", errors.New("git: not found") }
	onPath := func(p string) func(string) (string, error) {
		return func(string) (string, error) { return p, nil }
	}
	noBash := func(string) (string, error) { return "", errors.New("not found") }
	has := func(set ...string) func(string) bool {
		return func(p string) bool {
			for _, s := range set {
				if s == p {
					return true
				}
			}
			return false
		}
	}
	gitsBash := gitRoot + "/usr/bin/bash.exe"

	cases := []struct {
		name     string
		goos     string
		look     func(string) (string, error)
		git      func() (string, error)
		exists   func(string) bool
		wantBash string
		wantSkip string // a substring; "" = must run
	}{
		{"PowerShell PATH: only the WSL launcher, Git is installed", "windows", onPath(wsl), gitOK, has(gitsBash), gitsBash, ""},
		{"PATH has Git's bash, Git is installed: the derived one wins", "windows", onPath(`C:\Program Files\Git\usr\bin\bash.exe`), gitOK, has(gitsBash), gitsBash, ""},
		{"only the WSL launcher and no Git", "windows", onPath(wsl), gitGone, has(), "", "WSL launcher"},
		{"only the System32 launcher and no Git", "windows", onPath(`C:\Windows\System32\bash.exe`), gitGone, has(), "", "WSL launcher"},
		{"Git found but its bash is not where expected, WSL only", "windows", onPath(wsl), gitOK, has(), "", "WSL launcher"},
		{"another real bash on PATH (MSYS2) and no Git", "windows", onPath(`C:\msys64\usr\bin\bash.exe`), gitGone, has(), `C:\msys64\usr\bin\bash.exe`, ""},
		{"no bash at all on Windows", "windows", noBash, gitGone, has(), "", "no bash"},
		{"Linux with bash", "linux", onPath("/usr/bin/bash"), gitGone, has(), "/usr/bin/bash", ""},
		{"Linux without bash", "linux", noBash, gitGone, has(), "", "no bash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bash, skip := resolveGuardBash(tc.goos, tc.look, tc.git, tc.exists)
			if tc.wantSkip == "" {
				if skip != "" || bash != tc.wantBash {
					t.Fatalf("got bash=%q skip=%q, want bash=%q", bash, skip, tc.wantBash)
				}
				return
			}
			if bash != "" || !strings.Contains(skip, tc.wantSkip) {
				t.Fatalf("got bash=%q skip=%q, want a skip mentioning %q", bash, skip, tc.wantSkip)
			}
		})
	}
}

func TestGuardBashDirComesFirstOnThePathOfTheGuard(t *testing.T) {
	sep := string(os.PathListSeparator)
	bash := filepath.Join(t.TempDir(), "usr", "bin", "bash.exe")
	dir := filepath.Dir(bash)
	got := withBashOnPath([]string{"A=1", "Path=C:\\x" + sep + "C:\\y", "B=2"}, bash)
	var paths []string
	for _, kv := range got {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
			paths = append(paths, v)
		}
	}
	if len(paths) != 1 || !strings.HasPrefix(paths[0], dir+sep) || !strings.Contains(paths[0], "C:\\y") {
		t.Fatalf("PATH must start with the bash directory %q and keep the old entries, got %q", dir, paths)
	}
	if env := []string{"A=1"}; len(withBashOnPath(env, "bash")) != 1 {
		t.Error("a bare bash adds nothing")
	}
	if got := withBashOnPath(nil, bash); len(got) != 1 || got[0] != "PATH="+dir {
		t.Errorf("no PATH at all: %v", got)
	}
}

// The script travels as a forward-slash path: a backslash path through a launcher that is not Git's bash loses its separators.
func TestGuardScriptPathHasNoBackslashes(t *testing.T) {
	script := scriptPath(t)
	if strings.Contains(script, `\`) {
		t.Fatalf("the guard's path must be handed to bash with forward slashes: %q", script)
	}
}
