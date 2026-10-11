// Package gbnftest is a small reference interpreter for the GBNF subset that
// internal/gbnf emits: it answers "does this grammar admit exactly this text?",
// so a test can check what a grammar-constrained seat could have written without
// a llama.cpp build in the loop.
//
// It understands one rule per line (`name ::= expr`), alternation `|` (an empty
// alternative is allowed, as in `ws ::= | " "`), sequences, string literals with
// the escapes \" \\ \n \t \r \xHH, character classes with ranges and `^`
// negation, parenthesised groups, rule references, and the repetitions `*` `+` `?`
// `{m}` `{m,}` `{m,n}`. It matches by tracking every position a prefix can reach,
// so ambiguity costs nothing and nothing backtracks.
//
// It is a test oracle, not llama.cpp. It also holds a grammar to the two rules of
// llama.cpp's parser that a generator can break without knowing: a rule name is
// made of letters, digits and dashes only (an underscore, as in a JSON property
// name, is not a word character there and makes the whole grammar unparseable),
// and every referenced rule must be defined. The grammar was not compiled by a
// live llama.cpp when this interpreter was written; a match here is evidence about
// the shape the grammar admits, not about the engine accepting the text of it.
package gbnftest

import (
	"fmt"
	"strconv"
	"strings"
)

// Accepts reports whether the grammar's `root` rule admits the whole of input.
// An error means the grammar itself is malformed for this interpreter (a syntax
// error, a rule name llama.cpp's parser would refuse, an undefined rule, a missing
// `root`), which is a different answer from "the text is not admitted".
func Accepts(grammar, input string) (bool, error) {
	g, err := parse(grammar)
	if err != nil {
		return false, err
	}
	in := []rune(input)
	reached := g.match(ref{"root"}, in, set{0: {}})
	_, ok := reached[len(in)]
	return ok, nil
}

// Check parses the grammar and reports the first thing wrong with it, without
// matching anything.
func Check(grammar string) error {
	_, err := parse(grammar)
	return err
}

type set map[int]struct{}

type node interface{}

type (
	lit   struct{ r []rune }
	class struct {
		neg    bool
		ranges [][2]rune
	}
	ref struct{ name string }
	seq struct{ items []node }
	alt struct{ opts []node }
	rep struct {
		n        node
		min, max int // max < 0: unbounded
	}
)

type grammar struct{ rules map[string]node }

func parse(text string) (*grammar, error) {
	g := &grammar{rules: map[string]node{}}
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, body, ok := strings.Cut(line, "::=")
		if !ok {
			return nil, fmt.Errorf("line %d: no `::=` in %q", i+1, line)
		}
		name = strings.TrimSpace(name)
		if !validName(name) {
			return nil, fmt.Errorf("line %d: rule name %q is not letters, digits and dashes only", i+1, name)
		}
		if _, dup := g.rules[name]; dup {
			return nil, fmt.Errorf("line %d: rule %q is defined twice", i+1, name)
		}
		p := &parser{s: []rune(body)}
		n, err := p.alt()
		if err != nil {
			return nil, fmt.Errorf("line %d (rule %s): %w", i+1, name, err)
		}
		p.skip()
		if p.i < len(p.s) {
			return nil, fmt.Errorf("line %d (rule %s): unexpected %q", i+1, name, string(p.s[p.i]))
		}
		g.rules[name] = n
	}
	if _, ok := g.rules["root"]; !ok {
		return nil, fmt.Errorf("no root rule")
	}
	for name, n := range g.rules {
		if err := g.refsDefined(n); err != nil {
			return nil, fmt.Errorf("rule %s: %w", name, err)
		}
	}
	return g, nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

func (g *grammar) refsDefined(n node) error {
	switch v := n.(type) {
	case ref:
		if _, ok := g.rules[v.name]; !ok {
			return fmt.Errorf("references undefined rule %q", v.name)
		}
	case seq:
		for _, c := range v.items {
			if err := g.refsDefined(c); err != nil {
				return err
			}
		}
	case alt:
		for _, c := range v.opts {
			if err := g.refsDefined(c); err != nil {
				return err
			}
		}
	case rep:
		return g.refsDefined(v.n)
	}
	return nil
}

type parser struct {
	s []rune
	i int
}

func (p *parser) skip() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *parser) alt() (node, error) {
	var opts []node
	for {
		s, err := p.seq()
		if err != nil {
			return nil, err
		}
		opts = append(opts, s)
		p.skip()
		if p.i < len(p.s) && p.s[p.i] == '|' {
			p.i++
			continue
		}
		break
	}
	if len(opts) == 1 {
		return opts[0], nil
	}
	return alt{opts}, nil
}

func (p *parser) seq() (node, error) {
	var items []node
	for {
		p.skip()
		if p.i >= len(p.s) || p.s[p.i] == '|' || p.s[p.i] == ')' {
			break
		}
		a, err := p.atom()
		if err != nil {
			return nil, err
		}
		a, err = p.postfix(a)
		if err != nil {
			return nil, err
		}
		items = append(items, a)
	}
	return seq{items}, nil
}

