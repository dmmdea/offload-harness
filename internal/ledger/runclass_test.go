package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Run class (register F48 / job-error class C23, 2026-10-09): a bake, a measurement and a test run all wrote
// to the operator's production ledger and opened cards on the PAIR dashboard (42 cards and 444 rows in 48 h),
// so every error rate and the fleet-share figure mixed test traffic with real work. The ledger now labels a
// row's run class at the one place every writer passes through (Record), and every production view skips the
// labelled rows by default and says how many it skipped, so nothing is hidden.

func TestOriginResolvesTheRunClassFromTheEnvironmentThenTheOriginPrefix(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"nothing set is production", nil, ""},
		{"an explicit class", map[string]string{"LOCAL_OFFLOAD_RUN_CLASS": "bake"}, "bake"},
		{"trimmed and lower-cased", map[string]string{"LOCAL_OFFLOAD_RUN_CLASS": "  Measurement "}, "measurement"},
		{"a new label is kept (the set is not closed: a typo must not read as production)", map[string]string{"LOCAL_OFFLOAD_RUN_CLASS": "bak"}, "bak"},
		{"a malformed label is refused", map[string]string{"LOCAL_OFFLOAD_RUN_CLASS": "ba ke!"}, ""},
		{"an oversized label is refused", map[string]string{"LOCAL_OFFLOAD_RUN_CLASS": strings.Repeat("x", 33)}, ""},
		{"production says production", map[string]string{"LOCAL_OFFLOAD_RUN_CLASS": "production"}, ""},
		{"a class-prefixed origin", map[string]string{"LOCAL_OFFLOAD_ORIGIN": "bake:run-7"}, "bake"},
		{"a test-prefixed origin", map[string]string{"LOCAL_OFFLOAD_ORIGIN": "test:suite"}, "test"},
		{"a measurement-prefixed origin", map[string]string{"LOCAL_OFFLOAD_ORIGIN": "measurement:abc"}, "measurement"},
		{"an unknown prefix is only a session name", map[string]string{"LOCAL_OFFLOAD_ORIGIN": "opencode:9"}, ""},
		{"the explicit class wins over the origin prefix", map[string]string{"LOCAL_OFFLOAD_RUN_CLASS": "measurement", "LOCAL_OFFLOAD_ORIGIN": "bake:run-7"}, "measurement"},
		{"a Claude session id is no class", map[string]string{"CLAUDE_CODE_SESSION_ID": "7c561f0b-2e24-4047-baf9-3a81dadd3694"}, ""},
	}
	for _, c := range cases {
		if got := originFrom(envMap(c.env), 41, 7).Class; got != c.want {
			t.Errorf("%s: Class = %q, want %q", c.name, got, c.want)
		}
	}
	// The uncached twin applies the same rule, so a caller that must read another environment cannot disagree.
	if got := OriginFromEnv(envMap(map[string]string{"LOCAL_OFFLOAD_RUN_CLASS": "bake"})).Class; got != "bake" {
		t.Errorf("OriginFromEnv Class = %q, want bake", got)
	}
}

// go-test is a ROW label only: the repo's own tests point their PAIR emitters at fixtures, so a process whose
// origin is go-test is not a process class (Origin.Class stays empty), but a row it wrote reads as test traffic.
func TestTheGoTestOriginIsARowLabelNotAProcessClass(t *testing.T) {
	if got := originFrom(envMap(map[string]string{"LOCAL_OFFLOAD_ORIGIN": "go-test"}), 1, 1).Class; got != "" {
		t.Fatalf("a go-test origin must not be a process class: %q", got)
	}
	if got := ClassOf(Entry{Task: "agent", OriginSession: "go-test"}); got != "test" {
		t.Fatalf("a row written by go-test reads as test traffic (the 10-03 rows predate run_class): %q", got)
	}
}

