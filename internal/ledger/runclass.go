package ledger

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Run class (register F48 / job-error class C23, 2026-10-09).
//
// A bake, a measurement and a test run wrote to the operator's production ledger and opened cards on the
// PAIR dashboard: in 48 h, 42 cards for a seat named "bake" that no endpoint served, and 444 ledger rows for a
// fixture seat. Every error rate read off the ledger and the fleet-share figure then mixed test traffic with
// real work. A row now carries the CLASS of the run that wrote it, settled in Record (the one path every writer
// takes), and every production view of the ledger skips the labelled rows by default and says how many it
// skipped; a flag brings them back. An unlabelled row is production, so every row written before this existed
// reads as it always did.

// EnvRunClass names the class of this process's runs: a bake or measurement tool sets it once (for example
// LOCAL_OFFLOAD_RUN_CLASS=bake) and every row the process writes, and every PAIR card it would open, is that
// class. Any clean lower-case label works (test, bake, measurement, or a new one such as soak): the set is
// open so that a typo reads as a non-production class a reader can see in the skipped count, never as
// production. "production" (or "prod") says the run is real work and sets nothing.
const EnvRunClass = "LOCAL_OFFLOAD_RUN_CLASS"

// The classes the harness itself writes.
const (
	// ClassTest is test traffic: a go-test row, the fleet-smoke door, a test binary that opened a real ledger.
	ClassTest = "test"
	// ClassBake is a bake or bake-off run.
	ClassBake = "bake"
	// ClassMeasurement is a measurement: a call the caller pinned with pin_reason "measurement", or the
	// fleet-measure door.
	ClassMeasurement = "measurement"
)

const (
	// goTestOrigin is the LOCAL_OFFLOAD_ORIGIN every test binary of this repo sets (the isolating TestMain of the
	// root, delegate and mcpserver suites): a row it wrote reads as test traffic.
	goTestOrigin = "go-test"
	// maxClassLen bounds a class label like the origin label is bounded: one small O_APPEND-atomic line.
	maxClassLen = 32
)

// throwawayRoots are the directories a test binary's own ledgers live under: t.TempDir resolves beneath GOTMPDIR
// when it is set (Go 1.27 and later) and beneath the OS temp directory otherwise. A seam so a test can name
// other directories as "the operator's home".
var throwawayRoots = func() []string {
	roots := []string{os.TempDir()}
	if d := strings.TrimSpace(os.Getenv("GOTMPDIR")); d != "" {
		roots = append(roots, d)
	}
	return roots
}

// cleanClass is CleanClass under its original, unexported name (the process resolver and the tests call it).
func cleanClass(s string) string { return CleanClass(s) }

// CleanClass trims and lower-cases s and refuses anything that is not a short [a-z0-9_-] token, so a malformed
// environment value cannot bloat or break the one-line record. "production" is no class. It is the one rule for
// a run class wherever one is read: the process environment, and both ends of the fleet wire (the asker sends
// only a class that passes it, the node keeps only a class that passes it).
func CleanClass(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > maxClassLen || s == "production" || s == "prod" {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return ""
		}
	}
	return s
}

// classFromOriginPrefix reads a class off a "<class>:<name>" origin label (LOCAL_OFFLOAD_ORIGIN=bake:run-7), for
// the three classes the harness defines. Any other prefix is an ordinary session name.
func classFromOriginPrefix(session string) string {
	i := strings.IndexByte(session, ':')
	if i <= 0 {
		return ""
	}
	switch c := session[:i]; c {
	case ClassTest, ClassBake, ClassMeasurement:
		return c
	}
	return ""
}

// processClass is the class of this process: the explicit variable, else a class-prefixed origin label. The
// go-test origin is deliberately NOT a process class (ClassOf reads it per row): the repo's own tests point
// their PAIR emitters at fixtures on purpose, and a process class switches the emitters off.
func processClass(getenv func(string) string, session string) string {
	if c := cleanClass(getenv(EnvRunClass)); c != "" {
		return c
	}
	return classFromOriginPrefix(session)
}

// RunClassFromEnv is the process class of an environment (Origin.Class) without the pid walk OriginFromEnv
// does: os.Getppid is a process-table snapshot on Windows, which is too slow for a caller that asks on every
// request (the PAIR emitters ask whenever a config is turned into one, and a delegation does that per call:
// two 50 ms-scale waits are enough to reorder the queue-route test). The same rule as ProcessOrigin's, so the
// two cannot name different classes.
func RunClassFromEnv(getenv func(string) string) string {
	return processClass(getenv, sessionFrom(getenv))
}

// ClassOf is the run class of one row: "" for production, else the label. The stamped run_class wins; a row
// without one (written before the label existed, or by a writer that cannot know) is read by what it carries:
// the go-test origin and class-prefixed origins, a call its caller pinned for measurement, and the two doors
// that exist to produce test traffic. Everything else is production, and nothing is guessed from a seat name:
// a seat that is not in the served roster is a real misconfiguration the operator has to see.
func ClassOf(e Entry) string {
	if e.RunClass != "" {
		return e.RunClass
	}
	if e.OriginSession == goTestOrigin {
		return ClassTest
	}
	if c := classFromOriginPrefix(e.OriginSession); c != "" {
		return c
	}
	if e.PinReason == "measurement" {
		return ClassMeasurement
	}
	return ClassOfDoor(e.Door)
}

