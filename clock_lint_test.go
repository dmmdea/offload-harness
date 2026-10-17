package main

// One lint for the whole repository that keeps a test from failing because the machine it ran on was slow.
//
// WHY. A test that measures a stretch of time and fails when the stretch is long is a rule about the runner, not about the
// product. On a loaded or starved host (the gate machine runs the suite next to everything else; a CI runner shares its
// cores) a wall-clock span is stretched by whatever else wants the CPU, by the garbage collector, and by a process that is
// not scheduled for a second at a time. Each of the starvation fixes of the 0.18x series (the call-reserve start order, the
// queued-credit and cut-answer tests, the call-deadline windows, the lease-renewal helper, the busy-hold ceiling) was found
// after a green suite went red once, was reproduced only under a stress shape, and was fixed for that one test; the next
// test to put a number on a clock was not covered. This file is the one lint, over every *_test.go file of the module, with
// the sites that were read by hand written down, in the style of tempdir_lint_test.go.
//
// WHAT IT CHECKS (go/parser, no regular expressions on text): every UPPER bound on a measured elapsed time against a
// constant under clockBoundFloor (one second) is listed in clockBoundAllowlist (clock_lint_allowlist_test.go) with its
// count and its reason: what the bound rules out (the longer wait, or the ceiling, that the code under test would otherwise
// run into) and how wide the margin to it is. A thin margin is written down as thin, so that the next flake lands on a
// known entry and not on a new investigation. The measured time is time.Since(x); a.Sub(b) where a or b is a
// time.Now() stamp (or a variable that only ever holds one); a variable that only ever holds one of those; and that value
// read as a number (.Seconds(), .Milliseconds(), .Microseconds(), .Minutes(), .Nanoseconds()). The upper bound is `elapsed >
// C` or `elapsed >= C`, or the same written `C < elapsed` or `C <= elapsed`. C is a constant the lint can evaluate: a
// literal, a time unit product such as 250*time.Millisecond, a package-level const of the same package, or a const or a
// local that is assigned once in the same function. A bound of zero or less is not a bound. A bound at or above one second
// is left to the author's judgment: it is where a bound starts to be a hang guard (the test fails only when something is
// stuck), and where the list of sites stops being a review one person can read. The floor is a ratchet and not a proof of
// safety: a 2.5 s ceiling failed under starvation too, so raising clockBoundFloor (and listing the sites it then finds) is
// the next step, not a change of rule. The key is "file:elapsed>C" (for example "gpu_warm_watch_test.go:elapsed>500ms"), so
// moving a bound re-opens its review, and widening one to a second or more removes the site: a use that went away fails
// until its entry is removed, so the list stays the truth.
//
// WHAT IT CANNOT SEE. A bound against a value the lint cannot evaluate (a struct field, a parameter, a package variable, a
// constant from another package, a bound relative to a poll interval) is not seen. A bound written as `elapsed < C` that the
// test requires is not seen: `elapsed < C` is overwhelmingly a LOWER bound ("it waited at least C"), which a slow runner
// only makes truer. The lint does not tell a ceiling from the lower edge of a sampling window (`el > lo && el < hi`, a
// classification of samples); such a site is listed with that reason. A fixed time.Sleep before an assertion, a short call
// or context window handed to the code under test, and a helper that takes a sub-second timeout are not seen either; those
// are the other shapes of the same fault and are fixed, as before, by waiting on the event the product signals (a channel,
// a barrier, a held fake) instead of on a clock. `case <-time.After(C): t.Fatal(...)` ceilings were counted when this lint
// was written: none is under one second.
//
// REGENERATING THE LIST. CLOCK_LINT_PRINT=1 go test . -run TestTestsPutNoSubSecondCeilingOnMeasuredTime -v
// prints the entries the current tree needs, with an empty reason. The reason is the review: write it only after reading
// the code, and prefer an event the product signals to a listing.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const clockLintPrintEnv = "CLOCK_LINT_PRINT"

// clockBoundFloor is the line between a bound on measured time that the lint reviews and a hang guard it leaves alone.
const clockBoundFloor = float64(time.Second)

// clockAllow is one allowlisted site: how many bounds of that form the file holds, and why each holds on a starved runner.
type clockAllow struct {
	count int
	why   string
}

