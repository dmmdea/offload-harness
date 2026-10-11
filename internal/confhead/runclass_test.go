package confhead

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Register F48: the correctness head is fitted on the box's own labelled rows. A bake's rows carry a fixture's
// margins and labels. The control shows classify IS trained when the same rows are not labelled.
func TestConfheadTrainsOnProductionRowsOnly(t *testing.T) {
	write := func(class string) string {
		p := filepath.Join(t.TempDir(), "ledger.jsonl")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		for i := 0; i < 120; i++ {
			agreed := i%2 == 0
			e := ledger.Entry{Task: "classify", Margin: 0.5, InputChars: 50, RunClass: class,
				Feat:            map[string]float64{"len_chars": 50, "n_words": 9, "n_numbers": 0, "n_caps": 1, "has_code": 0, "has_url": 0},
				EscalatedAgreed: &agreed}
			b, _ := json.Marshal(e)
			f.Write(append(b, '\n'))
		}
		return p
	}
	out := filepath.Join(t.TempDir(), "confhead-weights.json")
	rep, err := Train(write(""), "", out)
	if err != nil || !strings.Contains(rep, "task=classify") || !strings.Contains(rep, "trained OK") {
		t.Fatalf("control: real rows must train classify: err=%v\n%s", err, rep)
	}
	rep, err = Train(write("bake"), "", out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rep, "trained OK") {
		t.Fatalf("a head was fitted on a bake's rows:\n%s", rep)
	}
}
