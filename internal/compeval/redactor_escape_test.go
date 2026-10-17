package compeval

import (
	"encoding/json"
	"strings"
	"testing"
)

// escapeCredentials are credentials that start a line of a file written by a tool call. Built from
// pieces at run time so no literal in the tree is itself credential-shaped.
func escapeCredentials() []struct{ name, line, value string } {
	v := tailFake(81, 15, tailAlnum)
	return []struct{ name, line, value string }{
		{"password", "PASS" + "WORD=" + v, v},
		{"token", "TO" + "KEN=" + v, v},
		{"api key", "api_" + "key=" + v, v},
		{"yaml password", "pass" + "word: " + v, v},
		{"underscore token", "GITHUB_" + "TOKEN=" + v, v},
		{"bearer", "Bear" + "er " + tailFake(82, 24, tailAlnum), tailFake(82, 24, tailAlnum)},
		{"aws", "AK" + "IA" + strings.ToUpper(tailFake(83, 16, "abcdefghijklmnopqrstuvwxyz0123456789")), ""},
		{"github", "gh" + "p_" + tailFake(84, 26, tailAlnum), ""},
	}
}

// writeFileArgs is the shape of a write_file tool call's Args: json.Marshal of the call's arguments,
// so a newline in the content is a backslash and an n.
func writeFileArgs(t *testing.T, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"path": ".env", "content": content})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRedactorCatchesACredentialAfterAJSONEscape: a secret at the start of a line inside JSON-escaped
// Args was neither redacted nor caught by the residual gate. After redaction the Args are still valid
// JSON, the lines around the secret are intact, and nothing is left of it.
func TestRedactorCatchesACredentialAfterAJSONEscape(t *testing.T) {
	for _, c := range escapeCredentials() {
		secret := c.value
		if secret == "" {
			secret = c.line
		}
		args := writeFileArgs(t, "APP=1\n"+c.line+"\nOTHER=2\n")
		r := NewRedactor()
		out := r.Redact(args)
		if run, ok := anyRun(secret, out, 8); ok {
			t.Errorf("%s: %q of the value is still in %q", c.name, run, out)
		}
		if res := r.Residual(out); len(res) != 0 {
			t.Errorf("%s: residual gate reports %v on %q", c.name, res, out)
		}
		var back map[string]string
		if err := json.Unmarshal([]byte(out), &back); err != nil {
			t.Errorf("%s: redacted Args are not valid JSON: %v: %q", c.name, err, out)
			continue
		}
		lines := strings.Split(back["content"], "\n")
		if len(lines) != 4 || lines[0] != "APP=1" || lines[2] != "OTHER=2" {
			t.Errorf("%s: the lines around the secret changed: %q", c.name, back["content"])
		}
	}
}

// TestRedactorKeepsTheEscapeBeforeAnEmail: the address after a JSON escape is replaced and the escape
// stays, so the Args are still valid JSON (the 'n' of the escape is not part of the address).
func TestRedactorKeepsTheEscapeBeforeAnEmail(t *testing.T) {
	args := writeFileArgs(t, "owner\nbob@example.com\nrest\n")
	out := NewRedactor().Redact(args)
	var back map[string]string
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("redacted Args are not valid JSON: %v: %q", err, out)
	}
	if back["content"] != "owner\n[redacted-email-1]\nrest\n" {
		t.Fatalf("content = %q", back["content"])
	}
}

// TestHarvestRedactsACredentialAfterAJSONEscape: end to end, a trace whose write_file Args hold a
// secret at the start of a line is harvested with the secret redacted, and the corpus it writes
// holds no part of it.
func TestHarvestRedactsACredentialAfterAJSONEscape(t *testing.T) {
	v := tailFake(85, 15, tailAlnum)
	dir := t.TempDir()
	tr := sampleTrace("a", "clean payload")
	tr.Transcript[2].ToolCalls[0].Name = "write_file"
	tr.Transcript[2].ToolCalls[0].Args = writeFileArgs(t, "APP=1\nPASS"+"WORD="+v+"\nOTHER=2\n")
	writeTraceFile(t, dir, "a.json", tr)
	entries, stats, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(stats.Skipped) != 0 {
		t.Fatalf("entries %d, skipped %+v", len(entries), stats.Skipped)
	}
	args := entries[0].Turns[2].ToolCalls[0].Args
	if strings.Contains(args, v) || !json.Valid([]byte(args)) {
		t.Fatalf("Args hold the secret or are not valid JSON: %q", args)
	}
	out := t.TempDir() + "/corpus.jsonl"
	if _, err := WriteCorpus(out, entries); err != nil {
		t.Fatal(err)
	}
}

// TestRedactorReplacesAHeaderCredentialAndKeepsTheRest: a Basic authorization header, a registry
// auth value and a cookie header are replaced whole; the quotes and text around them stay, and Args
// that were valid JSON still are.
func TestRedactorReplacesAHeaderCredentialAndKeepsTheRest(t *testing.T) {
	b64 := tailFake(111, 22, tailAlnum) + "=="
	sid := tailFake(112, 32, tailAlnum)
	for _, c := range []struct{ name, text, secret string }{
		{"basic header", `curl -H "Authorization: Ba` + `sic ` + b64 + `" https://host.invalid/x`, b64},
		{"proxy header", "Proxy-Authorization: Ba" + "sic " + b64 + " end", b64},
		{"registry auth", `{"auths":{"r.invalid":{"au` + `th":"` + b64 + `"}}}`, b64},
		{"cookie", `curl -H "Coo` + `kie: theme=dark; session=` + sid + `" https://host.invalid/x`, sid},
		{"set cookie", "Set-Coo" + "kie: sessionid=" + sid + "; HttpOnly", sid},
	} {
		for _, asArgs := range []bool{false, true} {
			in := c.text
			if asArgs {
				in = writeFileArgs(t, "line one\n"+c.text+"\nline three\n")
			}
			r := NewRedactor()
			out := r.Redact(in)
			if run, ok := anyRun(c.secret, out, 8); ok {
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
			} else if c.name == "basic header" && !strings.HasSuffix(out, `" https://host.invalid/x`) {
				t.Errorf("%s: the text after the value was eaten: %q", c.name, out)
			}
		}
	}
}
