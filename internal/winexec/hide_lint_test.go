package winexec

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestEveryChildProcessStartIsHidden makes the no-visible-windows house rule executable: every child the
// harness (or its tests) starts opens no console window on Windows. A console program started by a
// process that has no console of its own (a detached holder, a service, a launcher started hidden) gets a
// NEW visible console otherwise, and with Windows Terminal as the default terminal that is a Windows
// Terminal window on the operator's desktop - one test run put several there, one titled taskkill.exe.
//
// The walk reads every Go file of the repository that builds for Windows: production code, tests, and the
// separate modules under tools/ and contracts/ (it only parses them, it never imports them). A process
// start is
//   - a call of exec.Command / exec.CommandContext,
//   - one of those used as a value (`var run = exec.Command`),
//   - an exec.Cmd composite literal,
//   - os.StartProcess / syscall.StartProcess, and
//   - a syscall / golang.org/x/sys/windows CreateProcess* call.
//
// A start is hidden, and needs no entry, when its own function shows it: it calls one of the hiding
// helpers (winexec.HideWindow and its siblings, or a module's own hideWindow / hideSpawnedWindow) at least
// once per start, or - for a CreateProcess* call - names both CREATE_NO_WINDOW and SW_HIDE. Anything else
// is listed in processStartAllowlist with the reason it must stay as it is, keyed by file and enclosing
// function and counted, so a second start added inside a listed function fails too and an entry whose start
// is gone must be removed.
func TestEveryChildProcessStartIsHidden(t *testing.T) {
	root := moduleRoot(t)
	got := unhiddenStarts(t, root)
	if os.Getenv("DIRECT_EXEC_LINT_PRINT") != "" {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("\t%q: {%d, \"\"},\n", k, got[k])
		}
		t.Skip("printed the current sites")
	}
	for key, n := range got {
		want, ok := processStartAllowlist[key]
		switch {
		case !ok:
			t.Errorf("%s starts %d child process(es) that nothing hides: use winexec.Command / winexec.CommandContext (or winexec.HideWindow on a command built elsewhere) so it opens no console window on Windows, or list it in processStartAllowlist with the reason it must stay visible", key, n)
		case n > want.count:
			t.Errorf("%s starts %d child process(es) that nothing hides, the allowlist says %d: hide the new one, or raise the count with its reason", key, n, want.count)
		case n < want.count:
			t.Errorf("%s starts %d child process(es) that nothing hides, the allowlist says %d: lower the count so the list stays the truth", key, n, want.count)
		}
	}
	for key := range processStartAllowlist {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is in processStartAllowlist but starts no unhidden child any more: remove it", key)
		}
	}
	for key, e := range processStartAllowlist {
		if strings.TrimSpace(e.why) == "" {
			t.Errorf("%s: allowlist entry carries no reason", key)
		}
	}
}

type allowEntry struct {
	count int
	why   string
}

// processStartAllowlist: "file:func" -> the starts that may stay.
var processStartAllowlist = map[string]allowEntry{
	"internal/winexec/console_windows_test.go:runHelper":                 {1, "starts the helper with explicit creation flags (a hidden console, or none at all) so the console state it reports is known: nothing it starts can open a window"},
	"internal/winexec/hide_test.go:TestHideWindow_ReturnsTheSameCommand": {1, "builds commands to inspect them and never starts one"},
}

// hiders are the functions that make a command's child window-less: winexec's and the separate modules' own.
var hiders = map[string]bool{
	"HideWindow":            true,
	"HideWindowDetached":    true,
	"HideWindowIfNoConsole": true,
	"hideWindow":            true,
	"hideSpawnedWindow":     true,
}

// lintSite is one place that starts a child process.
type lintSite struct {
	Func   string // the enclosing function, "<file-scope>" outside one
	Kind   string // call, value, literal, startprocess, createprocess
	Line   int
	Hidden bool // the structure of its own function hides it
}

