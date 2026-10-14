package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// `ledger --failures`: the failure-class register. A mode of the existing ledger verb: main.go's verb
// switch calls runLedgerMode (its only code change; the usage line also names the flag), and `ledger` without the
// flag is byte-for-byte what it was.
//
// The ledger reader groups the failed job rows into classes (internal/ledger/failures.go). This file adds the
// human half: the table in docs/systems/failure-register.md gives each class its meaning, root cause and
// structural fix, and the two are joined here BY CLASS NAME. Nothing in code supplies a root cause or a fix: a
// class with no row in the table says "undocumented", a row with an empty cell says "not recorded" / "open".

// failureRegisterDoc is the page the table lives on, embedded so a built binary carries the table it was built
// with and needs no checkout to run. --table reads another copy.
//
//go:embed docs/systems/failure-register.md
var failureRegisterDoc string

const (
	// undocumentedClass is what the human columns read for a class with no row in the table.
	undocumentedClass = "undocumented"
	// notRecordedCause is an empty root-cause cell; openFix is an empty structural-fix cell.
	notRecordedCause = "not recorded"
	openFix          = "open"
)

// failureDoc is one row of the human-maintained table.
type failureDoc struct {
	Meaning, RootCause, StructuralFix string
}

// failureClassOut is a class of the ledger's register with the joined table row.
type failureClassOut struct {
	ledger.FailureClass
	Documented    bool   `json:"documented"`
	Meaning       string `json:"meaning"`
	RootCause     string `json:"root_cause"`
	StructuralFix string `json:"structural_fix"`
}

// failureReport is what `ledger --failures` prints: one JSON object, no trailing text.
type failureReport struct {
	LedgerFound           bool                      `json:"ledger_found"`
	UnparsedLines         int                       `json:"unparsed_lines"`
	JobRows               int                       `json:"job_rows"`
	FailureRows           int                       `json:"failure_rows"`
	RefusedAttempts       int                       `json:"refused_attempts"`
	StackFaultRows        int                       `json:"stack_fault_rows"`
	StackFaultShare       *float64                  `json:"stack_fault_share"`
	Kinds                 []ledger.FailureKindCount `json:"kinds"`
	ClassDerivedRows      int                       `json:"class_derived_rows"`
	CardsTokens           *int64                    `json:"cards_tokens"`
	RowsWithTokens        int                       `json:"rows_with_tokens"`
	ExcludedNonProduction map[string]int            `json:"excluded_non_production,omitempty"`
	UndocumentedClasses   []string                  `json:"undocumented_classes"`
	Classes               []failureClassOut         `json:"classes"`
}

// runLedgerMode is the ledger verb's entry (the one line main.go changed): the failure register when --failures is
// asked for, the savings summary otherwise.
func runLedgerMode(args []string) error {
	if ledgerFailuresRequested(args) {
		return runLedgerFailures(args)
	}
	return runLedger(args)
}

// ledgerFailuresRequested reports whether a ledger invocation asks for the register: a --failures (or -failures)
// flag that is not --failures=false, before any "--". It is checked before the verb's own flag set parses, which
// would refuse the unknown flag.
func ledgerFailuresRequested(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name != "failures" {
			continue
		}
		if !hasVal {
			return true
		}
		on, err := strconv.ParseBool(val)
		return err != nil || on
	}
	return false
}