func (p *parser) atom() (node, error) {
	c := p.s[p.i]
	switch {
	case c == '"':
		return p.literal()
	case c == '[':
		return p.class()
	case c == '(':
		p.i++
		n, err := p.alt()
		if err != nil {
			return nil, err
		}
		p.skip()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return nil, fmt.Errorf("unclosed (")
		}
		p.i++
		return n, nil
	}
	start := p.i
	for p.i < len(p.s) && (p.s[p.i] == '-' || p.s[p.i] == '_' || (p.s[p.i] >= '0' && p.s[p.i] <= '9') || (p.s[p.i] >= 'a' && p.s[p.i] <= 'z') || (p.s[p.i] >= 'A' && p.s[p.i] <= 'Z')) {
		p.i++
	}
	if p.i == start {
		return nil, fmt.Errorf("unexpected %q", string(c))
	}
	name := string(p.s[start:p.i])
	if !validName(name) {
		return nil, fmt.Errorf("rule name %q is not letters, digits and dashes only", name)
	}
	return ref{name}, nil
}

func (p *parser) escape() (rune, error) {
	// p.s[p.i] is the char after the backslash.
	if p.i >= len(p.s) {
		return 0, fmt.Errorf("dangling backslash")
	}
	c := p.s[p.i]
	p.i++
	switch c {
	case 'n':
		return '\n', nil
	case 't':
		return '\t', nil
	case 'r':
		return '\r', nil
	case '"', '\\', '[', ']', '-', '^', '/':
		return c, nil
	case 'x':
		if p.i+2 > len(p.s) {
			return 0, fmt.Errorf("short \\x escape")
		}
		v, err := strconv.ParseUint(string(p.s[p.i:p.i+2]), 16, 8)
		if err != nil {
			return 0, fmt.Errorf("bad \\x escape: %w", err)
		}
		p.i += 2
		return rune(v), nil
	}
	return 0, fmt.Errorf("unknown escape \\%c", c)
}

func (p *parser) literal() (node, error) {
	p.i++ // opening quote
	var out []rune
	for p.i < len(p.s) {
		c := p.s[p.i]
		p.i++
		switch c {
		case '"':
			return lit{out}, nil
		case '\\':
			r, err := p.escape()
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		default:
			out = append(out, c)
		}
	}
	return nil, fmt.Errorf("unterminated string literal")
}

func (p *parser) class() (node, error) {
	p.i++ // [
	c := class{}
	if p.i < len(p.s) && p.s[p.i] == '^' {
		c.neg = true
		p.i++
	}
	one := func() (rune, error) {
		if p.i >= len(p.s) {
			return 0, fmt.Errorf("unterminated character class")
		}
		r := p.s[p.i]
		p.i++
		if r == '\\' {
			return p.escape()
		}
		return r, nil
	}
	for {
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("unterminated character class")
		}
		if p.s[p.i] == ']' {
			p.i++
			return c, nil
		}
		lo, err := one()
		if err != nil {
			return nil, err
		}
		hi := lo
		if p.i+1 < len(p.s) && p.s[p.i] == '-' && p.s[p.i+1] != ']' {
			p.i++
			if hi, err = one(); err != nil {
				return nil, err
			}
		}
		c.ranges = append(c.ranges, [2]rune{lo, hi})
	}
}

func (p *parser) postfix(a node) (node, error) {
	if p.i >= len(p.s) {
		return a, nil
	}
	switch p.s[p.i] {
	case '*':
		p.i++
		return rep{a, 0, -1}, nil
	case '+':
		p.i++
		return rep{a, 1, -1}, nil
	case '?':
		p.i++
		return rep{a, 0, 1}, nil
	case '{':
		end := p.i
		for end < len(p.s) && p.s[end] != '}' {
			end++
		}
		if end >= len(p.s) {
			return nil, fmt.Errorf("unclosed {")
		}
		body := string(p.s[p.i+1 : end])
		p.i = end + 1
		lo, hi, hasComma := strings.Cut(body, ",")
		min, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("bad repetition {%s}", body)
		}
		max := min
		if hasComma {
			if strings.TrimSpace(hi) == "" {
				max = -1
			} else if max, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("bad repetition {%s}", body)
			}
		}
		return rep{a, min, max}, nil
	}
	return a, nil
}

// match returns every position reachable by matching n from any position in from.
func (g *grammar) match(n node, in []rune, from set) set {
	out := set{}
	switch v := n.(type) {
	case lit:
		for p := range from {
			if p+len(v.r) > len(in) {
				continue
			}
			ok := true
			for k, r := range v.r {
				if in[p+k] != r {
					ok = false
					break
				}
			}
			if ok {
				out[p+len(v.r)] = struct{}{}
			}
		}
	case class:
		for p := range from {
			if p >= len(in) {
				continue
			}
			hit := false
			for _, rg := range v.ranges {
				if in[p] >= rg[0] && in[p] <= rg[1] {
					hit = true
					break
				}
			}
			if hit != v.neg {
				out[p+1] = struct{}{}
			}
		}
	case ref:
		return g.match(g.rules[v.name], in, from)
	case seq:
		cur := from
		for _, it := range v.items {
			cur = g.match(it, in, cur)
			if len(cur) == 0 {
				return cur
			}
		}
		return cur
	case alt:
		for _, o := range v.opts {
			for p := range g.match(o, in, from) {
				out[p] = struct{}{}
			}
		}
	case rep:
		cur := from
		for i := 0; ; i++ {
			if i >= v.min {
				added := false
				for p := range cur {
					if _, seen := out[p]; !seen {
						out[p] = struct{}{}
						added = true
					}
				}
				if !added && v.max < 0 {
					break
				}
			}
			if v.max >= 0 && i >= v.max {
				break
			}
			cur = g.match(v.n, in, cur)
			if len(cur) == 0 {
				break
			}
		}
	}
	return out
}
