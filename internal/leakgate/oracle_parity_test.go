package leakgate

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// oracleCase is one input of the committed corpus and the findings the
// reference implementation reports on it: the Python port of the matcher that
// is kept outside the repository, run with the specified fold (the 244-entry
// Latin table, not NFKD), over the synthetic list of this package. Each
// finding is [line, column, "mode:text"].
type oracleCase struct {
	Text     string  `json:"text"`
	Findings [][]any `json:"findings"`
}

// TestOracleParityCorpus is the parity check as a committed test: the same
// (line, column, entry) findings as the reference implementation on 300 inputs
// that mix every class of the matrix (case variants, accents, zero-width and
// full-width forms, unicode escapes, escape glue, percent forms, glued and
// separated phrases, drive-letter forms, near misses). The corpus holds only
// the synthetic names; the real list is checked against the real tree by the
// local scanner's parity run (internal/leakgate is not told any real name).
// The digest-mode matcher must report the same positions.
func TestOracleParityCorpus(t *testing.T) {
	raw, err := os.ReadFile("testdata/oracle-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []oracleCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 300 {
		t.Fatalf("corpus has %d cases, want 300", len(cases))
	}
	plain := plainMatcher(t)
	digest, _ := digestMatcher(t, "ab", nil, nil)
	total := 0
	for i, c := range cases {
		var want []string
		for _, f := range c.Findings {
			want = append(want, key(int(f[0].(float64)), int(f[1].(float64)), f[2].(string)))
		}
		sort.Strings(want)
		total += len(want)
		fs, _ := plain.ScanText("x", []byte(c.Text))
		var got []string
		for _, f := range fs {
			got = append(got, key(f.Line, f.Col, f.Label()))
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("case %d differs from the reference\n text %q\n  got %q\n want %q", i, c.Text, got, want)
			continue
		}
		ds, _ := digest.ScanText("x", []byte(c.Text))
		var gotPos, wantPos []string
		for _, f := range ds {
			gotPos = append(gotPos, key(f.Line, f.Col, f.Mode))
		}
		for _, f := range fs {
			wantPos = append(wantPos, key(f.Line, f.Col, f.Mode))
		}
		sort.Strings(gotPos)
		sort.Strings(wantPos)
		if !reflect.DeepEqual(gotPos, wantPos) {
			t.Errorf("case %d: the digest matcher differs from the plaintext matcher: %q vs %q", i, gotPos, wantPos)
		}
	}
	if total < 1000 {
		t.Errorf("the corpus is too quiet: %d findings", total)
	}
}

func key(line, col int, label string) string {
	return strings.Join([]string{pad(line), pad(col), label}, " ")
}

func pad(n int) string {
	s := itoa(n)
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}