// analyzeGoSource lists the process starts in one Go source file and says which of them hide themselves.
func analyzeGoSource(filename string, src []byte) ([]lintSite, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	local := func(path string) string {
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) != path {
				continue
			}
			if imp.Name != nil {
				return imp.Name.Name
			}
			return path[strings.LastIndex(path, "/")+1:]
		}
		return ""
	}
	execN, osN, sysN, winN := local("os/exec"), local("os"), local("syscall"), local("golang.org/x/sys/windows")
	if execN == "" && osN == "" && sysN == "" && winN == "" {
		return nil, nil
	}
	is := func(e ast.Expr, name string) bool {
		id, ok := e.(*ast.Ident)
		return ok && name != "" && name != "_" && name != "." && id.Name == name
	}
	var out []lintSite
	for _, decl := range f.Decls {
		fn := "<file-scope>"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				fn = recvName(fd.Recv.List[0].Type) + "." + fn
			}
		}
		callees := map[ast.Node]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				callees[c.Fun] = true
			}
			return true
		})
		var sites []lintSite
		hideCalls := 0
		mentions := map[string]bool{}
		add := func(kind string, pos token.Pos) {
			sites = append(sites, lintSite{Func: fn, Kind: kind, Line: fset.Position(pos).Line})
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				switch fun := x.Fun.(type) {
				case *ast.Ident:
					if hiders[fun.Name] {
						hideCalls++
					}
				case *ast.SelectorExpr:
					if hiders[fun.Sel.Name] {
						hideCalls++
					}
					switch {
					case is(fun.X, execN) && (fun.Sel.Name == "Command" || fun.Sel.Name == "CommandContext"):
						add("call", x.Pos())
					case (is(fun.X, osN) || is(fun.X, sysN)) && fun.Sel.Name == "StartProcess":
						add("startprocess", x.Pos())
					case (is(fun.X, sysN) || is(fun.X, winN)) && strings.HasPrefix(fun.Sel.Name, "CreateProcess"):
						add("createprocess", x.Pos())
					}
				}
			case *ast.SelectorExpr:
				if is(x.X, execN) && (x.Sel.Name == "Command" || x.Sel.Name == "CommandContext") && !callees[x] {
					add("value", x.Pos())
				}
				if is(x.X, winN) || is(x.X, sysN) {
					mentions[x.Sel.Name] = true
				}
			case *ast.CompositeLit:
				if sel, ok := x.Type.(*ast.SelectorExpr); ok && is(sel.X, execN) && sel.Sel.Name == "Cmd" {
					add("literal", x.Pos())
				}
			}
			return true
		})
		starts := 0
		for _, s := range sites {
			if s.Kind != "createprocess" {
				starts++
			}
		}
		for i := range sites {
			if sites[i].Kind == "createprocess" {
				sites[i].Hidden = mentions["CREATE_NO_WINDOW"] && mentions["SW_HIDE"]
			} else {
				sites[i].Hidden = hideCalls >= starts
			}
		}
		out = append(out, sites...)
	}
	return out, nil
}

// moduleRoot: the directory of the go.mod that holds this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}

// unhiddenStarts walks every Go file under root that builds for Windows and returns "file:func" -> the number
// of process starts in it that nothing hides.
func unhiddenStarts(t *testing.T, root string) map[string]int {
	t.Helper()
	got := map[string]int{}
	win := build.Default
	win.GOOS, win.GOARCH, win.CgoEnabled = "windows", "amd64", false
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "testdata", "node_modules", "bin", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// a file that does not build for Windows (a _linux.go, a !windows tag) cannot open a window there
		if ok, merr := win.MatchFile(filepath.Dir(path), filepath.Base(path)); merr != nil || !ok {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		sites, aerr := analyzeGoSource(rel, src)
		if aerr != nil {
			return fmt.Errorf("%s: %w", rel, aerr)
		}
		for _, s := range sites {
			if s.Hidden {
				continue
			}
			got[rel+":"+s.Func]++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func recvName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return recvName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return recvName(x.X)
	}
	return "?"
}

// ---- the analyzer itself -------------------------------------------------------------------------------

func starts(t *testing.T, src string) (unhidden, hidden []string) {
	t.Helper()
	sites, err := analyzeGoSource("x.go", []byte("package p\n"+src))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sites {
		label := s.Func + ":" + s.Kind
		if s.Hidden {
			hidden = append(hidden, label)
		} else {
			unhidden = append(unhidden, label)
		}
	}
	return
}

func TestLint_SeesEveryWayToStartAProcess(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"exec.Command":         {"import \"os/exec\"\nfunc f() { exec.Command(\"x\") }", "f:call"},
		"exec.CommandContext":  {"import \"os/exec\"\nfunc f(c C) { exec.CommandContext(c, \"x\") }", "f:call"},
		"a function value":     {"import \"os/exec\"\nvar run = exec.Command", "<file-scope>:value"},
		"passed on":            {"import \"os/exec\"\nfunc f() { g(exec.CommandContext) }", "f:value"},
		"a Cmd literal":        {"import \"os/exec\"\nfunc f() { _ = &exec.Cmd{Path: \"x\"} }", "f:literal"},
		"os.StartProcess":      {"import \"os\"\nfunc f() { os.StartProcess(\"x\", nil, nil) }", "f:startprocess"},
		"syscall.StartProcess": {"import \"syscall\"\nfunc f() { syscall.StartProcess(\"x\", nil, nil) }", "f:startprocess"},
		"CreateProcessAsUser":  {"import \"golang.org/x/sys/windows\"\nfunc f() { windows.CreateProcessAsUser(0, nil, nil, nil, nil, true, 0, nil, nil, nil, nil) }", "f:createprocess"},
		"CreateProcess":        {"import \"syscall\"\nfunc f() { syscall.CreateProcess(nil, nil, nil, nil, true, 0, nil, nil, nil, nil) }", "f:createprocess"},
		"a renamed import":     {"import osexec \"os/exec\"\nfunc f() { osexec.Command(\"x\") }", "f:call"},
		"a method":             {"import \"os/exec\"\nfunc (s *S) run() { exec.Command(\"x\") }", "S.run:call"},
	} {
		un, _ := starts(t, tc.src)
		if len(un) != 1 || un[0] != tc.want {
			t.Errorf("%s: unhidden starts = %v, want [%s]", name, un, tc.want)
		}
	}
}