// clockUnits is the nanosecond value of each time.Duration unit constant.
var clockUnits = map[string]float64{
	"Nanosecond":  float64(time.Nanosecond),
	"Microsecond": float64(time.Microsecond),
	"Millisecond": float64(time.Millisecond),
	"Second":      float64(time.Second),
	"Minute":      float64(time.Minute),
	"Hour":        float64(time.Hour),
}

// clockNumberScale is how many nanoseconds one unit of the number a Duration method returns is worth.
var clockNumberScale = map[string]float64{
	"Nanoseconds":  float64(time.Nanosecond),
	"Microseconds": float64(time.Microsecond),
	"Milliseconds": float64(time.Millisecond),
	"Seconds":      float64(time.Second),
	"Minutes":      float64(time.Minute),
}

// clockDef is one definition of a name inside a function: the expression it is given, or nil when the lint cannot tell
// (a parameter, a range variable, a multi-value call, an op-assignment, an increment); zero marks `var x T` with no value.
type clockDef struct {
	expr ast.Expr
	zero bool
}

// clockEnv is what the lint knows about one file while it reads one function.
type clockEnv struct {
	timeName string             // the local name of the "time" import
	consts   map[string]float64 // package-level consts plus the function's once-assigned constants, in nanoseconds or plain numbers
	stamps   map[string]bool    // names that only ever hold a time.Now() stamp
	elapsed  map[string]float64 // names that only ever hold an elapsed time (or a number read from one), with their scale
}

// clockConst evaluates a constant expression: a number, a time unit, a name known to hold a constant, and the arithmetic
// and conversions between them. A bare number is its own value, a time unit is its nanoseconds, so 250*time.Millisecond is
// 250e6 and a plain 0.5 stays 0.5 (the caller scales a number read from .Seconds()).
func clockConst(e ast.Expr, env *clockEnv) (float64, bool) {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return clockConst(x.X, env)
	case *ast.BasicLit:
		if x.Kind != token.INT && x.Kind != token.FLOAT {
			return 0, false
		}
		text := strings.ReplaceAll(x.Value, "_", "")
		if n, err := strconv.ParseInt(text, 0, 64); err == nil {
			return float64(n), true
		}
		v, err := strconv.ParseFloat(text, 64)
		return v, err == nil
	case *ast.Ident:
		v, ok := env.consts[x.Name]
		return v, ok
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && id.Name == env.timeName {
			v, found := clockUnits[x.Sel.Name]
			return v, found
		}
	case *ast.UnaryExpr:
		v, ok := clockConst(x.X, env)
		switch {
		case !ok:
		case x.Op == token.SUB:
			return -v, true
		case x.Op == token.ADD:
			return v, true
		}
	case *ast.CallExpr:
		// A conversion: time.Duration(x), float64(x), int(x), int64(x).
		if len(x.Args) != 1 {
			return 0, false
		}
		switch fn := x.Fun.(type) {
		case *ast.SelectorExpr:
			if id, ok := fn.X.(*ast.Ident); ok && id.Name == env.timeName && fn.Sel.Name == "Duration" {
				return clockConst(x.Args[0], env)
			}
		case *ast.Ident:
			switch fn.Name {
			case "float64", "int", "int64":
				return clockConst(x.Args[0], env)
			}
		}
	case *ast.BinaryExpr:
		a, okA := clockConst(x.X, env)
		b, okB := clockConst(x.Y, env)
		if !okA || !okB {
			return 0, false
		}
		switch x.Op {
		case token.MUL:
			return a * b, true
		case token.ADD:
			return a + b, true
		case token.SUB:
			return a - b, true
		case token.QUO:
			if b != 0 {
				return a / b, true
			}
		}
	}
	return 0, false
}

// clockIsNow reports whether e is a time.Now() stamp: the call itself, a name that only ever holds one, or a stamp passed
// through UTC, Local, Round, Truncate or In (the same instant).
func clockIsNow(e ast.Expr, env *clockEnv) bool {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return clockIsNow(x.X, env)
	case *ast.Ident:
		return env.stamps[x.Name]
	case *ast.CallExpr:
		pkg, fn, ok := pkgSel(x.Fun)
		if ok && pkg == env.timeName && fn == "Now" && len(x.Args) == 0 {
			return true
		}
		if sel, isSel := x.Fun.(*ast.SelectorExpr); isSel {
			switch sel.Sel.Name {
			case "UTC", "Local", "Round", "Truncate", "In":
				return clockIsNow(sel.X, env)
			}
		}
	}
	return false
}

