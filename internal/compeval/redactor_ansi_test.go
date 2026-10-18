package compeval

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactorTakesACredentialAfterAControlSequence: colored output puts a color code right in front of
// the key word, and the 'm' that ends the code is a word character, so no word boundary was there. The
// value is redacted, the text after it stays (nothing after the value is eaten), and the residual gate
// is quiet. As Args or JSON output the escape byte is \u001b and the text stays valid JSON.
func TestRedactorTakesACredentialAfterAControlSequence(t *testing.T) {
	const esc = "\x1b"
	v := tailFake(301, 20, tailAlnum)
	bearer := tailFake(302, 24, tailAlnum)
	for _, c := range []struct{ name, line, secret string }{
		{"password", esc + "[32mpass" + "word=" + v + esc + "[0m done", v},
		{"colon", esc + "[1;34mpass" + "word: " + v + esc + "[0m done", v},
		{"name then reset", esc + "[36mDB_PASS" + "WORD " + esc + "[0m: " + v + " done", v},
		{"bearer", esc + "[33mBear" + "er " + bearer + esc + "[0m done", bearer},
	} {
		for _, asJSON := range []bool{false, true} {
			in := c.line
			if asJSON {
				b, err := json.Marshal(map[string]string{"content": "one\n" + c.line + "\ntwo"})
				if err != nil {
					t.Fatal(err)
				}
				in = string(b)
			}
			r := NewRedactor()
			out := r.Redact(in)
			if run, ok := anyRun(c.secret, out, 8); ok {
				t.Errorf("%s (json %v): %q of the value is still in %q", c.name, asJSON, run, out)
			}
			if res := r.Residual(out); len(res) != 0 {
				t.Errorf("%s (json %v): residual gate reports %v on %q", c.name, asJSON, res, out)
			}
			if asJSON {
				var back map[string]string
				if err := json.Unmarshal([]byte(out), &back); err != nil {
					t.Errorf("%s: redacted output is not valid JSON: %v: %q", c.name, err, out)
				} else if !strings.HasSuffix(back["content"], " done\ntwo") {
					t.Errorf("%s: the text after the value was eaten: %q", c.name, back["content"])
				}
			} else if !strings.HasSuffix(out, " done") {
				t.Errorf("%s: the text after the value was eaten: %q", c.name, out)
			}
		}
	}
}

// TestRedactorKeepsAResetCodeBehindTheValue: the reset code that follows a value is not part of it.
func TestRedactorKeepsAResetCodeBehindTheValue(t *testing.T) {
	const esc = "\x1b"
	v := tailFake(303, 20, tailAlnum)
	out := NewRedactor().Redact(esc + "[32mpass" + "word=" + v + esc + "[0m")
	if !strings.HasSuffix(out, esc+"[0m") {
		t.Errorf("the reset code behind the value was eaten: %q", out)
	}
}

// TestRedactorTakesAJavaSystemProperty: -Dpassword=<value> has a letter in front of the key word.
func TestRedactorTakesAJavaSystemProperty(t *testing.T) {
	v := tailFake(311, 20, tailAlnum)
	in := "java -Dpass" + "word=" + v + " -Dmode=fast -jar app.jar"
	r := NewRedactor()
	out := r.Redact(in)
	if run, ok := anyRun(v, out, 8); ok {
		t.Errorf("%q of the value is still in %q", run, out)
	}
	if !strings.HasSuffix(out, " -Dmode=fast -jar app.jar") {
		t.Errorf("the text after the value was eaten: %q", out)
	}
	if res := r.Residual(out); len(res) != 0 {
		t.Errorf("residual gate reports %v on %q", res, out)
	}
}
