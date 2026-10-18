package agent

import (
	"strings"
	"testing"
)

// The reducer's credential check is the corpus vet's table, and a table reads shapes: a YAML list item, a backtick
// string or a HAR name/value pair is a shape it does not know. The check of the residual gate (a credential header
// keyword with a long value run a few characters behind it, whatever stands between) is applied here too, so the output
// of a command that prints such text stays raw and never reaches the seat.

// A header keyword with a value behind it in a shape the table has no class for keeps the output away from the seat.
func TestReducerFallsBackOnAKeywordNearALongValue(t *testing.T) {
	// assembled at run time so no tracked file carries a credential-shaped literal
	v := strings.Repeat("q7Zx", 6)
	ck := "Coo" + "kie"
	for name, line := range map[string]string{
		"yaml list item":        "Set-" + ck + ":\n      - '" + v + "'",
		"backtick value":        `req.Header.Set("` + ck + `", ` + "`sid=" + v + "`)",
		"har pair":              `{"name":"` + ck + `","value":"sid=` + v + `"}`,
		"xml attributes":        `<header name="` + ck + `" value="sid=` + v + `"/>`,
		"digest scheme":         "Authorization: Digest " + v,
		"api key header, plain": "X-Api-" + "Key: " + v,
	} {
		reducerKeepsRaw(t, name, line)
	}
}

// The check is deliberately wide: a test or an identifier whose name starts with the keyword is a long run right behind
// it, and the output that prints one stays raw. That is the price of failing closed; the reducer is optional and the raw
// output is what the agent would have had without it.
func TestReducerKeepsRawAnOutputThatNamesATestAfterAKeyword(t *testing.T) {
	ck := "Coo" + "kie"
	reducerKeepsRaw(t, "test name", "--- FAIL: Test"+ck+"JarKeepsTheValuesAcrossRequests (0.00s)")
}

// A header line whose value is short, or whose value was replaced, followed by an ordinary header does not keep the
// output raw: the next line is not the value.
func TestReducerReducesAnOutputWhoseHeaderLinesAreNotValues(t *testing.T) {
	ck := "Coo" + "kie"
	body := strings.Replace(goTestBody("exit=1", 7000), "FAIL\nFAIL\t", "FAIL\n"+ck+": theme=dark\nContent-Type: application/json\nFAIL\t", 1)
	seat := &fakeSeat{reply: replyWith(goodPairs...)}
	got := loopRun(t, seat, "run_shell", goTestArgs, body)
	if got == body || seat.calls != 1 {
		t.Errorf("a short cookie followed by an ordinary header should be reduced (calls %d, reduced %v)", seat.calls, got != body)
	}
}
