package main

// Verb-level tests for the compaction-eval rung flags (--search-results,
// --log-collapse) and the kvbench additions (--arms, --nonce, sent_sha256).
// The library behaviour is pinned in internal/compeval; these tests pin that
// the verb parses the flags and hands them to the instruments, and that
// without them the output is the one the verb produced before.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/compeval"
)

func ceReadNormalized(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// Without a rung flag, `run` prints what it printed before the flags existed.
func TestCompactionEvalRunWithoutRungFlagsIsByteIdentical(t *testing.T) {
	corpus := filepath.Join("testdata", "compeval", "mini-corpus.jsonl")
	cases := []struct {
		name   string
		args   []string
		golden string
	}{
		{"no rung", nil, "run-base.golden.json"},
		{"gcf and skeleton", []string{"--gcf", "--skeleton"}, "run-gcf-skeleton.golden.json"},
	}
	for _, c := range cases {
		out := captureStdout(t, func() {
			if err := runCompactionEval(append([]string{"run", "--corpus", corpus}, c.args...)); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
		})
		want := ceReadNormalized(t, filepath.Join("testdata", "compeval", c.golden))
		if strings.ReplaceAll(out, "\r\n", "\n") != want {
			t.Errorf("%s: `run` output changed from %s", c.name, c.golden)
		}
	}
}

func rungVerbListing() string {
	var b strings.Builder
	for f := 0; f < 20; f++ {
		fmt.Fprintf(&b, "internal/pkg%02d/file%02d.go:\n", f, f)
		for h := 0; h < 5; h++ {
			text := []string{"let alpha = beta + gamma", "return delta", "count++", "x = y * 2", "defer unlock()"}[(f+h)%5]
			if f == 7 && h == 2 {
				text = "const fetchTimeout = 30 // exceeded on a large request"
			}
			fmt.Fprintf(&b, "  L%d: %s\n", 10+h*7, text)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func rungVerbLog(n int) string {
	var b strings.Builder
	b.WriteString("listening on port 18080\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "2026-10-14T09:12:%02d.%03dZ INFO  req=%08x GET /health 200 %dms\n", i%60, (i*37)%1000, uint32(i)*2654435761, 3+i%7)
		if i == n/3 {
			b.WriteString("2026-10-14T09:12:40.001Z WARN  slow request req=0badc0de took 2300ms\n")
		}
		if i == 2*n/3 {
			b.WriteString("2026-10-14T09:12:55.500Z ERROR upstream connection refused host=cache port=9000\n")
		}
	}
	b.WriteString("shutdown complete\n")
	return b.String()
}

func rungVerbFile(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%4d  func step%d(ctx context.Context) error { return run(ctx, %d) }\n", i, i, i*3)
	}
	return b.String()
}

func rungVerbEntry(id, objective string, calls ...[3]string) compeval.Entry {
	turns := []compeval.Turn{
		{Role: "system", Content: "You are a coding agent. Use the tools to answer."},
		{Role: "user", Content: objective},
	}
	for i, c := range calls {
		cid := fmt.Sprintf("c%d", i+1)
		turns = append(turns,
			compeval.Turn{Role: "assistant", ToolCalls: []compeval.TurnToolCall{{ID: cid, Name: c[0], Args: c[1]}}},
			compeval.Turn{Role: "tool", ToolCallID: cid, Content: c[2]},
		)
	}
	turns = append(turns, compeval.Turn{Role: "assistant", Content: "reading the result now"})
	return compeval.Entry{ID: id, Kind: "mixed", Turns: turns, KeepRecent: 4, ProtectedPrefix: 2}
}

// rungVerbCorpus writes a corpus with one entry per rung and returns its path.
func rungVerbCorpus(t *testing.T) string {
	t.Helper()
	search := rungVerbEntry("search", "Find where the fetch timeout is enforced and explain why a request fails with a deadline error.",
		[3]string{"search_files", `{"pattern":"timeout"}`, rungVerbListing()},
		[3]string{"read_file", `{"path":"internal/pkg07/file07.go"}`, rungVerbFile(30)},
		[3]string{"read_file", `{"path":"internal/pkg08/file08.go"}`, rungVerbFile(30)},
		[3]string{"read_file", `{"path":"internal/pkg09/file09.go"}`, rungVerbFile(10)},
	)
	logs := rungVerbEntry("logs", "Find out why the widget build is red.",
		[3]string{"run_shell", `{"command":"go test ./..."}`, rungVerbLog(170)},
		[3]string{"run_shell", `{"command":"go build ./..."}`, rungVerbLog(150)},
		[3]string{"read_file", `{"path":"widget.go"}`, rungVerbFile(30)},
		[3]string{"read_file", `{"path":"widget_test.go"}`, rungVerbFile(10)},
	)
	path := filepath.Join(t.TempDir(), "rungs.jsonl")
	if _, err := compeval.WriteCorpus(path, []compeval.Entry{search, logs}); err != nil {
		t.Fatal(err)
	}
	return path
}

func ceRunReport(t *testing.T, corpus string, flags ...string) compeval.Report {
	t.Helper()
	var rep compeval.Report
	out := captureStdout(t, func() {
		if err := runCompactionEval(append([]string{"run", "--corpus", corpus}, flags...)); err != nil {
			t.Fatalf("run %v: %v", flags, err)
		}
	})
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("run %v: %v\n%s", flags, err, out)
	}
	return rep
}

func ceTokensAfter(rep compeval.Report) map[string]int {
	m := map[string]int{}
	for _, e := range rep.Entries {
		m[e.ID] = e.TokensAfter
	}
	return m
}

func TestCompactionEvalRunRungFlagsTurnTheRungsOn(t *testing.T) {
	corpus := rungVerbCorpus(t)
	inc := ceRunReport(t, corpus, "--gcf", "--skeleton")
	search := ceRunReport(t, corpus, "--gcf", "--skeleton", "--search-results")
	logs := ceRunReport(t, corpus, "--gcf", "--skeleton", "--log-collapse")
	both := ceRunReport(t, corpus, "--gcf", "--skeleton", "--search-results", "--log-collapse")

	if inc.Ladder != "gcf+skeleton" || search.Ladder != "gcf+skeleton+search" ||
		logs.Ladder != "gcf+skeleton+logs" || both.Ladder != "gcf+skeleton+search+logs" {
		t.Fatalf("ladders: %q %q %q %q", inc.Ladder, search.Ladder, logs.Ladder, both.Ladder)
	}
	i, s, l := ceTokensAfter(inc), ceTokensAfter(search), ceTokensAfter(logs)
	if s["search"] == i["search"] || s["logs"] != i["logs"] {
		t.Errorf("--search-results: search entry %d->%d (must change), logs entry %d->%d (must not)", i["search"], s["search"], i["logs"], s["logs"])
	}
	if l["logs"] == i["logs"] || l["search"] != i["search"] {
		t.Errorf("--log-collapse: logs entry %d->%d (must change), search entry %d->%d (must not)", i["logs"], l["logs"], i["search"], l["search"])
	}
}

// ceFakeSeat records every request body and answers with a fixed completion that
// carries a timings block, so the bench can run against it.
type ceFakeSeat struct {
	mu     sync.Mutex
	bodies []string
}

func (f *ceFakeSeat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(b))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":120,"completion_tokens":1},"timings":{"cache_n":0,"prompt_n":120,"prompt_ms":10,"predicted_n":1,"predicted_ms":1}}`)
}

func ceKVBench(t *testing.T, corpus string, flags ...string) (compeval.KVReport, []string) {
	t.Helper()
	seat := &ceFakeSeat{}
	srv := httptest.NewServer(seat)
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "kv.json")
	args := append([]string{"kvbench", "--config", filepath.Join(t.TempDir(), "absent.json"),
		"--corpus", corpus, "--endpoint", srv.URL, "--model", "m", "--ctx-tokens", "8192", "--out", out}, flags...)
	// The fake seat reuses nothing, so the controls fail and the verb reports
	// INCONCLUSIVE after writing the report; the samples are what this reads.
	if err := runCompactionEval(args); err != nil && !strings.Contains(err.Error(), "INCONCLUSIVE") {
		t.Fatalf("kvbench %v: %v", flags, err)
	}
	var rep compeval.KVReport
	if err := json.Unmarshal([]byte(ceReadNormalized(t, out)), &rep); err != nil {
		t.Fatal(err)
	}
	seat.mu.Lock()
	defer seat.mu.Unlock()
	return rep, append([]string(nil), seat.bodies...)
}