func TestClassOfReadsTheLabelThenTheOriginThenWhatTheCallerDeclared(t *testing.T) {
	cases := []struct {
		name string
		e    Entry
		want string
	}{
		{"an ordinary row", Entry{Task: "summarize", OriginSession: "7c561f0b"}, ""},
		{"the stamped label wins", Entry{Task: "agent_delegate", RunClass: "bake", PinReason: "privacy", OriginSession: "go-test"}, "bake"},
		{"a class-prefixed origin on an old row", Entry{Task: "agent", OriginSession: "bake:run-7"}, "bake"},
		{"a call pinned for measurement", Entry{Task: "agent_delegate", PinReason: "measurement", Route: "local"}, "measurement"},
		{"a call pinned for another reason is real work", Entry{Task: "agent_delegate", PinReason: "privacy", Route: "local"}, ""},
		{"a hint is real work", Entry{Task: "agent_delegate", RouteAsked: "local", Route: "auto"}, ""},
		{"the fleet smoke door", Entry{Task: "agent_delegate", Door: "cli:fleet-smoke"}, "test"},
		{"the fleet measure door", Entry{Task: "generate_image", Door: "cli:fleet-measure"}, "measurement"},
		{"another door is real work", Entry{Task: "generate_image", Door: "cli:generate-image"}, ""},
	}
	for _, c := range cases {
		if got := ClassOf(c.e); got != c.want {
			t.Errorf("%s: ClassOf = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRecordStampsTheRunClassOnEveryRowAndLeavesProductionRowsByteIdentical(t *testing.T) {
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-class-1")
	t.Setenv("LOCAL_OFFLOAD_RUN_CLASS", "")
	resetOriginForTest()
	t.Cleanup(resetOriginForTest)

	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	rows := []Entry{
		{Task: "summarize", TokensIn: 10, TokensOut: 2},                                         // 0 production
		{Task: "agent_delegate", PinReason: "measurement", Route: "local", RouteAsked: "local"}, // 1 a pinned measurement
		{Task: "generate_image", Door: "cli:fleet-measure"},                                     // 2 the measure door
		{Task: "agent_delegate", Door: "cli:fleet-smoke"},                                       // 3 the smoke door
		{Task: "agent", RunClass: "soak"},                                                       // 4 a caller-named class is kept
	}
	for _, e := range rows {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	got, err := ReadAll(p)
	if err != nil || len(got) != len(rows) {
		t.Fatalf("ReadAll: err=%v rows=%d", err, len(got))
	}
	for i, want := range []string{"", "measurement", "measurement", "test", "soak"} {
		if got[i].RunClass != want {
			t.Errorf("row %d (%s): run_class = %q, want %q", i, got[i].Task, got[i].RunClass, want)
		}
	}
	// A production row carries no run_class key at all: the schema of every ordinary row is unchanged.
	raw, _ := os.ReadFile(p)
	first := strings.Split(strings.TrimSpace(string(raw)), "\n")[0]
	if strings.Contains(first, "run_class") {
		t.Fatalf("a production row must not carry run_class: %s", first)
	}
}

func TestRecordStampsTheProcessClassFromTheEnvironment(t *testing.T) {
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-class-2")
	t.Setenv("LOCAL_OFFLOAD_RUN_CLASS", "bake")
	resetOriginForTest()
	t.Cleanup(resetOriginForTest)

	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{{Task: "summarize", TokensIn: 5}, {Task: "agent_delegate"}, {Task: "agent_delegate", RunClass: "measurement"}} {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	got, _ := ReadAll(p)
	if len(got) != 3 || got[0].RunClass != "bake" || got[1].RunClass != "bake" || got[2].RunClass != "measurement" {
		t.Fatalf("every row of a bake process is a bake row, and a caller's own class is kept: %+v", got)
	}
}

// A test binary that opens a ledger outside its throwaway directory is an escape (the 2026-10-03 and 10-08 rows
// reached the operator's ledger this way): its rows are labelled test whatever the environment says. A ledger in
// the throwaway directory stays unlabelled, which is what every reader test relies on.
func TestATestBinaryWritingOutsideItsTempDirectoryLabelsItsRowsTest(t *testing.T) {
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-class-3")
	t.Setenv("LOCAL_OFFLOAD_RUN_CLASS", "")
	resetOriginForTest()
	t.Cleanup(resetOriginForTest)

	inside := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(inside)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Entry{Task: "summarize", TokensIn: 1}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if got, _ := ReadAll(inside); len(got) != 1 || got[0].RunClass != "" {
		t.Fatalf("a throwaway ledger stays unlabelled: %+v", got)
	}

	// Name the "operator's home" as a directory that does not contain the ledger.
	prev := throwawayRoots
	throwawayRoots = func() []string { return []string{filepath.Join(t.TempDir(), "elsewhere")} }
	t.Cleanup(func() { throwawayRoots = prev })
	escaped := filepath.Join(t.TempDir(), "ledger-escaped.jsonl")
	l, err = Open(escaped)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Entry{Task: "agent_delegate", Phase: PhaseStarted, JobID: "agd-1"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if got, _ := ReadAll(escaped); len(got) != 1 || got[0].RunClass != "test" {
		t.Fatalf("a row a test binary wrote outside its throwaway directory is test traffic: %+v", got)
	}
}

// fixtureRows is one ledger as an operator's would look after a busy day: real work, a bake (labelled), a
// measurement (declared by its pin only), a go-test escape, and the inner rows of the local jobs.
func fixtureRows() []Entry {
	return []Entry{
		{TS: 100, Task: "summarize", TokensIn: 100, TokensOut: 20, OriginSession: "sess-real"},
		{TS: 101, Task: "agent_delegate", JobID: "agd-real", Phase: PhaseStarted, OriginSession: "sess-real"},
		{TS: 102, Task: "agent_delegate", JobID: "agd-real", TokensOut: 30, OriginSession: "sess-real"},
		{TS: 102, Task: "agent", ParentJobID: "agd-real", TokensIn: 400, OriginSession: "sess-real"},
		{TS: 110, Task: "agent_delegate", JobID: "agd-bake", Phase: PhaseStarted, RunClass: "bake"},
		{TS: 111, Task: "agent_delegate", JobID: "agd-bake", Deferred: true, Reason: "agent seat \"bake\" is not in the endpoint's served roster", RunClass: "bake"},
		{TS: 111, Task: "agent", ParentJobID: "agd-bake", TokensIn: 900, RunClass: ""}, // the inner row names no class of its own
		{TS: 120, Task: "agent_delegate", JobID: "agd-meas", Phase: PhaseStarted, PinReason: "measurement", Route: "local"},
		{TS: 121, Task: "agent_delegate", JobID: "agd-meas", TokensOut: 50, PinReason: "measurement", Route: "local"},
		{TS: 121, Task: "agent", ParentJobID: "agd-meas", TokensIn: 700},
		{TS: 130, Task: "summarize", TokensIn: 60, TokensOut: 6, OriginSession: "go-test"},
		{TS: 131, Task: "agent", ParentJobID: "agd-orphan-real", TokensIn: 30, OriginSession: "sess-real"}, // an orphan inner row of a real job stays a job
	}
}

func writeRows(t *testing.T, rows []Entry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		b, _ := json.Marshal(e)
		f.Write(append(b, '\n'))
	}
	f.Close()
	return p
}

func TestProductionRowsDropsLabelledRowsAndTheInnerRowsOfLabelledJobsAndCountsThem(t *testing.T) {
	kept, excluded := ProductionRows(fixtureRows())
	var jobs []string
	for _, e := range kept {
		jobs = append(jobs, e.Task+"/"+e.JobID+e.ParentJobID)
	}
	want := []string{
		"summarize/", "agent_delegate/agd-real", "agent_delegate/agd-real", "agent/agd-real", "agent/agd-orphan-real",
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("kept = %v, want %v", jobs, want)
	}
	// One count per JOB: a marker and an inner row are not jobs (ledger.CountsAsJob), the go-test summarize is one.
	if wantEx := map[string]int{"bake": 1, "measurement": 1, "test": 1}; !reflect.DeepEqual(excluded, wantEx) {
		t.Fatalf("excluded = %v, want %v", excluded, wantEx)
	}
}

func TestProductionRowsOfACleanLedgerIsTheLedgerAndNoCounts(t *testing.T) {
	rows := []Entry{{Task: "summarize", TokensIn: 5, OriginSession: "sess-real"}, {Task: "classify", TokensIn: 7}}
	kept, excluded := ProductionRows(rows)
	if len(kept) != 2 || len(excluded) != 0 {
		t.Fatalf("a ledger with no test traffic is unchanged: kept=%d excluded=%v", len(kept), excluded)
	}
}

func TestSummarizeFileExcludesNonProductionTrafficByDefaultAndSaysSo(t *testing.T) {
	p := writeRows(t, fixtureRows())
	s, err := SummarizeFile(p, 0, DefaultPrices)
	if err != nil {
		t.Fatal(err)
	}
	// Production: the summarize (100 in, 20 out), the agd-real job (30 out) with its inner row's 400 saved, and the
	// orphan inner row (30 saved) counted as its job.
	if s.Calls != 3 || s.Completed != 3 || s.Deferred != 0 {
		t.Fatalf("production calls: %+v", s)
	}
	if s.TokensSaved != 100+400+30 || s.TokensOut != 20+30 {
		t.Fatalf("production tokens: saved=%d out=%d", s.TokensSaved, s.TokensOut)
	}
	if want := map[string]int{"bake": 1, "measurement": 1, "test": 1}; !reflect.DeepEqual(s.ExcludedNonProduction, want) {
		t.Fatalf("the summary must say what it skipped: %v", s.ExcludedNonProduction)
	}

	all, err := SummarizeFileWith(p, 0, DefaultPrices, true)
	if err != nil {
		t.Fatal(err)
	}
	if all.Calls != 6 || all.Deferred != 1 || len(all.ExcludedNonProduction) != 0 {
		t.Fatalf("with test traffic included every job counts and nothing is reported skipped: %+v", all)
	}
	if all.TokensSaved != 100+400+900+700+60+30 || all.TokensOut != 20+30+50+6 {
		t.Fatalf("included tokens: saved=%d out=%d", all.TokensSaved, all.TokensOut)
	}
}

func TestSummarizeFileCountsOnlyTheSkippedRowsInsideTheWindow(t *testing.T) {
	p := writeRows(t, fixtureRows())
	s, err := SummarizeFile(p, 115, DefaultPrices) // after the bake (110-111), before the measurement (120)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"measurement": 1, "test": 1}; !reflect.DeepEqual(s.ExcludedNonProduction, want) {
		t.Fatalf("excluded inside the window: %v", s.ExcludedNonProduction)
	}
}

func TestTopDeferReasonsSkipsNonProductionDefersByDefault(t *testing.T) {
	rows := fixtureRows()
	rows = append(rows, Entry{TS: 140, Task: "summarize", Deferred: true, Reason: "real outage", OriginSession: "sess-real"})
	p := writeRows(t, rows)
	got, err := TopDeferReasons(p, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "real outage" {
		t.Fatalf("the bake's roster defer must not rank among the real reasons: %+v", got)
	}
	all, err := TopDeferReasonsWith(p, 0, 5, true)
	if err != nil || len(all) != 2 {
		t.Fatalf("included: %+v err=%v", all, err)
	}
}

func TestReadProductionIsReadAllWithoutTheLabelledRows(t *testing.T) {
	p := writeRows(t, fixtureRows())
	kept, excluded, err := ReadProduction(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 5 || excluded["bake"] != 1 {
		t.Fatalf("kept=%d excluded=%v", len(kept), excluded)
	}
	if raw, _ := ReadAll(p); len(raw) != len(fixtureRows()) {
		t.Fatalf("ReadAll stays the raw reader: %d", len(raw))
	}
}

func TestSummaryJSONOmitsTheExcludedFieldWhenNothingWasSkipped(t *testing.T) {
	b, _ := json.Marshal(Summary{ByTask: map[string]int{}})
	if strings.Contains(string(b), "excluded_non_production") {
		t.Fatalf("a clean ledger's summary keeps its old shape: %s", b)
	}
}

// RunClassFromEnv is the pid-free read of the same rule: a PAIR emitter asks it on every request, and os.Getppid
// is a process-table walk on Windows. Both must name the same class for every environment.
func TestRunClassFromEnvNamesTheSameClassAsOriginFromEnv(t *testing.T) {
	for _, env := range []map[string]string{
		nil,
		{"LOCAL_OFFLOAD_RUN_CLASS": "bake"},
		{"LOCAL_OFFLOAD_RUN_CLASS": " Measurement "},
		{"LOCAL_OFFLOAD_RUN_CLASS": "production", "LOCAL_OFFLOAD_ORIGIN": "bake:run-7"},
		{"LOCAL_OFFLOAD_ORIGIN": "test:suite"},
		{"LOCAL_OFFLOAD_ORIGIN": "go-test"},
		{"CLAUDE_CODE_SESSION_ID": "7c561f0b-2e24-4047-baf9-3a81dadd3694"},
		{"LOCAL_OFFLOAD_RUN_CLASS": "ba ke!"},
	} {
		if got, want := RunClassFromEnv(envMap(env)), OriginFromEnv(envMap(env)).Class; got != want {
			t.Errorf("env %v: RunClassFromEnv = %q, OriginFromEnv.Class = %q", env, got, want)
		}
	}
}

// A card opens when a call begins, before any row exists, so the door alone has to say what it makes.
func TestClassOfDoorNamesTheDoorsThatMakeTestTraffic(t *testing.T) {
	for door, want := range map[string]string{
		"cli:fleet-smoke": "test", "cli:fleet-measure": "measurement",
		"cli:generate-image": "", "offload_generate_image": "", "fleet": "", "": "",
	} {
		if got := ClassOfDoor(door); got != want {
			t.Errorf("ClassOfDoor(%q) = %q, want %q", door, got, want)
		}
		if got := ClassOf(Entry{Door: door}); got != want {
			t.Errorf("ClassOf(door %q) = %q, want the door's class %q", door, got, want)
		}
	}
}
