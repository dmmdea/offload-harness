package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/piishape"
	"github.com/dmmdea/offload-harness/internal/quotecheck"
)

// Evidence-preserving reducer — an OPTIONAL step at the tool-result boundary,
// default off (config agent_evidence_reducer; Loop.WithEvidenceReducer). It is
// not a rung of the compaction ladder proper: the ladder rewrites OLDER bodies
// under budget pressure and is model-free by design (a cascade call on every
// step's critical path is what skeleton.go argues against). This step runs once,
// when a build or test result is first produced, and costs one cascade call.
//
// For a command result of 4 KiB or more, a cascade seat is asked for a few
// claims, each with one quote copied from the output. The delegator then looks
// every quote up in the output with the research digest's own lookup
// (quotecheck.Locate: the page's bytes first, then case, spacing, quote
// marks and punctuation set aside, publishing the output's own span), and the
// transcript keeps a receipt instead of the log: a disclosure line, the tool's
// exit line copied verbatim, and each claim over its verified quote. Anything
// that goes wrong keeps the raw output, unchanged.
//
// Properties, each pinned by a test in rung_reducer_test.go:
//
//   - Off unless installed: with no reducer the loop is byte-identical to before.
//   - Applies only to a build or test invocation through run or run_shell (the
//     producer gate the log-collapse rung uses, logsCallIsBuild) whose result
//     opens with the exit line those tools print and is 4 KiB to 64 KiB. Every
//     read, search, listing, fetch and any command the gate cannot read as a
//     build or test (cat, sed, grep, a chained command) is bypassed, because the
//     agent needs their exact text; an unknown producer fails closed.
//   - The exit status is the tool's: the receipt's second line is the raw first
//     line, copied by code, never written by the seat.
//   - All or nothing: one quote that is not in the output, a claim without a
//     usable quote (blank, or not a single line of 8 to 200 characters), no
//     claims, more than 8, a seat error, a deferral, an answer that is not the
//     requested JSON, a panic or a timeout leaves the raw output.
//   - Credential-shaped output never reaches the seat: the output is checked
//     with the corpus vet's table (piishape.Matching) before the call, and the
//     receipt is checked again before it replaces anything.
//   - Smaller or nothing: a receipt that is not strictly shorter than the output
//     it replaces is discarded.
//   - A repeat is honoured: a receipt is not the output, so an exact repeat of
//     the call re-runs it once and that result is not reduced (the same ramp the
//     compaction rungs use: content the model asks for again stops being
//     lossily rewritten), instead of being refused as "you already have that".
//
// Honest limits, stated here rather than papered over:
//
//   - A verified quote proves the line is in the output, not that the claim
//     follows from it; the claim wording is the seat's, and the receipt says so.
//     A quote may also be a framing line ("--- stdout ---"): the check cannot
//     tell evidence from boilerplate.
//   - quotecheck.Locate is tolerant on purpose (case, spacing, punctuation,
//     and "first words ... last words" with both parts in order), so a quote
//     found only that way is replaced by the output's own span. An elided
//     quote is not a span of the output, so it is refused (the raw output is
//     kept) rather than published.
//   - The seat reads the body the transcript would have carried (after the
//     per-result character cap), not the tool's full output.
//   - The seat call sits on the loop's critical path: seconds per qualifying
//     result. That cost, and whether the receipt costs the agent anything it
//     needed, is what the seat A/B measures; until it passes the key stays off.
//   - The credential check is the vet table, not a detector written for this
//     step. It also flags an email address (so a log that prints one stays raw).
//     It covers a key word (api key, private/access/auth key, secret, token,
//     passwd, password, PGPASSWORD, passphrase, credentials, and _PASS= or _PWD=)
//     in a name, including after an underscore (GITHUB_TOKEN=..., OPENAI_API_KEY=...,
//     DB_PASSWORD=..., AWS_SECRET_ACCESS_KEY=...) and after a JSON escape (the
//     newline in escaped text before a line that starts with PASSWORD=) followed by a value of 12 or
//     more characters that runs to the next whitespace, quote, comma, semicolon,
//     ampersand or backslash (so '.', '/', '+', '=', '@', '~' and ':' are inside
//     the value and the whole of a JWT or a slash-bearing secret key is matched,
//     and a leading symbol is allowed; a backslash ends it, being a JSON escape in
//     tool-call arguments; a value that starts with a quote runs to the closing
//     quote instead, so a space, comma, semicolon or ampersand inside it is part of
//     it, and so is a quoted literal behind :=, == or a type annotation), a
//     bearer token (the same value rule, 16 or more characters), a bare
//     provider key (sk-..., sk-proj-..., sk-ant-...), a JWT, a Basic
//     Authorization header, a registry config's "auth" value and a Cookie or
//     Set-Cookie header with a value of 12 or more characters (the whole line
//     goes with it, a double-quoted pair is stepped over with the escapes
//     inside it, as in Django's messages cookie, and a single quote or
//     an apostrophe ends the match). It does NOT
//     cover: a camel-case name (githubToken=...), a key name the table does
//     not hold (dbpassword=, sid=), a
//     command-line flag (--token value, curl -u user:pass), URL userinfo
//     (scheme://user:pass@host), an authorization scheme other than Bearer and
//     Basic, a provider prefix the table does not list
//     (sk_live_, xoxb-), an unsigned JWT, or a high-entropy string that has no
//     key name or known prefix, a value that starts with '/', '.', '[' or ']', and
//     the part of a value after a backslash inside it. Those reach the seat and
//     the receipt. The
//     table is shared with the corpus vet in internal/piishape, so widening it
//     widens both; a log that merely names a setting (MAX_TOKENS=4096,
//     token_count: 12) stays clean, and a value that starts with '/' or '.' (a
//     path after token_store=) is not read as a secret, while a relative path
//     that starts with a letter is, which is the safe direction.
//   - IsEvidenceReceipt recognises the opening line only. A raw body that
//     begins with it (a saved receipt read back) is taken for a receipt: its
//     repeat re-runs once and a trace holding it is not harvested.

