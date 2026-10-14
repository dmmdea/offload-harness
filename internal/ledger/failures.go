package ledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"os"
	"sort"
	"strings"
)

// The failure-class register: the ledger's failed jobs grouped into failure CLASSES, each with how
// many rows it holds, the door, seat and model they came through, and the card work they spent. It is a reader
// and nothing else: it never writes the ledger and invents no cause. The root cause and the structural fix of a
// class are a human's, kept in docs/systems/failure-register.md and joined by class name outside this package.
//
// The class of a failed row is its reason code (reasoncode.go): the closed set the ledger already stamps on every
// deferred row and every delegate row, which also holds the seven wire defer classes (CodeForDeferClass). The
// ledger has no column named defer_class, and no column for the seat or the model either; model_tier is the one
// identity column, `<node>:<seat>` on a delegation row and the model alias on a cascade row, so
//
//   - door  is the row's door as recorded,
//   - seat  is model_tier as recorded (on a delegation row, a leading ":" cut: written with no node),
//   - model is, on a delegation row, the part of the seat after its last ":" (the same alias on two nodes is one
//     entry): model_tier there is `<node>:<seat alias>`, so it is the SEAT ALIAS (a pool name, say), not a model:
//     the ledger records no model name for a delegation, and the register does not pretend otherwise. On every
//     other row model_tier is the model alias itself and is kept whole, a colon of its own (`family:size`) included.
//
// Nothing is guessed: a row with no door reads "(unknown)", and one with no model_tier reads "(unrecorded)".

const (
	// unknownDoor is the door of a row that carries none (pre-0.129.x rows and writers that stamp none).
	unknownDoor = "(unknown)"
	// unrecorded names a value a row does not carry (no model_tier, no reason); the word TopDeferReasons uses too.
	unrecorded = "(unrecorded)"
)

// FailureCount is a name and how many failed rows carry it.
type FailureCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// FailureGroup is the rows of one class that came through one (door, seat, model).
type FailureGroup struct {
	Door  string `json:"door"`
	Seat  string `json:"seat"`
	Model string `json:"model"`
	Count int    `json:"count"`
	// CardsTokens is the card work of the group's rows that recorded any, nil when none did (see FailureClass).
	CardsTokens *int64 `json:"cards_tokens"`
}

// FailureClass is one failure class: every failed job row of one reason code.
type FailureClass struct {
	Class string `json:"class"`
	// Kind is who owns this class of failure (failurekinds.go): stack_fault, capacity, caller, ceiling, work or
	// unclassified. A fact about the class, not the rows.
	Kind string `json:"kind"`
	Count int    `json:"count"`
	// Share is Count over the register's FailureRows, rounded to four places.
	Share float64 `json:"share"`
	// ExampleReason is the most frequent short reason among the class's rows (ties broken by the smaller text),
	// "(unrecorded)" when no row of the class stored one.
	ExampleReason string `json:"example_reason"`
	// CardsTokens is the card work the class's rows spent: the sum over the rows that MEASURED it, nil (null in
	// JSON) when none did. It is never 0 for "nobody measured". A row measured it when a stored figure above 0
	// says so, or its older token columns hold work, or its stored 0 is the row's own (see failureRow.tokensOf:
	// the row names no node, the reason code says no node ran the job, or the node wrote the row itself). A
	// delegator's 0 for a job a node owned is not a measurement: the node's work is not on that row. RowsWithTokens
	// says how many rows measured it, so a total over fewer rows than Count is a floor.
	CardsTokens    *int64         `json:"cards_tokens"`
	RowsWithTokens int            `json:"rows_with_tokens"`
	Doors          []FailureCount `json:"doors"`
	Seats          []FailureCount `json:"seats"`
	Models         []FailureCount `json:"models"`
	Groups         []FailureGroup `json:"groups"`
}

// FailureKindCount is the failure rows of one kind and their part of FailureRows (four places).
type FailureKindCount struct {
	Kind  string  `json:"kind"`
	Count int     `json:"count"`
	Share float64 `json:"share"`
}

