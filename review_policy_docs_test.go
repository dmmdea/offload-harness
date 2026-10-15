package main

// The review lane's answer to a defer (what the door waits for, what it retries and what it does not, and what a cold
// seat costs) is a decision its callers act on, so the page that states it is held to it. Two things drift: a defer shape
// that gets a new answer in code but not in the table, and a number the table quotes from a constant that has since moved.
// The table's rows are pinned by their first cell and the quoted numbers by the constants they come from.
//
// The last test refuses the sentence this page used to carry about the fenced fallthrough. It said the local leg waits
// and then defers; the in-run fence check made that verdict immediate two days after the sentence was written, and the
// page went on saying the old thing. A released CHANGELOG section is history and is not read here.
//
// The next two pin what ends a review. The door imposes no deadline: the wall is the run's expectation, and a run ends on
// a stall (filed `infrastructure`) or on the liveness ceiling (`budget`, or `capacity` when the run was held behind the
// seat's other work). A page that files a stall under `budget`, or sizes a client's patience as the admission budget plus
// the wall, sends a caller to the wrong advice and can cut a legitimate long review.

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

// reviewPolicyRow is one table row of the policy block, found by its first cell and read to the end of its line.
func reviewPolicyRow(t *testing.T, firstCell string) string {
	t.Helper()
	return docSpan(t, reviewPolicyPage, "| "+firstCell+" |", "")
}

func TestTheReviewLaneDocsFileAStallAsInfrastructureAndNameWhatEndsARun(t *testing.T) {
	// A stall is the seat going silent: the node files it `infrastructure` and its reason starts "stalled:". It is not a
	// ceiling, so the budget row does not list it and the row a caller reads for a silent seat does.
	ceiling := reviewPolicyRow(t, "A ceiling stopped the run")
	cell := strings.Split(ceiling, "|")
	if len(cell) < 3 || !strings.HasPrefix(strings.TrimSpace(cell[2]), "`budget`") {
		t.Fatalf("the ceiling row's class cell does not lead with `budget`:\n%s", ceiling)
	}
	if strings.Contains(strings.ToLower(ceiling), "stall") {
		t.Errorf("the ceiling row still lists a stall; the node files a stall `infrastructure`:\n%s", ceiling)
	}
	docNeeds(t, "the ceiling row", ceiling,
		"liveness ceiling", "step budget", "completion", "`wall`",
		// The same ceiling files `capacity` when the run was only held behind the seat's other work.
		"`capacity`", "held behind the seat's other work")

	docNeeds(t, "the row for a seat unfit to answer", reviewPolicyRow(t, "The seat is unfit to answer"),
		"`infrastructure`", "`stalled:`")

	// What ends a run is stated once, with the ceiling's floor and cap taken from the constants.
	docNeeds(t, "the review lane's defer policy", reviewPolicyText(t),
		"is not a deadline",
		fmt.Sprintf("2 x the wall, %d s", core.AgentCeilingSecFloor),
		fmt.Sprintf("never above %d s", core.AgentCeilingSecCap),
	)
}

func TestTheReviewLaneDocsDoNotBoundTheCallAtTheWall(t *testing.T) {
	// The patience bullet used to read as the admission budget plus a wall capped at 900 s, about 1200 s. A run still
	// producing is not cut at the wall: it runs to the liveness ceiling, which is at least 1800 s, and time in the seat's
	// run line comes on top. The wall is also not `agent_timeout_sec` alone, and the 30 to 90 s was another lane's figure.
	bullet := docSpan(t, reviewPolicyPage, "- Size the client's patience", "\n- The review is still owed")
	bullet = strings.Join(strings.Fields(bullet), " ")
	docNeeds(t, "the patience bullet", bullet,
		"no fixed upper bound at the wall",
		"run line",
		fmt.Sprintf("at least %d s", core.AgentCeilingSecFloor),
	)
	for _, retired := range []string{
		"the wall itself (`agent_timeout_sec`, capped at",
		"30 to 90 s a review measures",
	} {
		if strings.Contains(bullet, retired) {
			t.Errorf("the patience bullet still says %q:\n%s", retired, bullet)
		}
	}
}

func TestTheReviewLaneDocsDoNotSayTheFenceFallthroughWaits(t *testing.T) {
	raw, err := docText(".", reviewPolicyPage)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(raw), " ")
	for _, retired := range []string{
		"the wait, then the capacity defer",
		// The salvage paragraph carried the same claim in other words: a deferred fleet review was salvaged "instead of
		// falling through to a local wait behind the very lease that fenced it". With the in-run check there is no such wait.
		"a local wait behind the very lease",
	} {
		if strings.Contains(text, retired) {
			t.Errorf("%s still says %q: with the fleet declining, the local leg re-reads the lease and defers `capacity` at once while the fence holds; "+
				"only a fence taken after that check meets the cordon wait", reviewPolicyPage, retired)
		}
	}
}
