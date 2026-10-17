package compeval

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactorTakesACookieInCodeAndDumps: a cookie credential in a JSON list, a call that passes the header
// name and the value as two arguments, a subscript or a printed http.Header is replaced without the text
// around it, and as file content in Args the JSON stays valid.
func TestRedactorTakesACookieInCodeAndDumps(t *testing.T) {
	v := tailFake(721, 32, tailAlnum)
	const (
		ck  = "Coo" + "kie"
		sck = "Set-Coo" + "kie"
	)
	for _, c := range []struct{ name, text, after string }{
		{"json set-cookie list", `{"` + sck + `":["session=` + v + `; Path=/"]} end`, `"]} end`},
		{"json cookie list", `"` + ck + `": [ "sid=` + v + `" ] end`, `" ] end`},
		{"go Header.Set", `req.Header.Set("` + ck + `", "sid=` + v + `") end`, `") end`},
		{"js setRequestHeader", `xhr.setRequestHeader('` + ck + `','sid=` + v + `') end`, `') end`},
		{"subscript", `headers["` + ck + `"] = "sid=` + v + `" end`, `" end`},
		{"hash rocket", `'` + ck + `' => 'sid=` + v + `' end`, `' end`},
	} {
		for _, asArgs := range []bool{false, true} {
			in := c.text
			if asArgs {
				in = writeFileArgs(t, "one\n"+c.text+"\ntwo")
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
				} else if !strings.HasSuffix(back["content"], c.after+"\ntwo") {
					t.Errorf("%s: the text after the value was eaten: %q", c.name, back["content"])
				}
			} else if !strings.HasSuffix(out, c.after) {
				t.Errorf("%s: the text after the value was eaten: %q", c.name, out)
			}
		}
	}
}