// FailureRegister is the whole report.
type FailureRegister struct {
	// LedgerFound is false when the ledger file does not exist: the zeros beside it then mean "nothing was read",
	// not "nothing failed". UnparsedLines is the non-blank lines that did not decode as a ledger row (garbage, a
	// line cut off mid-file, a non-object, a last line the writer had not finished) and so are in none of the
	// counts below: a ledger that is partly unreadable must not look like a quiet one.
	LedgerFound   bool `json:"ledger_found"`
	UnparsedLines int  `json:"unparsed_lines"`
	// JobRows is the job rows the register looked at, after the window and the run-class rule: a dispatch marker,
	// an inner row whose parent is present and a refused attempt are not jobs. FailureRows of them failed.
	JobRows     int `json:"job_rows"`
	FailureRows int `json:"failure_rows"`
	// RefusedAttempts is the rows that are a node's capacity refusal at dispatch (Entry.Refusal): a place in line,
	// not an outcome. The subtask they belonged to landed elsewhere or ended on a row of its own, which is the one
	// that counts; counted here instead, with the same window and run-class rule, so queue pressure stays visible
	// without a refused-then-landed subtask reading as two failures. A ledger written before the column existed has
	// none marked, and its refusals count as the failures they were then recorded as.
	RefusedAttempts int `json:"refused_attempts"`
	// Kinds counts the failure rows per kind (failurekinds.go), count descending then kind, a kind with no row absent.
	Kinds []FailureKindCount `json:"kinds"`
	// StackFaultRows is the failure rows whose class says the stack itself failed, and StackFaultShare their part of
	// FailureRows (four places), nil (null) when there is no failure row: a share of nothing is not 0. Capacity, caller,
	// ceiling, work and unclassified failures are not in it, and an unclassified one is never counted as the stack's.
	StackFaultRows  int      `json:"stack_fault_rows"`
	StackFaultShare *float64 `json:"stack_fault_share"`
	// ClassDerivedRows is how many failed rows carried no usable reason code (rows from before it was stamped,
	// a code outside the closed set) and were classed here by DeferCode, the classifier Record itself uses.
	ClassDerivedRows int `json:"class_derived_rows"`
	// CardsTokens and RowsWithTokens total the classes' (same rule, same nil).
	CardsTokens    *int64 `json:"cards_tokens"`
	RowsWithTokens int    `json:"rows_with_tokens"`
	// ExcludedNonProduction counts the test, bake and measurement jobs left out, per class (absent when none, and
	// always absent with includeTraffic), the way every production view of the ledger reports them.
	ExcludedNonProduction map[string]int `json:"excluded_non_production,omitempty"`
	// Classes are ordered by Count descending, then Class: the same ledger gives the same bytes.
	Classes []FailureClass `json:"classes"`
}

// failureRow is a ledger row and the one fact Entry cannot carry: whether the line had the cards_tokens key.
// CardsTokens is a plain int, so an absent key and a stored 0 are the same after Unmarshal, and they mean
// different things (Entry.CardsTokens): a missing key is a row from before the schema that measures card work,
// which has only its older token columns to read. A present key says less than it looks like: Record fills it
// from the row's own columns, so a stored 0 is "this row recorded no work", and whether that is a measurement
// depends on whose row it is (tokensOf).
type failureRow struct {
	Entry
	// Declared shadows Entry.CardsTokens's JSON name one level up, so encoding/json fills this pointer and leaves
	// the embedded field at 0; decodeFailureRow then moves the value across.
	Declared *int `json:"cards_tokens"`
}

func decodeFailureRow(line []byte) (failureRow, bool) {
	var r failureRow
	if json.Unmarshal(line, &r) != nil {
		return r, false
	}
	if r.Declared != nil {
		r.Entry.CardsTokens = *r.Declared
	}
	return r, true
}

// tokensOf is the card work of a failed row of the given class and whether the row measured it (see
// FailureClass.CardsTokens). A stored figure above 0 is the ledger's own number (it can be larger than the row's
// own columns, because a call's row carries its earlier inner rows' work). A row that predates the key, or is an
// orphan inner row (its parent never landed, and Record stored 0 on it because the parent row holds the work), has
// no stored figure that says anything: its older columns are all there is. A stored 0 on any other row counts
// only when older columns hold work after all, or when it is a 0 the row owns (zeroIsOwn).
func (r failureRow) tokensOf(class string) (n int64, measured bool) {
	e := r.Entry
	w := int64(CardsTokensOf(e))
	switch {
	case e.ParentJobID != "" || r.Declared == nil:
		return w, w > 0
	case *r.Declared > 0:
		return int64(*r.Declared), true
	case w > 0:
		return w, true
	}
	return 0, zeroIsOwn(class, e)
}

// neverRanClasses are the classes whose reason code says no node ran the job: it was refused, never placed,
// withdrawn before it started, or stopped by a check made before any model ran. A row of one of them that stores
// 0 spent nothing, even when it names the node that refused it. Left out on purpose: queue_deadline (the node may
// still run a job it never confirmed taking back) and canceled (the caller left after a node held the job, which
// may already have worked), and every class that says a run happened (poll_deadline, remote_error, stalls, a failed
// verification, budget, abstention, infrastructure): the work of those is on the node.
var neverRanClasses = map[string]bool{
	ReasonNodeBusy: true, ReasonCapacityWait: true, ReasonQueueFull: true, ReasonNoEligibleNode: true,
	ReasonQueueWithdrawn: true, ReasonShed: true, ReasonDispatchRefused: true,
	ReasonConfig: true, ReasonContract: true, ReasonWrite: true, ReasonBadInput: true,
}

