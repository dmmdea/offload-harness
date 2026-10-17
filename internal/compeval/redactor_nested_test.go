package compeval

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactorTakesAValueInANestedEscapedQuote: a credential inside a double-quoted shell string that
// holds an escaped quote (bash -c "export KEY=\"value\"") was neither redacted nor seen by the residual
// gate, because as JSON text the quote in front of the value carries three backslashes. After
// redaction the Args are valid JSON, the command around the value is intact and nothing of the value
// is left.
func TestRedactorTakesAValueInANestedEscapedQuote(t *testing.T) {
	v := tailFake(201, 22, tailAlnum)
	for _, c := range []struct{ name, command, after string }{
		{"bash -c", `bash -c "export API_` + `KEY=\"` + v + `\" && ./run"`, ` && ./run"`},
		{"ssh", `ssh host "export TO` + `KEN=\"` + v + `\" && ./run"`, ` && ./run"`},
	} {
		b, err := json.Marshal(map[string]string{"command": c.command})
		if err != nil {
			t.Fatal(err)
		}
		r := NewRedactor()
		out := r.Redact(string(b))
		if run, ok := anyRun(v, out, 8); ok {
			t.Errorf("%s: %q of the value is still in %s", c.name, run, out)
		}
		if res := r.Residual(out); len(res) != 0 {
			t.Errorf("%s: the residual gate reports %v on %s", c.name, res, out)
		}
		var back map[string]string
		if err := json.Unmarshal([]byte(out), &back); err != nil {
			t.Errorf("%s: the redacted Args are not valid JSON: %v: %s", c.name, err, out)
			continue
		}
		if !strings.HasSuffix(back["command"], c.after) {
			t.Errorf("%s: the text after the value was eaten: %q", c.name, back["command"])
		}
	}
}
