package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Test, bake and measurement traffic on the ledger views (register F48 / job-error class C23).
//
// The ledger labels the rows of a run that was not real work (ledger.ClassOf), and the commands that report the
// ledger leave them out by default and say how many they left out, so a bake never lands in an error rate or a
// savings figure and nothing is hidden. --include-test-traffic counts them again.

const includeTrafficUsage = "count test, bake and measurement traffic too (default: left out, and the left-out jobs are counted per class)"

// includeTrafficFlag registers --include-test-traffic on fs.
func includeTrafficFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("include-test-traffic", false, includeTrafficUsage)
}

// productionEntries is the rows a ledger view reports on: all of them with include, else the production rows
// and the per-class count of the jobs left out (nil when none).
func productionEntries(entries []ledger.Entry, include bool) (kept []ledger.Entry, excluded map[string]int) {
	if include {
		return entries, nil
	}
	return ledger.ProductionRows(entries)
}

// describeExcluded renders a per-class count as "bake 40, test 3" (classes in name order).
func describeExcluded(excluded map[string]int) string {
	classes := make([]string, 0, len(excluded))
	for c := range excluded {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	parts := make([]string, 0, len(classes))
	for _, c := range classes {
		parts = append(parts, fmt.Sprintf("%s %d", c, excluded[c]))
	}
	return strings.Join(parts, ", ")
}

// noteExcluded says on stderr what a command left out, so a count printed on stdout (a JSON shape callers
// script against) keeps its shape and the omission is still stated.
func noteExcluded(excluded map[string]int) {
	if len(excluded) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "note: left out test, bake and measurement traffic (%s job(s)); --include-test-traffic counts it\n", describeExcluded(excluded))
}
