// redactor_export.go — the harvest's redaction, exported for readers of recorded runs that are not
// transcript replays (the trace-corpus verb). It is a thin face on the same unexported redactor and
// the same vet class table the compaction harvest uses, so a second harvester cannot redact a
// different set of classes than the first one: a class added to the vet table is redacted here
// the day it is added.
package compeval

import "sort"

// Redactor substitutes stable placeholders for vet-class matches. The same matched text always
// yields the same placeholder (numbered by first appearance, per class), so two different values
// stay two different values after redaction and a re-run over the same input is byte-identical.
type Redactor struct{ r *redactor }

// NewRedactor returns a fresh redactor; use one per corpus so placeholders number from 1.
func NewRedactor() *Redactor { return &Redactor{r: newRedactor()} }

// Redact returns s with every vet-class match replaced by its placeholder.
func (x *Redactor) Redact(s string) string { return x.r.redact(s) }

// Hits returns the substitutions made so far, by vet class (a copy).
func (x *Redactor) Hits() map[string]int {
	out := make(map[string]int, len(x.r.Hits))
	for class, n := range x.r.Hits {
		out[class] = n
	}
	return out
}

// PIIClasses names the vet classes that still match s, sorted. A harvest runs it over every
// string it is about to write, after redaction, as the exit gate: a non-empty answer means the
// redactor and the vet have drifted and nothing may be written.
func PIIClasses(s string) []string {
	var out []string
	for _, p := range piiPatterns {
		if p.re.MatchString(s) {
			out = append(out, p.name)
		}
	}
	sort.Strings(out)
	return out
}