// clockElapsed reports whether e is a measured elapsed time, and the scale of the value in nanoseconds per unit (1 for a
// time.Duration, 1e6 for the number .Milliseconds() returns).
func clockElapsed(e ast.Expr, env *clockEnv) (float64, bool) {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return clockElapsed(x.X, env)
	case *ast.Ident:
		s, ok := env.elapsed[x.Name]
		return s, ok
	case *ast.CallExpr:
		if pkg, fn, ok := pkgSel(x.Fun); ok && pkg == env.timeName && fn == "Since" && len(x.Args) == 1 {
			return 1, true
		}
		sel, isSel := x.Fun.(*ast.SelectorExpr)
		if !isSel {
			return 0, false
		}
		if sel.Sel.Name == "Sub" && len(x.Args) == 1 && (clockIsNow(sel.X, env) || clockIsNow(x.Args[0], env)) {
			return 1, true
		}
		if scale, ok := clockNumberScale[sel.Sel.Name]; ok && len(x.Args) == 0 {
			if inner, isElapsed := clockElapsed(sel.X, env); isElapsed && inner == 1 {
				return scale, true
			}
		}
	}
	return 0, false
}

// clockDefs gathers every definition of every name inside a function (its parameters included), so that a name is trusted
// only when each of its definitions says the same thing.
func clockDefs(fn *ast.FuncDecl) map[string][]clockDef {
	defs := map[string][]clockDef{}
	add := func(id *ast.Ident, d clockDef) {
		if id != nil && id.Name != "_" {
			defs[id.Name] = append(defs[id.Name], d)
		}
	}
	fields := func(list *ast.FieldList) {
		if list == nil {
			return
		}
		for _, f := range list.List {
			for _, n := range f.Names {
				add(n, clockDef{})
			}
		}
	}
	fields(fn.Recv)
	fields(fn.Type.Params)
	fields(fn.Type.Results)
	if fn.Body == nil {
		return defs
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			fields(x.Type.Params)
			fields(x.Type.Results)
		case *ast.AssignStmt:
			plain := x.Tok == token.DEFINE || x.Tok == token.ASSIGN
			for i, lhs := range x.Lhs {
				id, isID := lhs.(*ast.Ident)
				if !isID {
					continue
				}
				if plain && len(x.Lhs) == len(x.Rhs) {
					add(id, clockDef{expr: x.Rhs[i]})
				} else {
					add(id, clockDef{})
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				switch {
				case i < len(x.Values) && len(x.Names) == len(x.Values):
					add(id, clockDef{expr: x.Values[i]})
				case len(x.Values) == 0:
					add(id, clockDef{zero: true})
				default:
					add(id, clockDef{})
				}
			}
		case *ast.RangeStmt:
			if id, ok := x.Key.(*ast.Ident); ok {
				add(id, clockDef{})
			}
			if id, ok := x.Value.(*ast.Ident); ok {
				add(id, clockDef{})
			}
		case *ast.IncDecStmt:
			if id, ok := x.X.(*ast.Ident); ok {
				add(id, clockDef{})
			}
		}
		return true
	})
	return defs
}

