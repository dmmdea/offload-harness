package recipe

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// The three good fixtures are canonical: decoding one and encoding it again gives the same bytes.
func TestFixturesDecodeAndRoundTrip(t *testing.T) {
	for name, status := range map[string]string{
		"proposed.json": StatusProposed, "published.json": StatusPublished, "demoted.json": StatusDemoted,
	} {
		raw := fixture(t, name)
		r, err := Decode(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.Status != status || r.ID == "" || r.SeatTag == "" || len(r.Provenance) == 0 {
			t.Fatalf("%s decoded to %+v", name, r)
		}
		out, err := r.Encode()
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		if !bytes.Equal(out, raw) {
			t.Fatalf("%s is not canonical:\n got %q\nwant %q", name, out, raw)
		}
	}
}

// A key the record does not declare is refused, so a note, a script or a second status cannot ride along.
func TestDecodeRejectsAnUnknownTopLevelKey(t *testing.T) {
	_, err := Decode(fixture(t, "bad-unknown-key.json"))
	if err == nil {
		t.Fatal("a recipe carrying the key notes was accepted")
	}
	if !strings.Contains(err.Error(), "notes") {
		t.Fatalf("the refusal does not name the key: %v", err)
	}
}

// rules_code is the envharness key the ADR refuses by name: generated rule code inside the rule set.
func TestDecodeRejectsRuleCodeInsideTheRules(t *testing.T) {
	_, err := Decode(fixture(t, "bad-rules-code.json"))
	if err == nil {
		t.Fatal("a recipe carrying rules.rules_code was accepted")
	}
	if !strings.Contains(err.Error(), "rules_code") {
		t.Fatalf("the refusal does not name the key: %v", err)
	}
}

// Free text the model would read as an instruction is refused, and so is anything else that is not
// rule data: the vocabulary is shapes, not prose.
func TestDecodeRejectsFreeTextAndCodeOutsideTheVocabulary(t *testing.T) {
	if _, err := Decode(fixture(t, "bad-free-text.json")); err == nil {
		t.Fatal("a rewrite_error text with a command in it was accepted")
	}
	good := string(fixture(t, "proposed.json"))
	const text = `List the directory first.`
	cases := []struct{ name, from, to, want string }{
		{"tool name with code", `"list_dir": 2`, `"list_dir; import os": 2`, "max_calls_per_tool"},
		{"uppercase tool name", `"list_dir": 2`, `"List_Dir": 2`, "max_calls_per_tool"},
		{"argument name with code", `"limit": 400`, `"limit(1)": 400`, "arg_limits"},
		{"text with a backtick", text, "List `ls` first.", "rewrite_error"},
		{"text with a substitution", text, `List $(ls) first.`, "rewrite_error"},
		{"text with a brace", text, `List {x} first.`, "rewrite_error"},
		{"text with a call", text, `Then print(1).`, "rewrite_error"},
		{"text with a line break", text, `List it.\nThen stop.`, "rewrite_error"},
		{"text past the bound", text, strings.Repeat("a", 161), "rewrite_error"},
		{"text outside ASCII", text, `List the diréctory.`, "rewrite_error"},
		{"strip pattern that matches the empty string", `(?m)^Cookie notice.*$`, `x*`, "observation_strip"},
		{"strip pattern that does not compile", `(?m)^Cookie notice.*$`, `(unclosed`, "observation_strip"},
		{"rewrite pattern that matches the empty string", `no such file`, `.*`, "rewrite_error"},
		{"provenance entry with a space", `"run-0001"`, `"run 0001"`, "provenance"},
		{"duplicate provenance entry", `"run-0002"`, `"run-0001"`, "provenance"},
		{"id that climbs out of the store", `recipe-cap-listing-01`, `../recipe`, "id"},
		{"seat tag with a space", `seat-class-a`, `seat class a`, "seat_tag"},
		{"status that is not one of the three", `"proposed"`, `"applied"`, "status"},
	}
	for _, c := range cases {
		if !strings.Contains(good, c.from) {
			t.Fatalf("%s: the fixture lost %q", c.name, c.from)
		}
		_, err := Decode([]byte(strings.Replace(good, c.from, c.to, 1)))
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: the refusal does not name %q: %v", c.name, c.want, err)
		}
	}
}

// A recipe that changes nothing, or has no source run, is not a recipe.
func TestDecodeRejectsAnEmptyRuleSetAndNoProvenance(t *testing.T) {
	empty := `{"id":"recipe-empty-07","seat_tag":"seat-class-a","rules":{},"provenance":["run-1"],"status":"proposed"}`
	if _, err := Decode([]byte(empty)); err == nil {
		t.Error("a recipe with no rule was accepted")
	}
	noProv := `{"id":"recipe-empty-08","seat_tag":"seat-class-a","rules":{"max_observation_tokens":600},"provenance":[],"status":"proposed"}`
	if _, err := Decode([]byte(noProv)); err == nil {
		t.Error("a recipe with no provenance was accepted")
	}
	if _, err := Decode(append(fixture(t, "proposed.json"), []byte(`{}`)...)); err == nil {
		t.Error("trailing data after the record was accepted")
	}
}

// Publishing is only ever the audit's result: a published record carries the audit that earned it.
func TestPublishedStatusNeedsAPassingAudit(t *testing.T) {
	pub := string(fixture(t, "published.json"))
	cases := []struct{ name, from, to string }{
		{"verdict is not ADOPT", `"verdict": "ADOPT"`, `"verdict": "BLOCK"`},
		{"steps got worse", `"steps_candidate": 5.25`, `"steps_candidate": 7`},
		{"verdict is made up", `"verdict": "ADOPT"`, `"verdict": "MAYBE"`},
		{"baseline hash is short", `"baseline_hash": "0123`, `"baseline_hash": "x0123`},
	}
	for _, c := range cases {
		if !strings.Contains(pub, c.from) {
			t.Fatalf("%s: the fixture lost %q", c.name, c.from)
		}
		if _, err := Decode([]byte(strings.Replace(pub, c.from, c.to, 1))); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	unknown := strings.Replace(pub, `"audit": {`, `"unaudited": {`, 1)
	if _, err := Decode([]byte(unknown)); err == nil {
		t.Error("a record with an unknown audit key was accepted")
	}
	r, err := Decode(fixture(t, "published.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Audit == nil {
		t.Fatal("the published fixture decoded with no audit")
	}
	audit := *r.Audit
	r.Audit = nil
	if err := r.Validate(); err == nil {
		t.Error("a published record with no audit validated")
	}
	p, err := Decode(fixture(t, "proposed.json"))
	if err != nil {
		t.Fatal(err)
	}
	p.Audit = &audit
	if err := p.Validate(); err == nil {
		t.Error("a proposed record with an audit validated: nothing has audited it")
	}
}

// The fixtures carry no host, path or identity text. The banned words are assembled at run time so this
// file never holds one itself: a literal here would be the one place the text reached a tracked file.
func TestFixturesAreSynthetic(t *testing.T) {
	drivePath := regexp.MustCompile(`(^|[^A-Za-z0-9])[A-Za-z]:[\/]`)
	banned := []string{"/" + "home/", "/" + "Users/", "@", "Bearer ", "dev" + "-caches", "work" + "trees", "My" + " Drive", "GPU" + "-"}
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil || len(files) < 6 {
		t.Fatalf("fixtures: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		for _, b := range banned {
			if strings.Contains(s, b) {
				t.Errorf("%s carries %q", f, b)
			}
		}
		if drivePath.MatchString(s) {
			t.Errorf("%s carries a drive path", f)
		}
	}
}
