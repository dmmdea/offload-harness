package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The synthetic list: made-up names, never a name from the operator's list (the
// keyed gate scans test sources like every other file). No test here sets a key
// in the environment: keys come from -key-file and the environment is a stub.
const synthList = "sub\tzorblax\nword\tquuxel\nphrase\tplugh xyzzy\n"

func noEnv(string) string { return "" }

func hexKey(pair string) string { return strings.Repeat(pair, 32) }

// runCLI runs the command in process and returns its streams and exit code.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb, noEnv)
	return out.String(), errb.String(), code
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "core.autocrlf=false", "-c", "core.hooksPath=" + filepath.Join(dir, "no-hooks"), "-c", "commit.gpgsign=false",
		"-c", "user.name=Test", "-c", "user.email=test@example.com", "-C", dir}, args...)
	cmd := exec.Command("git", full...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newRepo makes a repository with the given files committed on its first commit.
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "fixture")
	return dir
}

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

var trailer = regexp.MustCompile(`(?m)^scanned (\d+) of (\d+) tracked files; findings (\d+) in (\d+) files; fatal (\d+); classes: runs>96=(\d+) binaries=(\d+) png-nonstd-chunks=(\d+) png-after-iend=(\d+) png-compressed-text=(\d+) png-no-iend=(\d+) oversize=(\d+) symlink=(\d+) submodule=(\d+)$`)

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"bogus"},
		{"scan"},
		{"scan", "-plain"},
		{"gen"},
		{"gen", "-plain", "x"},
		{"patterns"},
		{"check"},
		{"scan", "-plain", "x", "-dir", ".", "-ref", "HEAD"},
		{"scan", "-plain", "x", "extra"},
		{"genkey", "-nope"},
	} {
		_, _, code := runCLI(t, args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	out, _, code := runCLI(t, "help")
	if code != 0 || !strings.Contains(out, "usage:") {
		t.Errorf("help: %d %q", code, out)
	}
}

func TestScanDirFindingsAndTrailer(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"docs/a.md":        "hello\nthe Zorblax box\n",
		"docs/b.md":        "nothing to see\n",
		"src/plugh-x.go":   "package x // Plugh Xyzzy\n",
		"zorblax-notes.md": "clean body\n",
	})
	list := writeTemp(t, "list.txt", synthList)
	out, _, code := runCLI(t, "scan", "-plain", list, "-dir", repo)
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	for _, want := range []string{
		"docs/a.md:2:5 sub:zorblax",
		"src/plugh-x.go:1:14 phrase:plugh xyzzy",
		"zorblax-notes.md (name):1:1 sub:zorblax",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing line %q in:\n%s", want, out)
		}
	}
	m := trailer.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no trailer in:\n%s", out)
	}
	if m[1] != "4" || m[2] != "4" || m[3] != "3" || m[4] != "3" || m[5] != "0" {
		t.Errorf("trailer counts = %v", m[1:])
	}
	// findings print before the trailer, sorted
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[len(lines)-1], "scanned ") {
		t.Errorf("the trailer is not the last line: %q", lines[len(lines)-1])
	}
}

func TestScanCleanTreeExitsZero(t *testing.T) {
	repo := newRepo(t, map[string]string{"a.md": "clean\n", "b/c.txt": "also clean\n"})
	list := writeTemp(t, "list.txt", synthList)
	out, _, code := runCLI(t, "scan", "-plain", list, "-dir", repo)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if m := trailer.FindStringSubmatch(out); m == nil || m[3] != "0" || m[5] != "0" {
		t.Fatalf("trailer: %q", out)
	}
}

