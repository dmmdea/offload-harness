package gpulease

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// staging_drift_test.go checks the writers that call renameReplacing. Two writers it cannot see by construction are pinned here:
// one that renames with os.Rename itself, and the Node launch-marker writer, whose staged name is read by tree.go.

// goRenamesOutsideRenameReplacing lists, as file:line, every use of os.Rename in the sources outside renameReplacing (the one place
// that retries the rename and the only caller of it). A call and a reference as a value both count.
func goRenamesOutsideRenameReplacing(t *testing.T, sources map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var found []string
	for name, src := range sources {
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if isFunc && fn.Name.Name == "renameReplacing" {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Rename" {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "os" {
					at := fset.Position(sel.Pos())
					found = append(found, at.Filename+":"+strconv.Itoa(at.Line))
				}
				return true
			})
		}
	}
	return found
}

// A writer that stages under a hand-spelled name and renames with os.Rename itself is not a renameReplacing call, so the drift test
// over those calls never looks at it, and a NEW such writer leaves the count of call sites where it was. Every rename of a lease
// file goes through renameReplacing, which is what the drift test reads.
func TestNoGoWriterRenamesWithOsRenameOutsideRenameReplacing(t *testing.T) {
	for _, at := range goRenamesOutsideRenameReplacing(t, goSources(t)) {
		t.Errorf("%s renames with os.Rename: a lease file is replaced through renameReplacing(stagingPath(<the live name>), <the live name>), the call TestEveryGoWriterRenamesAFileItStagedUnderStagingPath reads", at)
	}
}

// The check itself: a writer that renames with os.Rename is named, wherever it sits, and renameReplacing's own call is not.
func TestTheOsRenameCheckNamesAWriterThatBypassesRenameReplacing(t *testing.T) {
	const bypass = `package gpulease

import "os"

func writeSomething(path string, b []byte) error {
	tmp := path + ".stg"
	if err := os.WriteFile(tmp, b, 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
`
	const viaValue = `package gpulease

import "os"

var rename = os.Rename
`
	const own = `package gpulease

import "os"

func renameReplacing(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
`
	if got := goRenamesOutsideRenameReplacing(t, map[string]string{"a.go": bypass}); len(got) != 1 {
		t.Errorf("a writer that renames with os.Rename: found %v, want one", got)
	}
	if got := goRenamesOutsideRenameReplacing(t, map[string]string{"a.go": viaValue}); len(got) != 1 {
		t.Errorf("os.Rename taken as a value: found %v, want one", got)
	}
	if got := goRenamesOutsideRenameReplacing(t, map[string]string{"a.go": own}); len(got) != 0 {
		t.Errorf("renameReplacing's own rename is the one allowed: found %v", got)
	}
}

// tree.go reads a launch marker through stagedInFlight, so it looks for the staged name stagingPath gives the marker. The marker
// is written by Node (render/comfy-ownership.mjs: restampLaunchOwner and stampLaunchFamily stage a file beside the marker and
// rename it over), so the staged name is spelled there. It must be the one stagingPath gives, or a marker mid-rename reads as no
// marker and a dead wrapper's lease is reclaimed with its instance running.
func TestTheNodeLaunchMarkerWriterStagesUnderTheNameStagingPathGives(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "render", "comfy-ownership.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")
	renames := regexp.MustCompile(`\brenameSync\(`).FindAllString(src, -1)
	staged := regexp.MustCompile(`\btmp\s*=\s*path\s*\+\s*"(\.[a-z]+)"`).FindAllStringSubmatch(src, -1)
	if len(renames) < 2 {
		t.Fatalf("found %d renameSync calls in render/comfy-ownership.mjs, want at least 2 (restampLaunchOwner and stampLaunchFamily): the test no longer reads the writers", len(renames))
	}
	if len(staged) != len(renames) {
		t.Fatalf("found %d renameSync calls and %d staged names of the form `tmp = path + \".<suffix>\"` in render/comfy-ownership.mjs: a writer's staged name is spelled some other way, and this test cannot check it against stagingPath", len(renames), len(staged))
	}
	dir := t.TempDir()
	for _, marker := range []string{".offload-launch.json", ".offload-launch-side.json"} {
		path := filepath.Join(dir, marker)
		want := strings.TrimPrefix(stagingPath(path), path)
		if want == "" || want == stagingPath(path) {
			t.Fatalf("stagingPath(%s) = %q: the marker has no staged name, and tree.go would never look for one", path, stagingPath(path))
		}
		for _, m := range staged {
			if m[1] != want {
				t.Errorf("render/comfy-ownership.mjs stages a launch marker under %q, and stagingPath(%s) is %q: the reader would not see a rename in flight", m[1], marker, want)
			}
		}
	}
}
