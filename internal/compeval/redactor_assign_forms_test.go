package compeval

import (
	"encoding/json"
	"strings"
	"testing"
)

// quotedLiteralLines are a quoted credential literal in code and config shapes (see the piishape test of
// the same name). The literal is the last thing in each line before its closers.
func quotedLiteralLines(v string) []struct{ name, text string } {
	return []struct{ name, text string }{
		{"go short declaration", `password := "` + v + `"`},
		{"typescript typed", `const token: string = "` + v + `"`},
		{"python bytes", `password = b"` + v + `"`},
		{"comparison", `if password == "` + v + `" {`},
		{"environ subscript", `os.environ["API_TOKEN"] = "` + v + `"`},
		{"setenv call", `t.Setenv("API_TOKEN", "` + v + `")`},
		{"hash rocket", `{'password' => '` + v + `'}`},
		{"quoted json key", `{"token": "` + v + `"}`},
	}
}

// TestRedactorTakesAQuotedLiteralInCodeShapes: the literal is replaced, what follows it stays (closing
// quote, paren, brace), the residual gate is quiet, and Args that held the line as file content are
// still valid JSON.
func TestRedactorTakesAQuotedLiteralInCodeShapes(t *testing.T) {
	v := tailFake(501, 22, tailAlnum)
	for _, c := range quotedLiteralLines(v) {
		after := c.text[strings.Index(c.text, v)+len(v):]
		for _, asArgs := range []bool{false, true} {
			in := c.text
			if asArgs {
				b, err := json.Marshal(map[string]string{"path": "x.go", "content": "one\n" + c.text + "\ntwo"})
				if err != nil {
					t.Fatal(err)
				}
				in = string(b)
			}
			r := NewRedactor()
			out := r.Redact(in)
			if run, ok := anyRun(v, out, 8); ok {
				t.Errorf("%s (args %v): %q of the value is still in %q", c.name, asArgs, run, out)
			}
			if res := r.Residual(out); len(res) != 0 {
				t.Errorf("%s (args %v): residual gate reports %v on %q", c.name, asArgs, res, out)
			}
			if asArgs {
				var back map[string]string
				if err := json.Unmarshal([]byte(out), &back); err != nil {
					t.Errorf("%s: redacted Args are not valid JSON: %v: %q", c.name, err, out)
				} else if !strings.HasSuffix(back["content"], after+"\ntwo") {
					t.Errorf("%s: the text after the value was eaten: %q", c.name, back["content"])
				}
			} else if !strings.HasSuffix(out, after) {
				t.Errorf("%s: the text after the value was eaten: %q", c.name, out)
			}
		}
	}
}

// TestRedactorRemembersOnlyTheValueOfACodeShape: the gate that looks for a surviving piece of a replaced
// value remembers the value, not the type annotation or the call in front of it. Ordinary code that
// shares a long type name with the redacted line is not a leftover.
func TestRedactorRemembersOnlyTheValueOfACodeShape(t *testing.T) {
	v := tailFake(502, 22, tailAlnum)
	r := NewRedactor()
	out := r.Redact(`const token: Record<string, string> = "` + v + `"`)
	if run, ok := anyRun(v, out, 8); ok {
		t.Fatalf("%q of the value is still in %q", run, out)
	}
	for _, clean := range []string{
		`const headers: Record<string, string> = {}`,
		`os.environ["API_TOKEN"] = value`,
		`t.Setenv("API_TOKEN", "x")`,
	} {
		if res := r.Residual(clean); len(res) != 0 {
			t.Errorf("residual gate reports %v on the clean line %q", res, clean)
		}
	}
	out = r.Redact(`t.Setenv("API_TOKEN", "` + v + `")`)
	if run, ok := anyRun(v, out, 8); ok {
		t.Fatalf("%q of the value is still in %q", run, out)
	}
	if res := r.Residual(`t.Setenv("API_TOKEN", "x")`); len(res) != 0 {
		t.Errorf("residual gate reports %v on a clean Setenv call", res)
	}
}
