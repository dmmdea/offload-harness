package compeval

import (
	"encoding/json"
	"strings"
	"testing"
)

// quotedValueCases are a quoted credential literal that holds a character which ends an unquoted
// value. Each half is shorter than 12 characters. Built from pieces at run time.
func quotedValueCases() []struct{ name, text, value string } {
	a, b := tailFake(611, 8, tailAlnum), tailFake(612, 8, tailAlnum)
	return []struct{ name, text, value string }{
		{"ampersand", `pass` + `word = "` + a + `&` + b + `" end`, a + `&` + b},
		{"semicolon", `pass` + `word = "` + a + `;` + b + `" end`, a + `;` + b},
		{"comma in a json value", `{"pass` + `word": "` + a + `,` + b + `"} end`, a + `,` + b},
		{"space, single quotes", `DB_PASS` + `WORD='` + a + ` ` + b + `' end`, a + ` ` + b},
		{"space, export", `export API_` + `KEY="` + a + ` ` + b + `" end`, a + ` ` + b},
	}
}

// TestRedactorTakesAWholeQuotedLiteral: a quoted literal with a space, comma, semicolon or ampersand
// inside was not matched at all, so the value went to the corpus verbatim. It is replaced whole, the
// closing quote and the text after it stay, and Args that were valid JSON still are.
func TestRedactorTakesAWholeQuotedLiteral(t *testing.T) {
	for _, c := range quotedValueCases() {
		for _, asArgs := range []bool{false, true} {
			in := c.text
			if asArgs {
				in = writeFileArgs(t, "line one\n"+c.text+"\nline three\n")
			}
			r := NewRedactor()
			out := r.Redact(in)
			if run, ok := anyRun(c.value, out, 6); ok {
				t.Errorf("%s (args %v): %q of the value is still in %q", c.name, asArgs, run, out)
			}
			if res := r.Residual(out); len(res) != 0 {
				t.Errorf("%s (args %v): residual gate reports %v on %q", c.name, asArgs, res, out)
			}
			if asArgs {
				var back map[string]string
				if err := json.Unmarshal([]byte(out), &back); err != nil {
					t.Errorf("%s: redacted Args are not valid JSON: %v: %q", c.name, err, out)
				} else if lines := strings.Split(back["content"], "\n"); len(lines) != 4 || lines[0] != "line one" || lines[2] != "line three" {
					t.Errorf("%s: the lines around the credential changed: %q", c.name, back["content"])
				}
			}
		}
	}
}

// TestValueTailGateCoversAWholeQuotedValue: the gate remembers a replaced value so that a surviving piece of
// it is refused. It cut the value at the last space, comma, semicolon or ampersand in the match, so the
// head of a quoted value was never remembered.
func TestValueTailGateCoversAWholeQuotedValue(t *testing.T) {
	a, b := tailFake(613, 14, tailAlnum), tailFake(614, 14, tailAlnum)
	for _, sep := range []string{" ", ",", ";", "&"} {
		r := newRedactor()
		r.remember("api-key-assignment", `pass`+`word = "`+a+sep+b)
		for name, piece := range map[string]string{"head": a, "tail": b} {
			if !r.survivingTail("xx " + piece + " yy") {
				t.Errorf("separator %q: the %s of the quoted value is not remembered", sep, name)
			}
		}
	}
}