const (
	// reducerMinChars is the smallest result worth a seat call ("4 KiB or more").
	reducerMinChars = 4 << 10
	// reducerMaxChars bounds the text handed to a seat; a larger result is left
	// raw rather than truncated, which would leave quotes the seat never saw.
	reducerMaxChars = 64 << 10
	// reducerAskClaims is what the seat is asked for; reducerMaxClaims what is
	// accepted. The extract task's 512-token completion budget holds about five
	// claims with short quotes; an answer cut by it is not JSON and falls back.
	reducerAskClaims = 5
	reducerMaxClaims = 8
	// A quote must be one line of at least reducerMinQuoteRunes characters (a
	// shorter one is found anywhere and evidences nothing) and at most
	// reducerMaxQuoteRunes (a longer one is the log again).
	reducerMinQuoteRunes = 8
	reducerMaxQuoteRunes = 200
	// reducerTimeout bounds the seat call.
	reducerTimeout = 60 * time.Second
	// receiptPrefix opens every receipt: disclosure to the model and the
	// recognition mark for IsEvidenceReceipt (pure ASCII, like the other rungs').
	receiptPrefix = "[evidence-receipt: original "
)

// reducerAsk is the instruction put in front of the output in the seat's input:
// the extract task builds its prompt from the field names alone.
const reducerAsk = "Below is the output of a build or test command. List the claims a reader needs from it " +
	"(what failed, which tests or files, the error messages, the counts, what passed), at most 5. " +
	"Each claim has `claim` (one short sentence) and `quote` (ONE line copied character for character from the output, " +
	"never shortened, reworded, translated or re-cased, at least 8 characters). Claim nothing you cannot quote."

const reducerSchemaJSON = `{"type":"object","properties":{"claims":{"type":"array","items":{"type":"object","properties":{"claim":{"type":"string"},"quote":{"type":"string"}},"required":["claim","quote"]}}},"required":["claims"]}`

