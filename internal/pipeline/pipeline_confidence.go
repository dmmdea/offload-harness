// Confidence gate and margins, featurize, exemplar feedback, answer agreement, label agreement and shadow capture.

package pipeline

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/dmmdea/offload-harness/internal/confidence"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/exemplars"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/parser"
	"github.com/dmmdea/offload-harness/internal/shadow"
)

// confidenceGate decides whether a validated triage/classify result is too shaky
// to accept and should escalate to a larger tier. It combines the model's
// self-reported confidence (classify) with the logprob-derived decision margin
// (both tasks). It ALWAYS returns the computed margin (0 if N/A) so the ledger
// can record it on success — that margin stream is what Phase 2 calibrates on.
// The threshold is per-task (data-derived via `calibrate`) with the config
// constant as fallback. Returns (reason, margin, source, escalate?).
//
// The SOURCE is returned beside the human-readable reason because the two serve
// different readers: the reason is for a person reading one deferred row, the
// source is a closed enum the ledger can group by. Without it, "was this the
// model's self-report or a structural signal?" is unanswerable in aggregate —
// which is exactly the gap measured on 2026-08-11.
func (p *Pipeline) confidenceGate(req core.Request, data []byte, lps []llamaclient.TokenLogprob) (string, marginInfo, core.EscalationSource, bool) {
	var mi marginInfo
	switch req.Task {
	case core.TaskClassify:
		if labels := labelClasses(req.Params); len(labels) >= 2 {
			mi = p.marginOf(lps, "label", labels)
		}
		if conf, low := lowConfidence(data, p.cfg.ClassifyMinConfidence); low {
			return fmt.Sprintf("low confidence %.2f", conf), mi, core.EscSelfConfidence, true
		}
		if t := p.marginGateThreshold(req.Task); t > 0 && mi.Margin > 0 && mi.Margin < t {
			return fmt.Sprintf("low decision margin %.2f<%.2f", mi.Margin, t), mi, core.EscMargin, true
		}
	case core.TaskTriage:
		mi = p.marginOf(lps, "decision", []string{"yes", "no", "unsure"})
		if t := p.marginGateThreshold(req.Task); t > 0 && mi.Margin > 0 && mi.Margin < t {
			return fmt.Sprintf("low decision margin %.2f<%.2f", mi.Margin, t), mi, core.EscMargin, true
		}
	}
	return "", mi, core.EscNone, false
}

// marginInfo is one decision position as the gate and the ledger see it: the
// margin ON A NAMED SCALE, plus the two facts that make a full-scale threshold
// re-derivable (register D-130). Scale is empty when no margin was resolved, so
// a task that never had one publishes a byte-identical row.
type marginInfo struct {
	Margin       float64
	Scale        string
	DeclaredMass float64
	Ambiguous    int
}

func (mi marginInfo) apply(m *core.Meta) {
	m.Margin = mi.Margin
	m.MarginScale = mi.Scale
	m.MarginDeclaredMass = mi.DeclaredMass
	m.MarginAmbiguous = mi.Ambiguous
}

// marginOf resolves the decision position ONCE and publishes the margin on the
// scale this box currently emits. The declared mass and the ambiguous-token
// count are recorded in BOTH modes on purpose: the data needed to derive a
// full-scale threshold has to accumulate BEFORE the flag is flipped, not after.
func (p *Pipeline) marginOf(lps []llamaclient.TokenLogprob, jsonKey string, classes []string) marginInfo {
	d, ok := confidence.MarginDetail(lps, jsonKey, classes)
	if !ok {
		return marginInfo{}
	}
	mi := marginInfo{Scale: core.MarginScaleOf(p.cfg.ConfidenceMarginFullDenominator), DeclaredMass: d.DeclaredMass, Ambiguous: d.Ambiguous}
	if p.cfg.ConfidenceMarginFullDenominator {
		mi.Margin = d.Margin
		return mi
	}
	mi.Margin = d.MatchedMargin
	return mi
}

