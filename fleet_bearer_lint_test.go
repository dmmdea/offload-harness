package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEveryFleetClientRequestSetsTheBearer pins, for code that does not exist yet, what the per-client
// tests (TestEveryRequestCarriesTheFleetBearer in each client package) pin for the code that does: a
// fleet node requires the fleet bearer on every lane, so a request to one must carry it. The lint reads the Go
// source of every fleet client package, and of any file that names a "/fleet/" route, and reports:
//
//   - a request built by http.NewRequest or http.NewRequestWithContext that no bearer-setting call reaches. The
//     judgment is per request, not per function: each request value (a second request in the same function, or the
//     same name assigned again) needs its own call, and a request that is not kept in a variable cannot be given one;
//   - a call to http.Get, http.Post, http.Head or http.PostForm, and to the same four methods on an *http.Client (a
//     parameter, a field, a package variable, http.DefaultClient, a client built on the spot, the result of a
//     function that returns one, a package variable that re-exports a client another package holds, or a local that
//     copies, points to or dereferences any of these): they build and send a request in one call and leave nowhere to
//     set a header;
//   - a Do on a request that was not built by http.NewRequest in the same function, was not handed in as a
//     *http.Request, and has no bearer-setting call on it, and a Do on a request built inline.
//
// A recognised bearer-setting call is one of two things: Header.Set or Header.Add of "Authorization" with a value that
// begins "Bearer " on that request value, or a call to a function or method of the same package (or of a package in
// this module, by import) that takes a *http.Request and does exactly that to it, or passes it on to one that does.
// A call named auth means nothing unless the function it names does so, and a string that says "Authorization"
// anywhere else, a header set on some other request, or a value that is not a bearer counts for nothing.
//
// What is not recognised by syntax alone: a client that reaches the shorthand through a name the lint cannot tie to
// *http.Client (it follows field, parameter, variable and constructor names, not types), and a request handed to a
// function through an interface or a function value. The shape of each case that is caught is in
// testdata/fleetbearerlint (red/ must be reported, green/ must not), and TestTheFleetBearerLintReportsEachShapeItShouldCatch
// holds the lint to both.
//
// A request that deliberately goes without (an identity read of a base that may not be a fleet node, a call to a local
// engine) is listed in fleetBearerAllowlist with the reason, and a stale entry fails too. Keyed by file and enclosing
// function, with "#2", "#3" after the first site found in a function, so a second bare request in an allowed function
// is a new site. tools/ is a separate module and out of scope.
func TestEveryFleetClientRequestSetsTheBearer(t *testing.T) {
	got := fleetRequestSitesWithoutBearer(t)
	if os.Getenv("FLEET_BEARER_LINT_PRINT") != "" {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("\t%q: \"\", // %s\n", k, got[k])
		}
		t.Skip("printed the current sites")
	}
	for key, what := range got {
		if _, ok := fleetBearerAllowlist[key]; !ok {
			t.Errorf("%s: %s. Set the fleet bearer on it (the package's auth helper, or Header.Set of \"Authorization\" with a \"Bearer \" value), or, if the request deliberately carries none, list it in fleetBearerAllowlist with the reason", key, what)
		}
	}
	for key, why := range fleetBearerAllowlist {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is in fleetBearerAllowlist but no longer sends a request without the bearer: remove it", key)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s: allowlist entry carries no reason", key)
		}
	}
}

// fleetClientDirs are the packages whose HTTP requests go to a fleet node.
var fleetClientDirs = map[string]bool{
	"internal/mediaremote":    true,
	"internal/composeremote":  true,
	"internal/sttremote":      true,
	"internal/textremote":     true,
	"internal/forecastremote": true,
	"internal/visionremote":   true,
	"internal/accelremote":    true,
	"internal/delegate":       true,
	"internal/fleetview":      true,
	"internal/pairworkloads":  true,
	"internal/rosterprobe":    true,
	"internal/llamaclient":    true,
}

