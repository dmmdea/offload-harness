package askjob

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// The question has to reach the seat.
	if !strings.Contains(c.Goal, "what is the queue cap") {
		t.Fatalf("goal must carry the question: %q", c.Goal)
	}
	// ...and must NOT carry the basenames. The goal is the anchor's disqualifier, so a
	// listed filename silently removes every identifier that is a substring of it —
	// measured on the first live run, where attaching buildinfo.go cost `buildinfo`.
	if strings.Contains(c.Goal, "cfg.go") {
		t.Fatalf("basenames in the goal disqualify the anchors they name: %q", c.Goal)
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

// ledgerSource has SIX candidates for THREE anchor slots: three names the file repeats
// and three one-offs. Which three survive the cut is therefore decided entirely by the
// ranking, which is what this fixture exists to hold still.
const ledgerSource = `package ledger

// SessionLedgerWriter batches rows. TemporaryScratchPath is incidental.
type SessionLedgerWriter struct{ RotateThreshold, FlushInterval int }

func (w *SessionLedgerWriter) Flush() { _ = w.FlushInterval; _ = w.RotateThreshold }

func (w *SessionLedgerWriter) Rotate() { _ = w.RotateThreshold; _ = w.FlushInterval }

var UnusedLegacyHook, DeprecatedShimName any
`

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

// TestBuildContractAnchorsOnWhatAnAnswerWouldCite is the citability regression pin, and the
// reason the ranking was inverted. On this fixture the candidate pool is
// {ErrQueueSaturated:3, defaultMaxQueueDepth:3, dispatchRetryBackoff:1}. The two tokens a
// correct answer to "what happens when the dispatcher is full" MUST quote are the two
// frequent ones — and under the original rarest-wins rule they both LOST to the
// retry-backoff constant, which no right answer would ever mention.
func TestBuildContractAnchorsOnWhatAnAnswerWouldCite(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "queue.go", sampleSource)

	c, err := BuildContract("what happens when the dispatcher is full", []string{p}, dir)
	if err != nil {
		t.Fatal(err)
	}
	check := c.Acceptance[0]
	for _, must := range []string{"ErrQueueSaturated", "defaultMaxQueueDepth"} {
		if !strings.Contains(check, must) {
			t.Fatalf("the grounding check must offer %q, the token a right answer cites: %q", must, check)
		}
	}
	if !strings.HasPrefix(check, "regex:(") {
		t.Fatalf("the grounding check must be one regex alternation: %q", check)
	}
}

// TestBuildContractAnchorsSurviveTheCutByCentrality is the ordering half of the citability
// pin. The fixture above has exactly three candidates, so ALL of them ride the alternation
// whatever the ranking does — it proves the tokens are offered, not that the ranking chose
// them. Here there are six candidates for three slots, so membership is decided purely by
// the ranking: the repeated, central names must make the cut and the one-off incidentals
// must not. Reverting the ranking to rarest-wins inverts this exactly.
func TestBuildContractAnchorsSurviveTheCutByCentrality(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "writer.go", ledgerSource)

	c, err := BuildContract("how often does the writer flush and when does it rotate", []string{p}, dir)
	if err != nil {
		t.Fatal(err)
	}
	check := c.Acceptance[0]
	for _, central := range []string{"SessionLedgerWriter", "FlushInterval", "RotateThreshold"} {
		if !strings.Contains(check, central) {
			t.Fatalf("the repeated, central name %q lost its slot: %q", central, check)
		}
	}
	for _, incidental := range []string{"TemporaryScratchPath", "UnusedLegacyHook", "DeprecatedShimName"} {
		if strings.Contains(check, incidental) {
			t.Fatalf("the one-off %q took a slot from a central name: %q", incidental, check)
		}
	}
}

// TestBuildContractAcceptanceAcceptsARightAnswerAndRejectsAParrot exercises the check the
// way the delegator actually does — through core's Eval — instead of only asserting on its
// text. Lint-cleanliness says the check is well FORMED; this says it is well AIMED.
func TestBuildContractAcceptanceAcceptsARightAnswerAndRejectsAParrot(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "queue.go", sampleSource)

	c, err := BuildContract("what happens when the dispatcher is full", []string{p}, dir)
	if err != nil {
		t.Fatal(err)
	}
	chk, err := core.ParseAcceptanceCheck(c.Acceptance[0])
	if err != nil {
		t.Fatalf("the generated check must parse: %v", err)
	}

	right := "Admit refuses: once accepted+running reaches defaultMaxQueueDepth (32) it returns ErrQueueSaturated."
	if pass, reason := chk.Eval(nil, right); !pass {
		t.Fatalf("a correct, well-cited answer must PASS the generated check: %s", reason)
	}
	// A model that echoes the instructions back must NOT pass — that is the whole point
	// of excluding every candidate the goal already contains.
	if pass, _ := chk.Eval(nil, c.Goal); pass {
		t.Fatalf("the goal text itself passed the check — it is parrot-passable: %q", c.Acceptance[0])
	}
}

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

