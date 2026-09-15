package ledger

import (
	"math"
	"path/filepath"
	"testing"
)

// The savings ledger prices BOTH halves of what a seat produced locally.
//
// The defect (found 2026-09-14): EstValueKeptLocal was
// `TokensSaved / 1M * opusPricePerMTok` and nothing else, so every output token
// a seat generated was multiplied by zero. Output is the EXPENSIVE half on
// Opus ($75/MTok against $15/MTok input), so the harness under-reported its own
// value by exactly the part of the work that costs the most to buy — a summary
// of 10,000 completions at 500 output tokens each reported $0.00 for 5M output
// tokens worth $375.
//
// The rows themselves are unchanged (no ledger-schema change): tokens_in and
// tokens_out were always recorded. Only the arithmetic over them is fixed.
func TestEstValueKeptLocalPricesInputAndOutput(t *testing.T) {
	cases := []struct {
		name    string
		rows    []Entry
		prices  Prices
		wantIn  int // TokensSaved
		wantOut int // TokensOut
		wantUSD float64
	}{
		{
			// The load-bearing case: output tokens exist and must be priced.
			// 1M in @ $15 = $15.00; 200k out @ $75 = $15.00; total $30.00.
			// Under the defect this read $15.00.
			name:    "input and output both priced",
			rows:    []Entry{{Task: "summarize", TokensIn: 1_000_000, TokensOut: 200_000}},
			prices:  Prices{InputPerMTok: 15, OutputPerMTok: 75},
			wantIn:  1_000_000,
			wantOut: 200_000,
			wantUSD: 30.0,
		},
		{
			// Output ALONE has value: a row that cost nothing to send and
			// generated 400k tokens is worth $30.00, not $0.00.
			name:    "output alone is not free",
			rows:    []Entry{{Task: "summarize", TokensIn: 0, TokensOut: 400_000}},
			prices:  Prices{InputPerMTok: 15, OutputPerMTok: 75},
			wantIn:  0,
			wantOut: 400_000,
			wantUSD: 30.0,
		},
		{
			// A DEFERRED row produced nothing Opus did not have to do itself:
			// neither half is priced. Pinned so the fix cannot be implemented
			// as "price every tokens_out on the file".
			name:    "a deferred row is priced at zero",
			rows:    []Entry{{Task: "summarize", TokensIn: 900_000, TokensOut: 900_000, Deferred: true}},
			prices:  Prices{InputPerMTok: 15, OutputPerMTok: 75},
			wantIn:  0,
			wantOut: 0,
			wantUSD: 0,
		},
		{
			// Mixed file: one completion, one cache hit (input saved, and the
			// cached answer never re-generated), one deferral.
			// in = 100k + 50k = 150k @ $15 = $2.25; out = 20k @ $75 = $1.50.
			name: "mixed file",
			rows: []Entry{
				{Task: "summarize", TokensIn: 100_000, TokensOut: 20_000},
				{Task: "classify", TokensIn: 50_000, TokensOut: 7_000, CacheHit: true},
				{Task: "extract", TokensIn: 70_000, TokensOut: 9_000, Deferred: true},
			},
			prices:  Prices{InputPerMTok: 15, OutputPerMTok: 75},
			wantIn:  150_000,
			wantOut: 20_000,
			wantUSD: 3.75,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "ledger.jsonl")
			l, err := Open(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range c.rows {
				if err := l.Record(e); err != nil {
					t.Fatal(err)
				}
			}
			l.Close()
			s, err := SummarizeFile(p, 0, c.prices)
			if err != nil {
				t.Fatal(err)
			}
			if s.TokensSaved != c.wantIn || s.TokensOut != c.wantOut {
				t.Fatalf("tokens: saved=%d out=%d, want %d/%d", s.TokensSaved, s.TokensOut, c.wantIn, c.wantOut)
			}
			if math.Abs(s.EstValueKeptLocal-c.wantUSD) > 1e-9 {
				t.Fatalf("EstValueKeptLocal = %v, want %v (input %d @ $%v/MTok + output %d @ $%v/MTok)",
					s.EstValueKeptLocal, c.wantUSD, c.wantIn, c.prices.InputPerMTok, c.wantOut, c.prices.OutputPerMTok)
			}
			if s.EstDollarSaved != s.EstValueKeptLocal {
				t.Fatalf("deprecated alias drifted: %v != %v", s.EstDollarSaved, s.EstValueKeptLocal)
			}
		})
	}
}

// TestOutputPriceOfZeroIsNotTheDefault: an operator who configures no output
// price gets the SHIPPED Opus rate, not a silent zero — the defect's exact
// shape must not be reachable through an empty config.
func TestOutputPriceOfZeroIsNotTheDefault(t *testing.T) {
	if DefaultPrices.OutputPerMTok <= DefaultPrices.InputPerMTok {
		t.Fatalf("DefaultPrices = %+v: output must bill above input on Opus", DefaultPrices)
	}
}
