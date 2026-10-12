package main

// GPU-I8: one lint for the whole repository that keeps tests from leaving files in the OS temp dir.
//
// WHY. On the machine that runs the gate all day, a test that makes a folder in the OS temp dir (C: on Windows) and
// does not remove it on every path adds one folder per run for ever: a stand-in test binary leaked 22,434 folders and
// about 70 GB in under two hours (F67), and the same shape was found again in three packages (F77, F77b). Each fix came
// with a lint that lived in ONE package, so the next package to do it was not covered. This file is the one lint, over
// every *_test.go file of the module, with the packages that were audited by hand written down.
//
// WHAT IT CHECKS (go/parser, no regular expressions on text):
//
//  1. Every direct use of the OS temp dir in a test file is listed in tempUseAllowlist (tempdir_lint_allowlist_test.go)
//     with its count and the reason its folder is gone on every path. The forms are: os.MkdirTemp / os.CreateTemp /
//     ioutil.TempDir / ioutil.TempFile with an empty directory argument; any os.TempDir; os.Getenv or os.LookupEnv of
//     TMP, TEMP or TMPDIR; and os.MkdirTemp / os.CreateTemp / ioutil.TempDir / ioutil.TempFile passed as a VALUE
//     (the runIsolated(m, os.MkdirTemp, ...) shape, where the empty argument is supplied somewhere the lint cannot
//     read). t.TempDir, and a directory argument that is not the empty string, are clean. A new use fails and names
//     the file and the form; a use that went away fails until the count is brought down, so the list stays the truth.
//  2. Every package whose tests start their own test binary (os.Args[0] or os.Executable) holds
//     TestARelaunchedTestBinaryLeavesNothingInTheTempDir, or is listed in relaunchExempt with the reason a copy
//     leaves nothing. A copy runs the package's init functions, package-level variables and TestMain before any test,
//     and a copy that exits from a test never returns to a cleanup, so this is where a folder per launch comes from.
//     An exemption is void when the package's TestMain, init functions or package-level variables themselves use the
//     OS temp dir (checked on the syntax of those bodies; a call into another function is not followed).
//
// WHAT IT CANNOT SEE. A temp file made by production code that a test calls (F79's class) is not in a test file; the
// tripwire script (scripts/test-temp-tripwire.sh, GPU-I8c) catches that by pointing the OS temp dir at a folder and
// listing what is left. A folder named by a variable that happens to hold the empty string is not seen either.
//
// REGENERATING THE LISTS. TEMPDIR_LINT_PRINT=1 go test . -run 'TestTestsReachForTheOSTempDirOnlyWhereItsRemovalIsOnEveryPath|TestEveryRelaunchingPackageProvesItLeavesNothing' -v
// prints the entries the current tree needs, with an empty reason. The reason is the review: write it only after
// reading the code, and prefer t.TempDir to a listing.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const tempLintPrintEnv = "TEMPDIR_LINT_PRINT"

// relaunchProofName is the test a package holds to show that a re-launched copy of its test binary leaves nothing.
const relaunchProofName = "TestARelaunchedTestBinaryLeavesNothingInTheTempDir"

// The forms of raw temp use. A value-pass form is "value:" plus the function, e.g. "value:os.MkdirTemp".
const (
	formTempDir = "os.TempDir"
	formTempEnv = "os.Getenv(TMP)"
)

// tempFile is what the lint learned from one test file.
type tempFile struct {
	forms      map[string]int // form -> number of uses in the file
	hookForms  int            // uses inside TestMain, init functions and package-level variable initializers
	relaunches bool           // os.Args[0] or os.Executable() appears
	starts     bool           // os.Args[0] or os.Executable() is the program of an exec.Command, exec.CommandContext or os.StartProcess call
	proof      bool           // the file holds a relaunchProofName that really relaunches, points the temp dir and reads the folder
}

// pkgFacts is what the lint learned from one directory's test files.
type pkgFacts struct {
	relaunches bool
	starts     bool
	proof      bool
	hookForms  int
}

type tempAllow struct {
	count int
	why   string
}

// importName returns the local name a file gives the package imported as path, or "" when it is not imported by a
// usable name (not imported, a blank import, a dot import).
func importName(f *ast.File, path, def string) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != path {
			continue
		}
		if imp.Name == nil {
			return def
		}
		if imp.Name.Name == "_" || imp.Name.Name == "." {
			return ""
		}
		return imp.Name.Name
	}
	return ""
}

// pkgSel splits "pkg.Name" into its two identifiers.
func pkgSel(e ast.Expr) (pkg, name string, ok bool) {
	sel, isSel := e.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	id, isID := sel.X.(*ast.Ident)
	if !isID {
		return "", "", false
	}
	return id.Name, sel.Sel.Name, true
}

