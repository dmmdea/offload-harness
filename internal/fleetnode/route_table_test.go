package fleetnode

// The tables the credential tests run over are complete by construction: a route the node serves that is not a row of
// them fails here, so a new route cannot be added and left out of the rules. registerRoutes (server.go) registers every
// route the node serves and fleetqueue.Mount registers the queue's, both through routeRegistrar; the test hands them a
// registrar that records the patterns and compares those with refusalDoors (every door that refuses a credential,
// refusal_every_door_test.go) and publicRoutes (the two that answer without it, refusal_public_routes_test.go).
//
//   - a route that takes a body (POST, PUT, PATCH, or a pattern that names no method) is a row of refusalDoors with a method
//     that takes one, which is what puts it under the checks that a caller without the bearer costs the node no body byte
//     (auth_before_body_doors_test.go) and is refused with one journal line (refusal_every_door_test.go);
//   - any other route is a row of refusalDoors or an entry of publicRoutes, and a public route is read-only.
//
// A row that names no route is as wrong as a route with no row: the table would be testing a door that is gone.
//
// The comparison sees what registerRoutes registers when the node is built the one way the test builds it, so a route registered
// anywhere else, or under a condition that node does not meet, would escape it. The source test at the end closes that, over the
// node's package and the queue's:
//
//   - a route is registered by a call to HandleFunc or Handle only inside registerRoutes (and, for the queue's, inside Mount);
//   - the one mux Handler builds goes only to registerRoutes and the ingress guard, whatever a call that takes it might mount;
//   - registerRoutes hands its registrar only to fleetqueue.Mount, and Mount uses its router only to register on it;
//   - the registrations that stand in those two functions are as many as the node registered, so a route under a condition the
//     test node does not meet, or in a loop, fails by count.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// recordingRegistrar remembers each pattern registered on it and serves nothing.
type recordingRegistrar struct{ patterns []string }

func (r *recordingRegistrar) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, pattern)
}

// takesBody reports whether a request of this method carries a body the node would read: POST, PUT and PATCH, and "",
// the method of a pattern that names none and so answers all of them.
func takesBody(method string) bool {
	switch method {
	case "", http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	}
	return false
}

// patternMethod is the method a ServeMux pattern names, or "" when it names none.
func patternMethod(pattern string) string {
	if method, _, ok := strings.Cut(pattern, " "); ok {
		return method
	}
	return ""
}

// servedRoutes records every pattern a node with the queue holder's surface registers.
func servedRoutes(t *testing.T) []string {
	t.Helper()
	s := refusalServer(t, holderKey)
	if s.queue == nil {
		t.Fatal("setup: the node has no queue holder, so its queue routes are not registered and not checked")
	}
	rec := &recordingRegistrar{}
	s.registerRoutes(rec)
	return rec.patterns
}