// fleetBearerAllowlist: requests that go without the bearer, and why.
var fleetBearerAllowlist = map[string]string{
	"internal/fleetnode/chat_lane.go:handleChat":       "the node's own forward to its loopback llama-swap, not a request to a fleet node",
	"internal/fleetnode/kvslots.go:handleKVSlot":       "the node's own forward to its loopback llama-swap, not a request to a fleet node",
	"internal/fleetview/topmodel.go:fetch":             "the terminal view reads the fleet-ui dashboard's own /api/overview, an unauthenticated route of the operator's dashboard and not a fleet node (the poller behind the dashboard sets the bearer on every node request)",
	"internal/llamaclient/client.go:Health":            "llama-swap's own /health on the client's endpoint, not a fleet node",
	"internal/llamaclient/lanes.go:probeFleetHealth":   "the identity read of a cascade lane base that may be a plain llama-swap: health is open, and the bearer must never be offered to a base that is not a fleet node",
	"internal/pairworkloads/nodeinfo.go:probeHostUUID": "PAIR's loopback node-info, not a fleet node",
	"internal/pairworkloads/nodeinfo.go:probeIngress":  "PAIR's loopback ingress, not a fleet node",
	"internal/pairworkloads/pairworkloads.go:post":     "a frame to PAIR's own ingress, not a fleet node (the relay to a fleet node is postRelay, which sets the bearer)",
	"internal/pairworkloads/seatwatch.go:get":          "a read of this box's own llama-swap, not a fleet node",
}

// modulePrefix is the import-path prefix of this module's packages.
const modulePrefix = "github.com/dmmdea/offload-harness/"

// fleetRequestSitesWithoutBearer returns the sites in the repo that send a request without the bearer.
func fleetRequestSitesWithoutBearer(t *testing.T) map[string]string {
	t.Helper()
	return fleetRequestSitesIn(t, ".")
}

// fleetRequestSitesIn reads the Go files under root. The repo's own scan starts at the repo root; the fixtures of the
// lint's own test start at their directories. Keys are "<file>:<function>" (and "#2", "#3" for later sites in the same
// function); the value says what the site does.
func fleetRequestSitesIn(t *testing.T, root string) map[string]string {
	t.Helper()
	l := &bearerLint{
		fset:         token.NewFileSet(),
		setters:      map[funcKey]map[int]bool{},
		clientFields: map[string]bool{},
		pkgClients:   map[string]map[string]bool{},
		clientFuncs:  map[string]bool{},
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "tools", "testdata", "node_modules", "bin":
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(l.fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		rel := filepath.ToSlash(path)
		lf := &lintFile{rel: rel, dir: filepath.ToSlash(filepath.Dir(path)), file: f, imports: map[string]string{}}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil || !strings.HasPrefix(p, modulePrefix) {
				continue
			}
			name := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			lf.imports[name] = strings.TrimPrefix(p, modulePrefix)
		}
		namesFleetRoute := false
		ast.Inspect(f, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING && strings.Contains(bl.Value, "/fleet/") {
				namesFleetRoute = true
			}
			return true
		})
		lf.scoped = fleetClientDirs[lf.dir] || rel == "internal/fleetnode/claimloop.go" || namesFleetRoute
		l.files = append(l.files, lf)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	l.collectClients()
	l.collectSetters()
	return l.sites()
}

// lintFile is one parsed non-test Go file.
type lintFile struct {
	rel     string            // slash path from the scan root
	dir     string            // its package directory
	file    *ast.File         // its syntax tree
	imports map[string]string // local package name -> directory, for this module's packages
	scoped  bool              // a fleet client package, or a file that names a "/fleet/" route: its requests are judged
}

// funcKey names a function or method of a package, by directory and name. A method is keyed by its name alone: the
// lint ties a call to the package it is in, not to a receiver type.
type funcKey struct{ dir, name string }