func TestScanRefEqualsScanDir(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"docs/a.md":         "the Zorblax box\nQuuxel\n",
		"b.txt":             "clean\n",
		"deep/zorblax/c.md": "x " + strings.Repeat("q", 100) + "\n",
	})
	list := writeTemp(t, "list.txt", synthList)
	dirOut, _, dirCode := runCLI(t, "scan", "-plain", list, "-dir", repo)
	refOut, _, refCode := runCLI(t, "scan", "-plain", list, "-ref", "HEAD", "-repo", repo)
	if dirCode != 1 || refCode != 1 {
		t.Fatalf("exit codes %d %d", dirCode, refCode)
	}
	if dirOut != refOut {
		t.Fatalf("-ref and -dir disagree on one tree\n-dir:\n%s\n-ref:\n%s", dirOut, refOut)
	}
	if !strings.Contains(refOut, "FATAL deep/zorblax/c.md:1 run over 96 characters") {
		t.Errorf("the long run is not reported as a fatal: %s", refOut)
	}
}

func TestScanFilesListRestrictsTheScan(t *testing.T) {
	repo := newRepo(t, map[string]string{
		"a.md":        "Zorblax\n",
		"b.md":        "Zorblax\n",
		"c.md":        "clean\n",
		"fonts/f.bin": "wOF2\x00\x01 binary",
	})
	list := writeTemp(t, "list.txt", synthList)
	only := writeTemp(t, "chunk.txt", "# a chunk\na.md\n?new/file.md\nc.md\n")
	out, _, code := runCLI(t, "scan", "-plain", list, "-dir", repo, "-files", only)
	if code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "a.md:1:1 sub:zorblax") || strings.Contains(out, "b.md") {
		t.Errorf("the file list did not restrict the scan:\n%s", out)
	}
	if strings.Contains(out, "FATAL") {
		t.Errorf("a file outside the list produced a fatal record:\n%s", out)
	}
	if m := trailer.FindStringSubmatch(out); m == nil || m[1] != "2" || m[2] != "4" {
		t.Errorf("trailer: %q", out)
	}
	// a clean chunk exits 0 even though the tree has findings and a binary elsewhere
	clean := writeTemp(t, "clean.txt", "c.md\n")
	if out, _, code := runCLI(t, "scan", "-plain", list, "-dir", repo, "-files", clean); code != 0 {
		t.Errorf("a clean chunk: exit %d\n%s", code, out)
	}
	// without a list the binary is a fatal record
	out, _, code = runCLI(t, "scan", "-plain", list, "-dir", repo)
	if code != 1 || !strings.Contains(out, "FATAL fonts/f.bin unknown binary") {
		t.Errorf("whole tree: exit %d\n%s", code, out)
	}
}

func TestScanFilesListErrorsAreNeverAVacuousPass(t *testing.T) {
	repo := newRepo(t, map[string]string{"a.md": "clean\n", "b.md": "clean\n"})
	list := writeTemp(t, "list.txt", synthList)
	if err := os.WriteFile(filepath.Join(repo, "untracked.md"), []byte("Zorblax\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"a stale path":               "gone.md\n",
		"a duplicate":                "a.md\na.md\n",
		"an optional untracked file": "a.md\n?untracked.md\n",
		"a rename with neither side": "x.md=>y.md\n",
		"a rename with both sides":   "a.md=>b.md\n",
		"a malformed line":           "/abs.md\n",
	} {
		lst := writeTemp(t, "l.txt", body)
		out, errs, code := runCLI(t, "scan", "-plain", list, "-dir", repo, "-files", lst)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2\n%s%s", name, code, out, errs)
		}
	}
}

