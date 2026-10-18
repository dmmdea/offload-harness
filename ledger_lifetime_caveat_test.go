package main

import (
	"strings"
	"testing"
)

// Register D-96: before 0.117.7 the ledger priced the tokens a local seat GENERATED at zero
// (est_value_kept_local was tokens_saved x the input rate only), so every dollar figure
// summed over rows older than that is an undercount. The history is not restated, by
// design, which makes the figure's own line the only place a reader can be told.
//
// The line is printed when the window can reach back before the fix, which is every
// lifetime read (no --since) and any --since long enough to cross it.
const ledgerCaveatWords = "before 0.117.7"

func TestLedgerSaysLifetimeTotalsUndercountRowsBeforeOutputWasPriced(t *testing.T) {
	cfgPath := sessionLedger(t)

	lifetime := runLedgerOut(t, "--config", cfgPath)
	if !strings.Contains(lifetime, ledgerCaveatWords) {
		t.Fatalf("a lifetime ledger read does not carry the pricing caveat:\n%s", lifetime)
	}
	for _, want := range []string{"priced generated tokens at zero", "undercount", "not restated"} {
		if !strings.Contains(lifetime, want) {
			t.Errorf("the caveat does not say %q:\n%s", want, lifetime)
		}
	}
	// It follows the figure it qualifies, on the next line, before anything else is printed.
	lines := strings.Split(strings.TrimRight(lifetime, "\n"), "\n")
	figure := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "tokens kept local (est.)") {
			figure = i
		}
	}
	if figure < 0 || figure+1 >= len(lines) || !strings.Contains(lines[figure+1], ledgerCaveatWords) {
		t.Fatalf("the caveat is not the line after the figure (figure at %d):\n%s", figure, lifetime)
	}

	// A window that reaches back far enough crosses the fix: the same caveat.
	if long := runLedgerOut(t, "--config", cfgPath, "--since", "3650"); !strings.Contains(long, ledgerCaveatWords) {
		t.Fatalf("a --since 3650 read does not carry the caveat:\n%s", long)
	}
	// A window that starts after the fix has nothing to caveat: no line, so the caveat does not
	// become noise on the daily read.
	if recent := runLedgerOut(t, "--config", cfgPath, "--since", "1"); strings.Contains(recent, ledgerCaveatWords) {
		t.Fatalf("a --since 1 read carries the caveat though it starts after the fix:\n%s", recent)
	}
}