type bearerLint struct {
	fset  *token.FileSet
	files []*lintFile
	// setters: the parameters (by index, the receiver not counted) of a function that take a *http.Request and put the
	// bearer on it.
	setters map[funcKey]map[int]bool
	// clientFields: names of struct fields and package variables, in any package, that hold an *http.Client.
	clientFields map[string]bool
	// pkgClients: the package-level variables of a package that hold an *http.Client.
	pkgClients map[string]map[string]bool
	// clientFuncs: names of functions that return an *http.Client.
	clientFuncs map[string]bool
}

func isHTTPSel(e ast.Expr, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "http"
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// isClientType: http.Client or *http.Client.
func isClientType(e ast.Expr) bool {
	if e == nil {
		return false
	}
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	return isHTTPSel(e, "Client")
}

// isRequestType: *http.Request.
func isRequestType(e ast.Expr) bool {
	star, ok := e.(*ast.StarExpr)
	return ok && isHTTPSel(star.X, "Request")
}

// isClientLiteral: &http.Client{...}, http.Client{...} or http.DefaultClient.
func isClientLiteral(e ast.Expr) bool {
	e = unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = unparen(u.X)
	}
	if cl, ok := e.(*ast.CompositeLit); ok {
		return isClientType(cl.Type)
	}
	return isHTTPSel(e, "DefaultClient")
}

func calleeName(call *ast.CallExpr) string {
	switch fn := unparen(call.Fun).(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// holdsClient: the expression is, or hands back, an *http.Client: a client literal, a call of a function that returns
// one, a field or package variable that holds one (by name, in any package: what a re-export or a copy of a field
// reads), a package variable of dir, a local that local reports as holding one, or a pointer to or a copy of any of
// these. local may be nil outside a function body.
func (l *bearerLint) holdsClient(dir string, local func(string) bool, e ast.Expr) bool {
	if isClientLiteral(e) {
		return true
	}
	switch v := unparen(e).(type) {
	case *ast.CallExpr:
		return l.clientFuncs[calleeName(v)]
	case *ast.SelectorExpr:
		return l.clientFields[v.Sel.Name]
	case *ast.Ident:
		return l.pkgClients[dir][v.Name] || (local != nil && local(v.Name))
	case *ast.StarExpr:
		return l.holdsClient(dir, local, v.X)
	case *ast.UnaryExpr:
		return v.Op == token.AND && l.holdsClient(dir, local, v.X)
	}
	return false
}

// collectClients finds, across the whole scan, where an *http.Client lives: the functions that return one, the struct
// fields and package variables that hold one.
func (l *bearerLint) collectClients() {
	for _, lf := range l.files {
		for _, decl := range lf.file.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Type.Results != nil {
				for _, r := range fd.Type.Results.List {
					if isClientType(r.Type) {
						l.clientFuncs[fd.Name.Name] = true
					}
				}
			}
		}
	}
	for _, lf := range l.files {
		ast.Inspect(lf.file, func(n ast.Node) bool {
			if st, ok := n.(*ast.StructType); ok {
				for _, field := range st.Fields.List {
					if isClientType(field.Type) {
						for _, name := range field.Names {
							l.clientFields[name.Name] = true
						}
					}
				}
			}
			return true
		})
	}
	// Package variables, until nothing new turns up: a variable can be initialised from a function that returns a client,
	// from a variable of this package, or from one another package holds.
	for changed := true; changed; {
		changed = false
		for _, lf := range l.files {
			for _, decl := range lf.file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if name.Name == "_" || l.pkgClients[lf.dir][name.Name] {
							continue
						}
						if isClientType(vs.Type) || (i < len(vs.Values) && l.holdsClient(lf.dir, nil, vs.Values[i])) {
							if l.pkgClients[lf.dir] == nil {
								l.pkgClients[lf.dir] = map[string]bool{}
							}
							l.pkgClients[lf.dir][name.Name] = true
							l.clientFields[name.Name] = true
							changed = true
						}
					}
				}
			}
		}
	}
}