// marginGateThreshold is the threshold for the scale in force. On the FULL
// scale that is confidence_margin_threshold_full and nothing else: the per-task
// conformal values and the config constant were both derived from matched-scale
// margins, which run ~10x larger, so applying either here would fire on almost
// every call. 0 there means the gate never fires — said once, loudly, because a
// silently disabled gate is exactly the failure this whole change is about.
func (p *Pipeline) marginGateThreshold(task core.TaskType) float64 {
	if !p.cfg.ConfidenceMarginFullDenominator {
		return p.marginThreshold(task)
	}
	if p.cfg.ConfidenceMarginThresholdFull <= 0 {
		warnFullMarginGateDisabled.Do(func() {
			fmt.Fprintln(os.Stderr, "note: confidence_margin_full_denominator is on with confidence_margin_threshold_full=0 - the decision-margin gate never fires; rows still carry margin_scale: full so a threshold can be re-derived")
		})
		return 0
	}
	return p.cfg.ConfidenceMarginThresholdFull
}

// warnFullMarginGateDisabled keeps the disabled-gate note to one line per
// process: it is a config state, not a per-call event.
var warnFullMarginGateDisabled sync.Once

// marginThreshold returns the per-task escalation threshold: a data-derived
// conformal threshold (Phase 2, loaded from thresholds.json into p.thresholds)
// when present, else the config constant.
func (p *Pipeline) marginThreshold(task core.TaskType) float64 {
	if thr := p.thresholdsSnap(); thr != nil {
		if t, ok := thr[string(task)]; ok {
			return t
		}
	}
	return p.cfg.ConfidenceMarginThreshold
}

