package compeval

import (
	"encoding/json"
	"strings"
	"testing"
)

// spanBsu is a backslash and a u, written as a byte value: a source literal of the escape that Go's encoder writes
// for an ampersand is rewritten into the character by some editors and tools.
const spanBsu = "\x5c" + "u"

// Names that would make a source line credential-shaped are written in two pieces.
const (
	spanCk      = "Coo" + "kie"
	spanSetCk   = "Set-Coo" + "kie"
	spanKwToken = "tok" + "en"
	spanKwPass  = "pass" + "word"
	spanKwSecr  = "sec" + "ret"
)

func spanEnc(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func spanShell(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(s)
}

// spanWrap puts one cookie line (header name and cookies) where the corpus meets it and says whether the text is a
// JSON document around it: plain text, a tool call's Args, a curl command, Args that hold one, a header folded
// after the colon, a Set-Cookie list on one line and a pretty-printed one in Args.
func spanWrap(t *testing.T, how, name, body string, elems []string) (text string, envelope bool) {
	t.Helper()
	line := name + ": " + body
	args := func(content string) string { return writeFileArgs(t, content) }
	list := func() string {
		q := make([]string, len(elems))
		for i, e := range elems {
			q[i] = `"` + spanEnc(e) + `"`
		}
		return strings.Join(q, ",")
	}
	switch how {
	case "plain":
		return "one\n" + line + "\nthree", false
	case "args":
		return args("one\n" + line + "\nthree\n"), true
	case "curl":
		return `curl -H "` + name + ": " + spanShell(body) + `" https://host.invalid/x`, false
	case "args of curl":
		return args(`curl -H "` + name + ": " + spanShell(body) + `" https://host.invalid/x`), true
	case "fold":
		return "one\r\n" + name + ":\r\n " + body + "\r\nthree", false
	case "list":
		return `{"` + spanSetCk + `":[` + list() + `]}`, false
	case "pretty list in args":
		b, _ := json.MarshalIndent(map[string][]string{spanSetCk: elems}, "", "  ")
		return args(string(b)), true
	}
	t.Fatalf("no wrapping named %q", how)
	return "", false
}

var spanWraps = []string{"plain", "args", "curl", "args of curl", "fold", "list", "pretty list in args"}

type spanProbe struct {
	name   string
	body   string
	elems  []string
	values []string
}

func spanProbes() []spanProbe {
	v, w := tailFake(901, 32, tailAlnum), tailFake(902, 32, tailAlnum)
	a, b := tailFake(903, 16, tailAlnum), tailFake(904, 16, tailAlnum)
	h := tailFake(905, 40, "abcdef0123456789")
	flash := `$[[\"__json_message\"\0540\05425\054\"Saved\"]]`
	quoted := func(mid string) string { return `m="` + a + mid + b + `"` }
	var out []spanProbe
	add := func(name string, values []string, cookies ...string) {
		out = append(out, spanProbe{name, strings.Join(cookies, "; "), cookies, values})
	}
	add("quoted value with a space", []string{a, b, v}, quoted(" "), "sessionid="+v)
	for _, c := range []struct{ name, mid string }{
		{"apostrophe", "'"}, {"ampersand", "&"}, {"comma", ","}, {"semicolon", ";"}, {"tab", "\t"},
		{"dollar sign", "$HOME"}, {"backtick", "`id`"}, {"html characters", "<b>&"},
	} {
		add("quoted value with a "+c.name, []string{a, b, v}, quoted(c.mid), "sessionid="+v)
	}
	add("django saved", []string{"Saved ok", v}, `messages="Saved ok"`, "sessionid="+v)
	add("django apostrophe", []string{"Can't save", v}, `messages="Can't save"`, "sessionid="+v)
	add("django ampersand", []string{"Tom&Jerry", v}, `messages="Tom&Jerry"`, "sessionid="+v)
	add("list, short first element", []string{v}, "theme=dark", "session="+v+"; Path=/")
	add("list, two long elements", []string{v, w}, "session="+v, "other="+w)
	add("list, long short long", []string{v, w}, "session="+v+"; HttpOnly", "theme=dark", "other="+w)
	add("html escapes before the credential", []string{v}, "a=x"+spanBsu+"0026y", "sessionid="+v)
	add("ampersand before the credential", []string{v}, `a=x&y`, "sessionid="+v)
	add("flash with escapes before the credential", []string{h, v}, `messages="`+h+flash+`"`, "sessionid="+v)
	add("key word names", []string{v, w}, "refresh_"+spanKwToken+"="+v, "sessionid="+w)
	add("password and secret names", []string{v, w}, spanKwPass+"="+v, spanKwSecr+"="+w)
	return out
}

// TestRedactorTakesTheWholeCookieSpan: whatever a cookie value holds (a space, an apostrophe, an ampersand, a comma, a
// semicolon, a tab, a shell dollar sign or backtick, Go's HTML escapes) and however the line has been escaped, the
// harvest redactor replaces the cookie line from the header name to the end of the line, the closing quote or the
// closing bracket, so no piece of 8 or more characters of any value on it is left, the vet names no class, the
// value-tail gate is quiet, and a tool call's Args stay valid JSON with the lines around the cookies intact.
func TestRedactorTakesTheWholeCookieSpan(t *testing.T) {
	cases := 0
	for _, p := range spanProbes() {
		for _, how := range spanWraps {
			for _, name := range []string{spanCk, spanSetCk} {
				if how == "list" || how == "pretty list in args" {
					if name == spanCk {
						continue
					}
				}
				text, envelope := spanWrap(t, how, name, p.body, p.elems)
				cases++
				found := false
				for _, c := range PIIClasses(text) {
					found = found || c == "cookie-header"
				}
				if !found {
					t.Errorf("%s, %s: the vet does not name cookie-header on %q", p.name, how, text)
				}
				r := NewRedactor()
				out := r.Redact(text)
				for _, v := range p.values {
					for _, form := range []string{v, spanEnc(v), spanShell(v), spanEnc(spanShell(v))} {
						if run, ok := anyRun(form, out, 8); ok {
							t.Errorf("%s, %s: %q of the value %q is still in %q", p.name, how, run, v, out)
							break
						}
					}
				}
				if res := r.Residual(out); len(res) != 0 {
					t.Errorf("%s, %s: residual gate reports %v on %q", p.name, how, res, out)
				}
				if envelope {
					var back map[string]string
					if err := json.Unmarshal([]byte(out), &back); err != nil {
						t.Errorf("%s, %s: redacted Args are not valid JSON: %v: %q", p.name, how, err, out)
					} else if how == "args" && !strings.HasPrefix(back["content"], "one\n") || how == "args" && !strings.HasSuffix(back["content"], "\nthree\n") {
						t.Errorf("%s, %s: the lines around the cookie changed: %q", p.name, how, back["content"])
					}
				}
			}
		}
	}
	t.Logf("%d probes", cases)
}

// TestHarvestKeepsATraceWithAKeyWordCookie: through the harvest, a trace whose tool output and tool-call Args hold a
// cookie line with key-word names is redacted whole and kept, with no skip note: nothing of either value is left
// for the tail gate to trip on. (TestRedactorTakesACookieWhoseNameHoldsAKeyWordWhicheverClassCountsIt does the same
// through the redactor.)
func TestHarvestKeepsATraceWithAKeyWordCookie(t *testing.T) {
	v, w := tailFake(913, 32, tailAlnum), tailFake(914, 32, tailAlnum)
	line := spanCk + ": refresh_" + spanKwToken + "=" + v + "; sessionid=" + w
	dir := t.TempDir()
	rec := sampleTrace("kw", "HTTP/1.1 200 OK\n"+line+"\nContent-Length: 3\n")
	rec.Transcript[2].ToolCalls[0].Args = writeFileArgs(t, line+"\n")
	writeTraceFile(t, dir, "kw.json", rec)
	entries, stats, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil {
		t.Fatalf("harvest refused: %v", err)
	}
	if len(entries) != 1 || len(stats.Skipped) != 0 {
		t.Fatalf("want the trace harvested with no skip, got %d entries and %+v", len(entries), stats.Skipped)
	}
	var all []string
	for _, turn := range entries[0].Turns {
		all = append(all, turn.Content)
		for _, c := range turn.ToolCalls {
			all = append(all, c.Args)
		}
	}
	joined := strings.Join(all, "\n")
	for _, secret := range []string{v, w} {
		if run, ok := anyRun(secret, joined, 8); ok {
			t.Errorf("%q of a value is still in the harvested entry", run)
		}
	}
	if stats.Redactions["cookie-header"] < 2 {
		t.Errorf("redactions %v, want the cookie class to take the header of both lines", stats.Redactions)
	}
}

// TestValueTailGateRemembersEveryValueInTheSpan: the gate remembers each value in the span whole, whatever it holds: a
// quoted body with a space, an apostrophe, an ampersand, a comma, a semicolon, a tab, a shell escape or an HTML
// escape (so a piece that crosses the character is remembered too, not only the text before it), every element
// of a list, and the values behind them; not the short pairs and not the Set-Cookie attributes.
func TestValueTailGateRemembersEveryValueInTheSpan(t *testing.T) {
	a, b, v, w := tailFake(921, 16, tailAlnum), tailFake(922, 16, tailAlnum), tailFake(923, 32, tailAlnum), tailFake(924, 26, tailAlnum)
	// crossing is the 12 characters of a quoted body that start 5 characters before the middle character.
	crossing := func(mid string) string { return (a + mid + b)[len(a)-5 : len(a)-5+12] }
	for _, c := range []struct {
		name, span string
		pieces     []string
	}{
		{"quoted body with a space", spanCk + `: m="` + a + ` ` + b + `"; sid=` + v, []string{b, v, crossing(" ")}},
		{"apostrophe", spanCk + `: m="` + a + `'` + b + `"; sid=` + v, []string{b, v, crossing("'")}},
		{"ampersand", spanCk + `: m="` + a + `&` + b + `"; sid=` + v, []string{b, v, crossing("&")}},
		{"comma", spanCk + `: m="` + a + `,` + b + `"; sid=` + v, []string{b, v, crossing(",")}},
		{"semicolon", spanCk + `: m="` + a + `;` + b + `"; sid=` + v, []string{b, v, crossing(";")}},
		{"tab", spanCk + ": m=\"" + a + "\t" + b + "\"; sid=" + v, []string{b, v, crossing("\t")}},
		{"html escapes", spanCk + `: m="` + a + `\u0026\u003c` + b + `"; sid=` + v, []string{b, v, crossing(`\u0026\u003c`)}},
		{"quoted body in args", spanCk + `: m=\"` + a + ` ` + b + `\"; sid=` + v, []string{b, v, crossing(" ")}},
		{"quoted body in a shell string in args", spanCk + `: m=\\\"` + a + `\\$x ` + b + `\\\"; sid=` + v, []string{b, v, crossing(`\\$x `)}},
		{"html escape before the value", spanCk + `: a=x\u0026y; sid=` + v, []string{v}},
		{"list elements", spanSetCk + `":["theme=dark","session=` + v + `; Path=/","other=` + w, []string{v, w}},
		{"folded", spanCk + ":\r\n sid=" + v + "; other=" + w, []string{v, w}},
		{"three cookies", spanCk + ": a=" + v + "; b=" + w + "; c=" + a + b, []string{v, w, a + b}},
	} {
		r := newRedactor()
		r.remember("cookie-header", c.span)
		for _, piece := range c.pieces {
			if !r.survivingTail("xx " + piece + " yy") {
				t.Errorf("%s: the piece %q of a value is not remembered", c.name, piece)
			}
		}
		for _, text := range []string{"theme=dark; lang=en", "Path=/; HttpOnly; Secure", "Saved ok"} {
			if r.survivingTail(text) {
				t.Errorf("%s: ordinary text is remembered: %q", c.name, text)
			}
		}
	}
}

// TestRedactorSweepOfCookieSpans: a sweep of cookie lines across the cookie shapes, the wrappings and the
// number of cookies, through the redactor, asserting what the probes assert.
func TestRedactorSweepOfCookieSpans(t *testing.T) {
	probes := spanProbes()
	n := len(probes)
	cases := 0
	for a := 0; a < n; a++ {
		for b := 0; b < n; b++ {
			joined := spanProbe{
				name:   probes[a].name + " then " + probes[b].name,
				body:   probes[a].body + "; " + probes[b].body,
				elems:  append(append([]string{}, probes[a].elems...), probes[b].elems...),
				values: append(append([]string{}, probes[a].values...), probes[b].values...),
			}
			how := spanWraps[(a*7+b)%len(spanWraps)]
			name := spanCk
			if how == "list" || how == "pretty list in args" || (a+b)%2 == 1 {
				name = spanSetCk
			}
			text, envelope := spanWrap(t, how, name, joined.body, joined.elems)
			cases++
			r := NewRedactor()
			out := r.Redact(text)
			for _, v := range joined.values {
				for _, form := range []string{v, spanEnc(v), spanShell(v), spanEnc(spanShell(v))} {
					if run, ok := anyRun(form, out, 8); ok {
						t.Errorf("%s, %s: %q of the value %q is still in %q", joined.name, how, run, v, out)
						break
					}
				}
			}
			if res := r.Residual(out); len(res) != 0 {
				t.Errorf("%s, %s: residual gate reports %v on %q", joined.name, how, res, out)
			}
			if envelope && !json.Valid([]byte(out)) {
				t.Errorf("%s, %s: redacted Args are not valid JSON: %q", joined.name, how, out)
			}
		}
	}
	t.Logf("%d sweep cases", cases)
}
