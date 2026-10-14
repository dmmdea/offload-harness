package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Characterization of the DEFAULT compaction ladder, captured BEFORE the log
// rung was wired in. Every digest below is the sha256 of what compact() returned
// on the unmodified ladder; the default ladder (no optional rung enabled) must
// keep producing exactly those bytes, so a rung that is off cannot change a
// single byte of a transcript. The inputs are the shipped mini replay corpus
// (testdata/compeval/mini-corpus.jsonl) and four synthetic log-shaped
// transcripts built here, because the mini corpus holds only two bodies that a
// log rung could touch and the ladder must also be pinned over multi-turn
// transcripts with repeated near-identical lines in several older bodies.

// lrCorpusEntry is the minimal decode of a mini-corpus line (internal/compeval
// imports this package, so its Entry type cannot be used from here).
type lrCorpusEntry struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Turns []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Args string `json:"args"`
		} `json:"tool_calls"`
	} `json:"turns"`
}

// lrFixture is one transcript plus the ladder knobs it is replayed with.
type lrFixture struct {
	name   string
	msgs   []Msg
	budget int
	keep   int
	prot   int
}

// lrMiniCorpus loads the shipped mini corpus with the same replay knobs the
// compaction eval derives for entries that set none: budget = 60% of the
// entry's own estimate, keep the final turn, protect the first.
func lrMiniCorpus(t *testing.T) []lrFixture {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "compeval", "mini-corpus.jsonl"))
	if err != nil {
		t.Fatalf("open mini corpus: %v", err)
	}
	defer f.Close()
	var out []lrFixture
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e lrCorpusEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("decode mini corpus line: %v", err)
		}
		msgs := make([]Msg, 0, len(e.Turns))
		for _, tu := range e.Turns {
			m := Msg{Role: tu.Role, Content: tu.Content, ToolCallID: tu.ToolCallID}
			for _, c := range tu.ToolCalls {
				m.ToolCalls = append(m.ToolCalls, ToolCall{ID: c.ID, Name: c.Name, Args: c.Args})
			}
			msgs = append(msgs, m)
		}
		budget := estimateTokens(msgs) * 6 / 10
		if budget < 1 {
			budget = 1
		}
		out = append(out, lrFixture{name: "mini/" + e.ID, msgs: msgs, budget: budget, keep: 1, prot: 1})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan mini corpus: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("mini corpus is empty")
	}
	return out
}

// lrTestLog is `go test -v` shaped output: run lines, pass lines, one failure.
func lrTestLog(n int) string {
	var b strings.Builder
	b.WriteString("go test -v ./widget/...\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "=== RUN   TestWidget_%03d\n", i)
		fmt.Fprintf(&b, "=== PAUSE TestWidget_%03d\n", i)
		fmt.Fprintf(&b, "=== CONT  TestWidget_%03d\n", i)
		if i == n/2 {
			b.WriteString("--- FAIL: TestWidget_broken (0.01s)\n")
			b.WriteString("    widget_test.go:88: got 3, want 4\n")
		}
	}
	b.WriteString("FAIL\n")
	b.WriteString("FAIL\texample.com/widget\t0.412s\n")
	b.WriteString("exit status 1\n")
	return b.String()
}

// lrServerLog is timestamped server output: one near-identical access line per
// request (timestamp, request id and latency all vary), with a slow-request
// warning and an upstream failure buried in the middle.
func lrServerLog(n int) string {
	var b strings.Builder
	b.WriteString("listening on 127.0.0.1:18080\n")
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

// lrProgressLog is a download/progress style body: counters and sizes vary.
func lrProgressLog(n int) string {
	var b strings.Builder
	b.WriteString("fetching dependencies\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  downloading module-%d.tgz %d/%d KB\n", i/50, (i*13)%1024, 1024)
	}
	b.WriteString("done: 1 package installed\n")
	return b.String()
}

// lrRepeatLog is a body of one literal line repeated, ended by a summary.
func lrRepeatLog(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("retrying connection to the build cache, attempt pending\n")
	}
	b.WriteString("build finished in 4s\n")
	return b.String()
}

