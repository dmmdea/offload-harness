package occontext

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Analyze builds the report for the selected sessions.
func Analyze(s *Store, o Options) *Report {
	r := &Report{Params: paramsOf(o)}
	r.Source.UnparsedRows = s.Unparsed
	a := &accum{groups: map[groupKey]*groupAcc{}}
	for _, sess := range selectSessions(s, o) {
		r.Sessions = append(r.Sessions, analyzeSession(sess, s.Messages[sess.ID], o, a))
	}
	for _, g := range a.groups {
		g.report.Cache.finish()
		g.report.Growth.finish()
		g.report.TTFTWarmMedianSec = median(g.warm)
		r.Groups = append(r.Groups, g.report)
	}
	sort.Slice(r.Groups, func(i, j int) bool {
		gi, gj := r.Groups[i], r.Groups[j]
		if gi.Kind != gj.Kind {
			return gi.Kind == "primary"
		}
		if gi.Agent != gj.Agent {
			return gi.Agent < gj.Agent
		}
		return gi.Model < gj.Model
	})
	r.Cache = a.cache
	r.Cache.finish()
	r.Hints = cacheHints(r.Sessions)
	return r
}

func paramsOf(o Options) Params {
	p := Params{Last: o.Last, Sessions: o.Sessions, Model: o.Model, CacheValidSince: o.CacheValidSince,
		CacheBlock: o.CacheBlock, ToolBytesPerToken: o.ToolBytesPerToken, TextBytesPerToken: o.TextBytesPerToken}
	if !o.Since.IsZero() {
		p.Since = o.Since.Format(time.RFC3339)
	}
	return p
}

type groupKey struct{ kind, agent, model string }

type groupAcc struct {
	report   GroupReport
	sessions map[string]bool
	warm     []float64
}

type accum struct {
	groups map[groupKey]*groupAcc
	cache  CacheStats
}

func (a *accum) group(kind, agent, model string) *groupAcc {
	k := groupKey{kind, agent, model}
	g := a.groups[k]
	if g == nil {
		g = &groupAcc{report: GroupReport{Kind: kind, Agent: agent, Model: model}, sessions: map[string]bool{}}
		a.groups[k] = g
	}
	return g
}