// zeroIsOwn reports whether a stored 0 on a failed row is that row's own measurement rather than a gap. Record
// writes the 0 from the row's own columns, so on a delegator's row for a job a node owned it only means the node's
// work is not here. It is a measurement when the class says no node ran the job (neverRanClasses), when the row
// names no node at all (nothing else ran), or when it is the node's own row for the job (served door or a
// requester): then the figure is that node's reading of its own work.
func zeroIsOwn(class string, e Entry) bool {
	if neverRanClasses[class] {
		return true
	}
	if e.Door == servedDoor || e.Requester != "" {
		return true
	}
	return e.FleetJobID == "" && e.Node == "" && e.NodeID == ""
}

// failureClassOf is the failure class of a job row, whether it was classed here rather than read from a stamped
// code, and whether the row is a failure at all. A cache hit never is (as TopDeferReasons reads it). The caller
// has already dropped the dispatch markers and the inner rows with a parent: they are not jobs.
func failureClassOf(e Entry) (class string, derived, failed bool) {
	if e.CacheHit {
		return "", false, false
	}
	if IsReasonCode(e.ReasonCode) && e.ReasonCode != ReasonOK && e.ReasonCode != ReasonStarted {
		return e.ReasonCode, false, true
	}
	switch {
	case e.Deferred:
		return DeferCode(e.ErrClass, e.Reason), true, true
	case e.AcceptanceResult == "fail":
		return ReasonFailedVerification, true, true
	}
	return "", false, false
}

// delegationTask is the task of the rows whose model_tier is `<node>:<seat alias>` (the delegator's finished row and
// its dispatch marker: the two writers join the node and the seat with a colon).
const delegationTask = "agent_delegate"

// seatAndModel splits an entry's model_tier into the register's seat and model (see the package comment). The cut
// at the last colon is a delegation row's alone: on every other row model_tier is a model alias, which can hold a
// colon of its own (`family:size`, `engine:file`), and cutting it would name the wrong model.
func seatAndModel(e Entry) (seat, model string) {
	seat = strings.TrimSpace(e.ModelTier)
	if e.Task == delegationTask {
		seat = strings.TrimPrefix(seat, ":")
	}
	if seat == "" {
		return unrecorded, unrecorded
	}
	model = seat
	if e.Task == delegationTask {
		if i := strings.LastIndexByte(seat, ':'); i >= 0 {
			model = seat[i+1:]
		}
	}
	if model == "" {
		model = unrecorded
	}
	return seat, model
}

type tokenSum struct {
	sum      int64
	measured int
}

func (t *tokenSum) add(n int64, measured bool) {
	if measured {
		t.sum += n
		t.measured++
	}
}

// ptr is the total as a pointer: nil when no row measured it.
func (t tokenSum) ptr() *int64 {
	if t.measured == 0 {
		return nil
	}
	v := t.sum
	return &v
}

type groupKey struct{ door, seat, model string }

type groupAcc struct {
	count  int
	tokens tokenSum
}

type classAcc struct {
	count   int
	tokens  tokenSum
	reasons map[string]int
	doors   map[string]int
	seats   map[string]int
	models  map[string]int
	groups  map[groupKey]*groupAcc
}

