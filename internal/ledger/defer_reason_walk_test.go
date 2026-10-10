package ledger

// F22 follow-up (2026-10-10): TestEveryWriterOfALedgerRowIsKnown pins which FILES write ledger rows, so a
// writer adding a new reason sentence was not caught: the review of the first F22 change replayed the real
// ledgers of a 48 hour window and 63 % of the deferred non-delegate rows still coded as "other", among them the audio
// and video "generation failed" sentences that the identical image sentence did not share a code with. This
// walks the pipeline's writers: every reason sentence a door passes to recordDefer or deferGen as a literal
// (or as a literal followed by an error's text) must classify to a code other than "other", unless it is
// listed here with the reason it cannot be classified from its words alone. A new sentence fails this test
// until DeferCode (or its table) knows it, which is the point.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// unclassifiableByPrefix are the literal prefixes whose code depends on text that is not in the prefix. Each
// is classified at its writer by an err_class (or by DeferCode's rule for the middle of the sentence), and
// the shapes test pins that.
var unclassifiableByPrefix = map[string]string{
	"compose_video: ": "the sentence continues with the composition's own class (compose_busy, ...), which the writer files as err_class",
	"pipeline ":       "the task name sits in the middle: DeferCode classifies `pipeline <task>: no artifacts configured` and `pipeline <task> failed: ...` on their ends (deferShapes pins both)",
}

// literalReasonPrefix is the string a reason expression starts with when it is a literal or a literal plus
// something ("image load: "+err.Error()); "" for anything else (a variable, a call).
func literalReasonPrefix(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			s, _ := strconv.Unquote(x.Value)
			return s
		}
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return literalReasonPrefix(x.X)
		}
	}
	return ""
}

func TestEveryLiteralReasonAPipelineDoorDefersWithHasACode(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "pipeline", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no pipeline sources found (%v)", err)
	}
	seen := map[string]string{} // prefix -> first writer
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Fatalf("%s: %v", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			idx := -1
			switch sel.Sel.Name {
			case "recordDefer":
				idx = 3 // (task, meta, inputChars, reason)
			case "deferGen":
				idx = 4 // (req, meta, start, inputChars, reason)
			}
			if idx < 0 || idx >= len(c.Args) {
				return true
			}
			if lit := literalReasonPrefix(c.Args[idx]); lit != "" {
				if _, dup := seen[lit]; !dup {
					seen[lit] = filepath.Base(path)
				}
			}
			return true
		})
	}
	if len(seen) < 30 {
		t.Fatalf("found only %d literal reasons in the pipeline's writers: the walk no longer reads them (recordDefer/deferGen signatures changed?)", len(seen))
	}
	var prefixes []string
	for p := range seen {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	used := map[string]bool{}
	for _, p := range prefixes {
		code := DeferCode("", p)
		if why, listed := unclassifiableByPrefix[p]; listed {
			used[p] = true
			if code != ReasonOther {
				t.Errorf("%q (%s) is listed as unclassifiable by prefix (%s) but now classifies as %q: delete the entry", p, seen[p], why, code)
			}
			continue
		}
		if code == ReasonOther {
			t.Errorf("the reason %q that %s passes to a ledger writer classifies as %q: add its prefix to reasonPrefixCodes (or its err_class to errClassCodes) so a reader can group it, or list it in unclassifiableByPrefix with the reason it cannot be classified from its words", p, seen[p], code)
		}
	}
	for p := range unclassifiableByPrefix {
		if !used[p] {
			t.Errorf("unclassifiableByPrefix lists %q, which no pipeline writer passes any more: delete the entry", p)
		}
	}
}

// The classes a node's defer carries on the wire reach the ledger row of the agent door through
// CodeForDeferClass; an empty or unknown class gives "" so the row is classified from its words instead.
func TestCodeForDeferClassCoversTheWireVocabularyAndNothingElse(t *testing.T) {
	want := map[string]string{
		"abstention": ReasonAbstention, "budget": ReasonBudget, "infrastructure": ReasonInfrastructure,
		"config": ReasonConfig, "contract": ReasonContract, "capacity": ReasonNodeBusy, "write": ReasonWrite,
	}
	for class, code := range want {
		if got := CodeForDeferClass(class); got != code {
			t.Errorf("CodeForDeferClass(%q) = %q, want %q", class, got, code)
		}
		if !IsReasonCode(CodeForDeferClass(class)) {
			t.Errorf("class %q maps outside the closed set", class)
		}
	}
	for _, class := range []string{"", "other", "Budget", "stalled"} {
		if got := CodeForDeferClass(class); got != "" {
			t.Errorf("CodeForDeferClass(%q) = %q: an unknown class must give no code, so the row is classified from its words", class, got)
		}
	}
}
