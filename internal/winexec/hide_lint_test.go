package winexec

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNoDirectExecInProduction makes the no-visible-windows house rule executable: every child the
// harness starts is built with winexec.Command / winexec.CommandContext (or winexec.HideWindow on a
// command built elsewhere), so on Windows it opens no console window. A console program started by a
// process that has no console of its own (a detached holder, a service, a launcher started hidden)
// gets a NEW visible window otherwise, and with Windows Terminal as the default terminal that is a
// Windows Terminal window on the operator's desktop - one test run put several there, one titled
// taskkill.exe.
//
// A direct exec.Command / exec.CommandContext in a non-test file that builds on Windows fails here
// unless it is in directExecAllowlist with the reason it must stay as it is. The list is keyed by
// file and enclosing function and counted, so a second call added inside a listed function fails too,
// and an entry whose call is gone must be removed. tools/ and contracts/ are separate modules (they
// cannot import this package) and are out of scope, like the fence lints at the repo root.
func TestNoDirectExecInProduction(t *testing.T) {
	root := moduleRoot(t)
	got := directExecSites(t, root)
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
		want, ok := directExecAllowlist[key]
		switch {
		case !ok:
			t.Errorf("%s builds %d child process(es) with os/exec directly: use winexec.Command / winexec.CommandContext so it opens no console window on Windows, or list it in directExecAllowlist with the reason it must stay visible", key, n)
		case n > want.count:
			t.Errorf("%s builds %d child process(es) with os/exec directly, the allowlist says %d: use winexec.Command for the new one, or raise the count with its reason", key, n, want.count)
		case n < want.count:
			t.Errorf("%s builds %d child process(es) with os/exec directly, the allowlist says %d: lower the count so the list stays the truth", key, n, want.count)
		}
	}
	for key := range directExecAllowlist {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is in directExecAllowlist but builds no os/exec child any more: remove it", key)
		}
	}
	for key, e := range directExecAllowlist {
		if strings.TrimSpace(e.why) == "" {
			t.Errorf("%s: allowlist entry carries no reason", key)
		}
	}
}

type directExecEntry struct {
	count int
	why   string
}

// directExecAllowlist: "file:func" -> the direct os/exec constructors that may stay.
var directExecAllowlist = map[string]directExecEntry{
	"internal/winexec/hide.go:Command":            {1, "the sanctioned constructor itself"},
	"internal/winexec/hide.go:CommandContext":     {1, "the sanctioned constructor itself"},
	"internal/winexec/hide.go:CommandDetached":    {1, "the sanctioned constructor itself"},
	"internal/winexec/hide.go:CommandIfNoConsole": {1, "the sanctioned constructor itself"},
	"gpu_cmd.go:detachHolder":                     {1, "hidden through hideWindow on the next line (gpu_hide_windows.go, which is winexec.HideWindow): the detached holder must never own a window, closing one kills the hold"},
	"cmd/leakdigest/main.go:git":                  {1, "a developer gate tool run from a terminal that reads git output; it is not one of the harness's running processes"},
	"cmd/leakdigest/main.go:newRefFS":             {1, "a developer gate tool run from a terminal that reads git output; it is not one of the harness's running processes"},
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

// directExecSites walks every non-test Go file of the root module that builds for Windows and
// returns "file:func" -> number of exec.Command / exec.CommandContext calls in it.
func directExecSites(t *testing.T, root string) map[string]int {
	t.Helper()
	fset := token.NewFileSet()
	sites := map[string]int{}
	win := build.Default
	win.GOOS, win.GOARCH, win.CgoEnabled = "windows", "amd64", false
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "tools", "contracts", "testdata", "node_modules", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// a file that does not build for Windows (a _linux.go, a !windows tag) cannot open a window there
		if ok, merr := win.MatchFile(filepath.Dir(path), filepath.Base(path)); merr != nil || !ok {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		execName := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "os/exec" {
				execName = "exec"
				if imp.Name != nil {
					execName = imp.Name.Name
				}
			}
		}
		if execName == "" || execName == "_" {
			return nil
		}
		for _, decl := range f.Decls {
			fn := "<file-scope>"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					fn = recvName(fd.Recv.List[0].Type) + "." + fn
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != execName {
					return true
				}
				if sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext" {
					sites[rel+":"+fn]++
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sites
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
