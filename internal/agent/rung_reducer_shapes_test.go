package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// reducerKeepsRaw runs a big failing build output that carries line (a credential-shaped line) through
// the reducer and reports the failures: the output must come back raw and the seat must not be called.
func reducerKeepsRaw(t *testing.T, name, line string) {
	t.Helper()
	body := strings.Replace(goTestBody("exit=1", 7000), "FAIL\nFAIL\t", "FAIL\n"+line+"\nFAIL\t", 1)
	seat := &fakeSeat{reply: replyWith(goodPairs...)}
	got := loopRun(t, seat, "run_shell", goTestArgs, body)
	if got != body {
		t.Errorf("%s: output with a credential shape was reduced: %q", name, head(got, 100))
	}
	if seat.calls != 0 {
		t.Errorf("%s: the seat was shown output with a credential shape (%d calls)", name, seat.calls)
	}
}

// A command echoed back as JSON text, with a nested escaped quote around the value, keeps the output
// away from the seat.
func TestReducerFallsBackOnANestedEscapedQuote(t *testing.T) {
	// assembled at run time so no tracked file carries a credential-shaped literal
	v := strings.Repeat("q7Zx", 6)
	b, err := json.Marshal(map[string]string{"command": `bash -c "export API_` + `KEY=\"` + v + `\" && ./run"`})
	if err != nil {
		t.Fatal(err)
	}
	reducerKeepsRaw(t, "nested escaped quote", string(b))
}

// Colored output (a color code right in front of the key word, a Java system property) keeps the
// output away from the seat.
func TestReducerFallsBackOnColoredCredentialOutput(t *testing.T) {
	// assembled at run time so no tracked file carries a credential-shaped literal
	const esc = "\x1b"
	v := strings.Repeat("q7Zx", 6)
	for name, line := range map[string]string{
		"colored password": esc + "[32mpass" + "word=" + v + esc + "[0m",
		"colored bearer":   esc + "[33mBear" + "er " + v,
		"java property":    "java -Dpass" + "word=" + v + " -jar app.jar",
	} {
		reducerKeepsRaw(t, name, line)
	}
}

// A Basic credential in a printed header map or a call that sets the header keeps the output away from
// the seat.
func TestReducerFallsBackOnABasicHeaderInCodeAndDumps(t *testing.T) {
	// assembled at run time so no tracked file carries a credential-shaped literal
	basic := "Ba" + "sic " + strings.Repeat("Zq8x", 6) + "=="
	for name, line := range map[string]string{
		"go header dump": "Header:map[Authorization:[" + basic + "]]",
		"js setHeader":   "req.setHeader('Authorization', '" + basic + "')",
	} {
		reducerKeepsRaw(t, name, line)
	}
}

// A quoted credential literal in source code (a Go :=, a call that sets an environment variable, a quoted
// JSON key) keeps the output away from the seat.
func TestReducerFallsBackOnAQuotedLiteralInCode(t *testing.T) {
	// assembled at run time so no tracked file carries a credential-shaped literal
	v := strings.Repeat("q7Zx", 6)
	for name, line := range map[string]string{
		"go short declaration": `password := "` + v + `"`,
		"setenv call":          `t.Setenv("API_TOKEN", "` + v + `")`,
		"quoted json key":      `{"token": "` + v + `"}`,
	} {
		reducerKeepsRaw(t, name, line)
	}
}
