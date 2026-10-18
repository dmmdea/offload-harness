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

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