// routeProblems compares the patterns a node registers with the tables, and says, one line each, what is missing.
func routeProblems(patterns []string) []string {
	served := http.NewServeMux()
	for _, p := range patterns {
		served.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	patternOf := func(method, path string) string {
		_, pattern := served.Handler(httptest.NewRequest(method, path, nil))
		return pattern
	}
	var problems []string
	covered, bodyCovered, public := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, d := range refusalDoors {
		p := patternOf(d.method, d.path)
		if p == "" {
			problems = append(problems, fmt.Sprintf("the row %q of refusalDoors (%s %s) names no route the node serves", d.name, d.method, d.path))
			continue
		}
		covered[p] = true
		if takesBody(d.method) {
			bodyCovered[p] = true
		}
	}
	for _, rt := range publicRoutes {
		p := patternOf(http.MethodGet, rt.path)
		switch {
		case p == "":
			problems = append(problems, fmt.Sprintf("the publicRoutes entry %q (%s) names no route the node serves", rt.name, rt.path))
		case patternMethod(p) != http.MethodGet:
			problems = append(problems, fmt.Sprintf("%q is a public route but %q is not read-only: a route that answers without the bearer takes no body", rt.name, p))
		default:
			public[p] = true
		}
	}
	for _, p := range patterns {
		switch {
		case takesBody(patternMethod(p)) && !bodyCovered[p]:
			problems = append(problems, fmt.Sprintf("%q takes a request body and no row of refusalDoors with a method that takes one reaches it: add the row, so a caller without the bearer is shown to cost the node no body byte", p))
		case !takesBody(patternMethod(p)) && !covered[p] && !public[p]:
			problems = append(problems, fmt.Sprintf("%q is served and is neither a row of refusalDoors nor an entry of publicRoutes", p))
		}
	}
	sort.Strings(problems)
	return problems
}

func TestEveryRouteTheNodeServesIsARowOfTheCredentialTables(t *testing.T) {
	patterns := servedRoutes(t)
	if len(patterns) == 0 {
		t.Fatal("setup: no route was registered")
	}
	for _, problem := range routeProblems(patterns) {
		t.Error(problem)
	}
}

// The check itself: a route added next to the real ones, of any shape that takes a body, is named; so is a public route
// that takes one, a row left behind by a door that is gone, and a route no table knows.
func TestTheRouteTableCheckNamesWhatIsMissing(t *testing.T) {
	real := servedRoutes(t)
	if p := routeProblems(real); len(p) != 0 {
		t.Fatalf("setup: the node's own routes already have problems: %q", p)
	}
	for _, tc := range []struct {
		name     string
		patterns []string
		want     string
	}{
		{"a new POST route", append(append([]string{}, real...), "POST /fleet/zz-new"), `"POST /fleet/zz-new" takes a request body`},
		{"a new PUT route", append(append([]string{}, real...), "PUT /fleet/zz-new"), `"PUT /fleet/zz-new" takes a request body`},
		{"a new PATCH route", append(append([]string{}, real...), "PATCH /fleet/zz-new/{id}"), `"PATCH /fleet/zz-new/{id}" takes a request body`},
		{"a new route that names no method", append(append([]string{}, real...), "/fleet/zz-new"), `"/fleet/zz-new" takes a request body`},
		{"a new read-only route", append(append([]string{}, real...), "GET /fleet/zz-new"), `"GET /fleet/zz-new" is served and is neither`},
		{"a new DELETE route", append(append([]string{}, real...), "DELETE /fleet/zz-new"), `"DELETE /fleet/zz-new" is served and is neither`},
		{"a public route that answers every method", withoutPattern(real, "GET /fleet/health", "/fleet/health"), `is a public route but`},
		{"a door that is gone", withoutPattern(real, "POST /fleet/vision"), `names no route the node serves`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := routeProblems(tc.patterns)
			for _, p := range problems {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Errorf("no problem contains %q; problems = %q", tc.want, problems)
		})
	}
}

// withoutPattern is patterns with the named one removed and, when given, another in its place.
func withoutPattern(patterns []string, remove string, add ...string) []string {
	var out []string
	for _, p := range patterns {
		if p != remove {
			out = append(out, p)
		}
	}
	return append(out, add...)
}

// routeSources is what the source test reads: the node's non-test Go files and the durable queue's (whose Mount registers the
// queue's routes on the registrar the node hands it), each by file name, and how many patterns the node registered when it was
// built the way the table test builds it.
type routeSources struct {
	node, queue map[string]string
	recorded    int
}

// readGoSources reads the non-test Go files of one package directory.
func readGoSources(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = string(b)
	}
	return out
}

// parseSources parses each file, in name order, and says which ones do not parse.
func parseSources(fset *token.FileSet, srcs map[string]string) ([]*ast.File, []string) {
	names := make([]string, 0, len(srcs))
	for name := range srcs {
		names = append(names, name)
	}
	sort.Strings(names)
	var files []*ast.File
	var problems []string
	for _, name := range names {
		file, err := parser.ParseFile(fset, name, srcs[name], 0)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		files = append(files, file)
	}
	return files, problems
}

// inspectWithAncestors calls visit for every node under root with the nodes above it, the nearest last.
func inspectWithAncestors(root ast.Node, visit func(n ast.Node, ancestors []ast.Node)) {
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		visit(n, stack)
		stack = append(stack, n)
		return true
	})
}

