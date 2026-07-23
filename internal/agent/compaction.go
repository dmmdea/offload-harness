package agent

import (
	"fmt"

	"github.com/dmmdea/offload-harness/internal/gcf"
)

// Transcript compaction. The agent loop resends the FULL running transcript on
// every step, but the local models are served with a small window (the shipped
// templates use --ctx-size 8192) and the loop reserves --max-tokens for the
// completion, so the effective INPUT budget is only a few thousand tokens. A
// few tool results overflow it, the server 400s, and the run aborts. This file
// keeps the transcript inside budget so long tasks finish, WITHOUT breaking
// assistant<->tool pairing (an orphaned tool result or tool call is a wire
// error) and WITHOUT touching system or the objective (the two things the model
// must never lose sight of).

// bytesPerToken is the crude bytes-per-token heuristic. ~4 bytes/token is the
// long-standing rule of thumb for English-ish text; the local Gemma tokenizer
// is close enough for a BUDGET (we only need to know "roughly how full are we",
// not an exact count). Deliberately conservative: over-estimating slightly just
// means we compact a touch early, which is the safe direction.
const bytesPerToken = 4

// perMsgOverhead approximates the wire framing each message costs beyond its
// content: role tag, delimiters, and (for assistant turns) the tool-call
// envelope. A flat small constant is enough for a budget estimate.
const perMsgOverhead = 4

// estimateTokens returns a cheap, approximate token count for a transcript:
// sum over messages of len(content)/4 + a small per-message overhead, plus the
// tool-call argument bytes (they go on the wire too). Approximate by design —
// see bytesPerToken. Never returns a negative number.
func estimateTokens(msgs []Msg) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content) / bytesPerToken
		total += perMsgOverhead
		for _, c := range m.ToolCalls {
			total += (len(c.Name) + len(c.Args)) / bytesPerToken
		}
	}
	return total
}

// compact reduces msgs to fit within budget (in estimated tokens) using the
// least-destructive edit that works, in this fixed order:
//
//  1. If already under budget, return msgs UNCHANGED (byte-for-byte) — this
//     preserves prefix stability so the server's KV cache is not invalidated on
//     the happy path.
//  2. ALWAYS keep, full and in place: the contiguous protected preamble
//     [0, protectedPrefix) — the caller passes its real length (system +
//     profile exemplars + recall + AGENT.md + objective), so the objective is
//     never guessed at and never dropped even when exemplars precede it — plus
//     the most recent keepRecent turns.
//     2a. (opts.GCF only, LOSSLESS) re-encode OLDER tool bodies that are
//     eligible JSON arrays into GCF columnar form (internal/gcf) — states the
//     repeated keys once, loses nothing; always tried before any lossy rung.
//     2b. (opts.Skeleton only) SKELETONIZE the bodies of OLDER tool-role messages
//     oldest-first until under budget: head/tail windows + buried signal lines
//     kept, the rest elided to counted run markers (see skeleton.go). Strictly
//     less destructive than step 3 — the error a task later needs usually
//     survives — and a skeleton can still fall through to a bare marker below.
//  3. Elide the bodies of OLDER tool-role messages to a compact marker
//     (preserving Role + ToolCallID so pairing is intact), oldest-first, until
//     under budget. Eliding a body is preferred over dropping a message.
//  4. If still over budget, drop whole OLDER turns oldest-first. A turn is an
//     assistant message that has ToolCalls PLUS all its matching tool results,
//     dropped as a unit so assistant<->tool pairing is never broken. The
//     protected preamble (incl. the objective) is never dropped.
//
// keepRecent counts assistant/tool turns to keep verbatim from the end; a
// non-positive value is treated as 0 (nothing pinned as "recent", though the
// protected prefix is still always kept).
//
// protectedPrefix is the length of the contiguous leading preamble the caller
// guarantees must be kept verbatim: system + profile exemplars + recall +
// AGENT.md + objective — everything before the first model turn. The loop passes
// its real preamble length so the objective is never guessed at (it is NOT
// necessarily the first user message once profile exemplars precede it). It is
// clamped to [0, len(msgs)]. The preamble is bounded (recall/AGENT.md are capped
// and exemplars are a small fixed set); if it ALONE exceeds budget, compaction
// cannot shrink it — the transcript stays over budget and the run errors honestly
// on the next Chat rather than silently dropping the objective to fit.
// compactOpts selects the optional gentler rungs of the ladder. Zero value =
// the original two-rung ladder (markers, drops) — the pinned default path.
type compactOpts struct {
	GCF      bool // lossless: re-encode eligible JSON-array tool bodies (internal/gcf)
	Skeleton bool // lossy-structural: reduce older tool bodies to signal skeletons
}

