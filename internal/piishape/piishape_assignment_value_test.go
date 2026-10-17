package piishape

import "testing"

// TestAssignmentValueNamesTheValueAlone: a redactor remembers what the class took as the value, not the key
// name, the operator, a type annotation or the opening quote. The value is the body of a quoted literal
// (spaces, commas, semicolons and ampersands included) or the unquoted run, for every form the class matches.
func TestAssignmentValueNamesTheValueAlone(t *testing.T) {
	a, b := valueFixture(801, 8, alnum), valueFixture(802, 8, alnum)
	whole := valueFixture(803, 22, alnum)
	type sample struct{ name, text, value string }
	var samples []sample
	for _, s := range quotedBodySamples(a, b) {
		samples = append(samples, sample{s.name, s.text, s.value})
	}
	for _, s := range assignFormSamples(whole) {
		samples = append(samples, sample{s.name, s.text, whole})
	}
	samples = append(samples,
		sample{"unquoted", `DB_PASS` + `WORD=` + whole, whole},
		sample{"unquoted colon", `to` + `ken: ` + whole, whole},
		sample{"short word", `MYSQL_P` + `WD=` + whole, whole},
		sample{"quote then unquoted run", `api_` + `key="` + whole + `", next`, whole},
		sample{"color code in front", "\x1b[32m" + `pass` + `word=` + whole, whole},
	)
	for _, s := range samples {
		m := assignmentMatch(s.text)
		if m == "" {
			t.Errorf("%s: no match in %q", s.name, s.text)
			continue
		}
		m = m[len(Lead(m)):]
		if got := AssignmentValue(m); got != s.value {
			t.Errorf("%s: AssignmentValue(%q) = %q, want %q", s.name, m, got, s.value)
		}
	}
	if got := AssignmentValue("not an assignment at all"); got != "" {
		t.Errorf("AssignmentValue of plain text = %q, want empty", got)
	}
}