// lrTranscript builds a multi-turn transcript: a protected preamble (system,
// profile exemplar, objective), one assistant+tool unit per body, then a recent
// assistant turn. The ids prefix nothing in the bodies, so identical bodies
// would be deduped by the always-on rung exactly as in production.
func lrTranscript(bodies ...string) []Msg {
	msgs := []Msg{
		{Role: "system", Content: "You are a coding agent. Keep answers short."},
		{Role: "user", Content: "exemplar: run the tests and report the result"},
		{Role: "user", Content: "objective: find out why the widget build is red"},
	}
	for i, body := range bodies {
		id := fmt.Sprintf("call-%d", i+1)
		msgs = append(msgs,
			Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Name: "run", Args: fmt.Sprintf(`{"cmd":"step %d"}`, i+1)}}},
			Msg{Role: "tool", ToolCallID: id, Content: body},
		)
	}
	msgs = append(msgs, Msg{Role: "assistant", Content: "looking at the output now"})
	return msgs
}

// lrSyntheticFixtures are the log-shaped transcripts. Budgets are fractions of
// the transcript's own estimate so the ladder genuinely works: 60% leaves the
// cheap rungs a chance, 25% forces the marker and drop rungs too.
func lrSyntheticFixtures() []lrFixture {
	build := func(name string, bodies ...string) []lrFixture {
		msgs := lrTranscript(bodies...)
		est := estimateTokens(msgs)
		return []lrFixture{
			{name: name + "/60", msgs: msgs, budget: est * 6 / 10, keep: 2, prot: 3},
			{name: name + "/25", msgs: msgs, budget: est * 25 / 100, keep: 2, prot: 3},
		}
	}
	var out []lrFixture
	out = append(out, build("synthetic/mixed-logs", lrTestLog(60), lrServerLog(150), lrProgressLog(120), lrRepeatLog(80))...)
	out = append(out, build("synthetic/server-only", lrServerLog(200), lrServerLog(220), lrServerLog(240))...)
	out = append(out, build("synthetic/test-only", lrTestLog(90), lrTestLog(70))...)
	out = append(out, build("synthetic/repeat-only", lrRepeatLog(100), lrRepeatLog(130), lrProgressLog(90))...)
	return out
}

