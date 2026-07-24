// entities.go — the deterministic FORCE_PRESERVE-class entity extractor.
// "Entity" here means the token classes a compaction step must never silently
// destroy: numbers, file paths, URLs, key=value keys, error-line signal,
// UPPER_SNAKE identifiers, and long hex ids. Retention is measured over the
// TOOL bodies (the only content the ladder edits) and every lost entity is
// listed by value — a retention percentage without the lost list is not
// reviewable, so the list is the primary artifact and the ratio is derived.
package compeval

import (
	"regexp"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// entityPatterns are the extraction classes. Deterministic and ordered; a
// token can belong to several classes but is stored once by value.
var entityPatterns = []*regexp.Regexp{
	regexp.MustCompile(`https?://\S+`),                          // URLs
	regexp.MustCompile(`[A-Za-z]:\\[^\s"']+`),                   // Windows paths
	regexp.MustCompile(`(?:^|[\s"'(=])(/[\w.\-]+){2,}`),         // POSIX paths (≥2 segments)
	regexp.MustCompile(`\b[\w.\-]+\.(?:go|ps1|mjs|py|json|yaml|yml|md|exe|dll|gguf|safetensors|png|wav|log|jsonl)\b`), // filenames by known ext
	regexp.MustCompile(`\b[A-Z][A-Z0-9_]{3,}\b`),                // UPPER_SNAKE constants / markers
	regexp.MustCompile(`\b[0-9a-fA-F]{7,40}\b`),                 // hex ids (short SHA .. SHA-1)
	regexp.MustCompile(`\b\d+(?:\.\d+)+\b`),                     // dotted versions / decimals
	regexp.MustCompile(`\b\d{2,}\b`),                            // plain numbers ≥2 digits
	regexp.MustCompile(`\b[\w\-]+=[^\s"']+`),                    // key=value pairs
}

// Entities extracts the entity SET of one string.
func Entities(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, re := range entityPatterns {
		for _, m := range re.FindAllString(s, -1) {
			m = strings.TrimSpace(m)
			// Trailing sentence punctuation is prose, not entity: "exit_code=2,"
			// and "path/file.go." must normalize to the bare value or retention
			// would depend on the sentence the entity sat in.
			m = strings.TrimRight(m, ".,;:)]}\"'")
			if m != "" {
				out[m] = struct{}{}
			}
		}
	}
	return out
}

// Retention compares entity sets over the TOOL bodies of a transcript before
// and after compaction. Returns the retention ratio in [0,1] (1 when the
// original had no entities) and the sorted lost-entity list.
func Retention(before, after []agent.Msg) (ratio float64, lost []string) {
	orig := map[string]struct{}{}
	for _, m := range before {
		if m.Role == "tool" {
			for e := range Entities(m.Content) {
				orig[e] = struct{}{}
			}
		}
	}
	if len(orig) == 0 {
		return 1, nil
	}
	kept := map[string]struct{}{}
	for _, m := range after {
		if m.Role == "tool" {
			for e := range Entities(m.Content) {
				kept[e] = struct{}{}
			}
		}
	}
	for e := range orig {
		if _, ok := kept[e]; !ok {
			lost = append(lost, e)
		}
	}
	sort.Strings(lost)
	return float64(len(orig)-len(lost)) / float64(len(orig)), lost
}