// stringArg returns the value of the n-th argument when it is a string literal.
func stringArg(call *ast.CallExpr, n int) (string, bool) {
	if len(call.Args) <= n {
		return "", false
	}
	lit, ok := call.Args[n].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// inspectTempForms counts the raw uses of the OS temp dir below node.
func inspectTempForms(node ast.Node, osName, ioutilName string) map[string]int {
	forms := map[string]int{}
	callee := map[ast.Expr]bool{} // the Fun of every call seen so far: a selector in that set is called, not passed
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			callee[x.Fun] = true
			pkg, fn, ok := pkgSel(x.Fun)
			if !ok {
				break
			}
			switch {
			case pkg == osName && (fn == "MkdirTemp" || fn == "CreateTemp"):
				if dir, isStr := stringArg(x, 0); isStr && dir == "" {
					forms["os."+fn+"(empty)"]++
				}
			case pkg == ioutilName && (fn == "TempDir" || fn == "TempFile"):
				if dir, isStr := stringArg(x, 0); isStr && dir == "" {
					forms["ioutil."+fn+"(empty)"]++
				}
			case pkg == osName && (fn == "Getenv" || fn == "LookupEnv"):
				if name, isStr := stringArg(x, 0); isStr && (name == "TMP" || name == "TEMP" || name == "TMPDIR") {
					forms[formTempEnv]++
				}
			}
		case *ast.SelectorExpr:
			pkg, fn, ok := pkgSel(x)
			if !ok {
				break
			}
			switch {
			case pkg == osName && fn == "TempDir":
				forms[formTempDir]++
			case callee[x]:
				// called: judged on its arguments in the CallExpr case
			case pkg == osName && (fn == "MkdirTemp" || fn == "CreateTemp"):
				forms["value:os."+fn]++
			case pkg == ioutilName && (fn == "TempDir" || fn == "TempFile"):
				forms["value:ioutil."+fn]++
			}
		}
		return true
	})
	return forms
}

// isOwnBinary reports whether e is os.Args[0] or a call to os.Executable: the path of the running test binary.
func isOwnBinary(e ast.Expr, osName string) bool {
	switch x := e.(type) {
	case *ast.IndexExpr:
		if pkg, name, ok := pkgSel(x.X); ok && pkg == osName && name == "Args" {
			lit, isLit := x.Index.(*ast.BasicLit)
			return isLit && lit.Kind == token.INT && lit.Value == "0"
		}
	case *ast.CallExpr:
		pkg, name, ok := pkgSel(x.Fun)
		return ok && pkg == osName && name == "Executable"
	}
	return false
}

// relaunchesItself reports whether node refers to os.Args[0] or calls os.Executable.
func relaunchesItself(node ast.Node, osName string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok && isOwnBinary(e, osName) {
			found = true
		}
		return !found
	})
	return found
}

// startsOwnBinary reports whether root hands the running test binary's path to a process start: the program
// argument of exec.Command, exec.CommandContext or os.StartProcess is os.Args[0], a call to os.Executable, or a name
// assigned from one of them. A path passed to some other function is not followed.
func startsOwnBinary(root ast.Node, osName, execName string) bool {
	own := map[string]bool{}
	mark := func(lhs []ast.Expr, rhs []ast.Expr) {
		for i, r := range rhs {
			if !isOwnBinary(r, osName) {
				continue
			}
			if i < len(lhs) {
				if id, ok := lhs[i].(*ast.Ident); ok {
					own[id.Name] = true
				}
			}
		}
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			mark(x.Lhs, x.Rhs)
		case *ast.ValueSpec:
			names := make([]ast.Expr, len(x.Names))
			for i, id := range x.Names {
				names[i] = id
			}
			mark(names, x.Values)
		}
		return true
	})
	isOwn := func(e ast.Expr) bool {
		if isOwnBinary(e, osName) {
			return true
		}
		id, ok := e.(*ast.Ident)
		return ok && own[id.Name]
	}
	found := false
	ast.Inspect(root, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		pkg, fn, ok := pkgSel(call.Fun)
		if !ok {
			return !found
		}
		prog := -1
		switch {
		case pkg == execName && fn == "Command", pkg == osName && fn == "StartProcess":
			prog = 0
		case pkg == execName && fn == "CommandContext":
			prog = 1
		}
		if prog >= 0 && prog < len(call.Args) && isOwn(call.Args[prog]) {
			found = true
		}
		return !found
	})
	return found
}