// clockBuildEnv works out, for one function, which names hold a constant, a time.Now() stamp or a measured elapsed time.
func clockBuildEnv(fn *ast.FuncDecl, timeName string, pkgConsts map[string]float64) *clockEnv {
	env := &clockEnv{timeName: timeName, consts: map[string]float64{}, stamps: map[string]bool{}, elapsed: map[string]float64{}}
	for k, v := range pkgConsts {
		env.consts[k] = v
	}
	defs := clockDefs(fn)
	for name := range defs { // a name defined in this function shadows a package const of the same name
		delete(env.consts, name)
	}
	// Three passes settle chains such as `a := 2*time.Second; b := a/4` and `s := time.Now(); d := time.Since(s); ms := d.Milliseconds()`.
	for pass := 0; pass < 3; pass++ {
		for name, list := range defs {
			if len(list) == 1 && list[0].expr != nil {
				if v, ok := clockConst(list[0].expr, env); ok {
					env.consts[name] = v
				}
			}
			stamp, consistent := true, true
			scaled, seen := 0.0, 0
			for _, d := range list {
				if d.zero {
					continue
				}
				seen++
				if d.expr == nil || !clockIsNow(d.expr, env) {
					stamp = false
				}
				s, ok := 0.0, false
				if d.expr != nil {
					s, ok = clockElapsed(d.expr, env)
				}
				if !ok || (scaled != 0 && s != scaled) {
					consistent = false
				} else {
					scaled = s
				}
			}
			if seen > 0 && stamp {
				env.stamps[name] = true
			}
			if seen > 0 && consistent && scaled != 0 {
				env.elapsed[name] = scaled
			}
		}
	}
	return env
}

// clockBound returns the constant, in nanoseconds, that e bounds a measured elapsed time from above, when it does.
func clockBound(x *ast.BinaryExpr, env *clockEnv) (float64, bool) {
	var elapsedSide, constSide ast.Expr
	switch x.Op {
	case token.GTR, token.GEQ:
		elapsedSide, constSide = x.X, x.Y
	case token.LSS, token.LEQ:
		elapsedSide, constSide = x.Y, x.X
	default:
		return 0, false
	}
	scale, ok := clockElapsed(elapsedSide, env)
	if !ok {
		return 0, false
	}
	c, ok := clockConst(constSide, env)
	if !ok {
		return 0, false
	}
	return c * scale, true
}

// clockForm names a bound for the allowlist: "elapsed>" and the constant as a Go duration.
func clockForm(ns float64) string {
	return "elapsed>" + time.Duration(ns).String()
}

// clockPackageConsts evaluates the const declarations of one package (all its test files together), settling chains that
// reach across files.
func clockPackageConsts(files []*ast.File, timeNames map[*ast.File]string) map[string]float64 {
	consts := map[string]float64{}
	for pass := 0; pass < 4; pass++ {
		for _, f := range files {
			env := &clockEnv{timeName: timeNames[f], consts: consts}
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, id := range vs.Names {
						if i < len(vs.Values) && len(vs.Names) == len(vs.Values) {
							if v, ok := clockConst(vs.Values[i], env); ok {
								consts[id.Name] = v
							}
						}
					}
				}
			}
		}
	}
	return consts
}

// clockSites reads the parsed test files (keyed by slash-separated path; the directory of the path is the package's) and
// returns every upper bound on measured time under the floor as "file:form" -> count.
func clockSites(files map[string]*ast.File) map[string]int {
	byPkg := map[string][]string{} // directory + package name -> file paths
	for name, f := range files {
		key := path.Dir(name) + "\x00" + f.Name.Name
		byPkg[key] = append(byPkg[key], name)
	}
	sites := map[string]int{}
	for _, names := range byPkg {
		sort.Strings(names)
		parsed := make([]*ast.File, len(names))
		timeNames := map[*ast.File]string{}
		for i, name := range names {
			parsed[i] = files[name]
			timeNames[parsed[i]] = importName(parsed[i], "time", "time")
		}
		pkgConsts := clockPackageConsts(parsed, timeNames)
		for _, name := range names {
			f := files[name]
			timeName := timeNames[f]
			if timeName == "" {
				continue
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				env := clockBuildEnv(fn, timeName, pkgConsts)
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if be, isBin := n.(*ast.BinaryExpr); isBin {
						if ns, bounded := clockBound(be, env); bounded && ns > 0 && ns < clockBoundFloor {
							sites[name+":"+clockForm(ns)]++
						}
					}
					return true
				})
			}
		}
	}
	return sites
}

// clockParse parses synthetic sources (name -> source) for the pinning tests.
func clockParse(srcs map[string]string) (map[string]*ast.File, error) {
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for name, src := range srcs {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		out[name] = f
	}
	return out, nil
}

// scanClockTree parses every *_test.go file below root, keyed by slash-separated relative path.
func scanClockTree(root string) (map[string]*ast.File, error) {
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			if skipTestDir(p, d.Name()) && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		out[filepath.ToSlash(rel)] = f
		return nil
	})
	return out, err
}