func ceAnyBody(bodies []string, re *regexp.Regexp) bool {
	for _, b := range bodies {
		if re.MatchString(b) {
			return true
		}
	}
	return false
}

var (
	verbFooterRE = regexp.MustCompile(`\[\.\.\. \d+ of \d+ lines elided, \d+ of \d+ files elided \.\.\.\]`)
	verbHeaderRE = regexp.MustCompile(`\[log-collapse: original \d+ chars, \d+ lines\]`)
)

func TestCompactionEvalKVBenchRungFlagsTurnTheRungsOn(t *testing.T) {
	corpus := rungVerbCorpus(t)
	base := []string{"--gcf", "--skeleton", "--arms", "on", "--nonce", "SHARED"}

	rep, bodies := ceKVBench(t, corpus, base...)
	if rep.Ladder != "gcf+skeleton" || ceAnyBody(bodies, verbFooterRE) || ceAnyBody(bodies, verbHeaderRE) {
		t.Fatalf("without a rung flag: ladder %q, search rung ran %v, log rung ran %v", rep.Ladder, ceAnyBody(bodies, verbFooterRE), ceAnyBody(bodies, verbHeaderRE))
	}
	rep, bodies = ceKVBench(t, corpus, append(base, "--search-results")...)
	if rep.Ladder != "gcf+skeleton+search" || !ceAnyBody(bodies, verbFooterRE) {
		t.Errorf("--search-results: ladder %q, search rung ran %v", rep.Ladder, ceAnyBody(bodies, verbFooterRE))
	}
	rep, bodies = ceKVBench(t, corpus, append(base, "--log-collapse")...)
	if rep.Ladder != "gcf+skeleton+logs" || !ceAnyBody(bodies, verbHeaderRE) {
		t.Errorf("--log-collapse: ladder %q, log rung ran %v", rep.Ladder, ceAnyBody(bodies, verbHeaderRE))
	}
}

