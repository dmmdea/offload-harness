package compeval

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactorTakesABasicHeaderInCodeAndDumps: a Basic credential in a printed http.Header or a call that
// sets the header is replaced without the closing brackets and parens behind it, and as file content in
// Args the JSON stays valid.
func TestRedactorTakesABasicHeaderInCodeAndDumps(t *testing.T) {
	b64 := tailFake(401, 22, tailAlnum) + "=="
	basic := "Ba" + "sic " + b64
	for _, c := range []struct{ name, text, after string }{
		{"go header dump", "Header:map[Authorization:[" + basic + "]] end", "]] end"},
		{"js setHeader", "req.setHeader('Authorization', '" + basic + "') end", "') end"},
		{"go Header.Set", `req.Header.Set("Authorization", "` + basic + `") end`, `") end`},
		{"subscript", `headers["Authorization"] = "` + basic + `" end`, `" end`},
	} {
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
			if run, ok := anyRun(b64, out, 8); ok {
				t.Errorf("%s (args %v): %q of the value is still in %q", c.name, asArgs, run, out)
			}
			if res := r.Residual(out); len(res) != 0 {
				t.Errorf("%s (args %v): residual gate reports %v on %q", c.name, asArgs, res, out)
			}
			if asArgs {
				var back map[string]string
				if err := json.Unmarshal([]byte(out), &back); err != nil {
					t.Errorf("%s: redacted Args are not valid JSON: %v: %q", c.name, err, out)
				} else if !strings.HasSuffix(back["content"], c.after+"\ntwo") {
					t.Errorf("%s: the text after the value was eaten: %q", c.name, back["content"])
				}
			} else if !strings.HasSuffix(out, c.after) {
				t.Errorf("%s: the text after the value was eaten: %q", c.name, out)
			}
		}
	}
}