// TestBuildContractDedupesRepeatedPaths: the same path twice is one document. Inlining it
// twice would double-charge the 256 KiB ceiling and hand the seat one file under two names.
func TestBuildContractDedupesRepeatedPaths(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "cfg.go", "package x\n\nconst FleetMaxQueueDepth = 32\n")

	c, err := BuildContract("what is the queue cap", []string{p, p, "./cfg.go"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Context) != 1 {
		t.Fatalf("repeats must collapse to one doc, got %d: %+v", len(c.Context), c.Context)
	}
	// A repeat must not consume a doc slot either: 16 copies of one file is one document.
	many := make([]string, core.AgentContextMaxDocs+4)
	for i := range many {
		many[i] = p
	}
	if _, err := BuildContract("what is the queue cap", many, dir); err != nil {
		t.Fatalf("repeats must not spend the doc cap: %v", err)
	}
}

func TestBuildContractRefusesTooManyPaths(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, core.AgentContextMaxDocs+1)
	for i := range paths {
		// DISTINCT names: repeats collapse to one document (see dedupePaths), so a
		// cap test built from one path repeated would prove nothing. Deliberately NOT
		// created either: the count is a cheap up-front refusal, so it must fire
		// before any file I/O.
		paths[i] = filepath.Join(dir, fmt.Sprintf("f%d.go", i))
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
//
// Each case asserts WHY the read was refused, not merely that something failed. A bare
// err != nil would pass just as happily on a mistyped filename, which would leave the
// containment itself unproven — so the control case below reads the very same file with
// read_root moved, and must SUCCEED.
func TestBuildContractConfinesReadsToReadRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, root, "in.go", "package x\n\nconst InsideTheRootMarker = 1\n")
	secret := write(t, outside, "secret.go", "package y\n\nconst OutsideTheRootMarker = 2\n")

	// Control: the file is readable and groundable — so every refusal below is about the
	// ROOT, not about the file.
	if _, err := BuildContract("what is here", []string{secret}, outside); err != nil {
		t.Fatalf("control: the same file inside its own root must be readable: %v", err)
	}

	_, err := BuildContract("what is here", []string{secret}, root)
	if err == nil {
		t.Fatal("an absolute path outside read_root must be refused, not read")
	}
	if !strings.Contains(err.Error(), "outside read_root") || !strings.Contains(err.Error(), "secret.go") {
		t.Fatalf("the refusal must name the path and the reason, got: %v", err)
	}

	rel := filepath.Join("..", filepath.Base(outside), "secret.go")
	_, err = BuildContract("what is here", []string{rel}, root)
	if err == nil {
		t.Fatal("a relative traversal out of read_root must be refused")
	}
	// os.Root refuses this in the KERNEL traversal; the wording is what proves the
	// refusal came from containment rather than from a missing file.
	if !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("the traversal must be refused by os.Root containment, got: %v", err)
	}

	// A symlink inside the root pointing out of it is the escape a path check alone
	// cannot see — os.Root rejects the reparse point itself. Creating one needs
	// privileges Windows does not grant by default, so an unsupported host SKIPS rather
	// than failing the suite.
	t.Run("symlink out of the root", func(t *testing.T) {
		link := filepath.Join(root, "link.go")
		if lerr := os.Symlink(secret, link); lerr != nil {
			t.Skipf("symlinks unsupported for this process: %v", lerr)
		}
		if _, err := BuildContract("what is here", []string{link}, root); err == nil {
			t.Fatal("a symlink escaping read_root must be refused, not followed")
		}
	})
}