// reducerParams is the extract call's params: a fresh map each time, because the
// closure that receives it is not ours.
func reducerParams() map[string]any {
	var schema map[string]any
	if err := json.Unmarshal([]byte(reducerSchemaJSON), &schema); err != nil {
		panic(err) // a constant: caught by every test
	}
	return map[string]any{"schema": schema}
}

// reducerExitRE is the exit line formatShellResult prints first.
var reducerExitRE = regexp.MustCompile(`^exit=-?[0-9]+(?: \(killed by signal [0-9]+\))?$`)

// reducerExitLine returns the body's first line when it is an exit line.
func reducerExitLine(body string) (string, bool) {
	line, _, _ := strings.Cut(body, "\n")
	return line, reducerExitRE.MatchString(line)
}

// evidenceReducer holds the cascade seam and the bound on one call.
type evidenceReducer struct {
	offload OffloadFunc
	timeout time.Duration
}

// IsEvidenceReceipt reports whether a tool body is a receipt written by the
// evidence reducer. Like IsLogCollapseArtifact it is separate from
// IsCompactionArtifact: that predicate drives the always-on dedupe rung, and a
// step that is off must not change the default ladder, even for a body that
// merely quotes the opening line. The trace harvest asks it so it refuses a
// transcript whose raw output a receipt already replaced; the loop asks it (only
// with the reducer installed) so a repeated call is re-run.
func IsEvidenceReceipt(content string) bool { return strings.HasPrefix(content, receiptPrefix) }

// resultIsReceipt reports whether the tool result for callID is a receipt.
func resultIsReceipt(msgs []Msg, callID string) bool {
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == callID {
			return IsEvidenceReceipt(m.Content)
		}
	}
	return false
}

// WithEvidenceReducer installs the evidence-preserving reducer over the cascade
// seam the loop's offload tools already use (pipeline's recordless offload: one
// named tier, no ledger row, no new client). nil = off, the default.
func (l *Loop) WithEvidenceReducer(offload OffloadFunc) *Loop {
	if offload == nil {
		l.reducer = nil
		return l
	}
	l.reducer = &evidenceReducer{offload: offload, timeout: reducerTimeout}
	return l
}

// reduceEvidence returns the body the transcript keeps for one tool result: a
// receipt, or body itself. Only a result the reducer applies to costs a seat
// call, and while it runs the liveness watch is on the seat call's bound.
func (l *Loop) reduceEvidence(ctx context.Context, call ToolCall, body string) string {
	r := l.reducer
	if r == nil || !r.applies(call, body) {
		return body
	}
	l.toolPhase(r.timeout)
	defer l.phase(PhaseDecoding, 0)
	receipt, why := r.reduce(ctx, body)
	if why != "" {
		log.Printf("agent evidence reducer: kept the raw output of %s (%d chars): %s", call.Name, len(body), why)
		return body
	}
	log.Printf("agent evidence reducer: %s output reduced from %d to %d chars", call.Name, len(body), len(receipt))
	return receipt
}

// applies is the producer and size gate: no seat call unless it says yes.
func (r *evidenceReducer) applies(call ToolCall, body string) bool {
	if len(body) < reducerMinChars || len(body) > reducerMaxChars {
		return false
	}
	if !logsCallIsBuild(call) {
		return false
	}
	_, ok := reducerExitLine(body)
	return ok
}

