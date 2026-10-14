package agent

// Characterization of the DEFAULT compaction ladder, byte for byte.
//
// The ladder is the loop's only defence against a context overflow, and every
// optional rung added to it must be invisible until it is switched on. This
// test pins that: it runs the ladder over fixed transcripts (the committed
// mini corpus plus the frozen search-shaped and mixed fixtures of
// ladder_fixtures_test.go) at three budgets under five option sets, and compares
// the sha256 of each result (messages, tool calls and the fit verdict) with a
// golden recorded from the ladder BEFORE any optional rung beyond GCF and
// skeleton existed.
//
// The golden is a characterization, not a hand-written expectation. It is
// recorded once, from the unmodified ladder, and it must keep passing unchanged
// when a rung is added behind a flag. When a change to the DEFAULT ladder is
// intended (a rung's default flips, a rung is reordered), re-record it on
// purpose and say so in the change:
//
//	LADDER_GOLDEN_WRITE=1 go test -run TestDefaultLadderMatchesPinnedGolden ./internal/agent
//
// Re-recording to make a red test green is the thing this file exists to stop.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const ladderGoldenPath = "testdata/ladder_default_golden.json"

// ladderGoldenBase names the tree the golden was recorded from.
const ladderGoldenBase = "9b55cb36"

type ladderGolden struct {
	Note  string            `json:"note"`
	Base  string            `json:"recorded_from"`
	Cases map[string]string `json:"cases"`
}

// miniCorpusCases decodes the committed mini corpus with a local struct (the
// compeval package imports this one, so it cannot be imported here) and the
// replay knobs compeval applies by default: keep the last turn, protect the first.
func miniCorpusCases(t *testing.T) []ladderGoldenCase {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "compeval", "mini-corpus.jsonl"))
	if err != nil {
		t.Fatalf("open mini corpus: %v", err)
	}
	defer f.Close()
	type wireCall struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Args string `json:"args"`
	}
	type wireTurn struct {
		Role       string     `json:"role"`
		Content    string     `json:"content"`
		ToolCalls  []wireCall `json:"tool_calls"`
		ToolCallID string     `json:"tool_call_id"`
	}
	var out []ladderGoldenCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e struct {
			ID    string     `json:"id"`
			Turns []wireTurn `json:"turns"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("decode mini corpus entry: %v", err)
		}
		msgs := make([]Msg, len(e.Turns))
		for i, tr := range e.Turns {
			m := Msg{Role: tr.Role, Content: tr.Content, ToolCallID: tr.ToolCallID}
			for _, c := range tr.ToolCalls {
				m.ToolCalls = append(m.ToolCalls, ToolCall{ID: c.ID, Name: c.Name, Args: c.Args})
			}
			msgs[i] = m
		}
		out = append(out, ladderGoldenCase{name: "mini-" + e.ID, msgs: msgs, keep: 1, protected: 1})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read mini corpus: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("mini corpus is empty")
	}
	return out
}

// ladderDigest is a length-prefixed, order-sensitive hash of a transcript and
// its fit verdict: any changed byte, role, id, tool call or message count moves it.
func ladderDigest(msgs []Msg, v fitVerdict) string {
	h := sha256.New()
	put := func(s string) { fmt.Fprintf(h, "%d:%s|", len(s), s) }
	put(fmt.Sprintf("n=%d v=%d", len(msgs), v))
	for _, m := range msgs {
		put(m.Role)
		put(m.Content)
		put(m.ToolCallID)
		put(fmt.Sprint(m.IsError))
		put(fmt.Sprint(len(m.ToolCalls)))
		for _, c := range m.ToolCalls {
			put(c.ID)
			put(c.Name)
			put(c.Args)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

type ladderGoldenOpts struct {
	name string
	opts compactOpts
}

func ladderGoldenOptSets() []ladderGoldenOpts {
	return []ladderGoldenOpts{
		{"zero", compactOpts{}},
		{"gcf", compactOpts{GCF: true}},
		{"skeleton", compactOpts{Skeleton: true}},
		{"gcf+skeleton", compactOpts{GCF: true, Skeleton: true}},
		{"gcf+skeleton+pinned", compactOpts{GCF: true, Skeleton: true, Pinned: map[string]bool{"c1": true}}},
	}
}

// ladderGoldenBudgetPcts are the budgets as a percentage of each transcript's
// own estimate: 90 (one or two rungs), 60 (the eval's default pressure), 25
// (every rung up to the drop rung).
var ladderGoldenBudgetPcts = []int{90, 60, 25}

func ladderGoldenCurrent(t *testing.T) map[string]string {
	t.Helper()
	cases := append(miniCorpusCases(t), ladderGoldenSynthetic()...)
	got := map[string]string{}
	for _, c := range cases {
		est := estimateTokens(c.msgs)
		for _, pct := range ladderGoldenBudgetPcts {
			budget := est * pct / 100
			if budget < 1 {
				budget = 1
			}
			for _, o := range ladderGoldenOptSets() {
				// compact copies its input, but hand each run its own slice anyway so
				// one case can never see another's mutation.
				in := make([]Msg, len(c.msgs))
				copy(in, c.msgs)
				out, v := compactWithVerdict(context.Background(), in, budget, c.keep, c.protected, o.opts)
				got[fmt.Sprintf("%s|b%d|%s", c.name, pct, o.name)] = ladderDigest(out, v)
			}
		}
	}
	return got
}

func TestDefaultLadderMatchesPinnedGolden(t *testing.T) {
	got := ladderGoldenCurrent(t)

	if os.Getenv("LADDER_GOLDEN_WRITE") == "1" {
		g := ladderGolden{
			Note:  "sha256 of the compaction ladder's output (messages, tool calls, fit verdict) per case, budget percentage and option set; recorded before the search-results rung existed; see ladder_default_golden_test.go",
			Base:  ladderGoldenBase,
			Cases: got,
		}
		b, err := json.MarshalIndent(g, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ladderGoldenPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %d digests to %s", len(got), ladderGoldenPath)
		return
	}

	raw, err := os.ReadFile(ladderGoldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var want ladderGolden
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	if len(want.Cases) != len(got) {
		t.Errorf("golden holds %d cases, the fixtures produce %d", len(want.Cases), len(got))
	}
	var diverged []string
	for k, g := range got {
		if w, ok := want.Cases[k]; !ok {
			diverged = append(diverged, k+" (not in the golden)")
		} else if w != g {
			diverged = append(diverged, k)
		}
	}
	for k := range want.Cases {
		if _, ok := got[k]; !ok {
			diverged = append(diverged, k+" (in the golden, not produced)")
		}
	}
	sort.Strings(diverged)
	if len(diverged) > 0 {
		t.Fatalf("the default ladder no longer matches the pinned golden (%d of %d cases): %s\n"+
			"An optional rung must be invisible while its flag is off. If the DEFAULT ladder was meant to change, re-record the golden deliberately (see the file header).",
			len(diverged), len(got), strings.Join(diverged, ", "))
	}
}