func compact(msgs []Msg, budget int, keepRecent int, protectedPrefix int, opts compactOpts) []Msg {
	if estimateTokens(msgs) <= budget {
		return msgs // happy path: untouched, KV cache preserved.
	}
	if keepRecent < 0 {
		keepRecent = 0
	}
	if protectedPrefix < 0 {
		protectedPrefix = 0
	}
	if protectedPrefix > len(msgs) {
		protectedPrefix = len(msgs)
	}

	// Work on a copy: never mutate the caller's slice or its backing array.
	out := make([]Msg, len(msgs))
	copy(out, msgs)

	protectedEnd := protectedPrefix // [0,protectedEnd) is always kept.
	recentStart := len(out) - keepRecent    // [recentStart,len) is always kept.
	if recentStart < protectedEnd {
		recentStart = protectedEnd
	}

	// Step 2a (flag-gated, LOSSLESS): re-encode older tool bodies that are
	// eligible JSON arrays into GCF columnar form — zero information loss, so
	// it always runs before any lossy rung. Deterministic and idempotent
	// (gcf.IsCompacted stops re-encoding).
	if opts.GCF {
		for i := protectedEnd; i < recentStart && estimateTokens(out) > budget; i++ {
			if out[i].Role == "tool" && !isElided(out[i].Content) && !isSkeletonized(out[i].Content) && !gcf.IsCompacted(out[i].Content) {
				if c, ok := gcf.Compact(out[i].Content); ok {
					out[i].Content = c
				}
			}
		}
		if estimateTokens(out) <= budget {
			return out
		}
	}

	// Step 2b (flag-gated): skeletonize OLDER tool bodies oldest-first — the
	// least-destructive body edit. Deterministic, so a transcript compacted
	// twice lands on identical bytes (isSkeletonized stops re-pruning).
	if opts.Skeleton {
		for i := protectedEnd; i < recentStart && estimateTokens(out) > budget; i++ {
			if out[i].Role == "tool" {
				if sk, ok := skeletonize(out[i].Content); ok {
					out[i].Content = sk
				}
			}
		}
		if estimateTokens(out) <= budget {
			return out
		}
	}

	// Step 3: elide OLDER tool-result bodies to markers, oldest-first. A
	// skeleton falls through to a bare marker here under harder pressure; its
	// marker reports the ORIGINAL body size (parsed from the skeleton's own
	// prefix), not the skeleton's — the marker discloses what was lost.
	for i := protectedEnd; i < recentStart && estimateTokens(out) > budget; i++ {
		if out[i].Role == "tool" && !isElided(out[i].Content) {
			n := len(out[i].Content)
			if orig, ok := skeletonOriginalSize(out[i].Content); ok {
				n = orig
			}
			out[i].Content = elisionMarker(n)
		}
	}
	if estimateTokens(out) <= budget {
		return out
	}

	// Step 4: drop whole OLDER turns oldest-first, keeping assistant<->tool
	// pairs together. Build a keep-mask so removal is a single pass with stable
	// order and no pairing breakage.
	keep := make([]bool, len(out))
	for i := range keep {
		keep[i] = true
	}
	for i := protectedEnd; i < recentStart && estimateTokens(masked(out, keep)) > budget; i++ {
		if !keep[i] {
			continue
		}
		if out[i].Role == "assistant" && len(out[i].ToolCalls) > 0 {
			// Drop this assistant turn AND its matching tool results as a unit.
			ids := map[string]bool{}
			for _, c := range out[i].ToolCalls {
				ids[c.ID] = true
			}
			keep[i] = false
			for j := i + 1; j < recentStart; j++ {
				if out[j].Role == "tool" && ids[out[j].ToolCallID] {
					keep[j] = false
				}
			}
		} else if out[i].Role != "tool" {
			// A bare assistant/other older message with no tool calls: safe to
			// drop on its own (no pairing to preserve). Never a tool result here
			// — those only exist as part of a pair handled above.
			keep[i] = false
		}
	}
	return masked(out, keep)
}

// masked returns the subslice of msgs whose keep flag is true, in order.
func masked(msgs []Msg, keep []bool) []Msg {
	out := make([]Msg, 0, len(msgs))
	for i, m := range msgs {
		if keep[i] {
			out = append(out, m)
		}
	}
	return out
}

// elisionMarker is the compact stand-in for an elided tool-result body. It
// records the original size so a reader (human or model) sees that content was
// dropped and how much.
func elisionMarker(origLen int) string {
	return fmt.Sprintf("[earlier result elided to fit context — %d chars]", origLen)
}

// isElided reports whether a tool body has already been replaced by a marker,
// so a re-compaction does not double-elide (which would misreport the size).
// Compaction only ever replaces a body with this fixed ASCII marker — it never
// partially truncates a body — so no multibyte rune can be split (the marker
// itself is pure ASCII). Rune-safe by construction; no headCut/tailCut needed.
func isElided(content string) bool {
	const prefix = "[earlier result elided to fit context —"
	return len(content) >= len(prefix) && content[:len(prefix)] == prefix
}