// stringLit returns the value of a string literal.
func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

// bearerValue: the expression begins "Bearer " ("Bearer "+x, fmt.Sprintf("Bearer %s", x), or the literal).
func bearerValue(e ast.Expr) bool {
	switch v := unparen(e).(type) {
	case *ast.BasicLit:
		s, ok := stringLit(v)
		return ok && strings.HasPrefix(s, "Bearer ")
	case *ast.BinaryExpr:
		return v.Op == token.ADD && bearerValue(v.X)
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Sprintf" && len(v.Args) > 0 && bearerValue(v.Args[0])
	}
	return false
}

// directBearerSet: the call is R.Header.Set("Authorization", "Bearer ...") or the same with Add. It returns R.
func directBearerSet(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Set" && sel.Sel.Name != "Add") || len(call.Args) != 2 {
		return "", false
	}
	hdr, ok := sel.X.(*ast.SelectorExpr)
	if !ok || hdr.Sel.Name != "Header" {
		return "", false
	}
	id, ok := hdr.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	if key, ok := stringLit(call.Args[0]); !ok || !strings.EqualFold(key, "Authorization") {
		return "", false
	}
	if !bearerValue(call.Args[1]) {
		return "", false
	}
	return id.Name, true
}

// bearerEvent is a place where a request value, named by the identifier it is held in, gets the bearer.
type bearerEvent struct {
	name string
	pos  token.Pos
}

// calleeKey ties a call to the function it names: in this package, or in another package of this module by import.
func (lf *lintFile) calleeKey(call *ast.CallExpr) (funcKey, bool) {
	switch fn := unparen(call.Fun).(type) {
	case *ast.Ident:
		return funcKey{lf.dir, fn.Name}, true
	case *ast.SelectorExpr:
		if id, ok := fn.X.(*ast.Ident); ok {
			if dir, isPkg := lf.imports[id.Name]; isPkg {
				return funcKey{dir, fn.Sel.Name}, true
			}
		}
		return funcKey{lf.dir, fn.Sel.Name}, true
	}
	return funcKey{}, false
}

// bearerEvents lists every call under node that puts the bearer on a request value held in an identifier.
func (l *bearerLint) bearerEvents(lf *lintFile, node ast.Node) []bearerEvent {
	var events []bearerEvent
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name, ok := directBearerSet(call); ok {
			events = append(events, bearerEvent{name, call.Pos()})
			return true
		}
		if key, ok := lf.calleeKey(call); ok {
			for idx := range l.setters[key] {
				if idx < len(call.Args) {
					if id, ok := unparen(call.Args[idx]).(*ast.Ident); ok {
						events = append(events, bearerEvent{id.Name, call.Pos()})
					}
				}
			}
		}
		return true
	})
	return events
}

// collectSetters finds the functions that put the bearer on a *http.Request they are given, to a fixed point: a function
// that hands its request to one of them is one too.
func (l *bearerLint) collectSetters() {
	for changed := true; changed; {
		changed = false
		for _, lf := range l.files {
			for _, decl := range lf.file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil || fd.Type.Params == nil {
					continue
				}
				events := l.bearerEvents(lf, fd.Body)
				idx := 0
				for _, field := range fd.Type.Params.List {
					names := field.Names
					if len(names) == 0 {
						idx++
						continue
					}
					for _, name := range names {
						if isRequestType(field.Type) {
							for _, ev := range events {
								if ev.name == name.Name {
									key := funcKey{lf.dir, fd.Name.Name}
									if l.setters[key] == nil {
										l.setters[key] = map[int]bool{}
									}
									if !l.setters[key][idx] {
										l.setters[key][idx] = true
										changed = true
									}
									break
								}
							}
						}
						idx++
					}
				}
			}
		}
	}
}

// unit is a body the lint judges: a function or method, or a package-level variable spec (a function literal in one).
type unit struct {
	name   string
	node   ast.Node
	params *ast.FieldList
}

