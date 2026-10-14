package compeval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

// cbParityCases pin the sha256 of the streamed operations (the chunks.jsonl bytes)
// of one instance per (family, seed, knobs). The digests were taken from the
// original generator at the commit named in the ported files' headers, run with the
// same parameters; matching them byte for byte is what "seed 42 here is seed 42
// there" means. Only digests are kept: no upstream output is committed.
var cbParityCases = []struct {
	name   string
	family string
	seed   int64
	knobs  CBKnobs
	nOps   int
	sha    string
}{
	{"kv-R130-s42", CBFamilyKV, 42, CBKnobs{Records: 130, ValueWords: 24, Queries: 24, RecordsPerChunk: 100}, 26, "6dbb403e4ec0c5c92a4de92a15ac196d95724bc1e2a155a6eb3cfc93cca2e5e8"},
	{"kv-R1000-s43", CBFamilyKV, 43, CBKnobs{Records: 1000, ValueWords: 24, Queries: 24, RecordsPerChunk: 100}, 34, "7f53ebb0b4238fd5f6fcc35aba0cf3db32a01e254f320b1bf5431b7d0bb1c895"},
	{"kv-more-queries-than-records", CBFamilyKV, 7, CBKnobs{Records: 10, ValueWords: 3, Queries: 30, RecordsPerChunk: 4}, 33, "e39a12f0a6eff75ff4d9a369fee5fe536d12bd36b07114f42f9975eb7413cc51"},
	{"log-L160-s42", CBFamilyLog, 42, CBKnobs{Lines: 160, LinesPerBatch: 14, Queries: 24}, 36, "b943d1a90c6f665f4e9fb2ddee4c4bee37ef66268c12758bcefd54833df81df5"},
	{"log-L40-s45", CBFamilyLog, 45, CBKnobs{Lines: 40, LinesPerBatch: 14, Queries: 10}, 13, "4434ac3235d48a8cd8a3faed1969ab2e0cf57025cac0582a03f614cb571ddfda"},
	{"log-L1600-s49", CBFamilyLog, 49, CBKnobs{Lines: 1600, LinesPerBatch: 14, Queries: 24}, 139, "9141f7602ad36f845b9903c619d8dc265177501f4132d90127a809ac59828416"},
	{"needle-K4-C4-s42", CBFamilyNeedle, 42, CBKnobs{NeedlesPerChunk: 4, Chunks: 4, FillerLines: 140, WordsPerLine: 10}, 4, "62ae7b633a0e37446c9b81e1f7072ae1ff2cb170da0c5d5bdb3326075097ba22"},
	{"needle-K8-C6-s46", CBFamilyNeedle, 46, CBKnobs{NeedlesPerChunk: 8, Chunks: 6, FillerLines: 20, WordsPerLine: 5}, 6, "8dfc865a8d177a46b59a5fca08b49de56dc5fcc54a94da0ce09900bce3d405fd"},
	{"sudoku-t30-s42", CBFamilySudoku, 42, CBKnobs{Box: 4, Givens: 90, Boards: 1, Turns: 30}, 30, "6cf1bb9a3d4bc22208dd53e022694cc4d562978b232103139c292ff8fee7e883"},
	{"sudoku-t4-s47", CBFamilySudoku, 47, CBKnobs{Box: 4, Givens: 90, Boards: 1, Turns: 4}, 4, "5b03d2f2209ba1bb76f53155b24b9eb7b2b34791475ad2b17c74e150da084f2d"},
	{"sudoku-two-boards-by-turns", CBFamilySudoku, 44, CBKnobs{Box: 4, Givens: 90, Boards: 1, Turns: 200}, 200, "620697b6f55be0bd36dca7a68f9db5b9d8a4bf2df060b94e23ac667f04b8dd07"},
	{"sudoku-two-boards-by-count", CBFamilySudoku, 9, CBKnobs{Box: 3, Givens: 30, Boards: 2}, 103, "798d3999a7e18b4fa6e58792dadfc645ee9f9505a4764eda2c349a740f2aa5ed"},
}

func cbGenerateKnobs(t *testing.T, family string, seed int64, k CBKnobs) *CBInstance {
	t.Helper()
	switch family {
	case CBFamilyKV:
		return GenerateKV(seed, k)
	case CBFamilyLog:
		return GenerateLog(seed, k)
	case CBFamilyNeedle:
		return GenerateNeedle(seed, k)
	case CBFamilySudoku:
		return GenerateSudoku(seed, k)
	}
	t.Fatalf("unknown family %q", family)
	return nil
}

func TestContextBenchGeneratorsMatchTheOriginalStreamByteForByte(t *testing.T) {
	for _, c := range cbParityCases {
		t.Run(c.name, func(t *testing.T) {
			in := cbGenerateKnobs(t, c.family, c.seed, c.knobs)
			if len(in.Ops) != c.nOps {
				t.Fatalf("%d ops, want %d", len(in.Ops), c.nOps)
			}
			sum := sha256.Sum256(in.OpsJSONL())
			if got := hex.EncodeToString(sum[:]); got != c.sha {
				t.Fatalf("ops digest %s, want %s", got, c.sha)
			}
		})
	}
}

