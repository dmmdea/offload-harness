package askjob

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// write drops one file under dir (creating parents) and returns its path.
func write(t *testing.T, dir, rel, body string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildContractProducesFlatSchemaAndGroundedAcceptance(t *testing.T) {
	dir := t.TempDir()
	// A distinctive identifier the question does NOT mention: the anchor must come from here.
	p := write(t, dir, "cfg.go", "package x\n\nconst FleetMaxQueueDepth = 32\n")

	c, err := BuildContract("what is the queue cap", []string{p}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Context) != 1 || c.Context[0].Name != "cfg.go" {
		t.Fatalf("context not inlined as one flat doc: %+v", c.Context)
	}
	if !strings.Contains(c.Context[0].Text, "FleetMaxQueueDepth") {
		t.Fatalf("doc body not carried: %q", c.Context[0].Text)
	}
	if len(c.OutputSchema) == 0 {
		t.Fatal("a remote placement REQUIRES an output_schema; none was built")
	}
	if len(c.Acceptance) == 0 {
		t.Fatal("acceptance must be generated, not left to the caller")
	}
	// The anchor must be grounded in the FILE, not echoed from the question, or the check is
	// PARROT-PASSABLE and a model that restates the question passes it.
	joined := strings.Join(c.Acceptance, " ")
	if !strings.Contains(joined, "FleetMaxQueueDepth") {
		t.Fatalf("acceptance is not anchored to file content: %v", c.Acceptance)
	}
	if strings.Contains(joined, "queue cap") {
		t.Fatalf("acceptance echoes the question - parrot-passable: %v", c.Acceptance)
	}
	// The builder owns the wire preamble the delegator would otherwise mint by hand.
	if c.SchemaVersion != core.AgentWireSchemaVersion || c.Depth != 0 {
		t.Fatalf("schema_version/depth not minted: %d/%d", c.SchemaVersion, c.Depth)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("BuildContract returned a contract that Validate rejects: %v", err)
	}
	// The question has to reach the seat, and the doc names save it a list_dir step.
	if !strings.Contains(c.Goal, "what is the queue cap") || !strings.Contains(c.Goal, "cfg.go") {
		t.Fatalf("goal must carry the question and the attached names: %q", c.Goal)
	}
}

// TestBuildContractPassesTheDelegatorLint is the real proof this feature works: the
// harness-authored acceptance must survive the SAME lint that exists to catch
// caller-authored acceptance which verifies less than it looks. A warning here means the
// built contract is defective — never that the assertion is wrong.
func TestBuildContractPassesTheDelegatorLint(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "queue.go", sampleSource)

	c, err := BuildContract("what happens when the dispatcher is full", []string{p}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if warns := delegate.LintAcceptance(c); len(warns) != 0 {
		t.Fatalf("the built contract must lint clean; got %d warning(s):\n%s\nacceptance=%v\ngoal=%q",
			len(warns), strings.Join(warns, "\n"), c.Acceptance, c.Goal)
	}
}

// sampleSource is a realistic (not toy) Go file: comments, imports, exported and
// unexported identifiers, and repeated tokens — the shape a caller actually attaches.
const sampleSource = "// Package fleet holds the dispatcher back-pressure knobs.\n" +
	"package fleet\n" +
	"\n" +
	"import \"errors\"\n" +
	"\n" +
	"// ErrQueueSaturated is returned once accepted+running reaches the cap.\n" +
	"var ErrQueueSaturated = errors.New(\"fleet: queue full\")\n" +
	"\n" +
	"const (\n" +
	"\t// defaultMaxQueueDepth is deliberately generous: a full delegate call is 8 subtasks.\n" +
	"\tdefaultMaxQueueDepth = 32\n" +
	"\tdispatchRetryBackoff = 250\n" +
	")\n" +
	"\n" +
	"type Dispatcher struct {\n" +
	"\taccepted int\n" +
	"\trunning  int\n" +
	"}\n" +
	"\n" +
	"func (d *Dispatcher) Admit() error {\n" +
	"\tif d.accepted+d.running >= defaultMaxQueueDepth {\n" +
	"\t\treturn ErrQueueSaturated\n" +
	"\t}\n" +
	"\td.accepted++\n" +
	"\treturn nil\n" +
	"}\n"

func TestBuildContractRefusesWhenNoAnchorExists(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "empty.txt", "the the the\n")
	// No distinctive token: we must NOT emit an ungrounded check that passes anything.
	_, err := BuildContract("what is here", []string{p}, dir)
	if err == nil {
		t.Fatal("expected refusal when no grounded anchor can be found")
	}
	if !errors.Is(err, ErrNoAnchor) {
		t.Fatalf("refusal must be typed ErrNoAnchor, got %v", err)
	}
}

// TestBuildContractRefusesAnchorsTheGoalAlreadyContains pins the correction the brief's
// pickAnchor got wrong: the lint measures parrot-passability against the WHOLE GOAL,
// whose boilerplate carries its own long words. A candidate matching one of those would
// trip the very lint this feature exists to satisfy, so it is not a candidate at all —
// and when the files hold nothing else, the answer is a refusal, not a decorative check.
func TestBuildContractRefusesAnchorsTheGoalAlreadyContains(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "boiler.txt", "attached inferring QUESTION evidence\n")
	_, err := BuildContract("what is here", []string{p}, dir)
	if !errors.Is(err, ErrNoAnchor) {
		t.Fatalf("goal-boilerplate words must not qualify as anchors, got %v", err)
	}
}