func (lf *lintFile) units() []unit {
	var out []unit
	for _, decl := range lf.file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				out = append(out, unit{d.Name.Name, d.Body, d.Type.Params})
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				vs := spec.(*ast.ValueSpec)
				out = append(out, unit{"var " + vs.Names[0].Name, vs, nil})
			}
		}
	}
	return out
}

// sites judges every unit of every scoped file.
func (l *bearerLint) sites() map[string]string {
	out := map[string]string{}
	for _, lf := range l.files {
		if !lf.scoped {
			continue
		}
		for _, u := range lf.units() {
			for i, what := range l.judge(lf, u) {
				key := lf.rel + ":" + u.name
				if i > 0 {
					key += "#" + strconv.Itoa(i+1)
				}
				out[key] = what
			}
		}
	}
	return out
}

// shorthandArity: the methods of *http.Client (and the package functions of the same names) that build a request from
// their arguments, with the number of arguments each takes.
var shorthandArity = map[string]int{"Get": 1, "Head": 1, "Post": 3, "PostForm": 2}

type lintHit struct {
	pos  token.Pos
	what string
}

// judge returns what a unit does that sends a request to a fleet node without the bearer, in source order.
func (l *bearerLint) judge(lf *lintFile, u unit) []string {
	locals := map[string]bool{}  // identifiers holding an *http.Client
	given := map[string]bool{}   // identifiers holding a *http.Request this unit was handed
	created := map[string]bool{} // identifiers holding a request built by http.NewRequest in this unit
	noteParams := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, field := range fl.List {
			for _, name := range field.Names {
				if isClientType(field.Type) {
					locals[name.Name] = true
				}
				if isRequestType(field.Type) {
					given[name.Name] = true
				}
			}
		}
	}
	noteParams(u.params)
	isLocalClient := func(name string) bool { return locals[name] }
	ast.Inspect(u.node, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncLit:
			noteParams(v.Type.Params)
		case *ast.ValueSpec:
			for i, name := range v.Names {
				if isClientType(v.Type) || (i < len(v.Values) && l.holdsClient(lf.dir, isLocalClient, v.Values[i])) {
					locals[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			for i, rhs := range v.Rhs {
				if id, ok := v.Lhs[min(i, len(v.Lhs)-1)].(*ast.Ident); ok && l.holdsClient(lf.dir, isLocalClient, rhs) {
					locals[id.Name] = true
				}
			}
		}
		return true
	})
	isClient := func(x ast.Expr) bool { return l.holdsClient(lf.dir, isLocalClient, x) }

	var found []lintHit

	// Requests built by http.NewRequest: each one needs a bearer call of its own.
	isNewRequest := func(e ast.Expr) (*ast.CallExpr, bool) {
		call, ok := unparen(e).(*ast.CallExpr)
		if !ok {
			return nil, false
		}
		if isHTTPSel(call.Fun, "NewRequest") || isHTTPSel(call.Fun, "NewRequestWithContext") {
			return call, true
		}
		return nil, false
	}
	type creation struct {
		name string
		pos  token.Pos
	}
	var creations []creation
	kept := map[*ast.CallExpr]bool{}
	ast.Inspect(u.node, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if len(v.Rhs) == 1 && len(v.Lhs) >= 1 {
				if call, ok := isNewRequest(v.Rhs[0]); ok {
					if id, ok := v.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
						creations = append(creations, creation{id.Name, v.Pos()})
						created[id.Name] = true
						kept[call] = true
					}
				}
			}
		case *ast.ValueSpec:
			if len(v.Values) == 1 && len(v.Names) >= 1 {
				if call, ok := isNewRequest(v.Values[0]); ok && v.Names[0].Name != "_" {
					creations = append(creations, creation{v.Names[0].Name, v.Pos()})
					created[v.Names[0].Name] = true
					kept[call] = true
				}
			}
		}
		return true
	})
	events := l.bearerEvents(lf, u.node)
	reached := func(name string, from, to token.Pos) bool {
		for _, ev := range events {
			if ev.name == name && ev.pos > from && (to == token.NoPos || ev.pos < to) {
				return true
			}
		}
		return false
	}
	for i, c := range creations {
		next := token.NoPos
		for _, later := range creations[i+1:] {
			if later.name == c.name && later.pos > c.pos {
				next = later.pos
				break
			}
		}
		if !reached(c.name, c.pos, next) {
			found = append(found, lintHit{c.pos, "builds the request " + c.name + ", and no bearer-setting call reaches it"})
		}
	}

	ast.Inspect(u.node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if _, isNew := isNewRequest(call); isNew && !kept[call] {
			found = append(found, lintHit{call.Pos(), "builds a request that is not kept in a variable, so nothing can set the bearer on it"})
			return true
		}
		if isHTTPSel(call.Fun, "Get") || isHTTPSel(call.Fun, "Head") || isHTTPSel(call.Fun, "Post") || isHTTPSel(call.Fun, "PostForm") {
			found = append(found, lintHit{call.Pos(), "sends through http." + call.Fun.(*ast.SelectorExpr).Sel.Name + ", which leaves nowhere to set the bearer"})
			return true
		}
		sel, ok := unparen(call.Fun).(*ast.SelectorExpr)
		if !ok || !isClient(sel.X) {
			return true
		}
		if arity, short := shorthandArity[sel.Sel.Name]; short && len(call.Args) == arity {
			found = append(found, lintHit{call.Pos(), "sends through the client's " + sel.Sel.Name + " shorthand, which leaves nowhere to set the bearer"})
			return true
		}
		if sel.Sel.Name == "Do" && len(call.Args) == 1 {
			switch a := unparen(call.Args[0]).(type) {
			case *ast.Ident:
				if !created[a.Name] && !given[a.Name] && !reached(a.Name, token.NoPos, token.NoPos) {
					found = append(found, lintHit{call.Pos(), "sends the request " + a.Name + ", which was not built by http.NewRequest here and has no bearer-setting call on it"})
				}
			default:
				found = append(found, lintHit{call.Pos(), "sends a request built inline, so nothing can set the bearer on it"})
			}
		}
		return true
	})

	sort.Slice(found, func(i, j int) bool { return found[i].pos < found[j].pos })
	out := make([]string, len(found))
	for i, f := range found {
		out[i] = fmt.Sprintf("line %d %s", l.fset.Position(f.pos).Line, f.what)
	}
	return out
}

