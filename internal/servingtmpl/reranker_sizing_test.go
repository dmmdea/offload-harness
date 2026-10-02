package servingtmpl

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Register A-122b (2026-10-01, ADR 0049 Amendment 5): the memory stack's reranker must start and stay resident
// beside the loaded ampere-16 agent seat with about 1 GiB of the card free. The memory stack's client truncates every
// document to 6,000 characters and sends each (query, document) pair as ONE non-causal sequence, which has to fit
// one ubatch and one slot: at most about 1,500-2,000 tokens. So the reranker is served at --batch-size 2048
// --ubatch-size 2048 on every template that renders it. The previous 8192/4096/4096 failed to start beside the
// loaded seat 7 times (card peak 14,717 MiB, 639 MiB free); 4096/2048/2048 peaked at 13,987 MiB with 1,369 MiB free.
//
// --ctx-size is the one number that differs between templates, and the reason is what this gate protects: llama.cpp
// DIVIDES --ctx-size among its slots (TestPairSpanningTemplatesShipTheMeasuredFanoutTwin records the in-house
// measurement). The Linux templates run the reranker on one slot, so 4096 is one 4,096-token slot: the measured
// point. The two Windows Blackwell templates pass --parallel 4, so 4096 there would leave 1,024 tokens per slot,
// below a worst-case pair, and long documents would fail; they keep 8192, which is 2,048 per slot, unchanged.
var rerankerSizing = []struct {
	file     string
	ctx      int
	parallel int // slots: 1 when the template passes no --parallel
}{
	{"llama-swap.linux-cuda.yaml", 4096, 1},
	{"llama-swap.linux-vulkan.yaml", 4096, 1},
	{"llama-swap.win-dual-blackwell.yaml", 8192, 4},
	{"llama-swap.win-triple-blackwell.yaml", 8192, 4},
}

// rerankerWorstCasePairTokens is the memory stack's worst-case (query, document) pair: a document truncated to 6,000
// characters plus its query. Both the ubatch and a slot must hold it.
const rerankerWorstCasePairTokens = 2048

var intFlagRe = map[string]*regexp.Regexp{}

// cmdIntFlag reads `--flag N` out of a joined command line, and reports whether the flag is there.
func cmdIntFlag(cmd, flag string) (int, bool) {
	re, ok := intFlagRe[flag]
	if !ok {
		re = regexp.MustCompile(regexp.QuoteMeta(flag) + `\s+(\d+)`)
		intFlagRe[flag] = re
	}
	m := re.FindStringSubmatch(cmd)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// rerankerCmd returns the bge-reranker-v2-m3 entry's cmd of a raw template, its folded lines joined by single
// spaces and its comments dropped.
func rerankerCmd(t *testing.T, file, tmpl string) string {
	t.Helper()
	const head = "\n  bge-reranker-v2-m3:\n"
	i := strings.Index(tmpl, head)
	if i < 0 {
		t.Fatalf("%s has no bge-reranker-v2-m3 entry: this gate went blind", file)
	}
	var cmd []string
	in := false
	for _, line := range strings.Split(tmpl[i+len(head):], "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case trim == "" || strings.HasPrefix(trim, "#"):
			continue
		case !strings.HasPrefix(line, "    "):
			// the entry ends at the next line indented by two spaces or fewer
			return strings.Join(cmd, " ")
		case strings.HasPrefix(trim, "cmd:"):
			in = true
		case !strings.HasPrefix(line, "      "):
			// another key of the entry (checkEndpoint, ttl, env): the folded cmd is over
			in = false
		case in:
			cmd = append(cmd, trim)
		}
	}
	return strings.Join(cmd, " ")
}

// TestEveryTemplateServesTheRerankerAtTheMeasuredSizing pins the reranker's batch, ubatch, context and slot count on
// every template that renders it, and fails when 8192/4096/4096 comes back or when a slot can no longer hold a
// worst-case pair. It is exact in both directions: a template that renders a reranker this list does not name is a
// reranker nobody sized beside the agent seat.
func TestEveryTemplateServesTheRerankerAtTheMeasuredSizing(t *testing.T) {
	const why = "the memory stack's reranker scores each pair as one non-causal sequence (at most about 2,000 tokens) that must fit one ubatch and one slot, and its 8192/4096/4096 sizing failed to start beside the loaded ampere-16 agent seat 7 times (ADR 0049 Amendment 5, register A-122b); re-measure beside the seat before changing it"
	dir := filepath.Join("..", "..", "setup", "templates")
	listed := map[string]bool{}
	for _, c := range rerankerSizing {
		listed[c.file] = true
		raw, err := os.ReadFile(filepath.Join(dir, c.file))
		if err != nil {
			t.Fatal(err)
		}
		cmd := rerankerCmd(t, c.file, string(raw))
		ctx, okCtx := cmdIntFlag(cmd, "--ctx-size")
		batch, okBatch := cmdIntFlag(cmd, "--batch-size")
		ubatch, okUbatch := cmdIntFlag(cmd, "--ubatch-size")
		parallel, okPar := cmdIntFlag(cmd, "--parallel")
		if !okPar {
			parallel = 1
		}
		if !okCtx || !okBatch || !okUbatch {
			t.Errorf("%s: the reranker command does not carry --ctx-size, --batch-size and --ubatch-size (got %q): %s", c.file, cmd, why)
			continue
		}
		if batch != 2048 || ubatch != 2048 {
			t.Errorf("%s: reranker --batch-size %d / --ubatch-size %d, want 2048 / 2048: %s", c.file, batch, ubatch, why)
		}
		if parallel != c.parallel {
			t.Errorf("%s: reranker --parallel %d, want %d: llama.cpp divides --ctx-size among its slots, so the slot count is part of the sizing: %s", c.file, parallel, c.parallel, why)
		}
		if ctx != c.ctx {
			t.Errorf("%s: reranker --ctx-size %d, want %d (with %d slot(s)): %s", c.file, ctx, c.ctx, c.parallel, why)
		}
		if perSlot := ctx / parallel; perSlot < rerankerWorstCasePairTokens {
			t.Errorf("%s: reranker --ctx-size %d over %d slot(s) is %d tokens per slot, below the %d a worst-case pair needs: %s", c.file, ctx, parallel, perSlot, rerankerWorstCasePairTokens, why)
		}
		if ubatch < rerankerWorstCasePairTokens {
			t.Errorf("%s: reranker --ubatch-size %d is below the %d tokens a worst-case pair needs in one ubatch: %s", c.file, ubatch, rerankerWorstCasePairTokens, why)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "llama-swap.") || !strings.HasSuffix(e.Name(), ".yaml") || listed[e.Name()] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "--reranking") {
			t.Errorf("%s renders a reranker that rerankerSizing does not list: size it beside the agent seat and register it, so the guard protects it", e.Name())
		}
	}
}
