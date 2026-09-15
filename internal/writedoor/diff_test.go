package writedoor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTakeSkipsNonRegularAndRecordsTree pins the two snapshot invariants the
// door depends on: every regular file under the root is recorded by its
// slash-separated relative path, and nothing else is.
func TestTakeSkipsNonRegularAndRecordsTree(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "one" + "\n")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "sub", "b.txt"), "two" + "\n")

	snap, err := Take(root)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d entries, want 2: %v", len(snap), snap)
	}
	if snap["sub/b.txt"] != "two"+"\n" {
		t.Errorf("sub/b.txt = %q; the key must be slash-separated and the content verbatim", snap["sub/b.txt"])
	}
}

// TestTakeOnMissingRootIsEmptyNotError: the write root is created on demand, so
// "not there yet" is a legitimate BEFORE state. An error here would turn every
// first write into an infrastructure defer.
func TestTakeOnMissingRootIsEmptyNotError(t *testing.T) {
	snap, err := Take(filepath.Join(t.TempDir(), "never-created"))
	if err != nil {
		t.Fatalf("Take on a missing root: %v", err)
	}
	if len(snap) != 0 {
		t.Fatalf("want the empty snapshot, got %v", snap)
	}
}

// TestChangesClassifiesAndSortsAndIgnoresEqual: the touched-path list is the
// caller's index into the diff, so its order must be deterministic (map
// iteration is not) and an unchanged file must never appear in it.
func TestChangesClassifiesAndSortsAndIgnoresEqual(t *testing.T) {
	before := Snapshot{"z.go": "old", "same.go": "s", "gone.go": "g"}
	after := Snapshot{"z.go": "new", "same.go": "s", "a.go": "fresh"}
	got := Changes(before, after)
	want := []struct{ path, kind string }{
		{"a.go", "added"},
		{"gone.go", "deleted"},
		{"z.go", "modified"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Path != w.path || got[i].Kind != w.kind {
			t.Errorf("change %d = %s/%s, want %s/%s", i, got[i].Path, got[i].Kind, w.path, w.kind)
		}
	}
}

// TestUnifiedEmptyChangesRendersNothing: "nothing changed" must be
// distinguishable at a glance from "changed, but the hunks are elsewhere".
func TestUnifiedEmptyChangesRendersNothing(t *testing.T) {
	diff, files := Unified(nil)
	if diff != "" || files != nil {
		t.Fatalf("Unified(nil) = %q, %v; want empty and nil", diff, files)
	}
}

// TestUnifiedDiffAppliesWithGit is the REAL check on the renderer: the diff is
// fed to `git apply` against the before-tree and the result must equal the
// after-tree byte for byte. A hunk header off by one, a missed context line or
// a wrong range renders a diff that LOOKS right and applies as garbage — the
// exact failure a string-comparison test cannot see.
func TestUnifiedDiffAppliesWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH: cannot prove the diff applies")
	}
	cases := []struct{ name, before, after string }{
		{"one line changed in the middle",
			joinLines("package p", "", "func f(n int) int {", "	return n - 1", "}", "", "// tail"),
			joinLines("package p", "", "func f(n int) int {", "	return n + 1", "}", "", "// tail")},
		{"line inserted", joinLines("a", "b", "c"), joinLines("a", "b", "b2", "c")},
		{"line deleted", joinLines("a", "b", "c"), joinLines("a", "c")},
		{"two distant edits",
			joinLines("1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14"),
			joinLines("1x", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14x")},
		{"two near edits",
			joinLines("1", "2", "3", "4", "5", "6"),
			joinLines("1x", "2", "3", "4", "5x", "6")},
		{"whole file replaced", joinLines("old1", "old2"), joinLines("new1", "new2", "new3")},
		{"file created", "", joinLines("brand", "new")},
		{"file emptied", joinLines("was", "here"), ""},
		{"no trailing newline on both sides", "a" + "\n" + "b", "a" + "\n" + "c"},
		{"trailing newline added", "a" + "\n" + "b", "a" + "\n" + "b" + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			run(t, work, "git", "init", "-q")
			// The operator's global core.autocrlf would rewrite every applied
			// line ending and make this test assert the checkout filter
			// instead of the renderer. Pin the repo to byte-for-byte.
			run(t, work, "git", "config", "core.autocrlf", "false")
			run(t, work, "git", "config", "core.eol", "lf")
			target := filepath.Join(work, "f.txt")
			if tc.before != "" {
				mustWrite(t, target, tc.before)
			}
			kind := "modified"
			switch {
			case tc.before == "":
				kind = "added"
			case tc.after == "":
				// An emptied file is still a modification in this door: the
				// path survives, its content does not.
			}
			diff, files := Unified([]Change{{Path: "f.txt", Kind: kind, Before: tc.before, After: tc.after}})
			if len(files) != 1 || files[0] != "f.txt" {
				t.Fatalf("touched files = %v, want [f.txt]", files)
			}
			patch := filepath.Join(work, "p.diff")
			mustWrite(t, patch, diff)
			if out, err := exec.Command("git", "-C", work, "apply", "--whitespace=nowarn", "p.diff").CombinedOutput(); err != nil {
				t.Fatalf("git apply refused the diff (%v): %s"+"\n"+"--- diff ---"+"\n"+"%s", err, out, diff)
			}
			got, err := os.ReadFile(target)
			if err != nil && tc.after != "" {
				t.Fatalf("reading applied file: %v", err)
			}
			if string(got) != tc.after {
				t.Errorf("applied content = %q, want %q"+"\n"+"--- diff ---"+"\n"+"%s", string(got), tc.after, diff)
			}
		})
	}
}

// TestUnifiedRendersOversizeFileAsOpaque: a file too large to read into the
// snapshot must still show up as CHANGED. A cap that hides a mutation is worse
// than no cap.
func TestUnifiedRendersOversizeFileAsOpaque(t *testing.T) {
	before := Snapshot{"big.bin": opaque(2<<20, "too large to diff")}
	after := Snapshot{"big.bin": opaque(3<<20, "too large to diff")}
	changes := Changes(before, after)
	if len(changes) != 1 {
		t.Fatalf("two different oversize versions must compare unequal; got %d changes", len(changes))
	}
	diff, files := Unified(changes)
	if len(files) != 1 || files[0] != "big.bin" {
		t.Fatalf("touched files = %v", files)
	}
	if !strings.Contains(diff, "Binary or oversize file big.bin differs") {
		t.Errorf("oversize rendering missing from diff:"+"\n"+"%s", diff)
	}
	if strings.Contains(diff, "@@") {
		t.Errorf("an opaque side must not render hunks (they would apply as garbage):"+"\n"+"%s", diff)
	}
}

// TestEditScriptFallsBackPastTheLCSBound: past lcsMaxLines the middle is
// rendered as a whole replacement rather than a 64-million-cell table.
func TestEditScriptFallsBackPastTheLCSBound(t *testing.T) {
	old := make([]line, lcsMaxLines+10)
	nw := make([]line, lcsMaxLines+10)
	for i := range old {
		old[i] = line{text: "o" + string(rune('a'+i%26))}
		nw[i] = line{text: "n" + string(rune('a'+i%26))}
	}
	script := editScript(old, nw)
	for _, o := range script {
		if o.kind == ' ' {
			t.Fatalf("past the bound the middle must be a whole replacement; found a context line %q", o.line.text)
		}
	}
}

func joinLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}