// TestBuildContractDeCollidesBasenames: two files with the same base name are a normal
// caller shape (config.go lives in many packages), and naive filepath.Base makes them one
// doc name — which Validate rejects outright, because the second would silently overwrite
// the first at materialization.
func TestBuildContractDeCollidesBasenames(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "internal/config/config.go", "package config\n\nconst DefaultLedgerRotateBytes = 1024\n")
	b := write(t, dir, "internal/core/config.go", "package core\n\nconst SeatWarmupGraceSeconds = 90\n")

	c, err := BuildContract("where is the rotate size set", []string{a, b}, dir)
	if err != nil {
		t.Fatalf("a basename collision must be de-collided, not refused: %v", err)
	}
	if len(c.Context) != 2 {
		t.Fatalf("both docs must be carried: %+v", c.Context)
	}
	if c.Context[0].Name == c.Context[1].Name {
		t.Fatalf("doc names still collide: %q", c.Context[0].Name)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("de-collided names must satisfy Validate: %v", err)
	}
	// Deterministic: the same input must produce the same names on every call, or a
	// re-run of the same question would be a different contract.
	again, err := BuildContract("where is the rotate size set", []string{a, b}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Context[0].Name != c.Context[0].Name || again.Context[1].Name != c.Context[1].Name {
		t.Fatalf("naming is not deterministic: %+v vs %+v", c.Context, again.Context)
	}
}

func TestBuildContractRefusesTooManyPaths(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, core.AgentContextMaxDocs+1)
	for i := range paths {
		// Deliberately NOT created: the count is a cheap up-front refusal, so it must
		// fire before any file I/O.
		paths[i] = filepath.Join(dir, "f.go")
	}
	_, err := BuildContract("what is here", paths, dir)
	if !errors.Is(err, ErrTooManyPaths) {
		t.Fatalf("over the %d-doc cap must be a typed refusal, got %v", core.AgentContextMaxDocs, err)
	}
}

func TestBuildContractRefusesOversizeContext(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("DistinctiveIdentifier ", 5000) // ~110 KB, under the 128 KiB per-file cap
	var paths []string
	for i := 0; i < 3; i++ {
		paths = append(paths, write(t, dir, "d/"+string(rune('a'+i))+".go", body))
	}
	_, err := BuildContract("what is here", paths, dir)
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("over the %d-byte cap must be a typed refusal, got %v", core.AgentContextMaxBytes, err)
	}
}

// TestBuildContractConfinesReadsToReadRoot: read_root is the only containment this
// surface has, and a caller-supplied path list is exactly where an escape would arrive.
func TestBuildContractConfinesReadsToReadRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, root, "in.go", "package x\n\nconst InsideTheRootMarker = 1\n")
	secret := write(t, outside, "secret.go", "package y\n\nconst OutsideTheRootMarker = 2\n")

	if _, err := BuildContract("what is here", []string{secret}, root); err == nil {
		t.Fatal("a path outside read_root must be refused, not read")
	}
	rel := filepath.Join("..", filepath.Base(outside), "secret.go")
	if _, err := BuildContract("what is here", []string{rel}, root); err == nil {
		t.Fatal("a relative traversal out of read_root must be refused")
	}
}

func TestBuildContractRefusesEmptyInput(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "cfg.go", "package x\n\nconst FleetMaxQueueDepth = 32\n")
	if _, err := BuildContract("   ", []string{p}, dir); err == nil {
		t.Fatal("an empty question must be refused")
	}
	if _, err := BuildContract("what is the queue cap", nil, dir); !errors.Is(err, ErrNoPaths) {
		t.Fatal("no paths means nothing to ground against; must be a typed refusal")
	}
}

// TestPickAnchorPrefersIdentifiersOverCommentProse pins the tier that keeps the anchor
// citable. Measured while building this: on the realistic file above, rarity ALONE picked
// "delegate" out of a comment — grounded, lint-clean, and a token no correct answer would
// ever quote, so every right answer would have come back unverified.
func TestPickAnchorPrefersIdentifiersOverCommentProse(t *testing.T) {
	docs := []core.ContextDoc{{
		Name: "a.go",
		// "unsurprising" is rarer (1) than the identifier (2) and sorts earlier, so
		// rarity alone would take it.
		Text: "// an unsurprising note\nfunc admitOneJob() {}\nfunc caller() { admitOneJob() }\n",
	}}
	got, err := pickAnchor("goal with nothing distinctive", docs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "admitOneJob" {
		t.Fatalf("pickAnchor = %q, want the identifier admitOneJob over the comment word", got)
	}
}

// TestPickAnchorFallsBackToProseWhenNoIdentifierExists: attaching markdown or a log is a
// legitimate use of this tool, and refusing every such file would be worse than a weaker
// anchor. The tier is a preference, not a filter.
func TestPickAnchorFallsBackToProseWhenNoIdentifierExists(t *testing.T) {
	docs := []core.ContextDoc{{Name: "notes.md", Text: "provisioning happens during the quarterly reconciliation window"}}
	got, err := pickAnchor("goal with nothing in common", docs)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("a prose-only file must still yield an anchor")
	}
	if !strings.Contains(docs[0].Text, got) {
		t.Fatalf("anchor %q is not grounded in the doc", got)
	}
}

// TestPickAnchorPrefersTheRarestToken: a token used once is far likelier to be the
// specific thing the answer must cite than a token used everywhere.
func TestPickAnchorPrefersTheRarestToken(t *testing.T) {
	docs := []core.ContextDoc{
		{Name: "a.go", Text: "CommonHelper CommonHelper CommonHelper RareSingleton"},
		{Name: "b.go", Text: "CommonHelper CommonHelper"},
	}
	got, err := pickAnchor("goal text with nothing distinctive", docs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "RareSingleton" {
		t.Fatalf("pickAnchor = %q, want the rarest token RareSingleton", got)
	}
}
