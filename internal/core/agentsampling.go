package core

import (
	"fmt"
	"strconv"
	"strings"
)

// AgentSampling is the planner's decoding policy for one seat (config
// `agent_sampling` / `agent_sampling_final`, register D-95b).
//
// The agent client has always sent `temperature: 0` and nothing else, which is
// the right default for tool-calling and grounded extraction — and is also the
// setting every Qwen3-class model card warns against for NON-THINKING
// generation, because greedy decoding is exactly what makes a small seat fall
// into a degenerate repetition loop (the Lenovo 4B's METHODOLOGY.md digest,
// 2026-09-14). This type is how an operator gives a MEASURED seat a different
// policy without changing the default for anyone else.
//
// Every field is a pointer: unset means "do not send this key", so a zero
// AgentSampling reproduces today's request byte for byte. There is no
// house-recommended value here on purpose — a sampling setting is a per-seat
// measurement, and baking one in would make every node inherit a number that
// was measured somewhere else.
type AgentSampling struct {
	Temperature       *float64 `json:"temperature,omitempty"`
	TopP              *float64 `json:"top_p,omitempty"`
	TopK              *int     `json:"top_k,omitempty"`
	PresencePenalty   *float64 `json:"presence_penalty,omitempty"`
	RepetitionPenalty *float64 `json:"repetition_penalty,omitempty"`
}

// IsZero reports whether nothing is set — the default, today's behaviour.
func (s *AgentSampling) IsZero() bool {
	return s == nil || (s.Temperature == nil && s.TopP == nil && s.TopK == nil &&
		s.PresencePenalty == nil && s.RepetitionPenalty == nil)
}

// Summary renders the EFFECTIVE sampling of a call, in the order the wire
// carries it — what `calls[].sampling` publishes so a measurement can prove
// which policy a run actually used. A zero policy renders the default the
// client has always sent, never an empty string: "no sampling recorded" and
// "the default was used" must not read the same.
func (s *AgentSampling) Summary() string {
	if s.IsZero() {
		return "temperature=0"
	}
	parts := make([]string, 0, 5)
	if s.Temperature != nil {
		parts = append(parts, "temperature="+num(*s.Temperature))
	} else {
		parts = append(parts, "temperature=0")
	}
	if s.TopP != nil {
		parts = append(parts, "top_p="+num(*s.TopP))
	}
	if s.TopK != nil {
		parts = append(parts, "top_k="+strconv.Itoa(*s.TopK))
	}
	if s.PresencePenalty != nil {
		parts = append(parts, "presence_penalty="+num(*s.PresencePenalty))
	}
	if s.RepetitionPenalty != nil {
		parts = append(parts, "repetition_penalty="+num(*s.RepetitionPenalty))
	}
	return strings.Join(parts, " ")
}

// Validate refuses values no OpenAI-compatible backend accepts, at the config
// door rather than as a 400 in the middle of a delegated run.
func (s *AgentSampling) Validate(key string) error {
	if s == nil {
		return nil
	}
	if s.Temperature != nil && (*s.Temperature < 0 || *s.Temperature > 2) {
		return fmt.Errorf("%s.temperature %v: want 0..2", key, *s.Temperature)
	}
	if s.TopP != nil && (*s.TopP <= 0 || *s.TopP > 1) {
		return fmt.Errorf("%s.top_p %v: want >0..1", key, *s.TopP)
	}
	if s.TopK != nil && *s.TopK < 1 {
		return fmt.Errorf("%s.top_k %d: want >= 1", key, *s.TopK)
	}
	if s.PresencePenalty != nil && (*s.PresencePenalty < -2 || *s.PresencePenalty > 2) {
		return fmt.Errorf("%s.presence_penalty %v: want -2..2", key, *s.PresencePenalty)
	}
	if s.RepetitionPenalty != nil && *s.RepetitionPenalty <= 0 {
		return fmt.Errorf("%s.repetition_penalty %v: want > 0", key, *s.RepetitionPenalty)
	}
	return nil
}

// num renders a float the way an operator wrote it in config (0.7, not
// 0.700000) — the summary is read, not parsed.
func num(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