// labelClasses extracts the classify label set from request params, accepting
// either []string or []any (JSON-decoded).
func labelClasses(params map[string]any) []string {
	v, ok := params["labels"]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func lowConfidence(data []byte, threshold float64) (float64, bool) {
	var c struct {
		Confidence float64 `json:"confidence"`
	}
	if json.Unmarshal(data, &c) != nil {
		return 0, false
	}
	return c.Confidence, c.Confidence < threshold
}

var (
	reNumber = regexp.MustCompile(`\d[\d.,]*`)
	reCaps   = regexp.MustCompile(`\b[A-Z][A-Za-z0-9]+`)
)

// featurize extracts cheap, deterministic input features for the entry-tier
// router (Phase 5) — all len/regex ops, sub-millisecond, no inference.
func featurize(task core.TaskType, input string) map[string]float64 {
	bf := func(c bool) float64 {
		if c {
			return 1
		}
		return 0
	}
	return map[string]float64{
		"len_chars": float64(len(input)),
		"n_words":   float64(len(strings.Fields(input))),
		"n_numbers": float64(len(reNumber.FindAllString(input, -1))),
		"n_caps":    float64(len(reCaps.FindAllString(input, -1))),
		"has_code":  bf(strings.Contains(input, "```") || strings.Contains(input, "func ") || strings.Contains(input, "def ")),
		"has_url":   bf(strings.Contains(input, "http://") || strings.Contains(input, "https://")),
	}
}

// goodExemplar gates which successful calls are harvested as few-shot examples:
// grounded (or N/A) and a confident margin (or N/A).
//
// The margin test is SCALE-AWARE (register D-130). The 0.6 constant is a
// matched-scale number; a full-scale margin runs ~10x smaller, so comparing one
// against it would harvest nearly nothing. On the full scale the harvest uses
// the re-derived full threshold, and with no such threshold set it harvests
// none of the rows that carry a margin at all - a deliberate stop rather than a
// silent cross-scale comparison.
func (p *Pipeline) goodExemplar(meta core.Meta) bool {
	if meta.Grounded != nil && !*meta.Grounded {
		return false
	}
	if meta.Margin <= 0 {
		return true // no margin for this task - unchanged, harvest on grounding alone
	}
	if meta.MarginScale == core.MarginScaleFull {
		t := p.cfg.ConfidenceMarginThresholdFull
		return t > 0 && meta.Margin >= t
	}
	return meta.Margin >= 0.6
}

// injectExemplars prepends a few-shot block (local-model tokens only) to the
// user prompt. Inputs are capped so the demonstrations stay small.
func injectExemplars(user string, ex []exemplars.Pair) string {
	var b strings.Builder
	b.WriteString("Examples of correct output for similar inputs:\n")
	for _, e := range ex {
		b.WriteString("INPUT: ")
		b.WriteString(truncateStr(e.Input, 400))
		b.WriteString("\nOUTPUT: ")
		b.WriteString(e.Output)
		b.WriteString("\n\n")
	}
	b.WriteString("Now do the same for the input below.\n\n")
	b.WriteString(user)
	return b.String()
}

// --- Task 1.5: cascade-agreement correctness-proxy labels for classify/triage ---
//
// classify/triage get no grounding label (grounding doesn't apply). But when one
// of those calls escalates (the entry tier was low-confidence) and a larger tier
// then answers, AGREEMENT between the entry tier's candidate and the larger
// tier's answer is a strong proxy that the entry tier was correct. We record
// those labeled rows to a SEPARATE sidecar (never the main ledger, which feeds
// the router/calibration/savings and must stay pristine); only the confhead
// reads it. Labels accrue as escalation traffic occurs.

// answersAgree reports whether the entry-tier candidate and the final answer pick
// the same class. ok=false when the task isn't class-pinned or either side is
// unparseable / missing the class field.
func answersAgree(task core.TaskType, candidate string, finalData []byte) (agreed bool, ok bool) {
	var field string
	switch task {
	case core.TaskClassify:
		field = "label"
	case core.TaskTriage:
		field = "decision"
	default:
		return false, false
	}
	// Parser-extract the candidate first so it's cleaned the SAME way final.Data
	// was (final.Data is already parser-extracted). The entry candidate is raw
	// gen.Content — fenced/prose-wrapped/comma-trailing output would fail the
	// strict Unmarshal in jsonStringField and silently drop a valid agreement.
	cand, perr := parser.Extract(candidate)
	if perr != nil {
		return false, false
	}
	a := jsonStringField(cand, field) // cand is json.RawMessage ([]byte)
	b := jsonStringField(finalData, field)
	if a == "" || b == "" {
		return false, false
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)), true
}

// AnswersAgree is a thin exported wrapper around answersAgree for use by the
// shadow-labeling flywheel (which lives in a separate package and cannot call the
// unexported function directly). task is a task-type string (e.g. "classify").
// Behavior is identical to answersAgree.
func AnswersAgree(task string, candidate string, finalData []byte) (agreed bool, ok bool) {
	return answersAgree(core.TaskType(task), candidate, finalData)
}

