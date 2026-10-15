package ledger

import (
	"encoding/json"
	"strings"
	"testing"
)

// A ledger row is a JSON object. encoding/json decodes the literal `null` into a struct without an error and without
// touching it, so a line holding only `null` used to read as a row with every field at its zero: a job that did not
// fail in the failure register, a completed call with no task in the savings summary. It is not a row; it is a
// damaged line, and the register's promise is that a partly unreadable ledger must not look like a quiet one.
//
// `{}` is different on purpose. It IS an object, so it is a row: every column of the ledger is optional (a row from
// before a column existed lacks it, and the writer's own rows omit what is empty), which means a reader cannot tell
// a minimal old row from an empty one by looking inside, and a rule that tried would be a schema rule that every
// new column has to be remembered in. The test pins what `{}` does so the choice is explicit: it counts as a job
// row that did not fail, in the register and in the savings summary alike, and is not in unparsed_lines.
const nullLineFailure = `{"ts":3,"task":"agent_delegate","deferred":true,"reason_code":"budget","job_id":"c"}`

func nullLineLedger(t *testing.T) string {
	t.Helper()
	return writeFailureLedger(t,
		`null`,
		`{}`,
		nullLineFailure,
		`{"ts":4,"task":"agent_delegate","deferred":true,"reason_code":"budget","job_id":"d"`, // truncated
	)
}

func TestALiteralNullLineIsUnparsedAndAnEmptyObjectIsARow(t *testing.T) {
	path := nullLineLedger(t)

	reg, err := FailureRegisterFile(path, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if reg.UnparsedLines != 2 {
		t.Errorf("unparsed_lines = %d, want 2 (the null line and the truncated line; {} is an object, so a row)", reg.UnparsedLines)
	}
	if reg.JobRows != 2 || reg.FailureRows != 1 {
		t.Errorf("job_rows/failure_rows = %d/%d, want 2/1 (the {} row, which did not fail, and the failure row)", reg.JobRows, reg.FailureRows)
	}
	b, _ := json.Marshal(reg)
	if !strings.Contains(string(b), `"unparsed_lines":2`) {
		t.Errorf("the register JSON does not carry unparsed_lines 2: %s", b)
	}
}

// The raw readers agree with the register on what a row is, so the savings summary does not count a `null` line as
// a completed call of no task while the register calls the same line unreadable.
func TestTheOtherLedgerReadersSkipANullLineToo(t *testing.T) {
	path := nullLineLedger(t)

	s, err := SummarizeFile(path, 0, Prices{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 2 || s.Completed != 1 || s.Deferred != 1 {
		t.Errorf("calls/completed/deferred = %d/%d/%d, want 2/1/1 (the {} row and the failure row; the null line is neither)", s.Calls, s.Completed, s.Deferred)
	}
	rows, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("ReadAll rows = %d, want 2 (the {} row and the failure row)", len(rows))
	}
}
