package eval

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }

func TestAllOutcomesFixedOrder(t *testing.T) {
	want := []AttemptOutcome{"pass", "task_fail", "cheat", "infra_error", "timeout", "judge_error", "not_run"}
	if got := AllOutcomes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllOutcomes = %v, want %v", got, want)
	}
	// A fresh slice each call: a caller that sorts or edits it cannot change the taxonomy.
	got := AllOutcomes()
	got[0] = "x"
	if AllOutcomes()[0] != OutcomePass {
		t.Fatal("AllOutcomes returned shared storage")
	}
	for _, o := range AllOutcomes() {
		if !o.Valid() {
			t.Errorf("%q is in AllOutcomes but not Valid", o)
		}
	}
	for _, bad := range []AttemptOutcome{"", "PASS", "passed", "not_checked", "fail"} {
		if bad.Valid() {
			t.Errorf("%q must not be Valid", bad)
		}
	}
}

func TestOutcomeScored(t *testing.T) {
	scored := map[AttemptOutcome]bool{OutcomePass: true, OutcomeTaskFail: true, OutcomeCheat: true}
	for _, o := range AllOutcomes() {
		if o.Scored() != scored[o] {
			t.Errorf("%s.Scored() = %v, want %v", o, o.Scored(), scored[o])
		}
	}
	if AttemptOutcome("bogus").Scored() {
		t.Error("an unknown outcome must not be Scored")
	}
}

func TestOutcomeFromCaliper(t *testing.T) {
	for _, s := range []string{"pass", "task_fail", "cheat", "infra_error", "timeout", "judge_error"} {
		got, ok := OutcomeFromCaliper(s)
		if !ok || string(got) != s {
			t.Errorf("OutcomeFromCaliper(%q) = %q, %v; want itself", s, got, ok)
		}
	}
	if got, ok := OutcomeFromCaliper("not_checked"); !ok || got != OutcomeNotRun {
		t.Errorf("not_checked must map to not_run, got %q, %v", got, ok)
	}
	for _, s := range []string{"", "not_run", "PASS", "skipped", "fail"} {
		if got, ok := OutcomeFromCaliper(s); ok || got != "" {
			t.Errorf("OutcomeFromCaliper(%q) = %q, %v; want unknown", s, got, ok)
		}
	}
}

// The enum in the embedded schema and the Go list are one list: parse the schema and compare.
func TestGateInputSchemaOutcomeEnumEqualsGoEnum(t *testing.T) {
	var doc struct {
		Schema string `json:"$schema"`
		Defs   struct {
			Outcome struct {
				Enum []string `json:"enum"`
			} `json:"outcome"`
			Attempt struct {
				Properties struct {
					Outcome struct {
						Ref string `json:"$ref"`
					} `json:"outcome"`
				} `json:"properties"`
			} `json:"attempt"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(GateInputSchema, &doc); err != nil {
		t.Fatalf("the embedded schema is not JSON: %v", err)
	}
	if doc.Schema != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema = %q, want draft 2020-12", doc.Schema)
	}
	var want []string
	for _, o := range AllOutcomes() {
		want = append(want, string(o))
	}
	if !reflect.DeepEqual(doc.Defs.Outcome.Enum, want) {
		t.Fatalf("schema outcome enum %v != Go enum %v", doc.Defs.Outcome.Enum, want)
	}
	if doc.Defs.Attempt.Properties.Outcome.Ref != "#/$defs/outcome" {
		t.Errorf("attempt.outcome must reference the one enum, got %q", doc.Defs.Attempt.Properties.Outcome.Ref)
	}
}

// Unresolved cost rule: a nil metric stays nil end to end and never serializes as 0.
func TestUnresolvedMetricNeverSerializesAsZero(t *testing.T) {
	a := Attempt{
		Outcome: OutcomePass,
		Score:   f64(1),
		Metrics: map[string]*float64{"forwarded_tokens": nil, "cost_usd": f64(0.5)},
	}
	j, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	s := string(j)
	if !strings.Contains(s, `"forwarded_tokens":null`) {
		t.Fatalf("an unresolved metric must serialize as null, got %s", s)
	}
	if strings.Contains(s, `"forwarded_tokens":0`) || strings.Contains(s, ":0,") || strings.Contains(s, ":0}") {
		t.Fatalf("an unresolved metric serialized as 0: %s", s)
	}
	var back Attempt
	if err := json.Unmarshal(j, &back); err != nil {
		t.Fatal(err)
	}
	if p, ok := back.Metrics["forwarded_tokens"]; !ok || p != nil {
		t.Fatalf("the null must come back as a present nil, got %v present=%v", p, ok)
	}
	if p := back.Metrics["cost_usd"]; p == nil || *p != 0.5 {
		t.Fatalf("a resolved metric must survive, got %v", p)
	}
	// An attempt with no metrics at all omits the key.
	j2, _ := json.Marshal(Attempt{Outcome: OutcomeNotRun})
	if strings.Contains(string(j2), "metrics") || strings.Contains(string(j2), "score") {
		t.Fatalf("empty optional fields must be omitted, got %s", j2)
	}
	// A deep copy keeps the nil: an unresolved value must not be turned into a zero by cloning.
	c := cloneArm(Arm{Tasks: []Task{{ID: "t", Attempts: []Attempt{a}}}})
	if p, ok := c.Tasks[0].Attempts[0].Metrics["forwarded_tokens"]; !ok || p != nil {
		t.Fatalf("cloneArm turned an unresolved metric into %v (present=%v)", p, ok)
	}
}

func TestTimeTermNames(t *testing.T) {
	for _, bad := range []string{"latency", "p95_latency_ms", "wall_time", "wallClockSeconds", "tokens_per_second",
		"duration", "elapsed", "time", "tok_s", "responseTime", "throughput", "speedup", "ms"} {
		if timeTerm(bad) == "" {
			t.Errorf("timeTerm(%q) found no wall-time term", bad)
		}
	}
	for _, ok := range []string{"forwarded_tokens", "cost_usd", "resident_gib", "pass_rate", "secondary_score",
		"timeouts", "answer_quality", "judge_0_10", "memory_gib"} {
		if got := timeTerm(ok); got != "" {
			t.Errorf("timeTerm(%q) = %q, want none", ok, got)
		}
	}
}
