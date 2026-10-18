package gpulease

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A reader tells a rename in flight from an absence by the staged file beside the name (settled.go, gate.go). That only works if the
// reader and the writer name the staged file the same way, in Go and in Node. These tests pin it from both ends:
//
//   - every Go writer takes its staged name from stagingPath, so a writer cannot stage under a name no reader looks for;
//   - every suffix stagingPath can return is one render/lease-read.mjs looks for (STAGING_SUFFIXES).

// goSources returns the non-test .go files of this package (name -> text).
func goSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	return out
}

func TestNoGoWriterBuildsAStagingNameWithoutTheHelper(t *testing.T) {
	literal := regexp.MustCompile(`"\.(tmp|restamp)"`)
	for name, src := range goSources(t) {
		if name == "settled.go" {
			continue // stagingPath itself
		}
		for i, line := range strings.Split(src, "\n") {
			code, _, _ := strings.Cut(line, "//")
			if literal.MatchString(code) {
				t.Errorf("%s:%d names a staged file by hand, want stagingPath(<the live name>): %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestTheNodeReaderLooksForEverySuffixAGoWriterStagesUnder(t *testing.T) {
	mjs, err := os.ReadFile(filepath.Join("..", "..", "render", "lease-read.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	decl := regexp.MustCompile(`(?m)^export const STAGING_SUFFIXES = \[([^\]]*)\]`).FindStringSubmatch(string(mjs))
	if decl == nil {
		t.Fatal("render/lease-read.mjs does not declare STAGING_SUFFIXES: a Node reader cannot tell a rename in flight from a lease that is gone")
	}
	node := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(\.[a-z]+)"`).FindAllStringSubmatch(decl[1], -1) {
		node[m[1]] = true
	}

	// every suffix stagingPath can return: the string literals of its body
	src := goSources(t)["settled.go"]
	start := strings.Index(src, "func stagingPath(")
	if start < 0 {
		t.Fatal("settled.go has no stagingPath")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	var goSuffixes []string
	for _, m := range regexp.MustCompile(`\+ "(\.[a-z]+)"`).FindAllStringSubmatch(body, -1) { // what is appended, not the claim test
		goSuffixes = append(goSuffixes, m[1])
	}
	if len(goSuffixes) < 2 {
		t.Fatalf("found %v in stagingPath, want at least .tmp and .restamp: the test no longer reads it", goSuffixes)
	}
	for _, s := range goSuffixes {
		if !node[s] {
			t.Errorf("stagingPath can stage under %q and render/lease-read.mjs STAGING_SUFFIXES (%v) does not look for it", s, decl[1])
		}
	}

	// and what stagingPath returns for the files the writers publish is covered, claims excepted (a claim is never staged)
	dir := t.TempDir()
	for _, p := range []string{
		filepath.Join(dir, "hb.7"), filepath.Join(dir, "e", "7.json"), filepath.Join(dir, "meta.json"), filepath.Join(dir, "gpu", "epoch"),
		filepath.Join(dir, "yield.7"), filepath.Join(dir, "orphan.7"), filepath.Join(dir, "seen.7"), filepath.Join(dir, "waiters", "1.2.json"),
	} {
		staged := stagingPath(p)
		if staged == "" {
			t.Errorf("stagingPath(%s) = \"\": a file that is replaced by a rename has a staged name", p)
			continue
		}
		if suffix := strings.TrimPrefix(staged, p); !node[suffix] {
			t.Errorf("stagingPath(%s) stages under %q, which render/lease-read.mjs does not look for", p, suffix)
		}
	}
	if got := stagingPath(filepath.Join(dir, "cards", "gpu-0.claim")); got != "" {
		t.Errorf("stagingPath of a card claim = %q, want none", got)
	}
}

// The line test above sees a staging name spelled ".tmp" or ".restamp" and nothing else: a writer that stages under `path + ".stg"`, a
// Sprintf, a backtick literal or a temp-file call goes by it, and a file staged under a name no reader looks for is a rename in flight
// nobody can see (the original defect). What a writer passes to renameReplacing as the file it stages is the thing to check, so it is
// parsed: the first argument of every call must be a call of stagingPath, or a name whose every assignment in the function is one.
func TestEveryGoWriterRenamesAFileItStagedUnderStagingPath(t *testing.T) {
	fset := token.NewFileSet()
	sites := 0
	isStagingPath := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := call.Fun.(*ast.Ident)
		return ok && id.Name == "stagingPath"
	}
	for name, src := range goSources(t) {
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "renameReplacing" {
				continue
			}
			// every right-hand side a name is given in this function (closures included)
			assigned := map[string][]ast.Expr{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.AssignStmt:
					if len(s.Lhs) == len(s.Rhs) {
						for i, l := range s.Lhs {
							if id, ok := l.(*ast.Ident); ok {
								assigned[id.Name] = append(assigned[id.Name], s.Rhs[i])
							}
						}
					} else {
						for _, l := range s.Lhs {
							if id, ok := l.(*ast.Ident); ok {
								assigned[id.Name] = append(assigned[id.Name], nil) // a multi-value call: not a staging name
							}
						}
					}
				case *ast.ValueSpec:
					for i, id := range s.Names {
						if i < len(s.Values) {
							assigned[id.Name] = append(assigned[id.Name], s.Values[i])
						} else {
							assigned[id.Name] = append(assigned[id.Name], nil)
						}
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "renameReplacing" || len(call.Args) != 2 {
					return true
				}
				sites++
				at := fset.Position(call.Pos())
				arg := call.Args[0]
				if isStagingPath(arg) {
					return true
				}
				id, ok := arg.(*ast.Ident)
				if !ok {
					t.Errorf("%s:%d renames %T, want a name that stagingPath gave or a call of it", at.Filename, at.Line, arg)
					return true
				}
				rhs := assigned[id.Name]
				if len(rhs) == 0 {
					t.Errorf("%s:%d renames %s, which is not assigned in %s: want stagingPath(<the live name>)", at.Filename, at.Line, id.Name, fn.Name.Name)
				}
				for _, e := range rhs {
					if e == nil || !isStagingPath(e) {
						t.Errorf("%s:%d renames %s, which %s gives a name that is not stagingPath(<the live name>): a reader that asks whether a rename is in flight would not look for it", at.Filename, at.Line, id.Name, fn.Name.Name)
						break
					}
				}
				return true
			})
		}
	}
	if sites < 12 {
		t.Fatalf("found %d calls of renameReplacing in the package's sources, want at least 12 (the writers of the lease files): the test no longer reads them", sites)
	}
}