// holdsRelaunchProof reports whether fn is a real relaunch-leaves-nothing test: it starts the test binary itself (an
// exec call whose program is os.Args[0] or os.Executable), points TMP, TEMP and TMPDIR somewhere, and reads that folder back. A stub with the right name proves nothing.
func holdsRelaunchProof(fn *ast.FuncDecl, osName, execName string) bool {
	if fn.Body == nil || !startsOwnBinary(fn.Body, osName, execName) {
		return false
	}
	pointed := map[string]bool{}
	reads := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil {
					pointed[s] = true
				}
			}
		case *ast.CallExpr:
			if pkg, name, ok := pkgSel(x.Fun); ok && pkg == osName && name == "ReadDir" {
				reads = true
			}
		}
		return true
	})
	return reads && pointed["TMP="] && pointed["TEMP="] && pointed["TMPDIR="]
}

// runsInEveryCopy reports whether a declaration runs in every started copy of the test binary before any test.
func runsInEveryCopy(decl ast.Decl) bool {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return d.Recv == nil && (d.Name.Name == "TestMain" || d.Name.Name == "init")
	case *ast.GenDecl:
		return d.Tok == token.VAR
	}
	return false
}

// analyzeTestSource parses one test file (src is nil to read name from disk).
func analyzeTestSource(fset *token.FileSet, name string, src any) (tempFile, error) {
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return tempFile{}, err
	}
	osName, ioutilName := importName(f, "os", "os"), importName(f, "io/ioutil", "ioutil")
	res := tempFile{forms: map[string]int{}}
	execName := importName(f, "os/exec", "exec")
	res.starts = startsOwnBinary(f, osName, execName)
	for _, decl := range f.Decls {
		hook := runsInEveryCopy(decl)
		for form, n := range inspectTempForms(decl, osName, ioutilName) {
			res.forms[form] += n
			if hook {
				res.hookForms += n
			}
		}
		if relaunchesItself(decl, osName) {
			res.relaunches = true
		}
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == relaunchProofName && holdsRelaunchProof(fd, osName, execName) {
			res.proof = true
		}
	}
	return res, nil
}

// skipTestDir reports whether the walk leaves a directory out: other modules (the repository's tools/ modules, the fixtures, the
// contract modules: any directory with a go.mod of its own), test data, dependencies and the repository's own folder. It goes by
// what a directory IS, never by a name that a package of this module could also carry: a directory called tools or bin is
// scanned like any other unless it holds a go.mod.
func skipTestDir(path, name string) bool {
	switch name {
	case ".git", "testdata", "node_modules":
		return true
	}
	if path == "." {
		return false
	}
	_, err := os.Stat(filepath.Join(path, "go.mod"))
	return err == nil
}

// scanTestTree analyzes every *_test.go file below root and returns them keyed by slash-separated relative path.
func scanTestTree(root string) (map[string]tempFile, error) {
	fset := token.NewFileSet()
	out := map[string]tempFile{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			if skipTestDir(path, d.Name()) && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		tf, perr := analyzeTestSource(fset, path, nil)
		if perr != nil {
			return perr
		}
		out[filepath.ToSlash(rel)] = tf
		return nil
	})
	return out, err
}

// tempUseSites flattens the files into "file:form" -> count.
func tempUseSites(files map[string]tempFile) map[string]int {
	sites := map[string]int{}
	for file, tf := range files {
		for form, n := range tf.forms {
			sites[file+":"+form] += n
		}
	}
	return sites
}

// packageFacts folds the files into one record per directory ("." is the root package).
func packageFacts(files map[string]tempFile) map[string]pkgFacts {
	pkgs := map[string]pkgFacts{}
	for file, tf := range files {
		dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(file)))
		p := pkgs[dir]
		p.relaunches = p.relaunches || tf.relaunches
		p.starts = p.starts || tf.starts
		p.proof = p.proof || tf.proof
		p.hookForms += tf.hookForms
		pkgs[dir] = p
	}
	return pkgs
}

// tempUseProblems compares the uses found with the allowlist and says what to do about each difference.
func tempUseProblems(got map[string]int, allow map[string]tempAllow) []string {
	var out []string
	for key, n := range got {
		file, form, _ := strings.Cut(key, ":")
		want, listed := allow[key]
		switch {
		case !listed:
			out = append(out, fmt.Sprintf("%s uses the OS temp dir %d time(s) as %s and is not in tempUseAllowlist: take the folder from t.TempDir(), or list %q with the reason it is removed on every path", file, n, form, key))
		case n > want.count:
			out = append(out, fmt.Sprintf("%s uses the OS temp dir %d time(s) as %s, the allowlist says %d: use t.TempDir() for the new one, or review it and raise the count of %q with its reason", file, n, form, want.count, key))
		case n < want.count:
			out = append(out, fmt.Sprintf("%s uses the OS temp dir %d time(s) as %s, the allowlist says %d: lower the count of %q so the list stays the truth", file, n, form, want.count, key))
		}
	}
	for key, a := range allow {
		if _, ok := got[key]; !ok {
			out = append(out, fmt.Sprintf("%q is in tempUseAllowlist but no such use exists any more: remove the entry", key))
		}
		if strings.TrimSpace(a.why) == "" {
			out = append(out, fmt.Sprintf("%q is in tempUseAllowlist without a reason: the reason (why its folder is gone on every path) is the review", key))
		}
	}
	sort.Strings(out)
	return out
}

