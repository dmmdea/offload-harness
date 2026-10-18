// redactor_export.go — the harvest's redaction, exported for readers of recorded runs that are not
// transcript replays (the trace-corpus verb). It is a thin face on the same unexported redactor and
// the same vet class table the compaction harvest uses, so a second harvester cannot redact a
// different set of classes than the first one: a class added to the vet table is redacted here
// the day it is added.
package compeval

import (
	"sort"

	"github.com/dmmdea/offload-harness/internal/piishape"
)

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
func PIIClasses(s string) []string { return piishape.Matching(s) }

// Residual names what is still wrong with s after this redactor has run over it: the vet classes
// that still match; "value-tail" when 12 or more consecutive characters of a credential value
// this redactor replaced are still in s (a tail such as ".payload.signature" matches no vet class);
// and the name of the keyword check (piishape.KeywordRunClass) when a credential header keyword
// (cookie, authorization, api key, auth token, security token) is followed within a few characters
// by a long value run, whatever shape stands between them. The last one fails closed: it does not
// read shapes, so it catches the ones the classes do not know, and it also stops text that merely
// follows a keyword (see piishape.KeywordRun).
// Run it over every string about to be written; a non-empty answer means nothing may be written.
func (x *Redactor) Residual(s string) []string {
	out := PIIClasses(s)
	if x.r.survivingTail(s) {
		out = append(out, "value-tail")
	}
	if piishape.KeywordRun(s) {
		out = append(out, piishape.KeywordRunClass)
	}
	sort.Strings(out)
	return out
}
