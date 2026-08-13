// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// cli-printing-press: novel-scaffold-test
// Novel command scaffold tests. Keep the wiring smoke test and add behavior cases as needed.

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/cliutil/testenv"
)

// TestNovelSeatLogHelpWires smoke-tests that the seat log command
// resolves at runtime and renders useful --help output. Catches wiring
// regressions (missing AddCommand, panicking RunE on --help, etc.) before
// review. Keep this smoke test when adding behavior-specific cases.
func TestNovelSeatLogHelpWires(t *testing.T) {
	cmd := RootCmd()
	cmd.SetArgs([]string{"seat", "log", "--help"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("seat log --help error = %v (novel command not wired correctly?)", err)
	}
	help := out.String()
	for _, want := range []string{"Usage:", "log"} {
		if !strings.Contains(help, want) {
			t.Fatalf("seat log --help missing %q in output:\n%s", want, help)
		}
	}
}

// --- seat log behavior -------------------------------------------------

const seatLogFixtureBody = `startPort: 9200

macros:
  server: "/opt/llama.cpp/llama-server --port ${PORT}"
  mdir:   "/models"

models:
  # ===== the workhorse =====
  "worker":
    cmd: "${server} -m ${mdir}/worker.gguf -ngl 99 -c 65536"   # ctx raised on ledger evidence
    aliases: ["work"]
    ttl: 300
`

// TestSeatLogChronologyFromCorpus builds a synthetic backup series with the
// awkward shapes a real corpus has — a byte-identical pair, a filename whose
// date disagrees with its mtime, a copy in a dated subdirectory, and a
// non-.yaml-suffixed copy — and asserts the chronology reads them correctly.
func TestSeatLogChronologyFromCorpus(t *testing.T) {
	testenv.Isolate(t, cliutil.StateDir)
	dir := t.TempDir()
	live := filepath.Join(dir, "llama-swap.yaml")

	v1 := strings.Replace(seatLogFixtureBody, "-c 65536", "-c 8192", 1)
	v1 = strings.Replace(v1, "# ctx raised on ledger evidence", "# initial", 1)
	v2 := strings.Replace(seatLogFixtureBody, "-c 65536", "-c 32768", 1)

	write := func(p, body string, mt time.Time) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "backup-2026-07-25", "llama-swap.yaml"), v1, time.Date(2026, 7, 25, 8, 0, 0, 0, time.Local))
	write(filepath.Join(dir, "llama-swap.yaml.pre-matrix"), v2, time.Date(2026, 8, 1, 8, 0, 0, 0, time.Local))
	// Byte-identical pair.
	write(filepath.Join(dir, "backup-2026-08-02-a.yaml"), v2, time.Date(2026, 8, 2, 8, 0, 0, 0, time.Local))
	write(filepath.Join(dir, "backup-2026-08-02-b.yaml"), v2, time.Date(2026, 8, 2, 9, 0, 0, 0, time.Local))
	// Label date disagrees with mtime.
	write(filepath.Join(dir, "backup-2026-08-09-mislabeled.yaml"), seatLogFixtureBody, time.Date(2026, 8, 5, 8, 0, 0, 0, time.Local))
	write(live, seatLogFixtureBody, time.Date(2026, 8, 6, 8, 0, 0, 0, time.Local))

	rep, err := buildSeatLog(context.Background(), live, "work", false, true)
	if err != nil {
		t.Fatalf("buildSeatLog: %v", err)
	}
	if rep.Model != "worker" || rep.MatchedBy != "alias:work" {
		t.Errorf("alias resolution: model=%q matchedBy=%q", rep.Model, rep.MatchedBy)
	}
	if rep.Corpus.HistoricalSources != 5 {
		t.Errorf("historical sources = %d, want 5", rep.Corpus.HistoricalSources)
	}
	if rep.Corpus.FlatHistoricalFiles != 4 {
		t.Errorf("flat historical = %d, want 4", rep.Corpus.FlatHistoricalFiles)
	}
	if len(rep.Corpus.IdenticalPairs) != 1 {
		t.Errorf("identical pairs = %+v", rep.Corpus.IdenticalPairs)
	}
	if len(rep.Corpus.LabelDateMismatches) != 1 || !strings.Contains(rep.Corpus.LabelDateMismatches[0].File, "mislabeled") {
		t.Errorf("label mismatches = %+v", rep.Corpus.LabelDateMismatches)
	}
	if len(rep.Corpus.OrphanBackups) != 1 || !strings.Contains(rep.Corpus.OrphanBackups[0], "mislabeled") {
		t.Errorf("orphan backups = %v (the mislabeled copy is byte-identical to live)", rep.Corpus.OrphanBackups)
	}
	if len(rep.Corpus.NonFlatSources) != 1 {
		t.Errorf("subdirectory source not reported: %v", rep.Corpus.NonFlatSources)
	}

	// 3 distinct content states; the byte-identical twin must not replay.
	if rep.States != 3 {
		var got []string
		for _, e := range rep.Timeline {
			got = append(got, e.Source)
		}
		t.Fatalf("states = %d (%v), want 3", rep.States, got)
	}
	if rep.Timeline[0].Change != "created" {
		t.Errorf("first state = %q, want created", rep.Timeline[0].Change)
	}
	deltas := map[string]string{}
	for _, e := range rep.Timeline[1:] {
		for _, d := range e.Deltas {
			deltas[d.From+"->"+d.To] = d.Flag
		}
	}
	if deltas["8192->32768"] != "-c" || deltas["32768->65536"] != "-c" {
		t.Errorf("context-size chronology not reconstructed: %+v", deltas)
	}
	// The annotation that landed WITH a flag change is the WHY. Here the
	// operator's note changed on the same edit that moved -c from 8192 to
	// 32768, so that state — not merely the newest one — must carry both.
	var annotated *seatLogEntry
	for i := range rep.Timeline {
		for _, d := range rep.Timeline[i].Deltas {
			if d.Flag == "-c" && d.From == "8192" && d.To == "32768" {
				annotated = &rep.Timeline[i]
			}
		}
	}
	if annotated == nil {
		t.Fatalf("the 8192->32768 change is missing from the timeline: %+v", rep.Timeline)
	}
	if !annotated.CommentChanged {
		t.Error("the annotation change accompanying that flag change was missed")
	}
	foundNote := false
	for _, ic := range annotated.Inline {
		if strings.Contains(ic.Text, "ledger evidence") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Errorf("inline annotation not carried alongside the flag delta: %+v", annotated.Inline)
	}
}

