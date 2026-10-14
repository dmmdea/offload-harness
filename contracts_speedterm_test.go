package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The speed-term gate over acceptance arrays (register H-03, INV-5, ADR 0050).
//
// INV-5 says quality outranks speed, and ADR 0050 spells out the consequence for
// this file's subject: "Acceptance criteria and pass rules stay wall-time-free".
// A seat decision may read a wall-time figure only as a feasibility floor or as
// an ordering key among seats that already passed the adequacy gate; a check that
// JUDGES an answer by how fast it arrived, or that a faster seat "wins", would put
// speed back into the verdict. Until this file nothing in the repository enforced
// that for the shipped contracts: contracts_fixture_test.go reads three of the
// twelve JSON files under contracts/, and the other nine (the four templates, the
// four write-door contracts, the node list) were not parsed by any test.
//
// Two structural facts do most of the work, and the tests pin both:
//
//  1. The acceptance DSL is closed, and no verb in it reads a clock. Every verb
//     reads the answer text, a structured field or the write set. So a time
//     criterion cannot be EXPRESSED as a check; the only ways a speed term gets
//     into an acceptance string are a content check that quotes a speed word, or
//     a future verb that reads a clock. TestAcceptanceVerbSetIsReviewed makes the
//     second one a reviewed decision instead of a silent addition.
//  2. A content check that quotes a speed word is caught by speedTermIn below. It
//     is a lint, not a proof: the de-escape of a regex argument is best effort
//     (`\d+`, `[0-9]+` and `(?:a|b)` spellings of a figure evade it), so the verb
//     pin, not the pattern, is the defence that does not depend on how a check is
//     spelled. A check that must quote a doc fact containing such a phrase anchors
//     on the figure instead (`regex:90`), exactly as contracts/README.md already
//     advises for every content anchor.
//
// speedTermPattern is the same pattern as the speed-term classifier that scans the
// operator's measurement drivers and plan register (a Python lint outside this
// repository), kept in lockstep with it by hand and by that lint's own check:
// change both or neither. It is a raw string so the lint can read the literal.
const speedTermPattern = `(?i)\b(slower|faster|speed-?up)\b|tok/s\s*[<>]=?|wall\s*<=?\s*[0-9.]+\s*x|<=\s*[0-9.]+x\s+the|[0-9.]+x\s+the\s+k=1|\b(near|within|under|below|at most)\s+~?[0-9][0-9.,]*\s*(s|sec|secs|seconds|min|mins|minutes)\b`

var speedTermRe = regexp.MustCompile(speedTermPattern)

// acceptanceFinding is one reason a contract file's acceptance array is refused.
// File is slash-separated and relative to the scanned directory; Path locates the
// array or string inside the file (`subtasks[1].acceptance[0]`).
type acceptanceFinding struct {
	File  string
	Path  string
	Check string
	Why   string
}

// deescapeRegexArg turns the arguments of a `regex:` check into the plain text the
// speed-term pattern reads: whitespace and word-boundary escapes become a space,
// an escaped dot becomes a dot, and the (?i) flag is dropped. Best effort only.
func deescapeRegexArg(s string) string { return deescapeReplacer.Replace(s) }

// The multi-character escapes come first: a Replacer tries its old strings in
// argument order at each position, so `\s*` is read before `\s`.
var deescapeReplacer = strings.NewReplacer(
	`(?i)`, ``,
	`\s*`, ` `,
	`\s+`, ` `,
	`\s`, ` `,
	`\b`, ` `,
	`\.`, `.`,
)

// speedTermIn reports the first speed term in one acceptance string, scanning the
// string as written and, for a `regex:` check, the de-escaped argument too.
func speedTermIn(check string) (term string, ok bool) {
	if m := speedTermRe.FindString(check); m != "" {
		return m, true
	}
	if arg, isRegex := strings.CutPrefix(check, "regex:"); isRegex {
		if m := speedTermRe.FindString(deescapeRegexArg(arg)); m != "" {
			return m, true
		}
	}
	return "", false
}