// calleeIs reports whether call is a call of x.name, with x the identifier pkg (any x when pkg is empty).
func calleeIs(call *ast.CallExpr, pkg, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	if pkg == "" {
		return true
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg
}

// isArgOf reports whether id is one of call's arguments.
func isArgOf(call *ast.CallExpr, id *ast.Ident) bool {
	for _, a := range call.Args {
		if a == ast.Expr(id) {
			return true
		}
	}
	return false
}

// handlerMuxProblems follows the mux Handler builds: every use of it must be an argument of registerRoutes, handed to it once, or
// of netguard.IngressGuard. A use of any other kind (a call that takes the mux and mounts routes on it, a copy under another
// name, a closure that captures it, a bare return) is a way to put a route on the served mux that registerRoutes does not hold.
func handlerMuxProblems(fset *token.FileSet, fd *ast.FuncDecl) []string {
	var problems []string
	built, handed := 0, 0
	var def *ast.Ident
	inspectWithAncestors(fd.Body, func(n ast.Node, up []ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok || !calleeIs(call, "http", "NewServeMux") {
			return
		}
		built++
		if as, ok := up[len(up)-1].(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 && as.Rhs[0] == ast.Expr(call) {
			if id, ok := as.Lhs[0].(*ast.Ident); ok {
				def = id
			}
		}
	})
	if built != 1 || def == nil {
		return append(problems, fmt.Sprintf("%s: %s, and the check cannot follow it: Handler does not build exactly one mux with `mux := http.NewServeMux()` (it builds %d)",
			fset.Position(fd.Pos()), wantMuxFlow, built))
	}
	inspectWithAncestors(fd.Body, func(n ast.Node, up []ast.Node) {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != def.Name || id == def {
			return
		}
		if sel, ok := up[len(up)-1].(*ast.SelectorExpr); ok && sel.Sel == id {
			return // a field or method of that name, not the variable
		}
		if call, ok := up[len(up)-1].(*ast.CallExpr); ok && isArgOf(call, id) {
			switch {
			case calleeIs(call, "", "registerRoutes"):
				handed++
				return
			case calleeIs(call, "netguard", "IngressGuard"):
				return
			}
		}
		problems = append(problems, fmt.Sprintf("%s: %s, and here it is used some other way; a route mounted from there never reaches the route table", fset.Position(id.Pos()), wantMuxFlow))
	})
	if handed != 1 {
		problems = append(problems, fmt.Sprintf("%s: %s, and Handler hands it to registerRoutes %d times, not once", fset.Position(fd.Pos()), wantMuxFlow, handed))
	}
	return problems
}

// registrarUses follows the first parameter of fd, the registrar the function is handed. A use that is the receiver of a
// HandleFunc or Handle call registers a route in the function's own body, where the count sees it; a use as the first argument of
// fleetqueue.Mount (counted in mounts, when mountOK) registers the queue's, which the count adds by the routes of Mount; any other
// use (the registrar copied, captured, or handed to a call that mounts routes on it) is named, with want as the start of the line.
func registrarUses(fset *token.FileSet, fd *ast.FuncDecl, mountOK bool, want string) (mounts int, problems []string) {
	if fd.Body == nil || fd.Type.Params == nil || len(fd.Type.Params.List) == 0 || len(fd.Type.Params.List[0].Names) == 0 {
		return 0, []string{fmt.Sprintf("%s: %s, and %s has no named first parameter for the check to follow", fset.Position(fd.Pos()), want, fd.Name.Name)}
	}
	name := fd.Type.Params.List[0].Names[0].Name
	inspectWithAncestors(fd.Body, func(n ast.Node, up []ast.Node) {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != name {
			return
		}
		switch parent := up[len(up)-1].(type) {
		case *ast.SelectorExpr:
			if parent.Sel == id {
				return // a field or method of that name, not the registrar
			}
			if len(up) >= 2 && (parent.Sel.Name == "HandleFunc" || parent.Sel.Name == "Handle") {
				if call, ok := up[len(up)-2].(*ast.CallExpr); ok && call.Fun == ast.Expr(parent) {
					return
				}
			}
		case *ast.CallExpr:
			if mountOK && len(parent.Args) > 0 && parent.Args[0] == ast.Expr(id) && calleeIs(parent, "fleetqueue", "Mount") {
				mounts++
				return
			}
		}
		problems = append(problems, fmt.Sprintf("%s: %s, and here it is used some other way; a route registered through it is not one the route table records", fset.Position(id.Pos()), want))
	})
	return mounts, problems
}

// queueRouteProblems reads the queue's package: a route is registered by Mount and nowhere else, Mount only registers on the
// router it is handed, and the routes it registers are counted.
func queueRouteProblems(fset *token.FileSet, files []*ast.File) (routes int, problems []string) {
	foundMount := false
	for _, file := range files {
		for _, decl := range file.Decls {
			fd, isFunc := decl.(*ast.FuncDecl)
			isMount := isFunc && fd.Recv == nil && fd.Name.Name == "Mount"
			inside := ""
			if isFunc {
				inside = fd.Name.Name
			}
			if isMount {
				foundMount = true
				_, ps := registrarUses(fset, fd, false, wantMountRouter)
				problems = append(problems, ps...)
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 || !(calleeIs(call, "", "HandleFunc") || calleeIs(call, "", "Handle")) {
					return true
				}
				if isMount {
					routes++
				} else {
					problems = append(problems, fmt.Sprintf("%s: %s, which route_table_test.go counts, and %q registers one", fset.Position(call.Pos()), wantQueueOutside, inside))
				}
				return true
			})
		}
	}
	if !foundMount {
		problems = append(problems, "no Mount was found in the queue's package: the source test no longer sees what registers the queue's routes")
	}
	return routes, problems
}

// routeSourceProblems reads the sources and says, one line each, where a route could be registered that the route table
// does not see.
func routeSourceProblems(src routeSources) []string {
	fset := token.NewFileSet()
	nodeFiles, problems := parseSources(fset, src.node)
	registerCalls, mounts := 0, 0
	for _, file := range nodeFiles {
		for _, decl := range file.Decls {
			inside := ""
			if fd, ok := decl.(*ast.FuncDecl); ok {
				inside = fd.Name.Name
				if inside == "Handler" && fd.Body != nil {
					problems = append(problems, handlerMuxProblems(fset, fd)...)
				}
				if inside == "registerRoutes" {
					m, ps := registrarUses(fset, fd, true, wantRegistrar)
					mounts += m
					problems = append(problems, ps...)
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
				at := fset.Position(call.Pos())
				switch sel.Sel.Name {
				case "HandleFunc", "Handle":
					if len(call.Args) != 2 { // a route takes a pattern and a handler; syscall.Handle(h) is a conversion
						return true
					}
					if inside != "registerRoutes" {
						problems = append(problems, fmt.Sprintf("%s: a route is registered by %s in %q; register it in registerRoutes, which route_table_test.go holds to the credential tables", at, sel.Sel.Name, inside))
					} else {
						registerCalls++
					}
				case "NewServeMux":
					if inside != "Handler" {
						problems = append(problems, fmt.Sprintf("%s: %q builds a mux of its own; the node serves the one Handler builds and hands to registerRoutes", at, inside))
					}
				}
				return true
			})
		}
	}
	if registerCalls == 0 {
		problems = append(problems, "no route registration was found in registerRoutes: the source test no longer sees what it guards")
	}
	queueFiles, parseProblems := parseSources(fset, src.queue)
	queueRoutes, queueProblems := queueRouteProblems(fset, queueFiles)
	problems = append(append(problems, parseProblems...), queueProblems...)
	// The table test records the patterns a node registers when built one way. A registration that stands in registerRoutes (or in
	// Mount) and was not recorded is one the tables never saw: it sits under a condition that node does not meet.
	if static := registerCalls + mounts*queueRoutes; static != src.recorded {
		problems = append(problems, fmt.Sprintf("registerRoutes holds %d route registrations (%d of its own, and %d in fleetqueue.Mount, called %d time(s)) and the node the table test builds registers %d: a route registered under a condition that node does not meet, or in a loop, is one the credential tables never saw; make the node meet the condition, or register the route without one",
			static, registerCalls, queueRoutes, mounts, src.recorded))
	}
	sort.Strings(problems)
	return problems
}

// The routes the node serves are the ones registerRoutes registers and no others: no function of the package registers a route
// on a mux of its own, and the only mux built is Handler's, which it hands to registerRoutes and wraps in the ingress guard.
func TestRoutesAreRegisteredOnlyThroughRegisterRoutes(t *testing.T) {
	src := routeSources{node: readGoSources(t, "."), queue: readGoSources(t, "../fleetqueue"), recorded: len(servedRoutes(t))}
	for _, problem := range routeSourceProblems(src) {
		t.Error(problem)
	}
}

// What each problem of the source test says, which the cases below look for.
const (
	wantMuxFlow       = "the mux Handler builds goes only to registerRoutes and netguard.IngressGuard"
	wantHandlerReturn = "Handler returns the netguard.IngressGuard call around its mux and nothing else"
	wantServerHandler = "the node's http.Server serves s.Handler() and nothing wrapped around it"
	wantRegistrar     = "registerRoutes hands its registrar only to fleetqueue.Mount"
	wantQueueOutside  = "fleetqueue registers a route only in Mount"
	wantMountRouter   = "Mount uses its router only as the receiver of HandleFunc"
	wantCount         = "route registrations"
)

// A node and a queue in the shape of the real ones: Handler builds one mux, hands it to registerRoutes and to the ingress guard;
// registerRoutes registers two routes and, when the queue is on, hands the registrar to fleetqueue.Mount, which registers two.
const routeFixtureNode = `package fleetnode

import "net/http"

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return netguard.IngressGuard(opts, mux)
}

func (s *Server) httpServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func (s *Server) registerRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET /fleet/health", s.handleHealth)
	mux.HandleFunc("POST /fleet/dispatch", s.handleDispatch)
	if s.queue != nil {
		fleetqueue.Mount(mux, s.queue, s.queueAuthOK)
	}
}
`

const routeFixtureQueue = `package fleetqueue

import "net/http"

func Mount(mux Router, q *Queue, authOK func(*http.Request) bool) {
	mux.HandleFunc("POST /fleet/queue/submit", nil)
	mux.HandleFunc("GET /fleet/queue/jobs/{id}", nil)
}
`

// routeFixtureRecorded is what a node built from the fixtures registers: its two routes and the two Mount registers.
const routeFixtureRecorded = 4

// replaceOnce is s with the one occurrence of old replaced by new.
func replaceOnce(t *testing.T, s, old, new string) string {
	t.Helper()
	if n := strings.Count(s, old); n != 1 {
		t.Fatalf("setup: %q occurs %d times, want 1", old, n)
	}
	return strings.Replace(s, old, new, 1)
}

// The check itself: a route that reaches the node's mux by a way the route table cannot record is named, wherever it is added.
func TestTheRouteSourceCheckNamesWhatEscapesTheRouteTable(t *testing.T) {
	control := routeSources{node: map[string]string{"server.go": routeFixtureNode}, queue: map[string]string{"http.go": routeFixtureQueue}, recorded: routeFixtureRecorded}
	if p := routeSourceProblems(control); len(p) != 0 {
		t.Fatalf("setup: the fixture already has problems: %q", p)
	}
	const queueRoute = "\tmux.HandleFunc(\"GET /fleet/queue/jobs/{id}\", nil)\n"
	for _, tc := range []struct {
		name       string
		node       [][2]string // old, new pairs applied to routeFixtureNode
		queue      [][2]string // old, new pairs applied to routeFixtureQueue
		extraQueue string      // appended to routeFixtureQueue
		recorded   int         // 0: the fixture's own
		want       string
	}{
		{name: "a cross-package mount handed the mux in Handler", want: wantMuxFlow,
			node: [][2]string{{"s.registerRoutes(mux)\n", "s.registerRoutes(mux)\n\tfleetqueue.MountExtra(mux)\n"}}},
		{name: "the mux copied under another name in Handler", want: wantMuxFlow,
			node: [][2]string{{"s.registerRoutes(mux)\n", "s.registerRoutes(mux)\n\textra := mux\n\tfleetqueue.MountExtra(extra)\n"}}},
		{name: "the mux handed to a closure in Handler", want: wantMuxFlow,
			node: [][2]string{{"s.registerRoutes(mux)\n", "s.registerRoutes(mux)\n\tfunc() { fleetqueue.MountExtra(mux) }()\n"}}},
		{name: "a route registered on the mux in Handler", want: wantMuxFlow,
			node: [][2]string{{"s.registerRoutes(mux)\n", "s.registerRoutes(mux)\n\tmux.Handle(\"/fleet/zz\", nil)\n"}}},
		{name: "Handler serves the bare mux", want: wantMuxFlow,
			node: [][2]string{{"return netguard.IngressGuard(opts, mux)", "return mux"}}},
		{name: "Handler does not hand the mux to registerRoutes", want: wantMuxFlow,
			node: [][2]string{{"\ts.registerRoutes(mux)\n", ""}}},
		// What the node serves is what Handler returns and what httpServer puts on the socket: a handler wrapped around either answers
		// for a path before the mux, the bearer rules and the route table, and registers nothing.
		{name: "a handler wrapped around the ingress guard in Handler", want: wantHandlerReturn,
			node: [][2]string{{"return netguard.IngressGuard(opts, mux)", "return zzWrap(netguard.IngressGuard(opts, mux))"}}},
		{name: "the guard assigned in Handler and a wrapper returned", want: wantHandlerReturn,
			node: [][2]string{{"return netguard.IngressGuard(opts, mux)", "guarded := netguard.IngressGuard(opts, mux)\n\treturn zzWrap(guarded)"}}},
		{name: "a second return in Handler", want: wantHandlerReturn,
			node: [][2]string{{"\treturn netguard.IngressGuard(opts, mux)", "\tif s.opts.Debug {\n\t\treturn zzDebug()\n\t}\n\treturn netguard.IngressGuard(opts, mux)"}}},
		{name: "a handler wrapped around s.Handler() in httpServer", want: wantServerHandler,
			node: [][2]string{{"Handler:           s.Handler(),", "Handler:           zzWrap(s.Handler()),"}}},
		{name: "httpServer serves a handler of its own", want: wantServerHandler,
			node: [][2]string{{"Handler:           s.Handler(),", "Handler:           http.NotFoundHandler(),"}}},
		{name: "an http.Server with no Handler", want: wantServerHandler,
			node: [][2]string{{"\t\tHandler:           s.Handler(),\n", ""}}},
		{name: "a second http.Server built outside httpServer", want: wantServerHandler,
			node: [][2]string{{"func (s *Server) registerRoutes(", "func (s *Server) elsewhere(h http.Handler) *http.Server {\n\treturn &http.Server{Handler: h}\n}\n\nfunc (s *Server) registerRoutes("}}},
		{name: "http.ListenAndServe with a handler of its own", want: wantServerHandler,
			node: [][2]string{{"func (s *Server) registerRoutes(", "func (s *Server) elsewhere() error {\n\treturn http.ListenAndServe(\":0\", nil)\n}\n\nfunc (s *Server) registerRoutes("}}},
		{name: "a route registered under a condition in registerRoutes", want: wantCount,
			node: [][2]string{{"\tif s.queue != nil {", "\tif s.opts.LoopbackListener {\n\t\tmux.HandleFunc(\"POST /fleet/zz-local\", s.handleZZ)\n\t}\n\tif s.queue != nil {"}}},
		{name: "routes registered in a loop in registerRoutes", want: wantCount, recorded: routeFixtureRecorded + 2,
			node: [][2]string{{"\tif s.queue != nil {", "\tfor _, p := range []string{\"POST /fleet/zz-a\", \"POST /fleet/zz-b\"} {\n\t\tmux.HandleFunc(p, s.handleZZ)\n\t}\n\tif s.queue != nil {"}}},
		{name: "a cross-package mount under a condition in registerRoutes", want: wantRegistrar,
			node: [][2]string{{"\tif s.queue != nil {", "\tif s.opts.LoopbackListener {\n\t\tfleetqueue.MountExtra(mux)\n\t}\n\tif s.queue != nil {"}}},
		{name: "the registrar copied under another name in registerRoutes", want: wantRegistrar,
			node: [][2]string{{"\tif s.queue != nil {", "\textra := mux\n\textra.HandleFunc(\"POST /fleet/zz\", s.handleZZ)\n\tif s.queue != nil {"}}},
		{name: "the queue registers a route outside Mount", want: wantQueueOutside,
			extraQueue: "\nfunc MountExtra(mux Router) {\n\tmux.HandleFunc(\"POST /fleet/queue/zz\", nil)\n}\n"},
		{name: "Mount registers a route under a condition", want: wantCount,
			queue: [][2]string{{queueRoute, queueRoute + "\tif q.extra {\n\t\tmux.HandleFunc(\"POST /fleet/queue/zz\", nil)\n\t}\n"}}},
		{name: "Mount hands its router on", want: wantMountRouter,
			queue: [][2]string{{queueRoute, queueRoute + "\tmountMore(mux)\n"}}},
		// The two rules that stood before.
		{name: "a route registered outside registerRoutes", want: "a route is registered by HandleFunc in",
			node: [][2]string{{"func (s *Server) registerRoutes(", "func (s *Server) elsewhere(mux routeRegistrar) {\n\tmux.HandleFunc(\"GET /fleet/zz\", s.handleZZ)\n}\n\nfunc (s *Server) registerRoutes("}}},
		{name: "a second mux built outside Handler", want: "builds a mux of its own",
			node: [][2]string{{"func (s *Server) registerRoutes(", "func (s *Server) elsewhere() http.Handler {\n\treturn http.NewServeMux()\n}\n\nfunc (s *Server) registerRoutes("}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, queue := routeFixtureNode, routeFixtureQueue+tc.extraQueue
			for _, r := range tc.node {
				node = replaceOnce(t, node, r[0], r[1])
			}
			for _, r := range tc.queue {
				queue = replaceOnce(t, queue, r[0], r[1])
			}
			recorded := tc.recorded
			if recorded == 0 {
				recorded = routeFixtureRecorded
			}
			problems := routeSourceProblems(routeSources{node: map[string]string{"server.go": node}, queue: map[string]string{"http.go": queue}, recorded: recorded})
			for _, p := range problems {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Errorf("no problem contains %q; problems = %q", tc.want, problems)
		})
	}
}

// Two additions made to the node's real sources, the shapes that put a route on the mux without a row in the tables: a mount made
// from Handler by a call that takes the mux, and a route registered under a condition the test node does not meet.
func TestTheRouteSourceCheckNamesTheseAdditionsToTheRealSources(t *testing.T) {
	queue, recorded := readGoSources(t, "../fleetqueue"), len(servedRoutes(t))
	if p := routeSourceProblems(routeSources{node: readGoSources(t, "."), queue: queue, recorded: recorded}); len(p) != 0 {
		t.Fatalf("setup: the real sources already have problems: %q", p)
	}
	for _, tc := range []struct{ name, old, new, old2, new2, want string }{
		{"a cross-package mount handed the mux in Handler", "s.registerRoutes(mux)", "s.registerRoutes(mux)\n\tfleetqueue.MountExtra(mux)", "", "", wantMuxFlow},
		{"a route registered under a condition in registerRoutes", `mux.HandleFunc("GET /fleet/health", s.handleHealth)`,
			"mux.HandleFunc(\"GET /fleet/health\", s.handleHealth)\n\tif s.opts.LoopbackListener {\n\t\tmux.HandleFunc(\"POST /fleet/zz-local\", s.handleHealth)\n\t}", "", "", wantCount},
		// A path-answering wrapper around the guarded handler, in Handler and in httpServer: neither touches the mux, registerRoutes
		// or a HandleFunc call.
		{"a handler wrapped around the ingress guard in Handler", "return netguard.IngressGuard(netguard.IngressOptions{", "return zzWrap(netguard.IngressGuard(netguard.IngressOptions{",
			"}, mux)", "}, mux))", wantHandlerReturn},
		{"a handler wrapped around s.Handler() in httpServer", "Handler:           s.Handler(),", "Handler:           zzWrap(s.Handler()),", "", "", wantServerHandler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := readGoSources(t, ".")
			node["server.go"] = replaceOnce(t, node["server.go"], tc.old, tc.new)
			if tc.old2 != "" {
				node["server.go"] = replaceOnce(t, node["server.go"], tc.old2, tc.new2)
			}
			problems := routeSourceProblems(routeSources{node: node, queue: queue, recorded: recorded})
			for _, p := range problems {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Errorf("no problem contains %q; problems = %q", tc.want, problems)
		})
	}
}