func TestSeatLogUnknownModel(t *testing.T) {
	testenv.Isolate(t, cliutil.StateDir)
	dir := t.TempDir()
	live := filepath.Join(dir, "llama-swap.yaml")
	if err := os.WriteFile(live, []byte(seatLogFixtureBody), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := buildSeatLog(context.Background(), live, "no-such-seat", false, true)
	if err == nil {
		t.Fatal("an unknown seat must fail")
	}
	if ExitCode(err) != ExitModelNotFound {
		t.Fatalf("exit code = %d, want %d", ExitCode(err), ExitModelNotFound)
	}
}

// TestSeatLogRealCorpusAcceptance asserts the independently-verified facts of
// the reference deployment's real backup corpus. It SKIPS when that corpus is
// not present, so it is a machine-specific acceptance gate, not a portable
// unit test. On failure it prints what it actually found — a fixture number
// that disagrees with reality is a fact about the corpus, not a licence to
// bend the code until the expected number appears.
//
// The assertions are FLOORS and structural invariants, not frozen equalities.
// The corpus is a live directory that gains a file every time a seat is
// changed, so `== 19` was a countdown to a false red: on 2026-08-13 one new
// backup (llama-swap.backup-2026-08-13-pre-reranker-gpu.yaml, byte-identical
// to its 08-12 predecessor) moved sources 19->20, flat files 17->18 and
// identical pairs 2->3 while the code under test was untouched. What the gate
// is actually for — that non-glob outliers are discovered, that byte-identical
// copies are detected by content rather than by filename, and that label/mtime
// lies are surfaced — is expressed directly below instead.
//
// The floors are the counts verified by hand on 2026-08-13 (20/18/16/3/5).
// They may only be raised, and only against a re-verified corpus.
func TestSeatLogRealCorpusAcceptance(t *testing.T) {
	const realConfig = `C:\llama-swap\llama-swap.yaml`
	if _, err := os.Stat(realConfig); err != nil {
		t.Skip("reference corpus not present on this machine")
	}
	testenv.Isolate(t, cliutil.StateDir)
	rep, err := buildSeatLog(context.Background(), realConfig, "bge-reranker-v2-m3", false, true)
	if err != nil {
		t.Fatalf("buildSeatLog: %v", err)
	}
	c := rep.Corpus
	floors := []struct {
		name  string
		got   int
		floor int
		extra any
	}{
		{"historical sources", c.HistoricalSources, 20, c.NonFlatSources},
		{"flat historical files", c.FlatHistoricalFiles, 18, nil},
		{"distinct content states", c.DistinctContentStates, 15, nil},
		{"byte-identical pairs", len(c.IdenticalPairs), 3, c.IdenticalPairs},
		{"label/mtime mismatches", len(c.LabelDateMismatches), 5, c.LabelDateMismatches},
	}
	for _, f := range floors {
		if f.got < f.floor {
			t.Errorf("%s = %d, want at least %d (the corpus only grows; a DROP means discovery regressed): %+v",
				f.name, f.got, f.floor, f.extra)
		}
	}
	// Content-addressed dedupe must never claim more distinct states than
	// there are files, and duplicates must actually reduce the count.
	if c.DistinctContentStates > c.FlatHistoricalFiles {
		t.Errorf("distinct content states (%d) exceeds flat files (%d)", c.DistinctContentStates, c.FlatHistoricalFiles)
	}
	if len(c.IdenticalPairs) > 0 && c.DistinctContentStates >= c.FlatHistoricalFiles {
		t.Errorf("%d byte-identical pairs found but distinct states (%d) did not fall below the file count (%d): dedupe is by filename, not content",
			len(c.IdenticalPairs), c.DistinctContentStates, c.FlatHistoricalFiles)
	}
	// The two copies a backup-*.yaml glob would miss.
	joined := strings.Join(c.NonFlatSources, " ")
	for _, want := range []string{"backup-2026-07-25/llama-swap.yaml", "backup-2026-07-26/llama-swap.yaml.pre-matrix"} {
		if !strings.Contains(joined, want) {
			t.Errorf("non-glob outlier %s not discovered; found: %v", want, c.NonFlatSources)
		}
	}
	// Whether any backup happens to be byte-identical to the LIVE file is a
	// property of the day (it stops being true the moment a seat is edited),
	// not of the code, so its presence is reported rather than required.
	t.Logf("corpus: %d sources, %d flat, %d distinct, %d identical pairs, %d orphan backups",
		c.HistoricalSources, c.FlatHistoricalFiles, c.DistinctContentStates, len(c.IdenticalPairs), len(c.OrphanBackups))
}
