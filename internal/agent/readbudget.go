package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"path"
	"path/filepath"
	"strings"
)

// The read budget (register D-103).
//
// defaultMaxSameTool counts CALLS, and for read_file a call is the wrong unit:
// the tool pages (offset / limit) and its own hint tells the seat to ("use
// offset=N to continue"), so a seat doing exactly as it was told spends one call
// per page. Live 2026-10-01 00:16: one 12,005-character context document, a seat
// whose window probe had fallen back to 8,192 tokens, twelve steps, eight
// read_file calls of at most 1,090 characters each, and the ninth refused "now
// DISABLED". Six of the eight results were exactly 1,090 characters long, which
// is what the loop's cut of ONE result at its window-derived cap looks like, and
// eight results of that size hold 8,720 of the document's 12,005 characters:
// under a count cap of eight no seat could have read it to the end.
//
// So read_file is metered in characters, and the count cap keeps counting what it
// was always for:
//
//   - Every read_file result is charged to a per-run budget at the length the
//     transcript keeps (after the loop-boundary trim: the figure the effect ledger
//     records as obs_chars). Once the charge reaches the budget the next read_file
//     call is refused and the tool is withdrawn for the rest of the run, by the
//     same disabledTools mechanism the same-name cap uses. The call that crosses
//     the line completes, so the overshoot is at most one result.
//
//   - A call counts against the same-name cap unless it is a NEW page of a file
//     the run has already read. The first read of each path counts, so for
//     read_file the cap is a cap on FILES opened: a six-file reconnaissance costs
//     six, exactly as before, and the ninth distinct file is still refused. A
//     page of a file already read costs characters, not a call. Calls that
//     cannot be told from a repeat or a miss keep counting as before: an exact
//     repeat of an earlier call (the exact-repeat refusal is untouched), a read
//     that failed (a path that does not exist is not "being read"), and a call
//     whose path cannot be read from its arguments.
//
//   - Setup replays are not charged (setup.go: the model's own budget under the
//     breakers stays whole), and neither is a call that did not run.
//
// Why paging of one file may skip the count cap: the cap exists to catch a model
// that keeps re-issuing a near-duplicate call instead of progressing. A different
// file is progress, and so is the next page of the same one; what is not progress
// is asking again for what it has, which the exact-repeat refusal catches byte for
// byte and the budget bounds in volume. The thrash the breakers bound (a seat
// hunting through read_file / search_files / list_dir until the steps run out)
// stays bounded by the step budget, the forced final step (D-89), the exact-repeat
// refusal (with the replay feeding it, D-48) and the withdrawal of a tool refused
// twice for one call (D-49); none of those is touched here.

// readFileTool is the one tool the loop meters in characters.
const readFileTool = "read_file"

// readBudgetChars is the budget a run starts with. An explicit WithReadBudgetChars
// wins; otherwise it is the larger of the step budget and the same-name cap,
// times the loop-boundary cap on one result (toolResultCapChars): what a seat
// could take in by reading one MAXIMAL result per call for the whole of either
// bound. Zero means no budget (WithoutReadBudget).
//
// Why that number and not a smaller one. Measured on the delegation log, the
// 3,374 runs of 2026-09-07 to 2026-10-01 (the days it carries the per-call
// trace) that issued read_file themselves: the 2,686 that finished took in 6.4 KB
// at the median, 25.7 KB at p90, 95 KB at p99 and 157 KB at most through
// read_file, on windows from 8,192 to 262,144 tokens (a single result reached
// 120 KB); the count cap refused 114 of those runs and 79 of them still finished.
// A constant budget would starve the large windows. A budget of "the count cap in
// characters" (8 maximal results) is what the count cap already was: a seat on a
// small window reads at the cap (six of the incident's eight results were exactly
// 1,090 characters), so it would be refused at the ninth or tenth read, the
// incident again. Sized to the steps, the budget cannot refuse a seat that issues
// one read per step: before its k-th read it has taken in at most k-1 results of
// at most the cap each, and k never exceeds the step budget. What it bounds is the
// rest, many reads in ONE step (214 of those runs issued parallel reads, up to 12
// in a step), the one way a run could put more than a window of file into the
// transcript at once. The step budget stays what bounds sequential reading, as it
// always did.
func (l *Loop) readBudgetChars() int {
	if l.noReadBudget {
		return 0
	}
	if l.readBudget > 0 {
		return l.readBudget
	}
	n := l.maxSameTool
	if n <= 0 { // the same-name cap is off: the count it would have had
		n = defaultMaxSameTool
	}
	if l.maxSteps > n {
		n = l.maxSteps
	}
	c := l.toolResultCapChars()
	if c > math.MaxInt/n {
		return math.MaxInt
	}
	return n * c
}

