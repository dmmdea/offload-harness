package ledger

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"sort"
	"strings"
)

// The failure-class register (RSI-OH-1): the ledger's failed jobs grouped into failure CLASSES, each with how
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
//   - seat  is model_tier as recorded (a leading ":" cut: a delegation row written with no node),
//   - model is the part of the seat after its last ":" (the same alias on two nodes is one model).
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
	Count int    `json:"count"`
	// Share is Count over the register's FailureRows, rounded to four places.
	Share float64 `json:"share"`
	// ExampleReason is the most frequent short reason among the class's rows (ties broken by the smaller text),
	// "(unrecorded)" when no row of the class stored one.
	ExampleReason string `json:"example_reason"`
	// CardsTokens is the card work the class's rows spent: the sum over the rows that MEASURED it, nil (null in
	// JSON) when none did. It is never 0 for "nobody measured": a row measured it when it carries the
	// cards_tokens key (a measured 0 is a real 0: the row deferred before any model ran), or, on a row that
	// predates the key, when its older token columns hold work. RowsWithTokens says how many rows that was, so a
	// total over fewer rows than Count is a floor.
	CardsTokens    *int64         `json:"cards_tokens"`
	RowsWithTokens int            `json:"rows_with_tokens"`
	Doors          []FailureCount `json:"doors"`
	Seats          []FailureCount `json:"seats"`
	Models         []FailureCount `json:"models"`
	Groups         []FailureGroup `json:"groups"`
}

// FailureRegister is the whole report.
type FailureRegister struct {
	// JobRows is the job rows the register looked at, after the window and the run-class rule: a dispatch marker
	// and an inner row whose parent is present are not jobs. FailureRows of them failed.
	JobRows     int `json:"job_rows"`
	FailureRows int `json:"failure_rows"`
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
// different things (Entry.CardsTokens): the key's presence marks a row written under the schema that measures
// card work, a missing key a row that never did.
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

// tokensOf is the card work of a failed row and whether the row measured it (see FailureClass.CardsTokens).
func (r failureRow) tokensOf() (n int64, measured bool) {
	e := r.Entry
	if e.ParentJobID != "" {
		// An orphan inner row (its parent never landed): Record stored 0 on it on purpose, because the parent
		// row holds the work, so the stored figure says nothing; the older columns are all there is.
		w := CardsTokensOf(e)
		return int64(w), w > 0
	}
	if r.Declared != nil {
		return int64(*r.Declared), true
	}
	w := CardsTokensOf(e)
	return int64(w), w > 0
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

// seatAndModel splits an entry's model_tier into the register's seat and model (see the package comment).
func seatAndModel(modelTier string) (seat, model string) {
	seat = strings.TrimPrefix(strings.TrimSpace(modelTier), ":")
	if seat == "" {
		return unrecorded, unrecorded
	}
	model = seat
	if i := strings.LastIndexByte(seat, ':'); i >= 0 {
		model = seat[i+1:]
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
// test, bake and measurement traffic is left out and counted per class (register F48). A missing ledger is an
// empty register. Lock-free, like the other file readers: a partial last line is skipped.
func FailureRegisterFile(path string, since int64, includeTraffic bool) (FailureRegister, error) {
	reg := FailureRegister{Classes: []FailureClass{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return reg, err
	}
	defer f.Close()
	var rows []failureRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		if line := sc.Bytes(); len(line) > 0 {
			if r, ok := decodeFailureRow(line); ok {
				rows = append(rows, r)
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
				if CountsAsJob(e, parents) {
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
		reg.JobRows++
		class, derived, failed := failureClassOf(e)
		if !failed {
			continue
		}
		reg.FailureRows++
		if derived {
			reg.ClassDerivedRows++
		}
		n, measured := r.tokensOf()
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
		seat, model := seatAndModel(e.ModelTier)
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
	for class, a := range accs {
		c := FailureClass{
			Class: class, Count: a.count,
			Share:          math.Round(float64(a.count)/float64(reg.FailureRows)*1e4) / 1e4,
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
	}
	sort.Slice(reg.Classes, func(i, j int) bool {
		if reg.Classes[i].Count != reg.Classes[j].Count {
			return reg.Classes[i].Count > reg.Classes[j].Count
		}
		return reg.Classes[i].Class < reg.Classes[j].Class
	})
	return reg, nil
}

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