// reduce asks the seat for claims and returns the receipt, or ("", why) when the
// raw output must be kept. It never panics.
func (r *evidenceReducer) reduce(ctx context.Context, body string) (receipt, why string) {
	defer func() {
		if p := recover(); p != nil {
			receipt, why = "", fmt.Sprintf("the reducer panicked: %v", p)
		}
	}()
	exit, _ := reducerExitLine(body)

	// The output is not shown to a seat when it looks like it carries a secret.
	if classes := piishape.Matching(body); len(classes) > 0 {
		return "", "the output looks like it carries a credential or an address (" + strings.Join(classes, ", ") + ")"
	}

	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	reply, err := r.offload(cctx, "extract", reducerAsk+"\n\n--- BEGIN OUTPUT ---\n"+body+"\n--- END OUTPUT ---", reducerParams())
	if err != nil {
		return "", fmt.Sprintf("the seat call failed: %v", err)
	}
	var ans struct {
		Deferred bool   `json:"deferred"`
		Reason   string `json:"reason"`
		Claims   []struct {
			Claim string `json:"claim"`
			Quote string `json:"quote"`
		} `json:"claims"`
	}
	if json.Unmarshal([]byte(reply), &ans) != nil {
		return "", "the seat's answer is not the requested JSON"
	}
	if ans.Deferred {
		return "", "the seat deferred: " + ans.Reason
	}
	n := len(ans.Claims)
	if n == 0 {
		return "", "the seat gave no claims"
	}
	if n > reducerMaxClaims {
		return "", fmt.Sprintf("the seat gave %d claims, more than the %d accepted", n, reducerMaxClaims)
	}
	claims := make([]string, n)
	quotes := make([]string, n)
	for i, c := range ans.Claims {
		claims[i] = strings.Join(strings.Fields(c.Claim), " ")
		quotes[i] = strings.TrimSpace(c.Quote)
		if claims[i] == "" || quotes[i] == "" {
			return "", fmt.Sprintf("claim %d has no text or no quote", i+1)
		}
	}

	// Every quote must be in the output: the research digest's own lookup
	// (quotecheck.Locate), so the two lanes cannot disagree about what a verbatim
	// quote is. What is published is the output's own span, never the seat's text.
	folds := quotecheck.NewFolds(body)
	spans := make([]string, n)
	for i, q := range quotes {
		span, how := quotecheck.Locate(body, folds, q)
		if how == quotecheck.Unverified {
			return "", "a quote is not in the output"
		}
		// Locate also accepts "first words ... last words" and returns the seat's own
		// text for it; the receipt says every quote was matched in the output, so what is
		// published must be a contiguous span of it (this also keeps fragments around an
		// ellipsis from slipping under the length minimum).
		if !strings.Contains(body, span) {
			return "", fmt.Sprintf("the quote for claim %d is not one contiguous span of the output", i+1)
		}
		if !reducerQuoteOK(span) { // one line of sensible length
			return "", fmt.Sprintf("the quote for claim %d is not a single line of %d to %d characters", i+1, reducerMinQuoteRunes, reducerMaxQuoteRunes)
		}
		spans[i] = span
	}

	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "%s%d chars, %d lines; every quote below was matched in the original output, the wording of each claim is the summarizer's]\n", receiptPrefix, len(body), len(lines))
	b.WriteString(exit + "\n")
	for i := range claims {
		b.WriteString("- " + claims[i] + "\n  > " + spans[i] + "\n")
	}
	b.WriteString("[evidence-receipt: the rest of the output is not kept here; run the same command again to get it whole]")
	receipt = b.String()

	if classes := piishape.Matching(receipt); len(classes) > 0 {
		return "", "the receipt looks like it carries a credential or an address (" + strings.Join(classes, ", ") + ")"
	}
	if len(receipt) >= len(body) {
		return "", fmt.Sprintf("the receipt (%d chars) is not smaller than the output (%d)", len(receipt), len(body))
	}
	if !reducerKeepsExit(receipt, exit) {
		return "", "the exit status did not survive into the receipt"
	}
	return receipt, ""
}

// reducerKeepsExit is the exit-status check: the receipt's second line, the one
// after the disclosure line, is the tool's exit line byte for byte.
func reducerKeepsExit(receipt, exit string) bool {
	got := strings.SplitN(receipt, "\n", 3)
	return len(got) == 3 && IsEvidenceReceipt(got[0]) && got[1] == exit
}

// reducerQuoteOK reports whether q can stand as a quote: one line, valid UTF-8,
// between reducerMinQuoteRunes and reducerMaxQuoteRunes characters.
func reducerQuoteOK(q string) bool {
	if strings.ContainsAny(q, "\r\n") || !utf8.ValidString(q) {
		return false
	}
	n := utf8.RuneCountInString(q)
	return n >= reducerMinQuoteRunes && n <= reducerMaxQuoteRunes
}
