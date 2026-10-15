package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/dmmdea/offload-harness/internal/compeval"
	"github.com/dmmdea/offload-harness/internal/core"
)

// The outcome scorer is shared by `compaction-eval ab` and `gate selftest`, so what it scores is
// pinned here: the numbers below are the ones the A/B has always produced.

// fourEntities has exactly four entities (the numbers of two or more digits).
const fourEntities = "user: check the run\ntool: build 41 took 96 s on 12 hosts, queue 77\n"

func summaryRun(summary string, seen *core.Request) func(context.Context, core.Request) core.Result {
	return func(_ context.Context, req core.Request) core.Result {
		if seen != nil {
			*seen = req
		}
		data, _ := json.Marshal(map[string]string{"summary": summary})
		return core.Result{OK: true, Data: data}
	}
}

func TestCompactionOutcomeScorerSourceHasFourEntities(t *testing.T) {
	if n := len(compeval.Entities(fourEntities)); n != 4 {
		t.Fatalf("the fixture must carry four entities, it carries %d", n)
	}
}

func TestCompactionOutcomeScorerValues(t *testing.T) {
	cases := []struct {
		name     string
		run      func(context.Context, core.Request) core.Result
		rendered string
		want     float64
	}{
		{"a deferred summary scores zero", func(context.Context, core.Request) core.Result {
			return core.Result{OK: false, Deferred: true, Reason: "no seat"}
		}, fourEntities, 0},
		{"an entity-free source is accepted and proves nothing more", summaryRun("nothing numeric here", nil), "user: hello\ntool: ok\n", 0.2},
		{"no entity carried", summaryRun("it went fine", nil), fourEntities, 0.2},
		{"half the entities carried", summaryRun("build 41 took 96 s", nil), fourEntities, 0.6},
		{"every entity carried", summaryRun("build 41 took 96 s on 12 hosts, queue 77", nil), fourEntities, 1.0},
		{"an entity the source lacks pays nothing", summaryRun("build 41, and 55 more", nil), fourEntities, 0.4},
		{"a result that is not a summary object is read as text", func(context.Context, core.Request) core.Result {
			return core.Result{OK: true, Data: json.RawMessage(`"41 96 12 77"`)}
		}, fourEntities, 1.0},
	}
	for _, c := range cases {
		got, err := compactionOutcomeScorer(c.run)(context.Background(), c.rendered)
		if err != nil || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got %v (err %v), want %v", c.name, got, err, c.want)
		}
	}
}

// Recall is capped at eight entities, so a source with more than eight is fully recalled by eight.
func TestCompactionOutcomeScorerCapsTheDenominatorAtEight(t *testing.T) {
	src := "tool: 10 11 12 13 14 15 16 17 18 19 20 21\n"
	if n := len(compeval.Entities(src)); n != 12 {
		t.Fatalf("the fixture must carry twelve entities, it carries %d", n)
	}
	got, _ := compactionOutcomeScorer(summaryRun("10 11 12 13 14 15 16 17", nil))(context.Background(), src)
	if math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("eight of twelve entities must score the full 1.0, got %v", got)
	}
}

// The scorer asks for a summary of the rendered transcript, nothing else.
func TestCompactionOutcomeScorerAsksForASummaryOfTheTranscript(t *testing.T) {
	var seen core.Request
	if _, err := compactionOutcomeScorer(summaryRun("x", &seen))(context.Background(), fourEntities); err != nil {
		t.Fatal(err)
	}
	if seen.Task != core.TaskSummarize || seen.Input != fourEntities {
		t.Fatalf("the request was %+v, want a summarize of the rendered transcript", seen)
	}
}

// A scorer must be usable by the self-test: it never returns an error of its own (a defer is a score).
func TestCompactionOutcomeScorerNeverErrors(t *testing.T) {
	failing := func(context.Context, core.Request) core.Result {
		return core.Result{OK: false, Reason: errors.New("seat down").Error()}
	}
	if _, err := compactionOutcomeScorer(failing)(context.Background(), fourEntities); err != nil {
		t.Fatalf("a failed run is a zero score, not an error: %v", err)
	}
}