// clockBoundProblems compares the bounds found with the allowlist and says what to do about each difference.
func clockBoundProblems(got map[string]int, allow map[string]clockAllow) []string {
	var out []string
	for key, n := range got {
		file, form, _ := strings.Cut(key, ":")
		bound := strings.TrimPrefix(form, "elapsed>")
		want, listed := allow[key]
		switch {
		case !listed:
			out = append(out, fmt.Sprintf("%s fails when a measured time is above %s (%d time(s)) and is not in clockBoundAllowlist: a loaded runner stretches any wall-clock span, so a bound under one second is a flake, not a rule. Wait on the event the product signals (a channel, a barrier, a held fake) instead, or bound the wait with a hang guard of a second or more; or list %q with the reason (what the bound rules out, and the margin to it)", file, bound, n, key))
		case n > want.count:
			out = append(out, fmt.Sprintf("%s fails when a measured time is above %s %d time(s), the allowlist says %d: wait on an event for the new one, or review it and raise the count of %q with its reason", file, bound, n, want.count, key))
		case n < want.count:
			out = append(out, fmt.Sprintf("%s fails when a measured time is above %s %d time(s), the allowlist says %d: lower the count of %q so the list stays the truth", file, bound, n, want.count, key))
		}
	}
	for key, a := range allow {
		if _, ok := got[key]; !ok {
			out = append(out, fmt.Sprintf("%q is in clockBoundAllowlist but no such bound exists any more: remove the entry", key))
		}
		if strings.TrimSpace(a.why) == "" {
			out = append(out, fmt.Sprintf("%q is in clockBoundAllowlist without a reason: the reason (what the bound rules out, and the margin to it) is the review", key))
		}
	}
	sort.Strings(out)
	return out
}

func clockPrintMode() bool { return os.Getenv(clockLintPrintEnv) != "" }

// ---------------------------------------------------------------------------------------------------- tests

// TestClockLintSeesEveryFormOfElapsedBound feeds the scanner one synthetic source per form (and the clean controls) and
// requires the exact sites, so a form dropped from the scanner turns this red whatever the tree holds.
func TestClockLintSeesEveryFormOfElapsedBound(t *testing.T) {
	cases := []struct {
		name string
		srcs map[string]string // name -> source; every name is in one directory unless it says otherwise
		want map[string]int
	}{
		{"time.Since above a constant", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return time.Since(start) > 500*time.Millisecond }`}, map[string]int{"p/a_test.go:elapsed>500ms": 1}},
		{"time.Since at or above a constant", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return time.Since(start) >= 250*time.Millisecond }`}, map[string]int{"p/a_test.go:elapsed>250ms": 1}},
		{"the constant on the left", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return 100*time.Millisecond < time.Since(start) }`}, map[string]int{"p/a_test.go:elapsed>100ms": 1}},
		{"the constant on the left, at most", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return 100*time.Millisecond <= time.Since(start) }`}, map[string]int{"p/a_test.go:elapsed>100ms": 1}},
		{"a variable that holds the elapsed time", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { took := time.Since(start); return took > 75*time.Millisecond }`}, map[string]int{"p/a_test.go:elapsed>75ms": 1}},
		{"the short declaration of an if", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) {
	if el := time.Since(start); el > 800*time.Millisecond {
		panic(el)
	}
}`}, map[string]int{"p/a_test.go:elapsed>800ms": 1}},
		{"a variable assigned in a var block", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { var d = time.Since(start); return d > 300*time.Millisecond }`}, map[string]int{"p/a_test.go:elapsed>300ms": 1}},
		{"a variable declared first and assigned later", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { var d time.Duration; d = time.Since(start); return d > 300*time.Millisecond }`}, map[string]int{"p/a_test.go:elapsed>300ms": 1}},
		{"time.Now().Sub", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return time.Now().Sub(start) > 200*time.Millisecond }`}, map[string]int{"p/a_test.go:elapsed>200ms": 1}},
		{"two stamps taken in the function", map[string]string{"p/a_test.go": `package p