// scanAcceptanceDir walks every *.json under dir. For each `acceptance` key found
// at any depth it records one finding per refusal reason: a value that is not an
// array of strings, a string the closed DSL does not parse, a string with a speed
// term. files lists the files that held at least one `acceptance` key (relative,
// slash-separated, in walk order) and strs counts the acceptance strings read.
func scanAcceptanceDir(dir string) (findings []acceptanceFinding, files []string, strs int, err error) {
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		var doc any
		if jerr := json.Unmarshal(bytes.TrimPrefix(raw, utf8BOM), &doc); jerr != nil {
			findings = append(findings, acceptanceFinding{File: rel, Why: "not valid JSON: " + jerr.Error()})
			return nil
		}
		w := acceptanceWalker{file: rel}
		w.walk(doc, "")
		findings = append(findings, w.findings...)
		strs += w.strs
		if w.keys > 0 {
			files = append(files, rel)
		}
		return nil
	})
	return findings, files, strs, err
}

var utf8BOM = []byte("\xef\xbb\xbf")

// acceptanceWalker collects the findings of one file. It looks at the key
// `acceptance` and nothing else: a context text that says "no speed term enters
// the choice" must stay legal.
type acceptanceWalker struct {
	file     string
	findings []acceptanceFinding
	strs     int // acceptance strings read
	keys     int // acceptance keys met
}

func (w *acceptanceWalker) add(path, check, why string) {
	w.findings = append(w.findings, acceptanceFinding{File: w.file, Path: path, Check: check, Why: why})
}

func (w *acceptanceWalker) walk(v any, path string) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if k == "acceptance" {
				w.keys++
				w.acceptance(x[k], p)
				continue
			}
			w.walk(x[k], p)
		}
	case []any:
		for i, e := range x {
			w.walk(e, fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

func (w *acceptanceWalker) acceptance(v any, path string) {
	arr, ok := v.([]any)
	if !ok {
		w.add(path, "", fmt.Sprintf("acceptance is %s, not an array of strings", jsonKind(v)))
		return
	}
	for i, e := range arr {
		p := fmt.Sprintf("%s[%d]", path, i)
		s, ok := e.(string)
		if !ok {
			w.add(p, "", fmt.Sprintf("acceptance element is %s, not a string", jsonKind(e)))
			continue
		}
		w.strs++
		if _, perr := core.ParseAcceptanceCheck(s); perr != nil {
			w.add(p, s, "does not parse in the acceptance DSL: "+perr.Error())
		}
		if term, hit := speedTermIn(s); hit {
			w.add(p, s, fmt.Sprintf("carries the speed term %q", term))
		}
	}
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	}
	return fmt.Sprintf("a %T", v)
}

// acceptanceVerbsInSource reads the Go source of the acceptance parser and returns
// the verbs it declares (the string values of the constants typed AcceptanceKind)
// and the verbs it accepts (the case labels of the switch inside
// ParseAcceptanceCheck, whether written as a constant name or a string literal),
// each sorted and without duplicates. A label it cannot resolve is an error, never
// a skipped verb.
func acceptanceVerbsInSource(src []byte) (consts, cases []string, err error) {
	f, err := parser.ParseFile(token.NewFileSet(), "agentwire.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]string{}
	declared := map[string]bool{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, typed := vs.Type.(*ast.Ident); !typed || id.Name != "AcceptanceKind" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					return nil, nil, fmt.Errorf("constant %s has no value", name.Name)
				}
				v, ok := verbLiteral(vs.Values[i])
				if !ok {
					return nil, nil, fmt.Errorf("constant %s is not a string literal", name.Name)
				}
				byName[name.Name] = v
				declared[v] = true
			}
		}
	}
	var parse *ast.FuncDecl
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "ParseAcceptanceCheck" && fd.Body != nil {
			parse = fd
		}
	}
	if parse == nil {
		return nil, nil, fmt.Errorf("no func ParseAcceptanceCheck in the source")
	}
	accepted := map[string]bool{}
	switches := 0
	var unresolved error
	ast.Inspect(parse.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		if sel, isSel := sw.Tag.(*ast.SelectorExpr); !isSel || sel.Sel.Name != "Kind" {
			return true
		}
		switches++
		for _, st := range sw.Body.List {
			cc, ok := st.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, e := range cc.List {
				if id, isIdent := e.(*ast.Ident); isIdent {
					v, known := byName[id.Name]
					if !known {
						unresolved = fmt.Errorf("case label %s is not a constant typed AcceptanceKind", id.Name)
						continue
					}
					accepted[v] = true
					continue
				}
				v, ok := verbLiteral(e)
				if !ok {
					unresolved = fmt.Errorf("case label of kind %T cannot be read", e)
					continue
				}
				accepted[v] = true
			}
		}
		return true
	})
	if unresolved != nil {
		return nil, nil, unresolved
	}
	if switches == 0 {
		return nil, nil, fmt.Errorf("ParseAcceptanceCheck has no switch on the check's Kind")
	}
	return sortedSet(declared), sortedSet(accepted), nil
}