// relaunchProblems judges every directory whose tests refer to the path of their own test binary. exempt lists the
// packages whose started copy leaves nothing (with the reason); neverStarted lists the packages that only read that path
// and never start a copy (with the reason), which the lint checks on the syntax of the package's exec calls.
func relaunchProblems(pkgs map[string]pkgFacts, exempt, neverStarted map[string]string) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	for dir, p := range pkgs {
		reason, isExempt := exempt[dir]
		nsReason, isNever := neverStarted[dir]
		switch {
		case isNever && isExempt:
			add("%s is in both relaunchExempt and relaunchNeverStarted: a package either starts a copy or does not", dir)
		case isNever && strings.TrimSpace(nsReason) == "":
			add("%s is in relaunchNeverStarted without a reason: say what its tests do with the path of their own test binary", dir)
		case isNever && !p.relaunches:
			add("%s is in relaunchNeverStarted but its tests no longer refer to their own test binary: remove the entry", dir)
		case isNever && p.starts:
			add("%s is in relaunchNeverStarted but a test hands the path of its own test binary to exec.Command, exec.CommandContext or os.StartProcess: it starts a copy, so add %s or list it in relaunchExempt", dir, relaunchProofName)
		case isNever && p.proof:
			add("%s holds %s and is also in relaunchNeverStarted: remove the entry, the test is the proof", dir, relaunchProofName)
		case isNever:
		case !p.relaunches && isExempt:
			add("%s is in relaunchExempt but its tests no longer refer to their own test binary: remove the entry", dir)
		case !p.relaunches:
		case p.proof && isExempt:
			add("%s holds %s and is also in relaunchExempt: remove the exemption, the test is the proof", dir, relaunchProofName)
		case p.proof:
		case !isExempt:
			add("%s uses its own test binary (os.Args[0] or os.Executable) and holds no %s, no relaunchExempt entry and no relaunchNeverStarted entry: add that test (see internal/mediaremote/relaunch_leak_test.go), or list %q with the reason a started copy leaves nothing", dir, relaunchProofName, dir)
		case strings.TrimSpace(reason) == "":
			add("%s is in relaunchExempt without a reason: say why a re-launched copy of its test binary leaves nothing", dir)
		case p.hookForms > 0:
			add("%s is exempt from the relaunch proof, but its TestMain, init functions or package-level variables use the OS temp dir %d time(s), and every started copy runs them: add %s", dir, p.hookForms, relaunchProofName)
		}
	}
	for _, list := range []struct {
		name string
		m    map[string]string
	}{{"relaunchExempt", exempt}, {"relaunchNeverStarted", neverStarted}} {
		for dir := range list.m {
			if _, ok := pkgs[dir]; !ok {
				add("%s is in %s but has no test files: remove the entry", dir, list.name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func printMode() bool { return os.Getenv(tempLintPrintEnv) != "" }

// ---------------------------------------------------------------------------------------------------- tests

// TestTempLintSeesEveryFormOfRawTempUse feeds the scanner one synthetic source per form (and the clean controls)
// and requires the exact counts, so a form dropped from the scanner turns this red whatever the tree holds.
func TestTempLintSeesEveryFormOfRawTempUse(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		want  map[string]int
		hooks int
	}{
		{"os.MkdirTemp with an empty directory", `package p
import "os"
func f() { _, _ = os.MkdirTemp("", "x-") }`, map[string]int{"os.MkdirTemp(empty)": 1}, 0},
		{"os.MkdirTemp with an empty raw string", "package p\nimport \"os\"\nfunc f() { _, _ = os.MkdirTemp(``, \"x-\") }", map[string]int{"os.MkdirTemp(empty)": 1}, 0},
		{"os.CreateTemp with an empty directory", `package p
import "os"
func f() { _, _ = os.CreateTemp("", "x-") }`, map[string]int{"os.CreateTemp(empty)": 1}, 0},
		{"ioutil.TempDir with an empty directory", `package p
import "io/ioutil"
func f() { _, _ = ioutil.TempDir("", "x-") }`, map[string]int{"ioutil.TempDir(empty)": 1}, 0},
		{"ioutil.TempFile with an empty directory", `package p
import "io/ioutil"
func f() { _, _ = ioutil.TempFile("", "x-") }`, map[string]int{"ioutil.TempFile(empty)": 1}, 0},
		{"os.TempDir called", `package p
import ("os"; "path/filepath")
func f() string { return filepath.Join(os.TempDir(), "x") }`, map[string]int{"os.TempDir": 1}, 0},
		{"os.TempDir passed as a value", `package p
import "os"
func f() func() string { return os.TempDir }`, map[string]int{"os.TempDir": 1}, 0},
		{"os.MkdirTemp passed as a value", `package p
import "os"
func run(mk func(string, string) (string, error)) {}
func f() { run(os.MkdirTemp) }`, map[string]int{"value:os.MkdirTemp": 1}, 0},
		{"os.CreateTemp kept in a variable", `package p
import "os"
func f() { mk := os.CreateTemp; _, _ = mk("", "x") }`, map[string]int{"value:os.CreateTemp": 1}, 0},
		{"ioutil.TempDir passed as a value", `package p
import "io/ioutil"
func run(mk func(string, string) (string, error)) {}
func f() { run(ioutil.TempDir) }`, map[string]int{"value:ioutil.TempDir": 1}, 0},
		{"ioutil.TempFile passed as a value", `package p
import "io/ioutil"
func f() { _ = ioutil.TempFile }`, map[string]int{"value:ioutil.TempFile": 1}, 0},
		{"TMPDIR read from the environment", `package p
import "os"
func f() string { return os.Getenv("TMPDIR") }`, map[string]int{"os.Getenv(TMP)": 1}, 0},
		{"TEMP and TMP looked up", `package p
import "os"
func f() { _, _ = os.LookupEnv("TEMP"); _, _ = os.LookupEnv("TMP") }`, map[string]int{"os.Getenv(TMP)": 2}, 0},
		{"an import under another name", `package p
import stdos "os"
func f() { _, _ = stdos.MkdirTemp("", "x-"); _ = stdos.TempDir() }`, map[string]int{"os.MkdirTemp(empty)": 1, "os.TempDir": 1}, 0},
		{"three uses in one file are three", `package p
import "os"
func f() { _, _ = os.MkdirTemp("", "a"); _, _ = os.MkdirTemp("", "b"); _, _ = os.MkdirTemp("", "c") }`, map[string]int{"os.MkdirTemp(empty)": 3}, 0},
		{"a use inside init counts as run by every started copy", `package p
import "os"
func init() { _, _ = os.MkdirTemp("", "x-") }`, map[string]int{"os.MkdirTemp(empty)": 1}, 1},
		{"a use inside TestMain counts as run by every started copy", `package p
import ("os"; "testing")
func TestMain(m *testing.M) { os.Exit(run(m, os.MkdirTemp)) }
func run(m *testing.M, mk func(string, string) (string, error)) int { return 0 }`, map[string]int{"value:os.MkdirTemp": 1}, 1},
		{"a package-level variable counts as run by every started copy", `package p
import "os"
var dir, _ = os.MkdirTemp("", "x-")`, map[string]int{"os.MkdirTemp(empty)": 1}, 1},
		{"a use inside a test function is not run by a copy", `package p
import ("os"; "testing")
func TestX(t *testing.T) { _, _ = os.MkdirTemp("", "x-") }`, map[string]int{"os.MkdirTemp(empty)": 1}, 0},
		// The controls: none of these is a raw use of the OS temp dir.
		{"t.TempDir is clean", `package p
import "testing"
func TestX(t *testing.T) { _ = t.TempDir() }`, map[string]int{}, 0},
		{"a named directory is clean", `package p
import "os"
func f(dir string) { _, _ = os.MkdirTemp(dir, "x-"); _, _ = os.CreateTemp(dir, "y-") }`, map[string]int{}, 0},
		{"a directory from t.TempDir is clean", `package p
import ("os"; "testing")
func TestX(t *testing.T) { _, _ = os.MkdirTemp(t.TempDir(), "x-") }`, map[string]int{}, 0},
		{"other environment variables are clean", `package p
import "os"
func f() string { return os.Getenv("HOME") + os.Getenv("PATH") }`, map[string]int{}, 0},
		{"a TempDir method on something else is clean", `package p
type fake struct{}
func (fake) TempDir() string { return "" }
func f() string { var os fake; return os.TempDir() }`, map[string]int{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tf, err := analyzeTestSource(token.NewFileSet(), "x_test.go", tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(tf.forms) != fmt.Sprint(tc.want) {
				t.Errorf("forms = %v, want %v", tf.forms, tc.want)
			}
			if tf.hookForms != tc.hooks {
				t.Errorf("hookForms = %d, want %d", tf.hookForms, tc.hooks)
			}
		})
	}
}

// TestTempLintSeesAProofOnlyWhenItReallyRelaunches pins what counts as using the test binary's own path, as starting a
// copy of it, and as the proof that a started copy leaves nothing.
func TestTempLintSeesAProofOnlyWhenItReallyRelaunches(t *testing.T) {
	const real = `package p
import ("os"; "os/exec"; "testing")
func TestARelaunchedTestBinaryLeavesNothingInTheTempDir(t *testing.T) {
	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelper$")
	cmd.Env = append(os.Environ(), "TMP="+tmp, "TEMP="+tmp, "TMPDIR="+tmp)
	_, _ = cmd.CombinedOutput()
	if e, _ := os.ReadDir(tmp); len(e) != 0 { t.Fatal(e) }
}`
	viaExecutable := replaceOnce(replaceOnce(real, "os.Args[0]", "self"), "tmp := t.TempDir()", "tmp := t.TempDir(); self, _ := os.Executable()")
	cases := []struct {
		name                      string
		src                       string
		relaunches, starts, proof bool
	}{
		{"a real proof", real, true, true, true},
		{"the same through a name assigned from os.Executable", viaExecutable, true, true, true},
		{"the same through exec.CommandContext", replaceOnce(replaceOnce(real, "exec.Command(os.Args[0],", "exec.CommandContext(context.Background(), os.Args[0],"), `"os/exec"`, `"os/exec"; "context"`), true, true, true},
		{"an empty stub with the right name", `package p
import "testing"
func TestARelaunchedTestBinaryLeavesNothingInTheTempDir(t *testing.T) {}`, false, false, false},
		{"a proof that starts a different program", replaceOnce(real, "exec.Command(os.Args[0], ", "exec.Command(os.Args[1], "), false, false, false},
		{"a proof that never points the temp dir", replaceOnce(real, `"TMP="+tmp, "TEMP="+tmp, "TMPDIR="+tmp`, `"X=1"`), true, true, false},
		{"a proof that never reads the folder back", replaceOnce(real, `os.ReadDir(tmp)`, `os.Stat(tmp)`), true, true, false},
		{"a proof that points only TMP", replaceOnce(real, `, "TEMP="+tmp, "TMPDIR="+tmp`, ``), true, true, false},
		{"a proof that leaves out TMPDIR", replaceOnce(real, `, "TMPDIR="+tmp`, ``), true, true, false},
		{"a proof that leaves out TEMP", replaceOnce(real, `"TEMP="+tmp, `, ``), true, true, false},
		{"a proof that leaves out TMP", replaceOnce(real, `"TMP="+tmp, `, ``), true, true, false},
		{"another test name does not count", replaceOnce(real, "TestARelaunchedTestBinaryLeavesNothingInTheTempDir", "TestSomethingElse"), true, true, false},
		{"os.Args[1] is not the test binary", `package p
import "os"
func f() string { return os.Args[1] }`, false, false, false},
		{"the path of the binary passed to another function is used, not started", `package p
import "os"
func f() { g(os.Args[0]) }
func g(string) {}`, true, false, false},
		{"os.Executable read for a hash is used, not started", `package p
import "os"
func f() { exe, _ := os.Executable(); _, _ = os.Open(exe) }`, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tf, err := analyzeTestSource(token.NewFileSet(), "x_test.go", tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if tf.relaunches != tc.relaunches || tf.starts != tc.starts || tf.proof != tc.proof {
				t.Errorf("relaunches=%v starts=%v proof=%v, want relaunches=%v starts=%v proof=%v", tf.relaunches, tf.starts, tf.proof, tc.relaunches, tc.starts, tc.proof)
			}
		})
	}
}

func replaceOnce(s, old, new string) string {
	if !strings.Contains(s, old) {
		panic("test premise: " + old + " is not in the source")
	}
	return strings.Replace(s, old, new, 1)
}

// TestASyntheticLeakInAnyPackageIsNamedByFileAndForm builds a small module tree, scans it the way the real lint
// scans the repository and requires: a leak in any directory is found and named by file and form; the directories
// the lint leaves out (testdata, node_modules and a nested module, which is how the repository's tools/ modules go) are left
// out while a directory that only has the name of one (a/tools, a/bin, bin) is scanned like any other; and a listed use is quiet.
func TestASyntheticLeakInAnyPackageIsNamedByFileAndForm(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	leak := "package p\nimport \"os\"\nfunc f() { _, _ = os.MkdirTemp(\"\", \"leak-\") }\n"
	write("go.mod", "module m\n")
	write("a/deep/er/leaky_test.go", leak)
	write("root_test.go", "package p\nimport \"os\"\nfunc f() string { return os.TempDir() }\n")
	write("clean/clean_test.go", "package p\nimport \"testing\"\nfunc TestX(t *testing.T) { _ = t.TempDir() }\n")
	write("listed/listed_test.go", leak)
	write("tools/go.mod", "module tools\n")
	write("tools/leak_test.go", leak)
	write("a/tools/leak_test.go", leak)
	write("a/bin/leak_test.go", leak)
	write("bin/leak_test.go", leak)
	write("a/testdata/leak_test.go", leak)
	write("node_modules/x/leak_test.go", leak)
	write("nested/go.mod", "module nested\n")
	write("nested/leak_test.go", leak)
	write("a/notatest.go", leak)

	files, err := scanTestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	got := tempUseSites(files)
	want := map[string]int{
		"a/deep/er/leaky_test.go:os.MkdirTemp(empty)": 1,
		"a/tools/leak_test.go:os.MkdirTemp(empty)":    1,
		"a/bin/leak_test.go:os.MkdirTemp(empty)":      1,
		"bin/leak_test.go:os.MkdirTemp(empty)":        1,
		"root_test.go:os.TempDir":                     1,
		"listed/listed_test.go:os.MkdirTemp(empty)":   1,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sites = %v, want %v (testdata, node_modules, a nested module and non-test files are left out; a directory named tools or bin is not)", got, want)
	}
	problems := tempUseProblems(got, map[string]tempAllow{
		"listed/listed_test.go:os.MkdirTemp(empty)": {1, "removed by a deferred RemoveAll"},
	})
	if len(problems) != 5 {
		t.Fatalf("problems = %q, want exactly the five unlisted uses", problems)
	}
	for _, want := range []string{"a/deep/er/leaky_test.go uses the OS temp dir 1 time(s) as os.MkdirTemp(empty)", "root_test.go uses the OS temp dir 1 time(s) as os.TempDir", "a/tools/leak_test.go uses the OS temp dir 1 time(s) as os.MkdirTemp(empty)", "a/bin/leak_test.go uses the OS temp dir 1 time(s) as os.MkdirTemp(empty)", "bin/leak_test.go uses the OS temp dir 1 time(s) as os.MkdirTemp(empty)"} {
		found := false
		for _, p := range problems {
			found = found || strings.HasPrefix(p, want)
		}
		if !found {
			t.Errorf("no problem starts with %q: %q", want, problems)
		}
	}
}

// TestTempUseAllowlistStaysTheTruthInBothDirections pins the comparison rules: more uses than listed, fewer, a stale
// entry and an entry without a reason each fail; an exact entry with a reason is quiet.
func TestTempUseAllowlistStaysTheTruthInBothDirections(t *testing.T) {
	const k = "x_test.go:os.TempDir"
	cases := []struct {
		name  string
		got   map[string]int
		allow map[string]tempAllow
		want  string // substring of the one expected problem; "" = none
	}{
		{"exact", map[string]int{k: 2}, map[string]tempAllow{k: {2, "why"}}, ""},
		{"not listed", map[string]int{k: 1}, nil, "is not in tempUseAllowlist"},
		{"more than listed", map[string]int{k: 3}, map[string]tempAllow{k: {2, "why"}}, "the allowlist says 2"},
		{"fewer than listed", map[string]int{k: 1}, map[string]tempAllow{k: {2, "why"}}, "lower the count"},
		{"stale entry", map[string]int{}, map[string]tempAllow{k: {1, "why"}}, "remove the entry"},
		{"no reason", map[string]int{k: 1}, map[string]tempAllow{k: {1, "  "}}, "without a reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tempUseProblems(tc.got, tc.allow)
			switch {
			case tc.want == "" && len(got) != 0:
				t.Errorf("want no problem, got %q", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("want one problem containing %q, got %q", tc.want, got)
			}
		})
	}
}

// TestRelaunchRuleJudgesEveryCase pins each verdict of relaunchProblems on synthetic packages.
func TestRelaunchRuleJudgesEveryCase(t *testing.T) {
	cases := []struct {
		name         string
		pkgs         map[string]pkgFacts
		exempt       map[string]string
		neverStarted map[string]string
		want         string // substring of the one expected problem; "" = none
	}{
		{"a package that does not use its test binary needs nothing", map[string]pkgFacts{"a": {}}, nil, nil, ""},
		{"a relaunching package with the proof is quiet", map[string]pkgFacts{"a": {relaunches: true, starts: true, proof: true}}, nil, nil, ""},
		{"a relaunching package with neither proof nor exemption fails", map[string]pkgFacts{"a": {relaunches: true, starts: true}}, nil, nil, "holds no TestARelaunchedTestBinaryLeavesNothingInTheTempDir"},
		{"an exempt package with a reason is quiet", map[string]pkgFacts{"a": {relaunches: true, starts: true}}, map[string]string{"a": "leaf helper"}, nil, ""},
		{"an exemption without a reason fails", map[string]pkgFacts{"a": {relaunches: true, starts: true}}, map[string]string{"a": " "}, nil, "without a reason"},
		{"an exemption whose package makes temp in TestMain or init fails", map[string]pkgFacts{"a": {relaunches: true, starts: true, hookForms: 1}}, map[string]string{"a": "leaf helper"}, nil, "every started copy runs them"},
		{"an exemption beside a proof fails", map[string]pkgFacts{"a": {relaunches: true, starts: true, proof: true}}, map[string]string{"a": "leaf helper"}, nil, "remove the exemption"},
		{"an exemption for a package that stopped using its binary fails", map[string]pkgFacts{"a": {}}, map[string]string{"a": "leaf helper"}, nil, "no longer refer to their own test binary"},
		{"an exemption for a package with no tests fails", map[string]pkgFacts{}, map[string]string{"gone": "leaf helper"}, nil, "has no test files"},
		{"a never-started package that only reads its path is quiet", map[string]pkgFacts{"a": {relaunches: true, hookForms: 1}}, nil, map[string]string{"a": "hashes the binary"}, ""},
		{"a never-started entry without a reason fails", map[string]pkgFacts{"a": {relaunches: true}}, nil, map[string]string{"a": ""}, "without a reason"},
		{"a never-started package that starts a copy fails", map[string]pkgFacts{"a": {relaunches: true, starts: true}}, nil, map[string]string{"a": "hashes the binary"}, "it starts a copy"},
		{"a never-started entry for a package that stopped using its binary fails", map[string]pkgFacts{"a": {}}, nil, map[string]string{"a": "hashes the binary"}, "no longer refer to their own test binary"},
		{"a package in both lists fails", map[string]pkgFacts{"a": {relaunches: true}}, map[string]string{"a": "x"}, map[string]string{"a": "y"}, "in both relaunchExempt and relaunchNeverStarted"},
		{"a never-started entry beside a proof fails", map[string]pkgFacts{"a": {relaunches: true, proof: true}}, nil, map[string]string{"a": "hashes the binary"}, "the test is the proof"},
		{"a never-started entry for a package with no tests fails", map[string]pkgFacts{}, nil, map[string]string{"gone": "x"}, "has no test files"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := relaunchProblems(tc.pkgs, tc.exempt, tc.neverStarted)
			switch {
			case tc.want == "" && len(got) != 0:
				t.Errorf("want no problem, got %q", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("want one problem containing %q, got %q", tc.want, got)
			}
		})
	}
}

// TestTestsReachForTheOSTempDirOnlyWhereItsRemovalIsOnEveryPath is the lint over the repository's own test files.
func TestTestsReachForTheOSTempDirOnlyWhereItsRemovalIsOnEveryPath(t *testing.T) {
	files, err := scanTestTree(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 100 {
		t.Fatalf("the walk found only %d test files: the lint is not looking at the repository", len(files))
	}
	got := tempUseSites(files)
	if printMode() {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("\t%q: {%d, \"\"},\n", k, got[k])
		}
		t.Skip("printed the current uses")
	}
	for _, p := range tempUseProblems(got, tempUseAllowlist) {
		t.Error(p)
	}
}

// TestEveryRelaunchingPackageProvesItLeavesNothing is the relaunch rule over the repository's own packages.
func TestEveryRelaunchingPackageProvesItLeavesNothing(t *testing.T) {
	files, err := scanTestTree(".")
	if err != nil {
		t.Fatal(err)
	}
	pkgs := packageFacts(files)
	if printMode() {
		var dirs []string
		for dir, p := range pkgs {
			_, exempt := relaunchExempt[dir]
			_, never := relaunchNeverStarted[dir]
			if p.relaunches && !p.proof && !exempt && !never {
				dirs = append(dirs, dir)
			}
		}
		sort.Strings(dirs)
		for _, dir := range dirs {
			fmt.Printf("\t%q: \"\",\n", dir)
		}
		t.Skip("printed the relaunching packages that hold no proof and no exemption")
	}
	for _, p := range relaunchProblems(pkgs, relaunchExempt, relaunchNeverStarted) {
		t.Error(p)
	}
}