func TestLint_ASelfHidingFunctionNeedsNoEntry(t *testing.T) {
	for name, src := range map[string]string{
		"winexec.HideWindow":      "import (\"os/exec\"; \"w/winexec\")\nfunc f() { c := exec.Command(\"x\"); winexec.HideWindow(c) }",
		"wrapped in the helper":   "import (\"os/exec\"; \"w/winexec\")\nfunc f() { winexec.HideWindow(exec.Command(\"x\")) }",
		"detached":                "import (\"os/exec\"; \"w/winexec\")\nfunc f() { winexec.HideWindowDetached(exec.Command(\"x\")) }",
		"a module's hideWindow":   "import \"os/exec\"\nfunc f() { c := exec.Command(\"x\"); hideWindow(c) }",
		"hideSpawnedWindow":       "import \"os/exec\"\nfunc f() { c := exec.Command(\"x\"); hideSpawnedWindow(c) }",
		"CreateProcess with both": "import \"golang.org/x/sys/windows\"\nfunc f() { _ = windows.CREATE_NO_WINDOW; _ = windows.SW_HIDE; windows.CreateProcessAsUser(0, nil, nil, nil, nil, true, 0, nil, nil, nil, nil) }",
	} {
		un, hid := starts(t, src)
		if len(un) != 0 || len(hid) != 1 {
			t.Errorf("%s: unhidden %v hidden %v, want one hidden start", name, un, hid)
		}
	}
}

func TestLint_AHideCallPerStartAndBothFlagsOfACreateProcess(t *testing.T) {
	// two starts, one hide: neither is vouched for
	un, _ := starts(t, "import \"os/exec\"\nfunc f() { a := exec.Command(\"x\"); b := exec.Command(\"y\"); hideWindow(a); _ = b }")
	if len(un) != 2 {
		t.Errorf("two starts and one hide call: unhidden = %v, want both", un)
	}
	// a hide call in ANOTHER function does not hide this one
	un, _ = starts(t, "import \"os/exec\"\nfunc f() { exec.Command(\"x\") }\nfunc g(c *exec.Cmd) { hideWindow(c) }")
	if len(un) != 1 {
		t.Errorf("a hide call in another function: unhidden = %v, want the start", un)
	}
	// one flag of the two is not enough for a CreateProcess call
	for name, flags := range map[string]string{"only CREATE_NO_WINDOW": "_ = windows.CREATE_NO_WINDOW", "only SW_HIDE": "_ = windows.SW_HIDE", "neither": "_ = windows.CREATE_SUSPENDED"} {
		un, _ = starts(t, "import \"golang.org/x/sys/windows\"\nfunc f() { "+flags+"; windows.CreateProcessAsUser(0, nil, nil, nil, nil, true, 0, nil, nil, nil, nil) }")
		if len(un) != 1 {
			t.Errorf("%s: unhidden = %v, want the CreateProcess call", name, un)
		}
	}
}

func TestLint_FilesThatStartNothingAreQuiet(t *testing.T) {
	for name, src := range map[string]string{
		"no imports":          "func f() {}",
		"os/exec not called":  "import \"os/exec\"\nfunc f() (*exec.Cmd, error) { _, err := exec.LookPath(\"x\"); return nil, err }",
		"a method called run": "import \"os\"\nfunc f(c C) { c.Command(\"x\"); os.Getpid() }",
	} {
		if un, hid := starts(t, src); len(un)+len(hid) != 0 {
			t.Errorf("%s: sites %v %v, want none", name, un, hid)
		}
	}
}

func TestLint_WalksTestsAndOtherModulesAndSkipsWhatCannotBuildForWindows(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	start := "package p\n\nimport \"os/exec\"\n\nfunc f() { exec.Command(\"x\") }\n"
	write("go.mod", "module m\n")
	write("a_test.go", start)               // a test file is walked
	write("tools/m2/go.mod", "module m2\n") // so is another module
	write("tools/m2/b.go", start)           //
	write("pkg/c_linux.go", start)          // a file that does not build for Windows is not
	write("pkg/d.go", "//go:build !windows\n\n"+start)
	write("testdata/e.go", start) // fixtures are skipped
	write("pkg/hidden.go", "package p\n\nimport \"os/exec\"\n\nfunc f() { c := exec.Command(\"x\"); hideWindow(c) }\n")
	got := unhiddenStarts(t, root)
	want := map[string]int{"a_test.go:f": 1, "tools/m2/b.go:f": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unhidden starts = %v, want %v", got, want)
	}
}