// verbLiteral reads "x" or AcceptanceKind("x").
func verbLiteral(e ast.Expr) (string, bool) {
	if call, ok := e.(*ast.CallExpr); ok && len(call.Args) == 1 {
		if id, isIdent := call.Fun.(*ast.Ident); isIdent && id.Name == "AcceptanceKind" {
			e = call.Args[0]
		}
	}
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSpeedTermVectors pins what counts as a speed term, and what must not.
func TestSpeedTermVectors(t *testing.T) {
	positives := []string{
		"contains:faster",
		"contains:speedup",
		"contains:speed-up",
		"not_contains:Slower",
		"regex:tok/s >= 20",
		"regex:wall <= 1.5x",
		"regex:<= 1.5x the baseline",
		"regex:2x the K=1",
		"regex:within 90 s",
		"regex:under 3 min",
		"regex:at most 90 seconds",
		"regex:near 22 s",
		// The same terms the way a regex check spells them: escapes that keep
		// the raw text from matching.
		`regex:(?i)\bfaster\b`,
		`regex:wall\s*<=\s*1\.5x`,
		`regex:tok/s\s*>=\s*20`,
		`regex:\bunder\s+3\s+min\b`,
	}
	for _, c := range positives {
		term, ok := speedTermIn(c)
		if !ok || term == "" {
			t.Errorf("speedTermIn(%q) = (%q, %v), want a speed term", c, term, ok)
		}
	}
	negatives := []string{
		"regex:[0-9]",
		"contains:fasterxml",
		"contains:underscore",
		"contains:wall",
		"regex:within the doc",
		"regex:within 3 sentences",
		"contains:below 5 items",
		"contains:tok/s",
		"contains:1568",
		"nonempty:summary",
		"min_items:findings:12",
		"diff_max_files:2",
		"regex:(evict_costs|LOCAL_OFFLOAD_ORIGIN)",
		`regex:\bwithin\s+the\s+doc\b`,
	}
	for _, c := range negatives {
		if term, ok := speedTermIn(c); ok {
			t.Errorf("speedTermIn(%q) = (%q, true), want no speed term", c, term)
		}
	}
}

// TestDeescapeRegexArg pins the escapes the de-escape reads.
func TestDeescapeRegexArg(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`(?i)\bfaster\b`, ` faster `},
		{`wall\s*<=\s*1\.5x`, `wall <= 1.5x`},
		{`tok/s\s+>=\s+20`, `tok/s >= 20`},
		{`a\sb`, `a b`},
		{`plain text`, `plain text`},
	} {
		if got := deescapeRegexArg(tc.in); got != tc.want {
			t.Errorf("deescapeRegexArg(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

type wantFinding struct{ file, path, why string }

// TestScanAcceptanceDirFixtures runs the scanner over small trees it builds in a
// temporary directory; it never reads the repository's own contracts.
func TestScanAcceptanceDirFixtures(t *testing.T) {
	bom := "\xef\xbb\xbf"
	for _, tc := range []struct {
		name      string
		files     map[string]string
		want      []wantFinding
		wantFiles []string
		wantStrs  int
	}{
		{
			// The pre-existing out-of-repo scan captured an acceptance array up to
			// its first `]`, so a `]` inside an earlier check hid the next one.
			name:      "a bracket inside an earlier check does not hide the next",
			files:     map[string]string{"c.json": `{"goal":"g","acceptance":["regex:[0-9]","regex:within 90 s"]}`},
			want:      []wantFinding{{"c.json", "acceptance[1]", "speed term"}},
			wantFiles: []string{"c.json"},
			wantStrs:  2,
		},
		{
			name:      "a clean single object",
			files:     map[string]string{"c.json": `{"goal":"g","acceptance":["regex:[0-9]","nonempty:summary"]}`},
			wantFiles: []string{"c.json"},
			wantStrs:  2,
		},
		{
			name: "a subtasks list nested in an object",
			files: map[string]string{"c.json": `{"subtasks":[{"goal":"a","acceptance":["contains:x"]},` +
				`{"goal":"b","acceptance":["regex:within 90 s"]}]}`},
			want:      []wantFinding{{"c.json", "subtasks[1].acceptance[0]", "speed term"}},
			wantFiles: []string{"c.json"},
			wantStrs:  2,
		},
		{
			name:      "a top-level array of contracts",
			files:     map[string]string{"c.json": `[{"acceptance":["contains:x"]},{"acceptance":["not_contains:faster"]}]`},
			want:      []wantFinding{{"c.json", "[1].acceptance[0]", "speed term"}},
			wantFiles: []string{"c.json"},
			wantStrs:  2,
		},
		{
			name: "speed words in the goal and the context text are not acceptance",
			files: map[string]string{"c.json": `{"goal":"answer faster than 20 tok/s > 5","context":[{"name":"a.md",` +
				`"text":"No speed term enters the choice; finish within 90 s"}],"acceptance":["contains:x"]}`},
			wantFiles: []string{"c.json"},
			wantStrs:  1,
		},
		{
			name:      "acceptance given as a bare string",
			files:     map[string]string{"c.json": `{"acceptance":"contains:x"}`},
			want:      []wantFinding{{"c.json", "acceptance", "not an array"}},
			wantFiles: []string{"c.json"},
		},
		{
			name:      "an acceptance element that is not a string",
			files:     map[string]string{"c.json": `{"acceptance":["contains:x",5]}`},
			want:      []wantFinding{{"c.json", "acceptance[1]", "not a string"}},
			wantFiles: []string{"c.json"},
			wantStrs:  1,
		},
		{
			// The closed DSL has no time verb, so a clock-shaped check does not parse.
			name:      "a pseudo-check the DSL does not know",
			files:     map[string]string{"c.json": `{"acceptance":["wall_ms:1500"]}`},
			want:      []wantFinding{{"c.json", "acceptance[0]", "does not parse"}},
			wantFiles: []string{"c.json"},
			wantStrs:  1,
		},
		{
			name:      "an escaped regex spelling of a speed term",
			files:     map[string]string{"c.json": `{"acceptance":["regex:wall\\s*<=\\s*1\\.5x"]}`},
			want:      []wantFinding{{"c.json", "acceptance[0]", "speed term"}},
			wantFiles: []string{"c.json"},
			wantStrs:  1,
		},
		{
			name:     "a file with no acceptance key is not counted",
			files:    map[string]string{"nodes.json": `{"nodes":[{"id":"node-a","enabled":true}]}`},
			wantStrs: 0,
		},
		{
			name:      "a file in a nested directory is found",
			files:     map[string]string{"sub/deeper/c.json": `{"acceptance":["regex:within 90 s"]}`},
			want:      []wantFinding{{"sub/deeper/c.json", "acceptance[0]", "speed term"}},
			wantFiles: []string{"sub/deeper/c.json"},
			wantStrs:  1,
		},
		{
			name:      "a byte order mark does not hide a file",
			files:     map[string]string{"c.json": bom + `{"acceptance":["regex:within 90 s"]}`},
			want:      []wantFinding{{"c.json", "acceptance[0]", "speed term"}},
			wantFiles: []string{"c.json"},
			wantStrs:  1,
		},
		{
			name:  "a file that is not valid JSON is a finding, never skipped",
			files: map[string]string{"c.json": `{"acceptance": ["regex:within 90 s"`},
			want:  []wantFinding{{"c.json", "", "not valid JSON"}},
		},
		{
			name:  "only .json files are read",
			files: map[string]string{"notes.md": `{"acceptance":["regex:within 90 s"]}`},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for rel, body := range tc.files {
				p := filepath.Join(dir, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			findings, files, strs, err := scanAcceptanceDir(dir)
			if err != nil {
				t.Fatalf("scanAcceptanceDir: %v", err)
			}
			var got []string
			for _, f := range findings {
				got = append(got, fmt.Sprintf("%s|%s|%s", f.File, f.Path, f.Why))
			}
			if len(findings) != len(tc.want) {
				t.Fatalf("got %d findings %q, want %d %+v", len(findings), got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				f := findings[i]
				if f.File != w.file || f.Path != w.path || !strings.Contains(f.Why, w.why) {
					t.Errorf("finding %d = %q, want file %q path %q why containing %q", i, got[i], w.file, w.path, w.why)
				}
			}
			if strings.Join(files, ",") != strings.Join(tc.wantFiles, ",") {
				t.Errorf("counted files = %q, want %q", files, tc.wantFiles)
			}
			if strs != tc.wantStrs {
				t.Errorf("acceptance strings read = %d, want %d", strs, tc.wantStrs)
			}
		})
	}
}

// acceptanceKeyRe finds an unescaped `"acceptance":` key in raw JSON bytes. An
// ADR quoted inside a context text carries its quotes as \" and never matches.
var acceptanceKeyRe = regexp.MustCompile(`"acceptance"\s*:`)

// filesWithAcceptanceKey is the second, independent way to learn which files hold
// an acceptance key: a byte search, not a JSON walk.
func filesWithAcceptanceKey(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.ToLower(filepath.Ext(d.Name())) != ".json" {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if acceptanceKeyRe.Match(raw) {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestContractAcceptanceCarriesNoSpeedTerm is the live gate: every acceptance
// string in every JSON file under contracts/ parses in the closed DSL and holds no
// speed term. Non-vacuity: the scanner must have read exactly the files that hold
// an acceptance key (found by a separate byte search) and at least one string, so
// a walk that silently reads nothing cannot pass.
func TestContractAcceptanceCarriesNoSpeedTerm(t *testing.T) {
	findings, files, strs, err := scanAcceptanceDir("contracts")
	if err != nil {
		t.Fatalf("scanAcceptanceDir(contracts): %v", err)
	}
	for _, f := range findings {
		t.Errorf("contracts/%s %s: %s (check %q): an acceptance string stays wall-time-free (INV-5, ADR 0050); anchor on a figure or a term from the docs instead", f.File, f.Path, f.Why, f.Check)
	}
	want := filesWithAcceptanceKey(t, "contracts")
	if len(want) == 0 {
		t.Fatal("no JSON file under contracts/ holds an acceptance key: the byte search is broken")
	}
	read := append([]string(nil), files...)
	sort.Strings(read)
	if strings.Join(read, ",") != strings.Join(want, ",") {
		t.Errorf("the scanner read %q, the byte search finds an acceptance key in %q", read, want)
	}
	if strs == 0 {
		t.Error("the scanner read no acceptance string at all")
	}
	t.Logf("scanned %d files holding %d acceptance strings", len(files), strs)
}

// reviewedAcceptanceVerbs is the closed verb set with the reason each verb is
// clock-free. A new verb is a reviewed decision: add it here with its reason, and
// say in the change why it cannot express a wall-time criterion.
var reviewedAcceptanceVerbs = map[string]string{
	"contains":       "reads the answer text; no clock",
	"not_contains":   "reads the answer text; no clock",
	"regex":          "reads the answer text; no clock",
	"min_items":      "reads a structured field's length; no clock",
	"nonempty":       "reads whether structured fields are present; no clock",
	"diff_touches":   "reads the write set's paths; no clock",
	"diff_max_files": "reads the write set's size; no clock",
}

func reviewedVerbNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestAcceptanceVerbSourceScan pins the source reader that the verb-set test
// below relies on, on small sources that carry each way a verb can be added.
func TestAcceptanceVerbSourceScan(t *testing.T) {
	const head = "package core\n\ntype AcceptanceKind string\n\ntype AcceptanceCheck struct{ Kind AcceptanceKind }\n\n"
	for _, tc := range []struct {
		name        string
		src         string
		consts, cas string
		wantErr     bool
	}{
		{
			name: "constants and plain case labels",
			src: head + "const (\n\tAccA AcceptanceKind = \"alpha\"\n\tAccB AcceptanceKind = \"beta\"\n)\n" +
				"func ParseAcceptanceCheck(s string) (AcceptanceCheck, error) {\n\tvar c AcceptanceCheck\n" +
				"\tswitch c.Kind {\n\tcase AccA:\n\tcase AccB:\n\tdefault:\n\t}\n\treturn c, nil\n}\n",
			consts: "alpha,beta", cas: "alpha,beta",
		},
		{
			name: "a string literal next to a constant name in one case",
			src: head + "const AccA AcceptanceKind = \"alpha\"\n" +
				"func ParseAcceptanceCheck(s string) (AcceptanceCheck, error) {\n\tvar c AcceptanceCheck\n" +
				"\tswitch c.Kind {\n\tcase AccA, \"budget_ms\":\n\t}\n\treturn c, nil\n}\n",
			consts: "alpha", cas: "alpha,budget_ms",
		},
		{
			name: "a conversion in a case label",
			src: head + "const AccA AcceptanceKind = \"alpha\"\n" +
				"func ParseAcceptanceCheck(s string) (AcceptanceCheck, error) {\n\tvar c AcceptanceCheck\n" +
				"\tswitch c.Kind {\n\tcase AccA, AcceptanceKind(\"slow\"):\n\t}\n\treturn c, nil\n}\n",
			consts: "alpha", cas: "alpha,slow",
		},
		{
			name: "a declared constant the switch does not accept",
			src: head + "const (\n\tAccA AcceptanceKind = \"alpha\"\n\tAccC AcceptanceKind = \"clock\"\n)\n" +
				"func ParseAcceptanceCheck(s string) (AcceptanceCheck, error) {\n\tvar c AcceptanceCheck\n" +
				"\tswitch c.Kind {\n\tcase AccA:\n\t}\n\treturn c, nil\n}\n",
			consts: "alpha,clock", cas: "alpha",
		},
		{
			name: "a case label naming nothing declared is an error",
			src: head + "const AccA AcceptanceKind = \"alpha\"\n" +
				"func ParseAcceptanceCheck(s string) (AcceptanceCheck, error) {\n\tvar c AcceptanceCheck\n" +
				"\tswitch c.Kind {\n\tcase AccA, AccMystery:\n\t}\n\treturn c, nil\n}\n",
			wantErr: true,
		},
		{
			name: "a parser with no switch on the kind is an error",
			src: head + "const AccA AcceptanceKind = \"alpha\"\n" +
				"func ParseAcceptanceCheck(s string) (AcceptanceCheck, error) {\n\treturn AcceptanceCheck{}, nil\n}\n",
			wantErr: true,
		},
		{
			name:    "no parser function is an error",
			src:     head + "const AccA AcceptanceKind = \"alpha\"\n",
			wantErr: true,
		},
		{
			name:    "source that does not parse is an error",
			src:     "package core\nfunc (",
			wantErr: true,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			consts, cases, err := acceptanceVerbsInSource([]byte(tc.src))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got consts %q cases %q", consts, cases)
				}
				return
			}
			if err != nil {
				t.Fatalf("acceptanceVerbsInSource: %v", err)
			}
			if got := strings.Join(consts, ","); got != tc.consts {
				t.Errorf("declared verbs = %q, want %q", got, tc.consts)
			}
			if got := strings.Join(cases, ","); got != tc.cas {
				t.Errorf("accepted verbs = %q, want %q", got, tc.cas)
			}
		})
	}
}

// unknownVerbList extracts the verb list the parser prints for an unknown verb.
var unknownVerbList = regexp.MustCompile(`\(want ([^)]*)\)$`)

// TestAcceptanceVerbSetIsReviewed keeps the "no verb reads a clock" fact true: the
// verbs the parser declares, the verbs its switch accepts, the verbs its error
// message offers and the reviewed set above must be the same list, and a verb
// named like a clock must be refused. A node parses a contract's acceptance when it
// ACKs it (internal/core Validate), so a clock verb added to the parser would be
// accepted fleet-wide before any test noticed.
func TestAcceptanceVerbSetIsReviewed(t *testing.T) {
	reviewed := reviewedVerbNames(reviewedAcceptanceVerbs)
	for verb, why := range reviewedAcceptanceVerbs {
		if strings.TrimSpace(why) == "" {
			t.Errorf("reviewed verb %q carries no reason", verb)
		}
	}
	src, err := os.ReadFile(filepath.Join("internal", "core", "agentwire.go"))
	if err != nil {
		t.Fatal(err)
	}
	consts, cases, err := acceptanceVerbsInSource(src)
	if err != nil {
		t.Fatalf("reading the parser source: %v", err)
	}
	t.Run("declared constants equal the reviewed set", func(t *testing.T) {
		if got, want := strings.Join(consts, ", "), strings.Join(reviewed, ", "); got != want {
			t.Errorf("AcceptanceKind constants = [%s], reviewed = [%s]: review the new verb (it must not read a clock) and add it to reviewedAcceptanceVerbs with its reason", got, want)
		}
	})
	t.Run("switch cases equal the reviewed set", func(t *testing.T) {
		if got, want := strings.Join(cases, ", "), strings.Join(reviewed, ", "); got != want {
			t.Errorf("ParseAcceptanceCheck accepts [%s], reviewed = [%s]: review the new verb (it must not read a clock) and add it to reviewedAcceptanceVerbs with its reason", got, want)
		}
	})
	t.Run("unknown verb message lists the reviewed set", func(t *testing.T) {
		_, perr := core.ParseAcceptanceCheck("zz:1")
		if perr == nil {
			t.Fatal("zz:1 parsed")
		}
		m := unknownVerbList.FindStringSubmatch(perr.Error())
		if m == nil {
			t.Fatalf("no verb list in %q", perr.Error())
		}
		offered := strings.Split(m[1], ", ")
		sort.Strings(offered)
		if got, want := strings.Join(offered, ", "), strings.Join(reviewed, ", "); got != want {
			t.Errorf("the unknown-verb message offers [%s], reviewed = [%s]", got, want)
		}
	})
	t.Run("clock-shaped verb names are refused", func(t *testing.T) {
		for _, verb := range []string{"wall", "wall_ms", "max_wall", "latency", "ttft", "tok_s",
			"within", "under", "faster", "slower", "elapsed", "duration"} {
			_, perr := core.ParseAcceptanceCheck(verb + ":1")
			if perr == nil || !strings.Contains(perr.Error(), "unknown verb") {
				t.Errorf("%s:1 = %v, want an unknown-verb refusal", verb, perr)
			}
		}
	})
}