import "time"
func f() bool { begin := time.Now(); end := time.Now(); return end.Sub(begin) > 150*time.Millisecond }`}, map[string]int{"p/a_test.go:elapsed>150ms": 1}},
		{"a stamp passed through UTC", map[string]string{"p/a_test.go": `package p
import "time"
func f(then time.Time) bool { return time.Now().UTC().Sub(then) > 150*time.Millisecond }`}, map[string]int{"p/a_test.go:elapsed>150ms": 1}},
		{"the elapsed time read as seconds", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return time.Since(start).Seconds() > 0.5 }`}, map[string]int{"p/a_test.go:elapsed>500ms": 1}},
		{"the elapsed time read as milliseconds through a variable", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { ms := time.Since(start).Milliseconds(); return ms >= 400 }`}, map[string]int{"p/a_test.go:elapsed>400ms": 1}},
		{"a package-level const", map[string]string{"p/a_test.go": `package p
import "time"
const window = 600 * time.Millisecond
func f(start time.Time) bool { return time.Since(start) > window }`}, map[string]int{"p/a_test.go:elapsed>600ms": 1}},
		{"a package-level const defined in another file of the package", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return time.Since(start) > window*2 }`, "p/b_test.go": `package p
import "time"
const base = 250 * time.Millisecond
const window = base`}, map[string]int{"p/a_test.go:elapsed>500ms": 1}},
		{"a const in the function", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { const limit = 450 * time.Millisecond; return time.Since(start) > limit }`}, map[string]int{"p/a_test.go:elapsed>450ms": 1}},
		{"a local assigned once", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { budget := 700 * time.Millisecond; return time.Since(start) > budget }`}, map[string]int{"p/a_test.go:elapsed>700ms": 1}},
		{"a local derived from another", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { wait := 2 * time.Second; limit := wait / 4; return time.Since(start) > limit }`}, map[string]int{"p/a_test.go:elapsed>500ms": 1}},
		{"an import under another name", map[string]string{"p/a_test.go": `package p
import clk "time"
func f(start clk.Time) bool { return clk.Since(start) > 500*clk.Millisecond }`}, map[string]int{"p/a_test.go:elapsed>500ms": 1}},
		{"the bound of a range check", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { d := time.Since(start); return d < 100*time.Millisecond || d > 3*time.Millisecond*100 }`}, map[string]int{"p/a_test.go:elapsed>300ms": 1}},
		{"three bounds of one form in one file are three", map[string]string{"p/a_test.go": `package p
import "time"
func f(a, b, c time.Time) bool { return time.Since(a) > time.Millisecond*500 || time.Since(b) > 500*time.Millisecond || time.Since(c) > 500*time.Millisecond }`}, map[string]int{"p/a_test.go:elapsed>500ms": 3}},
		{"the bound inside a closure", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) func() bool { return func() bool { return time.Since(start) > 500*time.Millisecond } }`}, map[string]int{"p/a_test.go:elapsed>500ms": 1}},
		{"one package and its external test package are read apart", map[string]string{"p/a_test.go": `package p