func TestScanRenameForm(t *testing.T) {
	repo := newRepo(t, map[string]string{"new/name.md": "Zorblax\n", "other.md": "clean\n"})
	list := writeTemp(t, "list.txt", synthList)
	lst := writeTemp(t, "l.txt", "old/name.md=>new/name.md\n")
	out, _, code := runCLI(t, "scan", "-plain", list, "-dir", repo, "-files", lst)
	if code != 1 || !strings.Contains(out, "new/name.md:1:1 sub:zorblax") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestScanDigestExemptRows(t *testing.T) {
	repo := newRepo(t, map[string]string{"a.md": "clean\n", "fonts/f.bin": "wOF2\x00\x01 binary"})
	list := writeTemp(t, "list.txt", synthList)
	keyFile := writeTemp(t, "k.key", hexKey("ab")+"\n")
	exempt := writeTemp(t, "exempt.txt", "# fonts\nbinary\tfonts/f.bin\n")
	digest := filepath.Join(t.TempDir(), "digests.json")
	if out, errs, code := runCLI(t, "gen", "-plain", list, "-key-file", keyFile, "-exempt", exempt, "-dir", repo, "-out", digest); code != 0 {
		t.Fatalf("gen: %d %s%s", code, out, errs)
	}
	out, _, code := runCLI(t, "scan", "-plain", list, "-dir", repo)
	if code != 1 || !strings.Contains(out, "FATAL fonts/f.bin") {
		t.Fatalf("without the digest the binary must be fatal: %d\n%s", code, out)
	}
	out, _, code = runCLI(t, "scan", "-plain", list, "-dir", repo, "-digest", digest)
	if code != 0 {
		t.Fatalf("with the exempt row: exit %d\n%s", code, out)
	}
	if m := trailer.FindStringSubmatch(out); m == nil || m[7] != "1" {
		t.Errorf("an exempt binary is still counted in the classes: %q", out)
	}
	// editing the file un-exempts it: the row is bound to the blob id
	if err := os.WriteFile(filepath.Join(repo, "fonts", "f.bin"), []byte("wOF2\x00\x02 changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	out, _, code = runCLI(t, "scan", "-plain", list, "-dir", repo, "-digest", digest)
	if code != 1 || !strings.Contains(out, "exempt row is stale for fonts/f.bin: regenerate the digest file") {
		t.Fatalf("a changed exempt file: exit %d\n%s", code, out)
	}
}

func TestGenkey(t *testing.T) {
	out, _, code := runCLI(t, "genkey")
	if code != 0 || !regexp.MustCompile(`^[0-9a-f]{64}\n$`).MatchString(out) {
		t.Fatalf("genkey to stdout: %d %q", code, out)
	}
	other, _, _ := runCLI(t, "genkey")
	if other == out {
		t.Error("two keys are identical")
	}
	path := filepath.Join(t.TempDir(), "sub", "leak-gate.key")
	out, _, code = runCLI(t, "genkey", "-out", path)
	if code != 0 || out != "" {
		t.Fatalf("genkey -out must print nothing: %d %q", code, out)
	}
	b, err := os.ReadFile(path)
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{64}\n$`).Match(b) {
		t.Fatalf("key file: %q %v", b, err)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Errorf("stat: %v", err)
	} else if os.PathSeparator == '/' && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("key file mode %v (want owner-only)", fi.Mode())
	}
	if _, errs, code := runCLI(t, "genkey", "-out", path); code != 2 || !strings.Contains(errs, "exists") {
		t.Errorf("an existing key file must not be overwritten: %d %q", code, errs)
	}
	if b2, _ := os.ReadFile(path); !bytes.Equal(b, b2) {
		t.Error("the key file changed on a refused overwrite")
	}
	if _, _, code := runCLI(t, "genkey", "-out", path, "-force"); code != 0 {
		t.Errorf("-force: exit %d", code)
	}
}

func TestGenAndCheck(t *testing.T) {
	list := writeTemp(t, "list.txt", synthList)
	keyFile := writeTemp(t, "k.key", hexKey("ab")+"\r\n")
	digest := filepath.Join(t.TempDir(), "testdata", "digests.json")
	out, errs, code := runCLI(t, "gen", "-plain", list, "-key-file", keyFile, "-out", digest)
	if code != 0 {
		t.Fatalf("gen: %d %s%s", code, out, errs)
	}
	if strings.Contains(out+errs, hexKey("ab")) {
		t.Error("gen printed the key")
	}
	if !strings.Contains(out, "wrote 4 entries") {
		t.Errorf("gen output: %q", out)
	}
	b, _ := os.ReadFile(digest)
	if bytes.Contains(b, []byte("zorblax")) || bytes.Contains(b, []byte("quuxel")) || bytes.Contains(b, []byte("\r")) {
		t.Errorf("the digest file holds a name or a CR: %s", b)
	}
	if out, _, code := runCLI(t, "check", "-plain", list, "-key-file", keyFile, "-digest", digest); code != 0 || !strings.HasPrefix(out, "ok:") {
		t.Errorf("check on a fresh file: %d %q", code, out)
	}
	// a CRLF checkout of the same file is the same file
	if err := os.WriteFile(digest, bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _, code := runCLI(t, "check", "-plain", list, "-key-file", keyFile, "-digest", digest); code != 0 {
		t.Errorf("check on a CRLF checkout: %d %q", code, out)
	}
	// a changed list is stale
	list2 := writeTemp(t, "list2.txt", synthList+"sub\tgribble\n")
	if out, _, code := runCLI(t, "check", "-plain", list2, "-key-file", keyFile, "-digest", digest); code != 1 || !strings.HasPrefix(out, "stale:") {
		t.Errorf("check after the list changed: %d %q", code, out)
	}
	// another key is stale as well
	key2 := writeTemp(t, "k2.key", hexKey("cd")+"\n")
	if _, _, code := runCLI(t, "check", "-plain", list, "-key-file", key2, "-digest", digest); code != 1 {
		t.Errorf("check under another key: exit %d", code)
	}
	// a missing digest file is an error, not a pass
	if _, _, code := runCLI(t, "check", "-plain", list, "-key-file", keyFile, "-digest", digest+".missing"); code != 2 {
		t.Errorf("check with no committed file: exit %d", code)
	}
}

func TestGenRefusesBadInput(t *testing.T) {
	keyFile := writeTemp(t, "k.key", hexKey("ab"))
	dir := t.TempDir()
	out := filepath.Join(dir, "d.json")
	for name, body := range map[string]string{
		"a short entry":     "sub\tabc\n",
		"a duplicate":       "sub\tzorblax\nsub\tzorblax\n",
		"an unknown mode":   "fuzzy\tzorblax\n",
		"punctuation":       "sub\tzor-blax\n",
		"a one-word phrase": "phrase\tsolo\n",
		"no tab":            "sub zorblax\n",
		"an empty list":     "# nothing\n",
		"the canary itself": "sub\t" + "zqleak" + "canary9\n",
	} {
		list := writeTemp(t, "l.txt", body)
		_, errs, code := runCLI(t, "gen", "-plain", list, "-key-file", keyFile, "-out", out)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s: a digest file was written", name)
		}
		if strings.Contains(errs, "zorblax") {
			t.Errorf("%s: the message repeats the entry: %q", name, errs)
		}
	}
	list := writeTemp(t, "l.txt", synthList)
	bad := writeTemp(t, "bad.key", "not a key")
	if _, errs, code := runCLI(t, "gen", "-plain", list, "-key-file", bad, "-out", out); code != 2 || strings.Contains(errs, "not a key") {
		t.Errorf("a malformed key file: %d %q", code, errs)
	}
}

func TestGenWithoutAnyKeyIsAnError(t *testing.T) {
	old := userConfigDir
	userConfigDir = func() (string, error) { return t.TempDir(), nil }
	t.Cleanup(func() { userConfigDir = old })
	list := writeTemp(t, "l.txt", synthList)
	_, errs, code := runCLI(t, "gen", "-plain", list, "-out", filepath.Join(t.TempDir(), "d.json"))
	if code != 2 || !strings.Contains(errs, "no gate key") {
		t.Fatalf("exit %d %q", code, errs)
	}
}

func TestGenFindsTheDefaultKeyFile(t *testing.T) {
	cfg := t.TempDir()
	old := userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = old })
	if _, _, code := runCLI(t, "genkey", "-out", defaultKeyFile()); code != 0 {
		t.Fatal("genkey into the default location")
	}
	list := writeTemp(t, "l.txt", synthList)
	if out, errs, code := runCLI(t, "gen", "-plain", list, "-out", filepath.Join(t.TempDir(), "d.json")); code != 0 {
		t.Fatalf("gen with the default key file: %d %s%s", code, out, errs)
	}
}

func TestPatternsCommand(t *testing.T) {
	list := writeTemp(t, "l.txt", synthList)
	out, _, code := runCLI(t, "patterns", "-plain", list)
	if code != 0 || !strings.HasPrefix(out, "# BEGIN leak-gate patterns") || !strings.HasSuffix(out, "# END leak-gate patterns\n") {
		t.Fatalf("patterns: %d %q", code, out)
	}
	if !strings.Contains(out, "[Zz][Oo][Rr][Bb][Ll][Aa][Xx]") || strings.Contains(strings.ToLower(out), "zqleak"+"canary9") {
		t.Errorf("patterns: %q", out)
	}
	target := writeTemp(t, "patterns.local", "keep me\n")
	if _, _, code := runCLI(t, "patterns", "-plain", list, "-into", target); code != 0 {
		t.Fatal("patterns -into")
	}
	first, _ := os.ReadFile(target)
	if !strings.HasPrefix(string(first), "keep me\n# BEGIN") {
		t.Errorf("appended block: %q", first)
	}
	list2 := writeTemp(t, "l2.txt", "sub\tgribble\n")
	if _, _, code := runCLI(t, "patterns", "-plain", list2, "-into", target); code != 0 {
		t.Fatal("patterns -into again")
	}
	second, _ := os.ReadFile(target)
	if strings.Count(string(second), "# BEGIN") != 1 || strings.Contains(string(second), "Zz") || !strings.Contains(string(second), "[Gg][Rr]") || !strings.HasPrefix(string(second), "keep me\n") {
		t.Errorf("a re-run must replace the block: %q", second)
	}
}

func TestParseLsTree(t *testing.T) {
	sha1, sha2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	got, err := parseLsTree([]byte("100644 blob " + sha1 + "\ta b/c.md\x00160000 commit " + sha2 + "\tsub\x00"))
	if err != nil || len(got) != 2 || got[0].Path != "a b/c.md" || got[0].Mode != "100644" || got[0].Blob != sha1 || got[1].Mode != "160000" {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"100644 blob " + sha1 + "\tx", "garbage\x00", "100644 blob\tx\x00"} {
		if _, err := parseLsTree([]byte(bad)); err == nil {
			t.Errorf("parseLsTree(%q) accepted a malformed record", bad)
		}
	}
}

// TestScanDirAndRefAreExclusive uses a valid list and a valid repository, so the
// only thing wrong with the command line is asking for both sources at once.
func TestScanDirAndRefAreExclusive(t *testing.T) {
	repo := newRepo(t, map[string]string{"a.md": "clean\n"})
	list := writeTemp(t, "list.txt", synthList)
	_, errs, code := runCLI(t, "scan", "-plain", list, "-dir", repo, "-ref", "HEAD", "-repo", repo)
	if code != 2 || !strings.Contains(errs, "exclusive") {
		t.Fatalf("exit %d %q", code, errs)
	}
	if _, _, code := runCLI(t, "scan", "-plain", list, "-ref", "HEAD", "-repo", repo); code != 0 {
		t.Errorf("-ref alone: exit %d", code)
	}
	if _, _, code := runCLI(t, "scan", "-plain", list, "-dir", repo); code != 0 {
		t.Errorf("-dir alone: exit %d", code)
	}
}

func TestScanOnAFreshRepositoryWithNoFilesIsAnError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	list := writeTemp(t, "l.txt", synthList)
	if _, _, code := runCLI(t, "scan", "-plain", list, "-dir", dir); code != 2 {
		t.Errorf("an empty tree is a blind gate, not a pass: exit %d", code)
	}
}