// selectSessions returns the roots the options pick (oldest first), each followed by its
// descendants (oldest first).
func selectSessions(s *Store, o Options) []Session {
	children := map[string][]Session{}
	known := map[string]bool{}
	for _, ss := range s.Sessions {
		known[ss.ID] = true
	}
	var roots []Session
	want := map[string]bool{}
	for _, id := range o.Sessions {
		want[id] = true
	}
	for _, ss := range s.Sessions {
		isChild := ss.ParentID != "" && known[ss.ParentID]
		if isChild {
			children[ss.ParentID] = append(children[ss.ParentID], ss)
		}
		switch {
		case len(want) > 0:
			if !want[ss.ID] {
				continue
			}
		case isChild:
			continue
		}
		if !o.Since.IsZero() && ss.Created.Before(o.Since) {
			continue
		}
		roots = append(roots, ss)
	}
	if o.Last > 0 && len(roots) > o.Last {
		roots = roots[len(roots)-o.Last:]
	}
	var out []Session
	seen := map[string]bool{}
	var walk func(ss Session)
	walk = func(ss Session) {
		if seen[ss.ID] {
			return
		}
		seen[ss.ID] = true
		out = append(out, ss)
		for _, c := range children[ss.ID] {
			walk(c)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return out
}

func modelName(m *Message) string {
	if m.Provider == "" {
		return m.Model
	}
	return m.Provider + "/" + m.Model
}

func cacheValid(m *Message, cuts []Cutoff) bool {
	for _, c := range cuts {
		if c.Model != "" && c.Model != m.Model && c.Model != modelName(m) {
			continue
		}
		if m.Created.Before(c.Since) {
			return false
		}
	}
	return true
}

// estimate converts bytes to tokens at the given ratio.
func estimate(bytes int, perToken float64) int {
	if bytes <= 0 || perToken <= 0 {
		return 0
	}
	return int(math.Round(float64(bytes) / perToken))
}

// pending collects what enters the context between one call and the next.
type pending struct {
	reasoning    int
	reasoningEst bool
	output       int
	toolBytes    int
	userBytes    int
	stubBytes    int // text and reasoning of zero-token assistant messages in between
}

// pendingFrom starts the collection with the call's own replayed content. When the
// server reports reasoning 0 but reasoning text was stored (llama.cpp counts reasoning
// inside output), the reasoning is estimated from the text and taken out of output.
func pendingFrom(m *Message, o Options) pending {
	p := pending{reasoning: m.Tokens.Reasoning, output: m.Tokens.Output}
	var reasoningBytes int
	for _, part := range m.Parts {
		switch part.Type {
		case "reasoning":
			reasoningBytes += len(part.Text)
		case "tool":
			p.toolBytes += len(part.ToolOut)
		}
	}
	if p.reasoning == 0 && reasoningBytes > 0 {
		est := estimate(reasoningBytes, o.TextBytesPerToken)
		if est > p.output {
			est = p.output
		}
		p.reasoning, p.output, p.reasoningEst = est, p.output-est, true
	}
	return p
}

func ttftOf(m *Message) *float64 {
	var first time.Time
	for _, p := range m.Parts {
		if (p.Type == "reasoning" || p.Type == "text") && !p.Start.IsZero() && (first.IsZero() || p.Start.Before(first)) {
			first = p.Start
		}
	}
	if first.IsZero() {
		for _, p := range m.Parts {
			if p.Type == "step-start" && !p.Created.IsZero() {
				first = p.Created
				break
			}
		}
	}
	if first.IsZero() || m.Created.IsZero() || first.Before(m.Created) {
		return nil
	}
	v := first.Sub(m.Created).Seconds()
	return &v
}

func analyzeSession(sess Session, msgs []Message, o Options, a *accum) SessionReport {
	kind := "primary"
	if sess.ParentID != "" {
		kind = "child"
	}
	sr := SessionReport{ID: sess.ID, ParentID: sess.ParentID, Kind: kind, Created: sess.Created, Models: []string{}}
	if o.Titles {
		sr.Title = sess.Title
	}
	var (
		prev          *Message // the last non-compaction call the next growth pair starts from
		pend          pending
		reset         bool // a compaction part was seen since the last call
		auto, over    bool // its flags
		awaitingAfter = -1 // index of a compaction still waiting for its after-prompt
		firstByAgent  = map[string]bool{}
		sessionCalls  int
	)
	for i := range msgs {
		m := &msgs[i]
		switch m.Role {
		case "user":
			for _, p := range m.Parts {
				switch p.Type {
				case "compaction":
					reset, auto, over = true, p.Auto, p.Overflow
				case "text":
					pend.userBytes += len(p.Text)
				}
			}
			continue
		case "assistant":
		default:
			continue
		}
		if m.Tokens.Prompt() == 0 {
			// A zero-token assistant message (the subtask launcher, an abort, an error)
			// is not a call, but what it holds still enters the next prompt.
			sr.Stubs++
			for _, p := range m.Parts {
				switch p.Type {
				case "tool":
					pend.toolBytes += len(p.ToolOut)
				case "text", "reasoning":
					pend.stubBytes += len(p.Text)
				}
			}
			continue
		}
		name := modelName(m)
		if o.Model != "" && !strings.Contains(name, o.Model) {
			prev, pend = nil, pending{} // a filtered call breaks the chain
			continue
		}
		isCompaction := m.Agent == "compaction" || m.Summary
		sessionCalls++
		cr := CallReport{N: sessionCalls, MessageID: m.ID, Agent: m.Agent, Model: name, Created: m.Created,
			Prompt: m.Tokens.Prompt(), Cached: m.Tokens.CacheRead, CacheWrite: m.Tokens.CacheWrite,
			Output: m.Tokens.Output, Reasoning: m.Tokens.Reasoning, CacheValid: cacheValid(m, o.CacheValidSince),
			First: sessionCalls == 1, Compaction: isCompaction, TTFTSec: ttftOf(m)}
		if !m.Completed.IsZero() && !m.Completed.Before(m.Created) {
			w := m.Completed.Sub(m.Created).Seconds()
			cr.WallSec = &w
		}
		own := pendingFrom(m, o) // the call's own content, replayed into the next prompt
		if own.reasoningEst {
			v := own.reasoning
			cr.ReasoningEst = &v
		}
		if o.CacheBlock > 0 {
			b := cr.Cached / o.CacheBlock
			cr.CachedBlocks, cr.BlockRemainder = &b, cr.Cached%o.CacheBlock
		}
		for _, p := range m.Parts {
			if p.Type == "tool" && p.Tool != "" {
				cr.Tools = append(cr.Tools, p.Tool)
			}
		}
		if !contains(sr.Models, name) {
			sr.Models = append(sr.Models, name)
		}
		g := a.group(kind, m.Agent, name)
		g.sessions[sess.ID] = true
		g.report.Sessions = len(g.sessions)
		g.report.Calls++
		if !firstByAgent[m.Agent] {
			firstByAgent[m.Agent] = true
			g.report.FirstCallPrompts = append(g.report.FirstCallPrompts, cr.Prompt)
			if cr.TTFTSec != nil {
				g.report.TTFTFirstSec = append(g.report.TTFTFirstSec, *cr.TTFTSec)
			}
		} else if cr.TTFTSec != nil {
			g.warm = append(g.warm, *cr.TTFTSec)
		}

		if isCompaction {
			ev := Compaction{MessageID: m.ID, At: m.Created, Auto: auto, Overflow: over, Prompt: cr.Prompt,
				Output: cr.Output, Reasoning: own.reasoning, ReasoningEstimated: own.reasoningEst}
			if cr.WallSec != nil {
				ev.WallSec = *cr.WallSec
			}
			owner := g
			if prev != nil {
				ev.BeforePrompt = prev.Tokens.Prompt()
				ev.BeforeTotal = prev.Tokens.Total
				if ev.BeforeTotal == 0 {
					ev.BeforeTotal = ev.BeforePrompt + prev.Tokens.Output + prev.Tokens.Reasoning
				}
				owner = a.group(kind, prev.Agent, modelName(prev))
			}
			owner.report.Compactions++
			sr.Compactions = append(sr.Compactions, ev)
			awaitingAfter = len(sr.Compactions) - 1
			prev = nil
		} else {
			if awaitingAfter >= 0 {
				v := cr.Prompt
				sr.Compactions[awaitingAfter].AfterPrompt = &v
				awaitingAfter = -1
			}
			if prev != nil && !reset {
				if modelName(prev) != name {
					sr.Growth.SkippedModelSwitch++
					g.report.Growth.SkippedModelSwitch++
				} else {
					gr := Growth{Delta: cr.Prompt - prev.Tokens.Prompt(), Reasoning: pend.reasoning,
						ReasoningEstimated: pend.reasoningEst,
						Output:             pend.output + estimate(pend.stubBytes, o.TextBytesPerToken),
						ToolOutput:         estimate(pend.toolBytes, o.ToolBytesPerToken),
						UserText:           estimate(pend.userBytes, o.TextBytesPerToken)}
					gr.Residual = gr.Delta - gr.Reasoning - gr.Output - gr.ToolOutput - gr.UserText
					cr.Growth = &gr
					sr.Growth.add(gr)
					g.report.Growth.add(gr)
				}
			}
			prev = m
		}
		sr.Calls = append(sr.Calls, cr)
		sr.Cache.add(cr, o.CacheBlock)
		g.report.Cache.add(cr, o.CacheBlock)
		a.cache.add(cr, o.CacheBlock)
		pend = own
		reset, auto, over = false, false, false
	}
	sr.Cache.finish()
	sr.Growth.finish()
	return sr
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func ratio(num, den int) *float64 {
	if den <= 0 {
		return nil
	}
	v := 100 * float64(num) / float64(den)
	return &v
}

func (c *CacheStats) add(cr CallReport, block int) {
	if !cr.CacheValid {
		c.ExcludedCalls++
		return
	}
	c.ValidCalls++
	c.Prompt += cr.Prompt
	c.Cached += cr.Cached
	if !cr.First {
		c.ExclFirstPrompt += cr.Prompt
		c.ExclFirstCached += cr.Cached
	}
	if !(cr.First && cr.Cached == 0) {
		c.ExclColdFirstPrompt += cr.Prompt
		c.ExclColdFirstCached += cr.Cached
	}
	if block > 0 && cr.Cached%block != 0 {
		c.BlockMisaligned++
	}
}

func (c *CacheStats) finish() {
	c.Pct = ratio(c.Cached, c.Prompt)
	c.ExclFirstPct = ratio(c.ExclFirstCached, c.ExclFirstPrompt)
	c.ExclColdFirstPct = ratio(c.ExclColdFirstCached, c.ExclColdFirstPrompt)
}

func (g *GrowthTotals) add(x Growth) {
	g.Pairs++
	if x.Delta < 0 {
		g.NegativePairs++
	}
	g.Delta += x.Delta
	g.Reasoning += x.Reasoning
	g.Output += x.Output
	g.ToolOutput += x.ToolOutput
	g.UserText += x.UserText
	g.Residual += x.Residual
}

func (g *GrowthTotals) finish() {
	if g.Delta <= 0 {
		return
	}
	g.ReasoningShare = ratio(g.Reasoning, g.Delta)
	g.OutputShare = ratio(g.Output, g.Delta)
	g.ToolOutputShare = ratio(g.ToolOutput, g.Delta)
	g.UserTextShare = ratio(g.UserText, g.Delta)
	g.ResidualShare = ratio(g.Residual, g.Delta)
}

func median(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	var v float64
	if n := len(s); n%2 == 1 {
		v = s[n/2]
	} else {
		v = (s[n/2-1] + s[n/2]) / 2
	}
	return &v
}

// cacheHints flags models whose warm calls logged 0 cached tokens before the model's
// first cache hit (or never hit at all): the signature of a server that was not
// reporting cached tokens yet. Only calls whose cache figures count are considered, so
// a cutoff that already covers them silences the hint.
func cacheHints(sessions []SessionReport) []string {
	type obs struct {
		firstHit     time.Time
		zeroWarm     int
		lastZeroWarm time.Time
		warm         int
	}
	byModel := map[string]*obs{}
	var calls []CallReport
	for _, s := range sessions {
		calls = append(calls, s.Calls...)
	}
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].Created.Before(calls[j].Created) })
	var order []string
	for _, c := range calls {
		if !c.CacheValid || c.Compaction {
			continue
		}
		ob := byModel[c.Model]
		if ob == nil {
			ob = &obs{}
			byModel[c.Model] = ob
			order = append(order, c.Model)
		}
		if c.Cached > 0 && ob.firstHit.IsZero() {
			ob.firstHit = c.Created
		}
		if c.First {
			continue
		}
		ob.warm++
		if c.Cached == 0 && ob.firstHit.IsZero() {
			ob.zeroWarm++
			ob.lastZeroWarm = c.Created
		}
	}
	var hints []string
	for _, model := range order {
		ob := byModel[model]
		switch {
		case ob.zeroWarm > 0 && !ob.firstHit.IsZero():
			hints = append(hints, fmt.Sprintf("%s: %d warm call(s) reported 0 cached tokens, the last at %s, before the first cache hit at %s. "+
				"If the server was not reporting cached tokens yet (vLLM needs --enable-prompt-tokens-details), pass "+
				"--cache-valid-since %s=<when it started>, somewhere in that interval.",
				model, ob.zeroWarm, ob.lastZeroWarm.Format(time.RFC3339), ob.firstHit.Format(time.RFC3339), model))
		case ob.firstHit.IsZero() && ob.warm >= 2:
			hints = append(hints, fmt.Sprintf("%s: all %d warm calls reported 0 cached tokens. The server may not report them "+
				"(vLLM needs --enable-prompt-tokens-details); its cache figures here are not measurements.", model, ob.warm))
		}
	}
	return hints
}
