package parser

import (
	"errors"
	"testing"
)

// The forms an unconstrained seat may not get away with, because nothing constrained its decoding.
func TestExtractOneRefusesWhatExtractAccepts(t *testing.T) {
	cases := map[string]string{
		"array of objects":     `[{"label":"a","confidence":0.9},{"label":"b","confidence":0.1}]`,
		"array of one":         `[{"label":"a","confidence":0.9}]`,
		"array before object":  `Options: [1,2] {"label":"a","confidence":0.9}`,
		"two objects":          `{"label":"a","confidence":0.9} {"label":"b","confidence":0.1}`,
		"two objects in fence": "```json\n{\"label\":\"a\"}\n{\"label\":\"b\"}\n```",
		"duplicate key":        `{"label":"zzz","label":"a","confidence":0.9}`,
		"nested duplicate key": `{"data":{"x":1,"x":2}}`,
		"duplicate in array":   `{"rows":[{"k":1,"k":2}]}`,
		"object then an array": `{"label":"a"} also [1]`,
	}
	for name, in := range cases {
		_, err := ExtractOne(in)
		if err == nil {
			t.Errorf("%s: ExtractOne accepted %q", name, in)
			continue
		}
		if !errors.Is(err, ErrNotOneObject) {
			t.Errorf("%s: want ErrNotOneObject, got %v", name, err)
		}
	}
	// The three reviewer cases are accepted by Extract (that is the defect being closed).
	for _, in := range []string{
		`[{"label":"a","confidence":0.9},{"label":"b","confidence":0.1}]`,
		`{"label":"a","confidence":0.9} {"label":"b","confidence":0.1}`,
		`{"label":"zzz","label":"a","confidence":0.9}`,
	} {
		if _, err := Extract(in); err != nil {
			t.Errorf("Extract should still be lenient on %q: %v", in, err)
		}
	}
}

// The valid forms stay accepted, and come back as the same bytes Extract gives.
func TestExtractOneAcceptsOneObject(t *testing.T) {
	cases := map[string]string{
		"plain":                 `{"label":"a","confidence":0.9}`,
		"padded":                "  \n{\"label\":\"a\",\"confidence\":0.9}\n ",
		"fenced":                "```json\n{\"label\":\"a\",\"confidence\":0.9}\n```",
		"fenced, prose after":   "Sure:\n```json\n{\"label\":\"a\",\"confidence\":0.9}\n```\nhope that {helps} [1]",
		"leading prose":         `The answer is {"label":"a","confidence":0.9}`,
		"trailing prose":        `{"label":"a","confidence":0.9} as requested.`,
		"trailing comma":        `{"label":"a","confidence":0.9,}`,
		"nested, distinct keys": `{"label":"a","confidence":0.9}`,
	}
	for name, in := range cases {
		got, err := ExtractOne(in)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		want, _ := Extract(in)
		if string(got) != string(want) {
			t.Errorf("%s: got %s, Extract gives %s", name, got, want)
		}
	}
	if _, err := ExtractOne(`no json here`); !errors.Is(err, ErrNoJSON) {
		t.Errorf("no object: got %v", err)
	}
}