// runLedgerFailures is `ledger --failures`: the register as JSON on stdout. Anything else it has to say (the
// traffic it left out) goes to stderr, so the stream can be redirected to a file and read back as one value.
func runLedgerFailures(args []string) error {
	fs := flag.NewFlagSet("ledger --failures", flag.ExitOnError)
	fs.String("config", "", "config file path")
	fs.Bool("failures", true, "print the failure-class register instead of the savings summary")
	since := fs.Int("since", 0, "only count entries from the last N days")
	includeTraffic := includeTrafficFlag(fs)
	table := fs.String("table", "", "markdown file holding the failure-class table (default: docs/systems/failure-register.md as embedded in this build)")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)

	text := failureRegisterDoc
	if *table != "" {
		raw, err := os.ReadFile(*table)
		if err != nil {
			return fmt.Errorf("ledger --failures: read table: %w", err)
		}
		text = string(raw)
	}
	docs, err := parseFailureTable(text)
	if err != nil {
		return fmt.Errorf("ledger --failures: failure-class table: %w", err)
	}
	var sinceTS int64
	if *since > 0 {
		sinceTS = time.Now().AddDate(0, 0, -*since).Unix()
	}
	reg, err := ledger.FailureRegisterFile(cfg.LedgerPath, sinceTS, *includeTraffic)
	if err != nil {
		return err
	}
	noteExcluded(reg.ExcludedNonProduction)
	noteLedgerReading(cfg.LedgerPath, reg)
	b, err := json.MarshalIndent(joinFailureDocs(reg, docs), "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// noteLedgerReading says on stderr what the JSON also says, so a person looking at the terminal sees it: the ledger
// was not there (every count is then zero because nothing was read), or some of its lines could not be read.
func noteLedgerReading(path string, reg ledger.FailureRegister) {
	if !reg.LedgerFound {
		fmt.Fprintf(os.Stderr, "note: no ledger at %s: nothing was read, so the zeros are not \"no failures\" (ledger_found is false)\n", path)
	}
	if reg.UnparsedLines > 0 {
		fmt.Fprintf(os.Stderr, "note: %d ledger line(s) could not be read and are in no count (unparsed_lines)\n", reg.UnparsedLines)
	}
}

// joinFailureDocs joins the table to the register by class name. A class the table has no row for reads
// "undocumented" in every human column; a table row for a class the register does not hold adds nothing.
func joinFailureDocs(reg ledger.FailureRegister, docs map[string]failureDoc) failureReport {
	out := failureReport{
		LedgerFound: reg.LedgerFound, UnparsedLines: reg.UnparsedLines,
		JobRows: reg.JobRows, FailureRows: reg.FailureRows, RefusedAttempts: reg.RefusedAttempts, StackFaultRows: reg.StackFaultRows, StackFaultShare: reg.StackFaultShare, Kinds: reg.Kinds, ClassDerivedRows: reg.ClassDerivedRows,
		CardsTokens: reg.CardsTokens, RowsWithTokens: reg.RowsWithTokens, ExcludedNonProduction: reg.ExcludedNonProduction,
		UndocumentedClasses: []string{}, Classes: make([]failureClassOut, 0, len(reg.Classes)),
	}
	for _, c := range reg.Classes {
		row := failureClassOut{FailureClass: c}
		if d, ok := docs[c.Class]; ok {
			row.Documented, row.Meaning, row.RootCause, row.StructuralFix = true, d.Meaning, d.RootCause, d.StructuralFix
		} else {
			row.Meaning, row.RootCause, row.StructuralFix = undocumentedClass, undocumentedClass, undocumentedClass
			out.UndocumentedClasses = append(out.UndocumentedClasses, c.Class)
		}
		out.Classes = append(out.Classes, row)
	}
	sort.Strings(out.UndocumentedClasses)
	return out
}

// parseFailureTable reads the failure-class table out of a markdown page: the table whose first column is
// `class`, with the columns `meaning`, `root cause` and `structural fix`. An empty root-cause cell reads
// "not recorded" and an empty structural-fix cell "open". A table with a missing column, an empty class cell or
// a class twice is an error, not a silent partial join.
func parseFailureTable(text string) (map[string]failureDoc, error) {
	header, rows, err := readMarkdownTable(text, "class")
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(h)] = i
	}
	for _, need := range []string{"meaning", "root cause", "structural fix"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("the table has no %q column (columns: %s)", need, strings.Join(header, " | "))
		}
	}
	docs := map[string]failureDoc{}
	for _, r := range rows {
		class := strings.Trim(r[0], "` ")
		if class == "" {
			return nil, fmt.Errorf("a table row has an empty class cell")
		}
		if _, dup := docs[class]; dup {
			return nil, fmt.Errorf("class %q has two rows", class)
		}
		d := failureDoc{Meaning: r[col["meaning"]], RootCause: r[col["root cause"]], StructuralFix: r[col["structural fix"]]}
		if d.RootCause == "" {
			d.RootCause = notRecordedCause
		}
		if d.StructuralFix == "" {
			d.StructuralFix = openFix
		}
		docs[class] = d
	}
	return docs, nil
}

var tableSeparatorCell = regexp.MustCompile(`^:?-{3,}:?$`)

// readMarkdownTable finds the first pipe table in text whose first header cell is firstHeader (compared without
// case) and returns its header cells (as written) and its rows, each with exactly as many cells as the header.
// The row after the header must be the separator; the table ends at the first line that is not a table row.
func readMarkdownTable(text, firstHeader string) (header []string, rows [][]string, err error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		cells, ok := splitTableRow(lines[i])
		if !ok || len(cells) == 0 || !strings.EqualFold(cells[0], firstHeader) {
			continue
		}
		if i+1 >= len(lines) {
			return nil, nil, fmt.Errorf("the %q table has no separator row", firstHeader)
		}
		sep, ok := splitTableRow(lines[i+1])
		if !ok || len(sep) != len(cells) {
			return nil, nil, fmt.Errorf("the %q table's header is not followed by a separator row of the same width", firstHeader)
		}
		for _, s := range sep {
			if !tableSeparatorCell.MatchString(s) {
				return nil, nil, fmt.Errorf("the %q table's separator row is malformed", firstHeader)
			}
		}
		for j := i + 2; j < len(lines); j++ {
			row, ok := splitTableRow(lines[j])
			if !ok {
				break
			}
			if len(row) != len(cells) {
				return nil, nil, fmt.Errorf("the %q table has a row of %d cells under a header of %d: %s", firstHeader, len(row), len(cells), lines[j])
			}
			rows = append(rows, row)
		}
		return cells, rows, nil
	}
	return nil, nil, fmt.Errorf("no table whose first column is %q", firstHeader)
}

// splitTableRow splits a pipe-table line into trimmed cells. A "\|" inside a cell is a literal pipe. ok is false
// for a line that is not a table row (it does not start with a pipe).
func splitTableRow(line string) (cells []string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") {
		return nil, false
	}
	line = line[1:]
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cur.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	// A row closed by its last pipe leaves nothing in cur; text after the last pipe is one more cell.
	if rest := strings.TrimSpace(cur.String()); rest != "" {
		cells = append(cells, rest)
	}
	return cells, true
}