func TestBuildContractRefusesEmptyInput(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "cfg.go", "package x\n\nconst FleetMaxQueueDepth = 32\n")
	if _, err := BuildContract("   ", []string{p}, dir); !errors.Is(err, ErrNoQuestion) {
		t.Fatalf("an empty question must be a typed refusal, got %v", err)
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
		// The comment word is deliberately MORE frequent than the identifier, so
		// frequency ranking alone would take it and only the tier explains the result.
		Text: "// unsurprising unsurprising unsurprising note\nfunc admitOneJob() {}\n",
	}}
	got, err := pickAnchors("goal with nothing distinctive", docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] != "admitOneJob" {
		t.Fatalf("pickAnchors = %v, want the identifier admitOneJob to LEAD", got)
	}
	// ...and the tier TOPS UP rather than replaces: only one identifier survives here, so
	// a spare slot goes to the best plain token instead of being left empty. "unsurprising"
	// is twelve characters, so it clears plainAnchorMinLen — the bar that keeps an ordinary
	// eight-letter word out, because easing the check eases it for an UNCITED answer too.
	// (This assertion previously demanded the opposite; replacing the pool outright was
	// silently dropping good plain candidates.)
	if !slices.Contains(got, "unsurprising") {
		t.Fatalf("a spare slot must be topped up from the plain pool, got %v", got)
	}
}

// TestPickAnchorsDoNotDisplaceIdentifiersWithProse: topping up fills only what is LEFT.
// When enough identifiers survive, plain tokens get no slot at all — otherwise the tier
// would be decorative.
func TestPickAnchorsDoNotDisplaceIdentifiersWithProse(t *testing.T) {
	docs := []core.ContextDoc{{
		Name: "a.go",
		Text: "unsurprising unsurprising unsurprising unsurprising " +
			"admitOneJob admitOneJob rotateOneFile rotateOneFile flushOneBatch",
	}}
	got, err := pickAnchors("goal with nothing distinctive", docs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"admitOneJob", "rotateOneFile", "flushOneBatch"}
	if len(got) != len(want) {
		t.Fatalf("pickAnchors = %v, want the three identifiers %v", got, want)
	}
	if slices.Contains(got, "unsurprising") {
		t.Fatalf("the most frequent PLAIN token took a slot from an identifier: %v", got)
	}
}

// TestPickAnchorFallsBackToProseWhenNoIdentifierExists: attaching markdown or a log is a
// legitimate use of this tool, and refusing every such file would be worse than a weaker
// anchor. The tier is a preference, not a filter.
func TestPickAnchorFallsBackToProseWhenNoIdentifierExists(t *testing.T) {
	docs := []core.ContextDoc{{Name: "notes.md", Text: "provisioning happens during the quarterly reconciliation window"}}
	got, err := pickAnchors("goal with nothing in common", docs)
	if err != nil {
		t.Fatal(err)
	}
	// Every candidate here occurs once, so the lexicographic tie-break fully determines
	// the result — asserted exactly, because a fallback that just returned the first
	// token it happened to see would pass a mere non-empty-and-grounded check.
	//
	// "quarterly" is nine characters and names no attached file, so it does not clear
	// plainAnchorMinLen: ordinary words need more than length 8 before they may anchor
	// anything. Two alternatives is the honest result, not three.
	want := []string{"provisioning", "reconciliation"}
	if !slices.Equal(got, want) {
		t.Fatalf("pickAnchors = %v, want %v", got, want)
	}
}

// TestPickAnchorsRankMostFrequentFirst pins the INVERSION. This test previously asserted
// the opposite (rarest-wins, from the original design). Within one file, centrality and
// frequency correlate — a name the file repeats is a name the file is ABOUT — so the token
// a right answer will quote is the frequent one, and ranking by rarity ranked away from it.
func TestPickAnchorsRankMostFrequentFirst(t *testing.T) {
	docs := []core.ContextDoc{
		{Name: "a.go", Text: "CommonHelper CommonHelper CommonHelper RareSingleton"},
		{Name: "b.go", Text: "CommonHelper CommonHelper"},
	}
	got, err := pickAnchors("goal text with nothing distinctive", docs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CommonHelper", "RareSingleton"}
	if !slices.Equal(got, want) {
		t.Fatalf("pickAnchors = %v, want %v (most frequent first)", got, want)
	}
}

// TestPickAnchorsKeepCommonWordsOutOfSpareSlots is the pin for the bar the plain pool has
// to clear. MEASURED on this exact text: with one bar for both pools the check became
// regex:(FleetMaxQueueDepth|accepted), and "accepted" is eight characters of ordinary
// English — a wrong answer ("requests are accepted until the cap") passes the grounding
// branch without having read anything. Easing the check eases it for an UNCITED answer too.
func TestPickAnchorsKeepCommonWordsOutOfSpareSlots(t *testing.T) {
	docs := []core.ContextDoc{{
		Name: "cfg.go",
		Text: "// FleetMaxQueueDepth caps accepted+running work.\nconst FleetMaxQueueDepth = 32\n",
	}}
	got, err := pickAnchors("what is the queue cap", docs)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got, "accepted") {
		t.Fatalf("an ordinary English word took a spare slot: %v", got)
	}
	want := []string{"FleetMaxQueueDepth"}
	if !slices.Equal(got, want) {
		t.Fatalf("pickAnchors = %v, want %v — a spare slot left empty beats a citable-by-accident one", got, want)
	}
}