func TestContextBenchGeneratorIsDeterministicPerSeed(t *testing.T) {
	for _, family := range CBFamilies {
		a, err := GenerateCB(family, cbSmallLevel(family), 42)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := GenerateCB(family, cbSmallLevel(family), 42)
		c, _ := GenerateCB(family, cbSmallLevel(family), 43)
		if !bytes.Equal(a.OpsJSONL(), b.OpsJSONL()) {
			t.Errorf("%s: same seed gave different bytes", family)
		}
		if bytes.Equal(a.OpsJSONL(), c.OpsJSONL()) {
			t.Errorf("%s: different seeds gave the same bytes", family)
		}
		if a.ID != family+"-"+cbSmallLevel(family)+"-seed42" {
			t.Errorf("%s: id %q", family, a.ID)
		}
	}
}

func cbSmallLevel(family string) string {
	return map[string]string{CBFamilyKV: "R130", CBFamilyLog: "L160", CBFamilyNeedle: "K4-C4", CBFamilySudoku: "t4"}[family]
}

func TestContextBenchLevelLookup(t *testing.T) {
	if l, err := CBLookupLevel(CBFamilyKV, ""); err != nil || l.Name != "R1000" {
		t.Fatalf("default kv level = %+v, %v", l, err)
	}
	if _, err := CBLookupLevel(CBFamilyKV, "L160"); err == nil {
		t.Fatal("a log level must not resolve under kv_store")
	}
	if _, err := CBLookupLevel("nope", ""); err == nil {
		t.Fatal("unknown family must error")
	}
	if _, err := GenerateCB(CBFamilyKV, "", -1); err == nil {
		t.Fatal("a negative seed must be refused")
	}
	for _, family := range CBFamilies {
		for _, name := range CBLevelNames(family) {
			if _, err := CBLookupLevel(family, name); err != nil {
				t.Errorf("%s/%s: %v", family, name, err)
			}
		}
	}
}

// The gold must agree with the stream it was generated with.
func TestContextBenchGoldAgreesWithTheStream(t *testing.T) {
	kv := GenerateKV(42, CBKnobs{Records: 30, ValueWords: 6, Queries: 5, RecordsPerChunk: 10})
	stream := joinBodies(kv.Ops)
	for _, q := range kv.Gold.KV {
		if !strings.Contains(stream, "SET "+q.Key+" = "+q.Value+"\n") {
			t.Errorf("kv gold %s: value is not in the SET stream", q.Key)
		}
		if !strings.Contains(stream, "GET "+q.Key+"\n") {
			t.Errorf("kv gold %s: no GET op", q.Key)
		}
	}

	lg := GenerateLog(42, CBKnobs{Lines: 120, LinesPerBatch: 20, Queries: 8})
	stream = joinBodies(lg.Ops)
	var lookups, counts int
	for _, q := range lg.Gold.Log {
		switch q.Type {
		case "lookup":
			lookups++
			if !strings.Contains(stream, q.Answer) {
				t.Errorf("log lookup %d: answer not in the stream", q.QID)
			}
		case "count":
			counts++
			level, svc := between(q.Prompt, "[", "]"), between(q.Prompt, "service \"", "\"")
			n := strings.Count(stream, "["+level+"] "+svc+" req=")
			if q.Answer != strconv.Itoa(n) {
				t.Errorf("log count %d: gold %s, stream has %d", q.QID, q.Answer, n)
			}
		}
	}
	if lookups != 4 || counts != 4 {
		t.Errorf("want 4 lookups and 4 counts, got %d and %d", lookups, counts)
	}

	nd := GenerateNeedle(42, CBKnobs{NeedlesPerChunk: 3, Chunks: 4, FillerLines: 10, WordsPerLine: 6})
	if len(nd.Gold.NeedleLines) != 12 {
		t.Fatalf("needle gold has %d lines, want 12", len(nd.Gold.NeedleLines))
	}
	stream = joinBodies(nd.Ops)
	for _, l := range nd.Gold.NeedleLines {
		if !strings.Contains(stream, l+"\n") {
			t.Errorf("needle %q is not in the stream", l)
		}
	}

	sd := GenerateSudoku(42, CBKnobs{Box: 3, Givens: 30, Boards: 1})
	if sd.Gold.Side != 9 || sd.Gold.InitialGrid == "" {
		t.Fatalf("sudoku gold side/initial: %d %q", sd.Gold.Side, sd.Gold.InitialGrid)
	}
	if len(sd.Ops) != 81-30 {
		t.Fatalf("a 9x9 board with 30 givens streams %d moves, got %d", 81-30, len(sd.Ops))
	}
	if got := len(sd.Gold.Deltas); got != len(sd.Ops)+1 {
		t.Fatalf("sudoku gold has %d deltas for %d ops", got, len(sd.Ops))
	}
	for _, d := range sd.Gold.Deltas[1:] {
		if len(d.Cells) != 1 {
			t.Errorf("version %d changes %d cells, want exactly one", d.V, len(d.Cells))
		}
	}
}

func joinBodies(ops []CBOp) string {
	var b strings.Builder
	for _, op := range ops {
		b.WriteString(op.Body)
		b.WriteString("\n")
	}
	return b.String()
}

func between(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
