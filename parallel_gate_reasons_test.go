package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// scripts/parallel-sessions-gate.ps1 is the live proof behind ADR 0032: it counts the
// defer reasons the harness files and prints one verdict. ADR 0066 added two — a seat
// that is not serving (`seat not serving:`, a 5xx) and a seat that went down under the
// run (`seat down:`) — and moved `seat contended:` to a 429 only. A gate that counts
// only the old prefix files those defers as "other" and, because its verdict never
// read that counter, can print PASS over a run whose seat could not serve. Nothing
// else reads the script, so this pins its wiring to the constants the producers use.
func TestParallelSessionsGateKnowsEveryReasonASeatCanFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("scripts", "parallel-sessions-gate.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)

	for _, prefix := range []string{core.SeatDownReason, core.SeatNotServingReason} {
		lit := `"` + strings.TrimSpace(prefix) + `*"`
		if !strings.Contains(script, lit) {
			t.Errorf("the gate never counts %s: a defer with that reason would be filed as an unexpected one", lit)
		}
	}
	if !strings.Contains(script, `"seat contended:*"`) {
		t.Error(`the gate lost its "seat contended:*" counter`)
	}
	// Both prefixes feed ONE counter, the one the verdict reads.
	feed := `(?m)elseif .*` + regexp.QuoteMeta(`"`+strings.TrimSpace(core.SeatNotServingReason)+`*"`) + `.*` +
		regexp.QuoteMeta(`"`+strings.TrimSpace(core.SeatDownReason)+`*"`) + `.*\{ \$seatUnhealthy\+\+ \}`
	if !regexp.MustCompile(feed).MatchString(script) {
		t.Error("the branch that matches the seat-not-serving and seat-down prefixes does not increment $seatUnhealthy")
	}

	// The verdict must READ the counters: a counter nothing consults is decoration.
	for _, counter := range []string{"seatUnhealthy", "otherDefers"} {
		if !regexp.MustCompile(`(?m)^\s*elseif \(\$` + counter + ` -gt 0\)`).MatchString(script) {
			t.Errorf("the verdict never reads $%s: a run with such defers can print PASS", counter)
		}
	}
	// ...and the PASS branch is last, after every refusal.
	pass := strings.Index(script, `"PASS (`)
	for _, guard := range []string{`$seatUnhealthy -gt 0`, `$otherDefers -gt 0`, `$contended -gt 0`, `$failedVerification -gt 0`} {
		at := strings.Index(script, guard)
		if at < 0 || pass < 0 || at > pass {
			t.Errorf("the guard %s does not come before the PASS verdict", guard)
		}
	}
}
