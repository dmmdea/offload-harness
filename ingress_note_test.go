package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The ingress guard defaults to warn, which serves every request it would refuse. A stock
// install is therefore unguarded, and the changelog headline ("an ingress guard") reads as if
// it were not. The mode in force is printed where the operator looks: at fleet-serve and
// fleet-ui start, and in doctor.
func TestIngressGuardNoteSaysWhichModeRuns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  []string // every fragment must appear
		not   []string // none may
	}{
		{"unset is warn and says it serves", "", []string{"ingress guard: warn", "default", "fleet_ingress_guard", "SERVED", "enforce"}, []string{"OFF"}},
		{"explicit warn is not called the default", "warn", []string{"ingress guard: warn", "fleet_ingress_guard=warn", "SERVED"}, []string{"default"}},
		{"enforce says what it refuses", "enforce", []string{"ingress guard: enforce", "421", "403", "415"}, []string{"SERVED"}},
		{"off says nothing is checked", "off", []string{"ingress guard: OFF", "fleet_ingress_guard=off", "no Host, Origin or Content-Type check"}, []string{"SERVED"}},
		{"case and spaces are the config's", "  ENFORCE ", []string{"ingress guard: enforce"}, []string{"not a mode"}},
		{"a mistyped mode is enforce, and says so", "enfroce", []string{"ingress guard: enforce", `"enfroce"`, "not a mode"}, []string{"SERVED", "OFF"}},
	} {
		got := ingressGuardNote(config.Config{FleetIngressGuard: tc.value})
		for _, frag := range tc.want {
			if !strings.Contains(got, frag) {
				t.Errorf("%s: note %q lacks %q", tc.name, got, frag)
			}
		}
		for _, frag := range tc.not {
			if strings.Contains(got, frag) {
				t.Errorf("%s: note %q must not contain %q", tc.name, got, frag)
			}
		}
		if strings.Contains(got, "\n") {
			t.Errorf("%s: the note must be one line: %q", tc.name, got)
		}
	}
}

func TestDoctorPrintsTheIngressGuardMode(t *testing.T) {
	for _, value := range []string{"", "warn", "enforce", "off"} {
		cfg := config.Config{FleetIngressGuard: value}
		var b strings.Builder
		writeIngressGuardSection(&b, cfg)
		want := ingressGuardNote(cfg) + "\n"
		if want == "\n" || b.String() != want {
			t.Errorf("fleet_ingress_guard=%q: doctor printed %q, want the note %q", value, b.String(), want)
		}
	}
}

// calledIn returns the names of the plain function calls inside one top-level function of a
// source file of this package.
func calledIn(t *testing.T, file, fn string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		calls := map[string]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok {
					calls[id.Name] = true
				}
			}
			return true
		})
		return calls
	}
	t.Fatalf("%s has no function %s: this pin has nothing to check", file, fn)
	return nil
}

// fleet-serve, fleet-ui and doctor are never run by a test, so the seam above proves nothing
// unless the verbs go through it. Pinned at the source, as the poll-lease note is.
func TestIngressGuardNoteIsPrintedWhereTheOperatorLooks(t *testing.T) {
	for _, w := range []struct{ file, fn, call string }{
		{"main.go", "runFleetServe", "ingressGuardNote"},
		{"fleet_ui_cmd.go", "runFleetUI", "ingressGuardNote"},
		{"main.go", "doctorRunChecked", "writeIngressGuardSection"},
	} {
		if !calledIn(t, w.file, w.fn)[w.call] {
			t.Errorf("%s: %s never calls %s: the mode in force (warn serves what it would refuse) would carry no sign at start", w.file, w.fn, w.call)
		}
	}
}