func TestCompactionEvalKVBenchArmsOnAndSharedNonce(t *testing.T) {
	corpus := rungVerbCorpus(t)
	both, _ := ceKVBench(t, corpus, "--gcf", "--skeleton", "--nonce", "SHARED")
	on, bodies := ceKVBench(t, corpus, "--gcf", "--skeleton", "--arms", "on", "--nonce", "SHARED")

	var offSamples int
	for _, s := range both.Samples {
		if s.Arm == "off" {
			offSamples++
		}
	}
	if offSamples == 0 {
		t.Fatal("the default run has no off arm")
	}
	if len(on.Samples) == 0 {
		t.Fatal("--arms on produced no samples")
	}
	for _, s := range on.Samples {
		if s.Arm != "on" {
			t.Fatalf("--arms on ran an %q-arm sample", s.Arm)
		}
	}
	if ceAnyBody(bodies, regexp.MustCompile(`/off\]`)) {
		t.Error("--arms on sent an off-arm request")
	}
	if on.RunNonce != "SHARED" || both.RunNonce != "SHARED" {
		t.Errorf("--nonce was not used: %q / %q", on.RunNonce, both.RunNonce)
	}
	// A second run with the same nonce and ladder sends byte-identical requests.
	again, _ := ceKVBench(t, corpus, "--gcf", "--skeleton", "--arms", "on", "--nonce", "SHARED")
	for i, s := range on.Samples {
		if again.Samples[i].SentSHA256 == "" || again.Samples[i].SentSHA256 != s.SentSHA256 {
			t.Fatalf("sample %d: sent_sha256 differs between two identical runs sharing a nonce", i)
		}
	}
}

func TestCompactionEvalKVBenchRefusesAnUnknownArm(t *testing.T) {
	srv := httptest.NewServer(&ceFakeSeat{})
	defer srv.Close()
	err := runCompactionEval([]string{"kvbench", "--config", filepath.Join(t.TempDir(), "absent.json"),
		"--corpus", rungVerbCorpus(t), "--endpoint", srv.URL, "--model", "m", "--ctx-tokens", "8192", "--arms", "onn"})
	if err == nil || !strings.Contains(err.Error(), "arm") {
		t.Fatalf("an unknown arm was not refused: %v", err)
	}
}

// The four new flags are named in the compaction-eval section of the docs page.
func TestCompactionEvalNewFlagsAreDocumented(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("docs", "systems", "coding-agent.md"))
	if err != nil {
		t.Fatal(err)
	}
	docs := bytes.ReplaceAll(page, []byte("\r\n"), []byte("\n"))
	for _, want := range []string{"--search-results", "--log-collapse", "--arms", "--nonce", "sent_sha256"} {
		if !bytes.Contains(docs, []byte(want)) {
			t.Errorf("docs/systems/coding-agent.md does not document %s", want)
		}
	}
}