// jsonStringField returns the string value of `field` in a JSON object, or "" if
// the JSON is unparseable, the field is absent, or its value is not a string.
func jsonStringField(raw []byte, field string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if v, ok := m[field]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// labelAgreement records a cascade-agreement correctness-proxy label for the
// entry tier to the confhead sidecar (best-effort telemetry; never fails the
// request). entry is the entry-tier (ci==0) feature snapshot; final is the
// larger tier's accepted result. No-op when the sidecar is unconfigured or the
// task isn't class-pinned / either answer is unparseable.
func (p *Pipeline) labelAgreement(task core.TaskType, entry ledger.Entry, candidate string, final core.Result, inputChars int) {
	if p.cfg.ConfHeadLabelsPath == "" {
		return
	}
	agreed, ok := answersAgree(task, candidate, final.Data)
	if !ok {
		// COUNT WHAT IS DISCARDED. answersAgree returns ok=false for two very different
		// reasons -- a task it does not judge (anything but classify/triage), and a
		// candidate it could not parse -- and both used to vanish here without trace.
		//
		// That silence is not neutral: an unparseable candidate is disproportionately an
		// EXTREME disagreement (a truncated or malformed answer), so dropping it quietly
		// biases the measured agreement rate UPWARD, in the direction that argues against
		// acting. A rate whose losses are uncountable cannot state its own coverage, and
		// this file's whole job is to produce a rate somebody will make a decision from.
		// SPLIT, because the two causes support different conclusions. A task answersAgree
		// does not judge (anything but classify/triage) is a STRUCTURAL exclusion with no
		// bias implication -- it never belonged to this population. An unparseable candidate
		// IS a bias term: it is disproportionately an extreme disagreement, so losing it
		// pulls the published rate upward. Counting both in one number would let a box whose
		// escalations are mostly summarize report "92% dropped, the rate is badly biased"
		// when zero judgeable candidates were lost and coverage is in fact 100%.
		if task == core.TaskClassify || task == core.TaskTriage {
			p.labelDropsUnparseable.Add(1)
			// PERSISTED, because the consumer is a different process. loupe reads the
			// sidecar from disk; an in-memory counter on a serving Pipeline could only ever
			// describe that process's uptime against a file spanning weeks -- and
			// NewInLoopPipeline is a second instance writing the same file. Best-effort:
			// telemetry must never fail the work it describes.
			appendDropCounter(p.cfg.ConfHeadLabelsPath, p.labelDropsUnparseable.Load())
		} else {
			p.labelDropsUnjudgeableTask.Add(1)
		}
		return
	}
	entry.Grounded = nil
	entry.EscalatedAgreed = &agreed
	entry.LabelSource = ledger.LabelSourceLiveEscalation
	_ = ledger.AppendLabel(p.cfg.ConfHeadLabelsPath, entry)
}

// shadowCaptureTasks are the tasks whose non-escalated rows are captured into
// the shadow queue for nightly counterfactual labeling. Phase A judges
// classify/triage/extract with the existing in-process judges (answersAgree /
// grounding.Check); Phase B adds summarize (judged by the B2 summarize judge).
var shadowCaptureTasks = map[string]bool{"classify": true, "triage": true, "extract": true, "summarize": true}

// captureShadow appends a sampled, non-escalated entry-tier row to the shadow
// queue for nightly counterfactual labeling. Cheap (one append, no inference);
// best-effort (a queue error never affects the request). Capture is off by
// default (ShadowEnabled=false) and never touches the grammar/generation path.
func (p *Pipeline) captureShadow(req core.Request, e ledger.Entry, res core.Result) {
	if !p.cfg.ShadowEnabled || p.cfg.ShadowQueuePath == "" {
		return
	}
	if e.Escalations != 0 || !shadowCaptureTasks[strings.ToLower(e.Task)] {
		return
	}
	if rand.Float64() >= p.cfg.ShadowRate {
		return
	}
	_ = shadow.Enqueue(p.cfg.ShadowQueuePath, shadow.Item{
		TS:          e.TS,
		Task:        e.Task,
		Input:       req.Input,
		Params:      req.Params,
		EntryTier:   e.ModelTier,
		EntryOutput: string(res.Data),
		Feat:        e.Feat,
	})
}

// appendDropCounter writes the running unparseable-drop count beside the label sidecar so a
// SEPARATE process (loupe) can read it.
//
// Whole-file rewrite of a tiny integer rather than an append: the value is a running total,
// so an append-only log would need summing and would grow without bound for one number.
// Best-effort throughout -- a telemetry write must never fail the work it describes, which
// is the same posture pipeline.record takes.
func appendDropCounter(labelsPath string, n int64) {
	if labelsPath == "" {
		return
	}
	_ = os.WriteFile(labelsPath+".drops", []byte(strconv.FormatInt(n, 10)), 0o644)
}