// lrDigest hashes a transcript unambiguously: every field is length-prefixed so
// no two different transcripts share a byte stream.
func lrDigest(msgs []Msg) string {
	h := sha256.New()
	put := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	for _, m := range msgs {
		put(m.Role)
		put(m.Content)
		put(m.ToolCallID)
		if m.IsError {
			put("err")
		} else {
			put("ok")
		}
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(m.ToolCalls)))
		h.Write(n[:])
		for _, c := range m.ToolCalls {
			put(c.ID)
			put(c.Name)
			put(c.Args)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// lrGolden maps "<fixture>|<ladder>" to the sha256 of the ladder's output as
// captured on the ladder before the log rung existed.
var lrGolden = map[string]string{
	"mini/tj-1|base":                        "062321fff8bd9b232ff7ad846770190139c7c0a99a5814fab0500a2b32c609c4",
	"mini/tj-1|gcf+skeleton":                "db391f644cf28b427e2f5fbd870567e0f893aa3e22ab504bee340b83b45dc02a",
	"mini/lg-1|base":                        "2e670d0b16b5d04e010c34931f94bbc7bf78f8547d04bb730fc74cd5b3e2d060",
	"mini/lg-1|gcf+skeleton":                "2fb91e6fe014f22145c76a1c8d4b5cf9806ce7d8b1d2b5bdf31fcb1fea116a01",
	"mini/cd-1|base":                        "f006428da2703254864d9c08f73ec97673a9a3e9b24c50b91a47fbef442f5aaa",
	"mini/cd-1|gcf+skeleton":                "e2afbcb2ab0fd36236237628333fcb6922cff4a07ea8afaeb5e6b2dc90427d4d",
	"mini/pr-1|base":                        "2e2e08f894938c36cefc30991bc6bc7bf9ab7adee86969b9dbb741aeceec4633",
	"mini/pr-1|gcf+skeleton":                "2e2e08f894938c36cefc30991bc6bc7bf9ab7adee86969b9dbb741aeceec4633",
	"mini/mx-1|base":                        "413d80e8493a484e8374ac63437013ca5ff8b5ed0a878c68b5501894d23a48e5",
	"mini/mx-1|gcf+skeleton":                "413d80e8493a484e8374ac63437013ca5ff8b5ed0a878c68b5501894d23a48e5",
	"mini/tt-1|base":                        "eda559b14025edb24bdb0eab380a9ce2811c1a1ac60a7f686af7e0d922e29a4b",
	"mini/tt-1|gcf+skeleton":                "baaada48ed198a97000697e781d23d4f104bd9887d5ed1b05bf02f1c87a07f4c",
	"synthetic/mixed-logs/60|base":          "035798d642a3fd70ca8a745d35d6228c5a1bcf692d4d22f6f708ead530a60629",
	"synthetic/mixed-logs/60|gcf+skeleton":  "9b48ab5912f841f1b523b8818d945a682f6bd442e26b939c1c7d23500f7036bb",
	"synthetic/mixed-logs/25|base":          "adb09bedd5a32e57070257ed4c8d50407154752693250736be21667bcac9978b",
	"synthetic/mixed-logs/25|gcf+skeleton":  "3ba5e63a4ea28f698cd70ea2c602effc4960e1fcde4b510eb0903618387d258b",
	"synthetic/server-only/60|base":         "67eb309c2846b8e96f1a911876296b9585631f44a3798e986c5f9d014c8692c0",
	"synthetic/server-only/60|gcf+skeleton": "3684a199b65b99f53200aab8597f27b55fa674bb82eedec99bd2ccc9d53a8c82",
	"synthetic/server-only/25|base":         "6f38fc6b23eaa6e77bb0d5fc338cba6a37e0882a7c0f87cd3c4f625977902906",
	"synthetic/server-only/25|gcf+skeleton": "6f38fc6b23eaa6e77bb0d5fc338cba6a37e0882a7c0f87cd3c4f625977902906",
	"synthetic/test-only/60|base":           "210c46a284dd1decf855e4c447e34cd07f4220a7fda9077fc55650fef386638a",
	"synthetic/test-only/60|gcf+skeleton":   "acbcd72cf8f577221ed741b48889721551ab2c98c0c47335e3ebbe8ce4db128a",
	"synthetic/test-only/25|base":           "7ea8d83644dc25b9bca009a80a137161286a97d6a45ad09da637bbcf3bf53a91",
	"synthetic/test-only/25|gcf+skeleton":   "7ea8d83644dc25b9bca009a80a137161286a97d6a45ad09da637bbcf3bf53a91",
	"synthetic/repeat-only/60|base":         "8885c4d592e2818f18d5f2b6a8af1278baf9a3ef27d9dd6e7a5dd25e3fa7d377",
	"synthetic/repeat-only/60|gcf+skeleton": "3457dbf81a2e4eb024b8080fe34957007b3548d0b5e6e2bc02ccb213caa41005",
	"synthetic/repeat-only/25|base":         "8885c4d592e2818f18d5f2b6a8af1278baf9a3ef27d9dd6e7a5dd25e3fa7d377",
	"synthetic/repeat-only/25|gcf+skeleton": "8885c4d592e2818f18d5f2b6a8af1278baf9a3ef27d9dd6e7a5dd25e3fa7d377",
}

// TestLogsRungDefaultLadderBytesPinned replays every fixture through the
// default ladder (no optional rung) and through the shipped production setting
// (GCF and skeleton on), and demands the exact bytes the unmodified ladder
// produced. It is the proof that a rung which is off changes nothing.
func TestLogsRungDefaultLadderBytesPinned(t *testing.T) {
	fixtures := append(lrMiniCorpus(t), lrSyntheticFixtures()...)
	ladders := []struct {
		name string
		opts compactOpts
	}{
		{"base", compactOpts{}},
		{"gcf+skeleton", compactOpts{GCF: true, Skeleton: true}},
	}
	for _, fx := range fixtures {
		for _, ld := range ladders {
			key := fx.name + "|" + ld.name
			out := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, ld.opts)
			got := lrDigest(out)
			if len(lrGolden) == 0 {
				t.Logf("GOLDEN %q: %q,", key, got)
				continue
			}
			want, ok := lrGolden[key]
			if !ok {
				t.Errorf("no pinned digest for %s", key)
				continue
			}
			if got != want {
				t.Errorf("default ladder bytes changed for %s: got %s want %s", key, got, want)
			}
		}
	}
	if len(lrGolden) != 0 && len(lrGolden) != len(fixtures)*len(ladders) {
		t.Errorf("pinned %d digests but %d fixture/ladder pairs exist", len(lrGolden), len(fixtures)*len(ladders))
	}
}