// TestTheFleetBearerLintReportsEachShapeItShouldCatch runs the lint over the fixtures in testdata/fleetbearerlint: every
// function in red/ is a request that goes to a fleet node without the bearer, in a shape the lint must report, and nothing
// else in red/ may be reported; green/ is requests that do carry it, and none may be.
func TestTheFleetBearerLintReportsEachShapeItShouldCatch(t *testing.T) {
	const redDir, greenDir = "testdata/fleetbearerlint/red", "testdata/fleetbearerlint/green"
	want := map[string]bool{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(redDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := redDir + "/" + e.Name()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatal(perr)
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && strings.HasPrefix(fd.Name.Name, "Red") {
				want[path+":"+fd.Name.Name] = true
			}
		}
	}
	if len(want) < 15 {
		t.Fatalf("the red fixtures hold %d shapes, want the whole set", len(want))
	}
	got := fleetRequestSitesIn(t, redDir)
	for key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("the lint does not report the red fixture %s", key)
		}
	}
	for key := range got {
		if !want[key] {
			t.Errorf("the lint reports %s in the red fixtures, and it is not one of their shapes", key)
		}
	}
	for key := range fleetRequestSitesIn(t, greenDir) {
		t.Errorf("the lint reports %s in the green fixtures, and that request carries the bearer", key)
	}
}