// ClassOfSession is the class a session label names on its own: "bake" for "bake:run-7", "test" for the go-test
// label, "" for an ordinary label. ClassOf reads the same off a row that names no class, so a label that carries a
// class labels every row written under it.
func ClassOfSession(session string) string { return ClassOf(Entry{OriginSession: session}) }

// ClassOfDoor is the class a door makes on its own: fleet-smoke and fleet-measure exist to produce test and
// measurement traffic, so everything admitted through them is. A caller that must decide before it has a row
// (a card opened when a call begins) asks this instead of building one.
func ClassOfDoor(door string) string {
	switch door {
	case "cli:fleet-smoke":
		return ClassTest
	case "cli:fleet-measure":
		return ClassMeasurement
	}
	return ""
}

// escapedTestWriter reports whether this is a test binary writing a ledger outside its throwaway directory:
// the shape of every test row that ever reached the operator's real ledger (2026-10-03: 1,204 rows; 2026-10-08:
// 444). A test's own ledger lives under the throwaway directory and is not an escape.
func escapedTestWriter(path string) bool {
	if !testing.Testing() {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	for _, r := range throwawayRoots() {
		root, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, abs)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
	}
	return true
}

// settleRunClass labels e when it carries no class: the process class (a bake or measurement tool said so), else
// the escape label of a test binary's ledger, else what the row itself declares. Stamped on the one path every
// writer takes, so no record site can leave it off. A production row stays unlabelled and byte-identical.
func settleRunClass(e *Entry, ledgerClass string) {
	if e.RunClass != "" {
		return
	}
	if c := ProcessOrigin().Class; c != "" {
		e.RunClass = c
		return
	}
	if ledgerClass != "" {
		e.RunClass = ledgerClass
		return
	}
	e.RunClass = ClassOf(*e)
}

// jobClasses maps a job id to the class of the rows that carry it, so the inner row of a labelled job (the
// pipeline's `agent` row, which names the parent job and nothing of the caller's pin) is skipped with its parent.
func jobClasses(entries []Entry) map[string]string {
	m := map[string]string{}
	for _, e := range entries {
		if e.JobID == "" {
			continue
		}
		if c := ClassOf(e); c != "" {
			m[e.JobID] = c
		}
	}
	return m
}

// classIn is ClassOf with the inner-row rule: a row with no class of its own that names a parent job takes the
// parent's.
func classIn(e Entry, jobs map[string]string) string {
	if c := ClassOf(e); c != "" {
		return c
	}
	if e.ParentJobID != "" {
		return jobs[e.ParentJobID]
	}
	return ""
}

// ProductionRows is entries without the non-production rows, and how many JOBS it skipped per class (a dispatch
// marker and an inner row are not jobs: CountsAsJob). The input is not modified. A ledger with no test traffic
// comes back whole with a nil map.
func ProductionRows(entries []Entry) (kept []Entry, excluded map[string]int) {
	jobs := jobClasses(entries)
	parents := ParentJobIDs(entries)
	kept = make([]Entry, 0, len(entries))
	for _, e := range entries {
		c := classIn(e, jobs)
		if c == "" {
			kept = append(kept, e)
			continue
		}
		if CountsAsJob(e, parents) {
			if excluded == nil {
				excluded = map[string]int{}
			}
			excluded[c]++
		}
	}
	return kept, excluded
}

// ReadProduction is ReadAll without the non-production rows (ProductionRows over the file), for a reader that
// reports or trains on real work. ReadAll stays the raw reader.
func ReadProduction(path string) (kept []Entry, excluded map[string]int, err error) {
	all, err := ReadAll(path)
	if err != nil {
		return nil, nil, err
	}
	kept, excluded = ProductionRows(all)
	return kept, excluded, nil
}

// classIndexInFile is the first pass over a ledger file for a streaming reader: the parent job ids
// (ParentJobIDs over the file) and the class of every job that has one.
func classIndexInFile(path string) (parents map[string]bool, jobs map[string]string, err error) {
	parents, jobs = map[string]bool{}, map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return parents, jobs, nil
		}
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		// Only the columns the job rule and ClassOf read: a ledger is read whole here, once more per summary.
		var e struct {
			JobID         string `json:"job_id"`
			ParentJobID   string `json:"parent_job_id"`
			Phase         string `json:"phase"`
			RunClass      string `json:"run_class"`
			OriginSession string `json:"origin_session"`
			PinReason     string `json:"pin_reason"`
			Door          string `json:"door"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.JobID == "" {
			continue
		}
		if e.ParentJobID == "" && e.Phase != PhaseStarted {
			parents[e.JobID] = true
		}
		if c := ClassOf(Entry{RunClass: e.RunClass, OriginSession: e.OriginSession, PinReason: e.PinReason, Door: e.Door}); c != "" {
			jobs[e.JobID] = c
		}
	}
	return parents, jobs, sc.Err()
}
