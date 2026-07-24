// Package compeval is the COMPACTION evaluation harness (OmniRoute-harvest
// Phase B): it replays a pinned corpus of transcript slices through the
// PRODUCTION compaction ladder (internal/agent.CompactReplay — never a
// reimplementation) and reports, per content kind: compression ratio,
// entity retention with explicit lost-entity lists, and a tokens-per-entry
// ratchet against a frozen baseline. Its reports are what gate every
// compaction default flip (--skeleton-prune, gcf_compact): savings claims
// exist ONLY as measured mean ratios stamped with the corpus hash — inherited
// or estimated numbers are not admissible.
//
// Methodology provenance: the eval-and-ratchet approach is harvested from the
// OmniRoute compression service's test methodology (MIT); the metrics and
// signals here are this harness's own.
package compeval

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// Entry is one corpus instance: a transcript slice to replay through the
// ladder at a fixed budget. Kind buckets results by content class so a mean
// ratio is never quoted across unlike content.
type Entry struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // tool-json | tool-text | logs | code | prose | mixed
	// Turns is the transcript slice. Compaction targets tool-role bodies; the
	// harness replays the WHOLE slice so protected-prefix/keep-recent semantics
	// match production.
	Turns []agent.Msg `json:"turns"`
	// BudgetTokens is the compaction budget to replay at. 0 = derived as 60% of
	// the entry's own estimated tokens (forces the ladder to actually work).
	BudgetTokens int `json:"budget_tokens,omitempty"`
	// KeepRecent / ProtectedPrefix mirror the production knobs. Defaults: keep
	// the final turn, protect the first (objective-style) turn.
	KeepRecent      int `json:"keep_recent,omitempty"`
	ProtectedPrefix int `json:"protected_prefix,omitempty"`
}

// KnownKinds is the closed bucket set; Load rejects entries outside it so a
// typo'd kind can never silently fragment the per-kind aggregation.
var KnownKinds = map[string]bool{
	"tool-json": true, "tool-text": true, "logs": true,
	"code": true, "prose": true, "mixed": true,
}

// Load reads a JSONL corpus and returns the entries plus the CORPUS HASH —
// sha256 over the raw file bytes. The hash is the pin: every report and every
// baseline is stamped with it, and a ratchet comparison across different
// hashes is refused. Malformed lines are an ERROR here (a corpus is a fixed
// artifact, not a best-effort stream).
func Load(path string) ([]Entry, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<24)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, "", fmt.Errorf("corpus %s line %d: %w", path, line, err)
		}
		if e.ID == "" {
			return nil, "", fmt.Errorf("corpus %s line %d: missing id", path, line)
		}
		if !KnownKinds[e.Kind] {
			return nil, "", fmt.Errorf("corpus %s line %d (%s): unknown kind %q", path, line, e.ID, e.Kind)
		}
		if len(e.Turns) == 0 {
			return nil, "", fmt.Errorf("corpus %s line %d (%s): no turns", path, line, e.ID)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, "", err
	}
	return out, hash, nil
}

// piiPatterns are the DETERMINISTIC refusal classes for corpus vetting: a
// corpus is a committed/replayed artifact, so credential- or identity-shaped
// content refuses the whole corpus rather than being silently scrubbed
// (scrubbing would change the bytes the hash pins).
var piiPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)},
	{"private-key-block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"bearer-token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9_\-\.=]{16,}`)},
	{"api-key-assignment", regexp.MustCompile(`(?i)\b(api[_-]?key|secret|token|passwd|password)\s*[=:]\s*['"]?[A-Za-z0-9_\-]{12,}`)},
	{"aws-access-key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"github-token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
}

// PIIFinding names one refusal hit (entry + class only — the matched text is
// deliberately NOT echoed back).
type PIIFinding struct {
	EntryID string
	Class   string
}

// VetPII scans every turn body and returns the findings. A non-empty result
// means the corpus is NOT usable — callers refuse to evaluate it.
func VetPII(entries []Entry) []PIIFinding {
	var out []PIIFinding
	for _, e := range entries {
		seen := map[string]bool{}
		for _, t := range e.Turns {
			for _, p := range piiPatterns {
				if !seen[p.name] && p.re.MatchString(t.Content) {
					seen[p.name] = true
					out = append(out, PIIFinding{EntryID: e.ID, Class: p.name})
				}
			}
		}
	}
	return out
}

