package main

// The review lane's answer to a defer (what the door waits for, what it retries and what it does not, and what a cold
// seat costs) is a decision its callers act on, so the page that states it is held to it. Two things drift: a defer shape
// that gets a new answer in code but not in the table, and a number the table quotes from a constant that has since moved.
// The table's rows are pinned by their first cell and the quoted numbers by the constants they come from.
//
// The second test refuses the sentence this page used to carry about the fenced fallthrough. It said the local leg waits
// and then defers; the in-run fence check made that verdict immediate two days after the sentence was written, and the
// page went on saying the old thing. A released CHANGELOG section is history and is not read here.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/seatwait"
)

const (
	reviewPolicyPage = "docs/systems/mcp-server.md"
	reviewPolicyLead = "**What a defer means here: retry, backoff and the warm seat.**"
)

// reviewPolicyText is the policy block of the page, on one line per paragraph break: the block runs from its lead to the next
// top-level heading, and whitespace is folded so a phrase that wraps across lines is still found.
func reviewPolicyText(t *testing.T) string {
	t.Helper()
	span := docSpan(t, reviewPolicyPage, reviewPolicyLead, "\n## Important flows")
	return strings.Join(strings.Fields(span), " ")
}

func TestTheReviewLaneDocsStateWhatEachDeferShapeGets(t *testing.T) {
	text := reviewPolicyText(t)

	docNeeds(t, "the review lane's defer policy", text,
		// The stance, in the two words a reader acts on.
		"no retry loop and no backoff of its own",
		"never on the same seat",
		// One row per defer shape, found by its first cell.
		"| A foreign lease fences the seat |",
		"| A fence taken after the door looked |",
		"| The seat's run line is full |",
		"| The seat is unfit to answer |",
		"| The stack failed or the seat is misconfigured |",
		"| The seat answered and nothing usable came of it |",
		"| A ceiling stopped the run |",
		"| The call cannot be run as asked |",
		// The classes the rows are keyed by, as the wire spells them.
		"`capacity`", "`infrastructure`", "`abstention`", "`budget`", "`config`", "`contract`",
		// The two cases that are not defers.
		"A cold seat is not a defer",
		"A plain text reservation is not a fence",
		// What the caller does.
		"What the caller does",
	)

	// The bounds the page quotes are the constants' own, so a changed constant fails here and not in a reader's expectation.
	secs := func(d time.Duration) int { return int(d / time.Second) }
	docNeeds(t, "the review lane's defer policy", text,
		fmt.Sprintf("`agent_admission_wait_sec`, %d s by default", core.AgentAdmissionSecDefault),
		fmt.Sprintf("`seat_contention_wait_sec`, %d s by default", secs(seatwait.DefaultBudget)),
		fmt.Sprintf("`agent_placement_wait_sec`, %d s by default", secs(config.DefaultPlacementWait)),
		fmt.Sprintf("capped at %d s", core.AgentTimeoutSecCap),
	)
}

func TestTheReviewLaneDocsDoNotSayTheFenceFallthroughWaits(t *testing.T) {
	raw, err := docText(".", reviewPolicyPage)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(raw), " ")
	for _, retired := range []string{
		"the wait, then the capacity defer",
	} {
		if strings.Contains(text, retired) {
			t.Errorf("%s still says %q: with the fleet declining, the local leg re-reads the lease and defers `capacity` at once while the fence holds; "+
				"only a fence taken after that check meets the cordon wait", reviewPolicyPage, retired)
		}
	}
}
