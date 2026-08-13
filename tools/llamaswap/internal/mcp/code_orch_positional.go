// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave LS-1): path-parameter backfill for the code-orchestration
// execute tool.
//
// THE BUG THIS FIXES. Every entry in the generated codeOrchEndpoints table
// ships `Positional: []string{}`, and handleCodeOrchExecute substitutes path
// placeholders by iterating exactly that slice:
//
//	for _, p := range ep.Positional {
//	    if v, ok := params[p]; ok && strings.Contains(path, "{"+p+"}") { ... }
//	}
//
// With an empty slice the loop body never runs, so NO templated path is ever
// substituted. `llamaswap_execute` against upstream.props sent
// /upstream/%7Bmodel%7D/props?model=embeddinggemma — the placeholder escaped
// into the URL and the model name leaked into the query string. Every
// {model}-templated endpoint (the entire /upstream passthrough, models unload,
// and the rest) was unreachable through the execute tool.
//
// WHY A BACKFILL RATHER THAN EDITING THE TABLE. codeOrchEndpoints is generated
// and rewritten wholesale by a reprint, so 40-odd hand-edited literals would
// be silently reverted the next time the CLI is printed. Deriving the
// positionals FROM THE PATH at init time cannot go stale: a reprint that adds
// a new templated endpoint gets its placeholders backfilled too, and a reprint
// that starts populating Positional itself is respected untouched.
//
// The same defect and the same shape of fix were found independently in the
// comfyui twin of this generator.

package mcp

import "regexp"

func init() { backfillCodeOrchPositionals() }

// codeOrchPathParamRE matches an OpenAPI-style `{name}` path placeholder.
var codeOrchPathParamRE = regexp.MustCompile(`\{([A-Za-z0-9_.\-]+)\}`)

// backfillCodeOrchPositionals fills Positional from the path template for any
// endpoint that declares none.
//
// An endpoint whose Positional is already populated is left ALONE: the
// generator is the authority when it says something, and this only speaks
// when it says nothing.
func backfillCodeOrchPositionals() {
	for i := range codeOrchEndpoints {
		ep := &codeOrchEndpoints[i]
		if len(ep.Positional) > 0 {
			continue
		}
		matches := codeOrchPathParamRE.FindAllStringSubmatch(ep.Path, -1)
		if len(matches) == 0 {
			continue
		}
		seen := make(map[string]bool, len(matches))
		for _, m := range matches {
			name := m[1]
			if seen[name] {
				continue
			}
			seen[name] = true
			ep.Positional = append(ep.Positional, name)
		}
	}
}

// codeOrchPathParams returns the placeholder names in a path template. Used by
// the guard test that asserts no templated endpoint is left unsubstitutable.
func codeOrchPathParams(path string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range codeOrchPathParamRE.FindAllStringSubmatch(path, -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	return out
}
