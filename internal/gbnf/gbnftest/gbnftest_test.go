package gbnftest

import (
	"strings"
	"testing"
)

// The oracle has to say no as well as yes, or every test built on it passes
// vacuously. Each row is checked in both directions against hand-written grammars.
func TestAcceptsAndRejects(t *testing.T) {
	const g = `root ::= "{" ws "\"a\"" ws ":" ws ( num | str ) ws "}"
ws ::= | " " | "\n" [ \t]{0,2}
num ::= "-"? [0-9]+ ("." [0-9]+)?
str ::= "\"" ( [^"\\] | "\\" ["\\/bfnrt] )* "\""
`
	cases := []struct {
		in   string
		want bool
	}{
		{`{"a":1}`, true},
		{`{"a":-12.50}`, true},
		{`{"a":"x\"y"}`, true},
		{`{ "a" : 1 }`, true},
		{"{\n  \"a\":1}", true},
		{"{\n   \"a\":1}", false}, // three indent chars: past {0,2}
		{`{"a":}`, false},
		{`{"a":1`, false},
		{`{"a":1}x`, false},
		{`{"b":1}`, false},
		{`{"a":"unterminated}`, false},
		{`{"a":1.}`, false},
		{``, false},
	}
	for _, c := range cases {
		got, err := Accepts(g, c.in)
		if err != nil {
			t.Fatalf("Accepts(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("Accepts(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRepetitionBounds(t *testing.T) {
	cases := []struct {
		rule string
		in   string
		want bool
	}{
		{`"a"{2}`, "aa", true},
		{`"a"{2}`, "a", false},
		{`"a"{2}`, "aaa", false},
		{`"a"{1,3}`, "aaa", true},
		{`"a"{1,3}`, "aaaa", false},
		{`"a"{1,3}`, "", false},
		{`"a"{2,}`, "aaaaa", true},
		{`"a"{2,}`, "a", false},
		{`"a"*`, "", true},
		{`"a"+`, "", false},
		{`"a"?`, "aa", false},
		{`("ab" | "a")+ "c"`, "aabc", true}, // "a" then "ab": either alternative can start
		{`[a-c]+ [^a-c]`, "abcd", true},
		{`[a-c]+ [^a-c]`, "abc", false},
		{`[-+] [0-9a-f]`, "-f", true},
		{`[-+] [0-9a-f]`, "*f", false},
	}
	for _, c := range cases {
		got, err := Accepts("root ::= "+c.rule+"\n", c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.rule, err)
		}
		if got != c.want {
			t.Errorf("root ::= %s on %q = %v, want %v", c.rule, c.in, got, c.want)
		}
	}
}

// A recursive rule (a JSON value inside a JSON value) must terminate and match.
func TestRecursiveRule(t *testing.T) {
	const g = `root ::= v
v ::= "[" ( v ("," v)* )? "]" | "x"
`
	for in, want := range map[string]bool{`x`: true, `[]`: true, `[x,[x,[]]]`: true, `[x,]`: false, `[[x]`: false} {
		got, err := Accepts(g, in)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Accepts(%q) = %v, want %v", in, got, want)
		}
	}
}

// What llama.cpp's parser refuses is an error here, not a quiet "no".
func TestMalformedGrammarsAreErrors(t *testing.T) {
	cases := []struct {
		name, grammar, want string
	}{
		{"underscore in a rule name", "root ::= key_facts\nkey_facts ::= \"a\"\n", "letters, digits and dashes"},
		{"underscore in a defined name", "root ::= \"a\"\nkey_facts ::= \"b\"\n", "letters, digits and dashes"},
		{"undefined rule", "root ::= other\n", "undefined rule"},
		{"no root", "main ::= \"a\"\n", "no root"},
		{"duplicate rule", "root ::= \"a\"\nroot ::= \"b\"\n", "defined twice"},
		{"unterminated literal", "root ::= \"a\n", "unterminated"},
		{"unclosed group", "root ::= (\"a\"\n", "unclosed"},
		{"unclosed class", "root ::= [ab\n", "unterminated"},
		{"no arrow", "root = \"a\"\n", "::="},
	}
	for _, c := range cases {
		if _, err := Accepts(c.grammar, "a"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
	if err := Check("root ::= \"a\"\n"); err != nil {
		t.Errorf("Check of a good grammar: %v", err)
	}
}