// FailureRegisterFile reads the ledger at path and builds the failure-class register. since (unix seconds, 0 =
// all) keeps only rows from the window; the job and class indexes are the whole ledger's, so an inner row inside
// the window is still judged by a parent that is older (the way SessionSharesFile does). Unless includeTraffic,
// test, bake and measurement traffic is left out and counted per class (the run-class rule every production view
// of the ledger follows). A missing ledger is an empty register with LedgerFound false. Lock-free, like the other
// file readers: a last line still being written is not a row, and is counted in UnparsedLines with every other line
// that does not decode.
func FailureRegisterFile(path string, since int64, includeTraffic bool) (FailureRegister, error) {
	reg := FailureRegister{Classes: []FailureClass{}, Kinds: []FailureKindCount{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return reg, err
	}
	defer f.Close()
	reg.LedgerFound = true
	var rows []failureRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			if r, ok := decodeFailureRow(line); ok {
				rows = append(rows, r)
			} else {
				reg.UnparsedLines++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return reg, err
	}
	all := make([]Entry, len(rows))
	for i, r := range rows {
		all[i] = r.Entry
	}
	parents := ParentJobIDs(all)
	jobs := jobClasses(all)

	accs := map[string]*classAcc{}
	var total tokenSum
	for _, r := range rows {
		e := r.Entry
		if since > 0 && e.TS < since {
			continue
		}
		if !includeTraffic {
			if c := classIn(e, jobs); c != "" {
				if CountsAsJob(e, parents) && !e.Refusal {
					if reg.ExcludedNonProduction == nil {
						reg.ExcludedNonProduction = map[string]int{}
					}
					reg.ExcludedNonProduction[c]++
				}
				continue
			}
		}
		if !CountsAsJob(e, parents) {
			continue
		}
		if e.Refusal {
			reg.RefusedAttempts++
			continue
		}
		reg.JobRows++
		class, derived, failed := failureClassOf(e)
		if !failed {
			continue
		}
		reg.FailureRows++
		if derived {
			reg.ClassDerivedRows++
		}
		n, measured := r.tokensOf(class)
		total.add(n, measured)

		a := accs[class]
		if a == nil {
			a = &classAcc{reasons: map[string]int{}, doors: map[string]int{}, seats: map[string]int{}, models: map[string]int{},
				groups: map[groupKey]*groupAcc{}}
			accs[class] = a
		}
		a.count++
		a.tokens.add(n, measured)
		if reason := ShortReason(e.Reason); reason != "" {
			a.reasons[reason]++
		}
		door := e.Door
		if door == "" {
			door = unknownDoor
		}
		seat, model := seatAndModel(e)
		a.doors[door]++
		a.seats[seat]++
		a.models[model]++
		k := groupKey{door, seat, model}
		g := a.groups[k]
		if g == nil {
			g = &groupAcc{}
			a.groups[k] = g
		}
		g.count++
		g.tokens.add(n, measured)
	}

	reg.CardsTokens, reg.RowsWithTokens = total.ptr(), total.measured
	kinds := map[string]int{}
	for class, a := range accs {
		c := FailureClass{
			Class: class, Kind: FailureKindOf(class), Count: a.count,
			Share:          share(a.count, reg.FailureRows),
			ExampleReason:  topReason(a.reasons),
			CardsTokens:    a.tokens.ptr(),
			RowsWithTokens: a.tokens.measured,
			Doors:          sortedCounts(a.doors), Seats: sortedCounts(a.seats), Models: sortedCounts(a.models),
			Groups: make([]FailureGroup, 0, len(a.groups)),
		}
		for k, g := range a.groups {
			c.Groups = append(c.Groups, FailureGroup{Door: k.door, Seat: k.seat, Model: k.model, Count: g.count, CardsTokens: g.tokens.ptr()})
		}
		sort.Slice(c.Groups, func(i, j int) bool {
			x, y := c.Groups[i], c.Groups[j]
			switch {
			case x.Count != y.Count:
				return x.Count > y.Count
			case x.Door != y.Door:
				return x.Door < y.Door
			case x.Seat != y.Seat:
				return x.Seat < y.Seat
			}
			return x.Model < y.Model
		})
		reg.Classes = append(reg.Classes, c)
		kinds[c.Kind] += c.Count
	}
	for kind, n := range kinds {
		reg.Kinds = append(reg.Kinds, FailureKindCount{Kind: kind, Count: n, Share: share(n, reg.FailureRows)})
	}
	sort.Slice(reg.Kinds, func(i, j int) bool {
		if reg.Kinds[i].Count != reg.Kinds[j].Count {
			return reg.Kinds[i].Count > reg.Kinds[j].Count
		}
		return reg.Kinds[i].Kind < reg.Kinds[j].Kind
	})
	if reg.FailureRows > 0 {
		reg.StackFaultRows = kinds[KindStackFault]
		s := share(reg.StackFaultRows, reg.FailureRows)
		reg.StackFaultShare = &s
	}
	sort.Slice(reg.Classes, func(i, j int) bool {
		if reg.Classes[i].Count != reg.Classes[j].Count {
			return reg.Classes[i].Count > reg.Classes[j].Count
		}
		return reg.Classes[i].Class < reg.Classes[j].Class
	})
	return reg, nil
}

// share is n over total rounded to four places.
func share(n, total int) float64 { return math.Round(float64(n)/float64(total)*1e4) / 1e4 }

// topReason is the most frequent reason, the smaller text on a tie, "(unrecorded)" when there is none.
func topReason(counts map[string]int) string {
	best, bestN := "", 0
	for r, n := range counts {
		if n > bestN || (n == bestN && r < best) {
			best, bestN = r, n
		}
	}
	if bestN == 0 {
		return unrecorded
	}
	return best
}

// sortedCounts is a count map as a list, count descending then name.
func sortedCounts(m map[string]int) []FailureCount {
	out := make([]FailureCount, 0, len(m))
	for name, n := range m {
		out = append(out, FailureCount{Name: name, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}
