package occontext

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

func comma(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func signed(n int) string {
	if n > 0 {
		return "+" + comma(n)
	}
	return comma(n)
}

func pctText(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", *p)
}

func secText(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *p)
}

func localTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func cacheLine(c CacheStats) string {
	s := fmt.Sprintf("cache %s all, %s excl first, %s excl cold first (valid calls %d", pctText(c.Pct),
		pctText(c.ExclFirstPct), pctText(c.ExclColdFirstPct), c.ValidCalls)
	if c.ExcludedCalls > 0 {
		s += fmt.Sprintf(", %d before the cutoff", c.ExcludedCalls)
	}
	if c.BlockMisaligned > 0 {
		s += fmt.Sprintf(", %d NOT a whole number of cache blocks", c.BlockMisaligned)
	}
	return s + ")"
}

func growthLine(g GrowthTotals) string {
	if g.Pairs == 0 {
		return "growth: no consecutive-call pairs"
	}
	s := fmt.Sprintf("growth %s over %d pair(s): reasoning %s, output %s, tool output~ %s, user text~ %s, residual %s",
		signed(g.Delta), g.Pairs, pctText(g.ReasoningShare), pctText(g.OutputShare), pctText(g.ToolOutputShare),
		pctText(g.UserTextShare), pctText(g.ResidualShare))
	if g.NegativePairs > 0 {
		s += fmt.Sprintf("; %d pair(s) shrank", g.NegativePairs)
	}
	if g.SkippedModelSwitch > 0 {
		s += fmt.Sprintf("; %d model switch(es) skipped", g.SkippedModelSwitch)
	}
	return s
}

// WriteTable renders the report as text: one block per session with a row per call, then
// the rollup per session kind, agent and model, then the overall cache figures and hints.
// It prints ids, counts and times only — no session text, tool output or (unless the
// report was built with Titles) title.
func WriteTable(w io.Writer, r *Report) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	p("opencode context — read from a private copy (%s; %d attempt(s); quick_check %s)\n",
		strings.Join(r.Source.Copied, ", "), r.Source.Attempts, map[bool]string{true: "ok", false: "not run"}[r.Source.QuickCheckOK])
	var cuts []string
	for _, c := range r.Params.CacheValidSince {
		scope := "all models"
		if c.Model != "" {
			scope = c.Model
		}
		cuts = append(cuts, fmt.Sprintf("%s since %s", scope, c.Since.Format(time.RFC3339)))
	}
	cutText := "cache figures: every call counts (no --cache-valid-since)"
	if len(cuts) > 0 {
		cutText = "cache figures valid: " + strings.Join(cuts, "; ")
	}
	blockText := "no block check"
	if r.Params.CacheBlock > 0 {
		blockText = "block " + comma(r.Params.CacheBlock)
	}
	p("%s · %s · estimates: tool output %.2g B/tok, text %.2g B/tok\n", cutText, blockText,
		r.Params.ToolBytesPerToken, r.Params.TextBytesPerToken)
	if r.Source.UnparsedRows > 0 {
		p("WARNING: %d message/part rows did not parse and are not counted\n", r.Source.UnparsedRows)
	}

	for _, s := range r.Sessions {
		p("\n-- %s  %s", s.ID, s.Kind)
		if s.ParentID != "" {
			p(" of %s", s.ParentID)
		}
		p("  %s", localTime(s.Created))
		if len(s.Models) > 0 {
			p("  %s", strings.Join(s.Models, ", "))
		}
		if s.Title != "" {
			p("  %q", s.Title)
		}
		p("\n")
		if len(s.Calls) == 0 {
			p("   no recorded calls (%d stub(s))\n", s.Stubs)
			continue
		}
		p("   %2s %-10s %8s %9s %4s %6s %6s %7s %7s | %8s %6s %6s %6s %6s %7s | %s\n",
			"#", "agent", "prompt", "cached", "blk", "out", "reas", "ttft_s", "wall_s",
			"growth", "=reas", "+out", "+tool~", "+user~", "+resid", "tools")
		for _, c := range s.Calls {
			cached := comma(c.Cached)
			if !c.CacheValid {
				cached += "*"
			}
			blk := ""
			if c.CachedBlocks != nil {
				blk = strconv.Itoa(*c.CachedBlocks)
				if c.BlockRemainder != 0 {
					blk += "!"
				}
			}
			grow := [6]string{}
			if g := c.Growth; g != nil {
				rs := comma(g.Reasoning)
				if g.ReasoningEstimated {
					rs = "~" + rs
				}
				grow = [6]string{signed(g.Delta), rs, comma(g.Output), comma(g.ToolOutput), comma(g.UserText), signed(g.Residual)}
			}
			reas := comma(c.Reasoning)
			if c.ReasoningEst != nil {
				reas = "~" + comma(*c.ReasoningEst)
			}
			agent := c.Agent
			if c.Compaction {
				agent = "COMPACT"
			}
			p("   %2d %-10.10s %8s %9s %4s %6s %6s %7s %7s | %8s %6s %6s %6s %6s %7s | %s\n",
				c.N, agent, comma(c.Prompt), cached, blk, comma(c.Output), reas, secText(c.TTFTSec), secText(c.WallSec),
				grow[0], grow[1], grow[2], grow[3], grow[4], grow[5], strings.Join(c.Tools, ","))
		}
		for i, k := range s.Compactions {
			flags := []string{}
			if k.Auto {
				flags = append(flags, "auto")
			}
			if k.Overflow {
				flags = append(flags, "overflow")
			}
			after := "-"
			if k.AfterPrompt != nil {
				after = comma(*k.AfterPrompt)
			}
			reas := comma(k.Reasoning)
			if k.ReasoningEstimated {
				reas = "~" + reas + " (inside out)"
			}
			p("   compaction %d at %s [%s]: before %s prompt (%s total) -> summary call prompt %s, out %s, reasoning %s, %.1f s -> after %s\n",
				i+1, localTime(k.At), strings.Join(flags, ","), comma(k.BeforePrompt), comma(k.BeforeTotal), comma(k.Prompt),
				comma(k.Output), reas, k.WallSec, after)
		}
		p("   %d call(s), %d stub(s) · %s · %s\n", len(s.Calls), s.Stubs, cacheLine(s.Cache), growthLine(s.Growth))
	}
	if len(r.Sessions) > 0 {
		p("\n   * cached before --cache-valid-since: a reporting artifact, left out of every cache ratio\n")
		p("   ~ estimated from bytes; growth = the previous call's replayed reasoning + output, tool output, user text, residual\n")
	}

	p("\n== by session kind / agent / model\n")
	for _, g := range r.Groups {
		p("%s/%s  %s  sessions %d  calls %d  compactions %d\n", g.Kind, g.Agent, g.Model, g.Sessions, g.Calls, g.Compactions)
		var firsts []string
		for _, v := range g.FirstCallPrompts {
			firsts = append(firsts, comma(v))
		}
		p("   first-call prompt: %s\n", strings.Join(firsts, " · "))
		p("   %s\n", cacheLine(g.Cache))
		p("   %s\n", growthLine(g.Growth))
		var ttfts []string
		for _, v := range g.TTFTFirstSec {
			ttfts = append(ttfts, fmt.Sprintf("%.1f", v))
		}
		p("   ttft: first call %s s; warm median %s s\n", orDash(strings.Join(ttfts, " · ")), secText(g.TTFTWarmMedianSec))
	}
	p("\n== overall %s\n", cacheLine(r.Cache))
	for _, h := range r.Hints {
		p("hint: %s\n", h)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