// TestPickAnchorsTopUpWithADocNameStem is the second, narrower door into the plain pool.
// `buildinfo` is NINE characters, so a length bar alone would exclude it — yet it is the
// name of an attached file, which makes it a proper noun of this corpus and exactly what a
// citing answer reaches for. It is also the real case the top-up exists to serve: on the
// live run it never reached the pool at all.
func TestPickAnchorsTopUpWithADocNameStem(t *testing.T) {
	docs := []core.ContextDoc{{
		Name: "buildinfo.go",
		Text: "package buildinfo\n\n// buildinfo carries the compiled-in version.\nfunc SelfHash() string { return \"\" }\n",
	}}
	got, err := pickAnchors("goal with nothing distinctive", docs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "buildinfo") {
		t.Fatalf("a token naming an attached file must earn a spare slot: %v", got)
	}
	if got[0] != "SelfHash" {
		t.Fatalf("the identifier must still LEAD the alternation: %v", got)
	}
	// "compiled" is eight characters and names no file — it must not ride along.
	if slices.Contains(got, "compiled") {
		t.Fatalf("a short ordinary word slipped in beside the stem: %v", got)
	}
}

// TestPickAnchorsRejectBlobs: identRe has no upper bound and identifierShaped says yes to
// anything carrying a digit, so a lockfile hash or an integrity map would otherwise seat a
// 40-to-500-character string as an "identifier" — an acceptance check no answer could ever
// cite, and an absurd `acceptance` field in the response. Reachable from an ordinary input:
// a config directory that happens to hold a lockfile.
func TestPickAnchorsRejectBlobs(t *testing.T) {
	blob := "sha512" + strings.Repeat("a1b2c3d4", 12) // 102 chars, digit-bearing, count 1
	docs := []core.ContextDoc{{
		Name: "lock.json",
		Text: blob + " " + blob + "X realIdentifierName realIdentifierName",
	}}
	got, err := pickAnchors("goal with nothing distinctive", docs)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if len(a) > anchorMaxLen {
			t.Fatalf("a %d-character blob was seated as an anchor: %q", len(a), a)
		}
	}
	if !slices.Contains(got, "realIdentifierName") {
		t.Fatalf("the real identifier must survive the blob filter: %v", got)
	}
}

// TestBuildContractRefusesWhenOnlyBlobsRemain: with nothing but blobs there is no anchor,
// and a refusal is the honest answer — never a check built on a 100-character hash.
func TestBuildContractRefusesWhenOnlyBlobsRemain(t *testing.T) {
	dir := t.TempDir()
	blob := strings.Repeat("9f8e7d6c", 14) // 112 chars
	p := write(t, dir, "lock.json", blob+" "+blob+"a "+blob+"b")
	if _, err := BuildContract("what is here", []string{p}, dir); !errors.Is(err, ErrNoAnchor) {
		t.Fatalf("a blob-only file must refuse, got %v", err)
	}
}

// TestPickAnchorsCapsTheAlternatives keeps the check readable and the regex bounded.
func TestPickAnchorsCapsTheAlternatives(t *testing.T) {
	docs := []core.ContextDoc{{Name: "a.go", Text: "AlphaMarkerOne BetaMarkerTwo GammaMarkerThree DeltaMarkerFour EpsilonMarkerFive"}}
	got, err := pickAnchors("goal text with nothing distinctive", docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != anchorAlternatives {
		t.Fatalf("pickAnchors returned %d anchors, want the cap of %d: %v", len(got), anchorAlternatives, got)
	}
}