// WithReadBudgetChars sets the read_file character budget of a run explicitly
// (see readBudgetChars for the default). A non-positive value restores the
// derived default; it never turns the budget off, WithoutReadBudget does.
func (l *Loop) WithReadBudgetChars(n int) *Loop {
	if n < 0 {
		n = 0
	}
	l.readBudget, l.noReadBudget = n, false
	return l
}

// WithoutReadBudget turns the read budget off: every read_file call then counts
// against the same-name cap, which is the loop as it was before D-103. For tests
// whose subject is the call count, and the one-line rollback.
func (l *Loop) WithoutReadBudget() *Loop { l.noReadBudget = true; return l }

// readLedger is Run's account of read_file: per-Run state, like the breaker
// maps, because --serve shares one *Loop across concurrent handlers. A nil
// ledger is "no budget": every method is then a no-op and read_file is counted
// like any other tool.
type readLedger struct {
	budget int             // characters this run may take in through read_file
	chars  int             // characters read_file results have put in the transcript so far
	calls  int             // read_file calls seen this run, paged or not (the refusal text says how many)
	opened map[string]bool // paths read successfully at least once
}

// newReadLedger opens this run's ledger, or returns nil when the budget is off.
func (l *Loop) newReadLedger() *readLedger {
	b := l.readBudgetChars()
	if b <= 0 {
		return nil
	}
	return &readLedger{budget: b, opened: map[string]bool{}}
}

// sawCall counts one read_file call that reached the breakers.
func (r *readLedger) sawCall(tool string) {
	if r != nil && tool == readFileTool {
		r.calls++
	}
}

// callCount is the number of read_file calls this run has made, or 0 for any
// other tool or no ledger. The same-name refusal quotes it in place of the
// capped count, which leaves paged calls out.
func (r *readLedger) callCount(tool string) int {
	if r == nil || tool != readFileTool {
		return 0
	}
	return r.calls
}

// isNewPage reports whether this call is a page of a file the run has already
// read: its path was read successfully before, and the call itself is not an
// exact repeat (exactCount is the number of times this exact name+args pair has
// now been seen, this call included).
func (r *readLedger) isNewPage(call ToolCall, exactCount int) bool {
	if r == nil || call.Name != readFileTool || exactCount > 1 {
		return false
	}
	p, ok := readFilePath(call.Args)
	return ok && r.opened[p]
}

// spent is the refusal for a read_file call made after the budget ran out, or
// "" while there is budget left (or no budget, or another tool).
func (r *readLedger) spent(tool string) string {
	if r == nil || tool != readFileTool || r.chars < r.budget {
		return ""
	}
	return fmt.Sprintf("NOT executed: %s has already returned %d characters in this task, which is its read budget (%d), so it is now DISABLED for the rest of this task. Proceed with the remaining steps using what you already have; %s is no longer available.", tool, r.chars, r.budget, tool)
}

// note charges a read_file result that reached the transcript. content is the
// text AFTER the loop-boundary trim, the length the model received. A call that
// did not run (a breaker refusal, an env-rule block, a park) charges nothing,
// and only a result that succeeded marks its path as read.
func (r *readLedger) note(call ToolCall, content string, eff EffectStatus, isErr bool) {
	if r == nil || call.Name != readFileTool || eff == EffectNone {
		return
	}
	r.chars += len(content)
	if eff == EffectCommitted && !isErr {
		if p, ok := readFilePath(call.Args); ok {
			r.opened[p] = true
		}
	}
}

// readFilePath reads the file a read_file call names out of its arguments, in a
// form where two spellings of one path agree ("a/b.md", "./a/b.md", "a//b.md").
// ok is false when the arguments do not parse or name no path: such a call
// cannot be recognised as a page of anything, so it counts like any other call.
func readFilePath(args string) (string, bool) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil || strings.TrimSpace(in.Path) == "" {
		return "", false
	}
	return path.Clean(filepath.ToSlash(in.Path)), true
}