import "time"
const window = 600 * time.Millisecond
func f(start time.Time) bool { return time.Since(start) > window }`, "p/b_test.go": `package p_test
import "time"
const window = 900 * time.Millisecond
func g(start time.Time) bool { return time.Since(start) > window }`}, map[string]int{"p/a_test.go:elapsed>600ms": 1, "p/b_test.go:elapsed>900ms": 1}},
		// The controls: none of these is a sub-second upper bound on measured time.
		{"a lower bound is clean", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return time.Since(start) < 500*time.Millisecond }`}, map[string]int{}},
		{"a lower bound with the constant on the left is clean", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return 500*time.Millisecond > time.Since(start) }`}, map[string]int{}},
		{"a bound of one second is a hang guard", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return time.Since(start) > time.Second }`}, map[string]int{}},
		{"a bound above a second is a hang guard", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return time.Since(start) > 5*time.Second }`}, map[string]int{}},
		{"just under a second is flagged", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return time.Since(start) > 999*time.Millisecond }`}, map[string]int{"p/a_test.go:elapsed>999ms": 1}},
		{"a bound of zero is not a bound", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return time.Since(start) > 0 }`}, map[string]int{}},
		{"a negative bound is not a bound", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { return time.Since(start) > -time.Millisecond }`}, map[string]int{}},
		{"a bound the lint cannot evaluate is not seen", map[string]string{"p/a_test.go": `package p
import "time"
type c struct{ bound time.Duration }
func f(start time.Time, tc c, limit time.Duration) bool { return time.Since(start) > tc.bound || time.Since(start) > limit }`}, map[string]int{}},
		{"a difference of two values that are not stamps is not elapsed time", map[string]string{"p/a_test.go": `package p
import "time"
func f(rec, until time.Time) bool { d := rec.Sub(until); return d > time.Millisecond }`}, map[string]int{}},
		{"a parameter named like an elapsed variable is not one", map[string]string{"p/a_test.go": `package p
import "time"
func f(took time.Duration) bool { return took > 500*time.Millisecond }`}, map[string]int{}},
		{"a name that also holds something else is not trusted", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time, cfg time.Duration) bool { d := time.Since(start); _ = d; d = cfg; return d > 500*time.Millisecond }`}, map[string]int{}},
		{"a name added to is not trusted", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { d := time.Since(start); d += time.Second; return d > 500*time.Millisecond }`}, map[string]int{}},
		{"a constant that is reassigned is not trusted", map[string]string{"p/a_test.go": `package p
import "time"
func f(start time.Time) bool { limit := 500 * time.Millisecond; limit = time.Second * 9; return time.Since(start) > limit }`}, map[string]int{}},
		{"a local shadows a package const", map[string]string{"p/a_test.go": `package p
import "time"
const window = 600 * time.Millisecond
func f(start time.Time, window time.Duration) bool { return time.Since(start) > window }`}, map[string]int{}},
		{"a time method on something else is clean", map[string]string{"p/a_test.go": `package p
type fake struct{}
func (fake) Since(x int) int { return x }
func f() bool { var time fake; return time.Since(1) > 500 }`}, map[string]int{}},
		{"a file that does not import time is clean", map[string]string{"p/a_test.go": `package p
func f(x int) bool { return x > 500 }`}, map[string]int{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, err := clockParse(tc.srcs)
			if err != nil {
				t.Fatal(err)
			}
			got := clockSites(files)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("sites = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClockBoundAllowlistStaysTheTruthInBothDirections pins the comparison rules: more bounds than listed, fewer, a stale
// entry and an entry without a reason each fail; an exact entry with a reason is quiet.
func TestClockBoundAllowlistStaysTheTruthInBothDirections(t *testing.T) {
	const k = "x_test.go:elapsed>500ms"
	cases := []struct {
		name  string
		got   map[string]int
		allow map[string]clockAllow
		want  string // substring of the one expected problem; "" = none
	}{
		{"exact", map[string]int{k: 2}, map[string]clockAllow{k: {2, "why"}}, ""},
		{"not listed", map[string]int{k: 1}, nil, "is not in clockBoundAllowlist"},
		{"more than listed", map[string]int{k: 3}, map[string]clockAllow{k: {2, "why"}}, "the allowlist says 2"},
		{"fewer than listed", map[string]int{k: 1}, map[string]clockAllow{k: {2, "why"}}, "lower the count"},
		{"stale entry", map[string]int{}, map[string]clockAllow{k: {1, "why"}}, "remove the entry"},
		{"no reason", map[string]int{k: 1}, map[string]clockAllow{k: {1, "  "}}, "without a reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clockBoundProblems(tc.got, tc.allow)
			switch {
			case tc.want == "" && len(got) != 0:
				t.Errorf("want no problem, got %q", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("want one problem containing %q, got %q", tc.want, got)
			}
		})
	}
}

// TestTestsPutNoSubSecondCeilingOnMeasuredTime is the lint over the repository's own test files.
func TestTestsPutNoSubSecondCeilingOnMeasuredTime(t *testing.T) {
	files, err := scanClockTree(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 100 {
		t.Fatalf("the walk found only %d test files: the lint is not looking at the repository", len(files))
	}
	got := clockSites(files)
	if clockPrintMode() {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("\t%q: {%d, \"\"},\n", k, got[k])
		}
		t.Skip("printed the current bounds")
	}
	for _, p := range clockBoundProblems(got, clockBoundAllowlist) {
		t.Error(p)
	}
}
