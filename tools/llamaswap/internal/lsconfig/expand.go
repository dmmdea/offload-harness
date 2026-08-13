// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package lsconfig

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// macroToken matches ${name}, ${env.VAR}, ${PORT}. The character class is
// deliberately narrow (llama-swap's own schema restricts macro names to
// [a-zA-Z0-9_-]); the extra '.' admits the env. prefix.
var macroToken = regexp.MustCompile(`\$\{([A-Za-z0-9_.\-]+)\}`)

// ReservedMacros are substituted by llama-swap itself at spawn time, not by
// the config layer. They stay symbolic through expansion so an expanded cmd
// can still be compared against a live cmd by normalizing the runtime value.
var ReservedMacros = map[string]bool{"PID": true, "PORT": true, "MODEL_ID": true}

// EnvMacroPrefix is the prefix that routes a macro token to the environment.
const EnvMacroPrefix = "env."

// Expansion records one substitution so `config explain` can show which macro
// became what, and lint can report unset/undefined names with a source.
type Expansion struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	// Kind is macro | env | env-unset | reserved | undefined.
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
}

// Expander resolves ${...} tokens against a macro map and an environment
// lookup. Resolution is recursive with a name stack, so a macro whose value
// references another macro expands fully in one call and a cycle is reported
// rather than looping (or silently truncating at a pass limit).
type Expander struct {
	macros    map[string]string
	lookupEnv func(string) (string, bool)
	// maxDepth bounds pathological nesting that is not a strict cycle.
	maxDepth int
}

// NewExpander builds an expander. A nil lookupEnv uses the process
// environment.
func NewExpander(macros map[string]string, lookupEnv func(string) (string, bool)) *Expander {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if macros == nil {
		macros = map[string]string{}
	}
	return &Expander{macros: macros, lookupEnv: lookupEnv, maxDepth: 32}
}

// Expand substitutes every ${...} token in s. It returns the expanded string,
// the ordered substitution trace, and any resolution errors (cycles, depth
// overflow). Undefined macros and unset env vars are NOT errors: the token is
// left symbolic and recorded in the trace with kind "undefined"/"env-unset",
// so a caller can report them at the severity it thinks is right instead of
// having the parse fail.
//
// Order is env-then-macros within each token: an ${env.X} token resolves
// against the environment first and never falls through to the macro map, and
// a macro whose value contains ${env.Y} has that resolved during its own
// recursive expansion.
func (e *Expander) Expand(s string) (string, []Expansion, []string) {
	if s == "" {
		return "", nil, nil
	}
	var subs []Expansion
	var errs []string
	seen := map[string]bool{}
	out := e.expand(s, nil, &subs, &errs, seen)
	return out, subs, errs
}

func (e *Expander) expand(s string, stack []string, subs *[]Expansion, errs *[]string, seen map[string]bool) string {
	if len(stack) > e.maxDepth {
		*errs = append(*errs, fmt.Sprintf("macro nesting exceeded %d levels at %s", e.maxDepth, strings.Join(stack, " -> ")))
		return s
	}
	return macroToken.ReplaceAllStringFunc(s, func(tok string) string {
		name := tok[2 : len(tok)-1]
		switch {
		case ReservedMacros[name]:
			record(subs, seen, Expansion{Token: tok, Name: name, Kind: "reserved"})
			return tok
		case strings.HasPrefix(name, EnvMacroPrefix):
			v, ok := e.lookupEnv(strings.TrimPrefix(name, EnvMacroPrefix))
			if !ok {
				record(subs, seen, Expansion{Token: tok, Name: name, Kind: "env-unset"})
				return tok
			}
			record(subs, seen, Expansion{Token: tok, Name: name, Kind: "env", Value: v})
			return v
		}
		raw, ok := e.macros[name]
		if !ok {
			record(subs, seen, Expansion{Token: tok, Name: name, Kind: "undefined"})
			return tok
		}
		for _, prior := range stack {
			if prior == name {
				*errs = append(*errs, fmt.Sprintf("macro cycle: %s -> %s", strings.Join(stack, " -> "), name))
				return tok
			}
		}
		val := e.expand(raw, append(append([]string{}, stack...), name), subs, errs, seen)
		record(subs, seen, Expansion{Token: tok, Name: name, Kind: "macro", Value: val})
		return val
	})
}

func record(subs *[]Expansion, seen map[string]bool, ex Expansion) {
	key := ex.Kind + "\x00" + ex.Name + "\x00" + ex.Value
	if seen[key] {
		return
	}
	seen[key] = true
	*subs = append(*subs, ex)
}

// MacroCycles walks every declared macro looking for self-reference. Called by
// lint so a cycle is reported even when no model cmd references the cycling
// macro (an unused cycle is still a landmine for the next edit).
func (e *Expander) MacroCycles() []string {
	var out []string
	names := make([]string, 0, len(e.macros))
	for n := range e.macros {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if path, ok := e.findCycle(n, nil, map[string]bool{}); ok {
			out = append(out, strings.Join(path, " -> "))
		}
	}
	return out
}

func (e *Expander) findCycle(name string, stack []string, visited map[string]bool) ([]string, bool) {
	for _, prior := range stack {
		if prior == name {
			return append(append([]string{}, stack...), name), true
		}
	}
	if visited[name] {
		return nil, false
	}
	visited[name] = true
	raw, ok := e.macros[name]
	if !ok {
		return nil, false
	}
	next := append(append([]string{}, stack...), name)
	for _, m := range macroToken.FindAllStringSubmatch(raw, -1) {
		child := m[1]
		if ReservedMacros[child] || strings.HasPrefix(child, EnvMacroPrefix) {
			continue
		}
		if path, found := e.findCycle(child, next, visited); found {
			return path, true
		}
	}
	return nil, false
}

// ReferencedMacros returns every ${...} name appearing in s, in source order.
func ReferencedMacros(s string) []string {
	var out []string
	for _, m := range macroToken.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}
