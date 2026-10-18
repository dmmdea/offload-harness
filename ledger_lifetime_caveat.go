package main

import "time"

// ledgerOutputPricedFrom is the first moment a ledger window can start at without reaching
// rows written before generated tokens were priced. 0.117.7 priced them (register D-96) and
// was released on 2026-09-14, and the nodes carried it by that evening; the bound is one UTC
// day later. CHOSEN, not measured: a ledger row does not record the build that wrote it, so
// the window's start is the only thing there is to compare.
var ledgerOutputPricedFrom = time.Date(2026, time.September, 16, 0, 0, 0, 0, time.UTC)

// ledgerLifetimeCaveat is the line printed under the dollar figure of `ledger`, "" when the
// window starts after the pricing fix. Before 0.117.7 est_value_kept_local was tokens_saved x
// the input rate only: the tokens a local seat generated were priced at zero, which is the
// expensive half on Opus (the output rate is five times the input rate). A total that sums
// rows from before then undercounts, and the history is not restated, so the line is where a
// reader of the figure is told. sinceTS is the window's start in Unix seconds, 0 for the
// lifetime read.
func ledgerLifetimeCaveat(sinceTS int64) string {
	if sinceTS > 0 && sinceTS >= ledgerOutputPricedFrom.Unix() {
		return ""
	}
	return "note: rows written before 0.117.7 (2026-09-14) priced generated tokens at zero, so a total that reaches back before then undercounts; those rows are not restated"
}
