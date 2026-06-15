// Package config loads harness configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// Config controls endpoint, model, thresholds, and storage paths.
type Config struct {
	// Endpoint is the llama-server base URL (e.g. http://127.0.0.1:11436).
	Endpoint string `json:"endpoint"`
	// CompletionPath is the native completion route used to pass a GBNF grammar.
	CompletionPath string `json:"completion_path"`
	// Model is the default workhorse (E4B) — used for summarize/extract and as
	// the fallback for any task without a specific route. Empty = dedicated server.
	Model string `json:"model"`
	// TriageModel serves the fast tier (triage/classify). Empty = use Model.
	// NOTE on 8GB: the tiers are swap-exclusive, so routing a few triages to a
	// cold E2B costs a model swap; for latency-critical mixed workloads set this
	// to "" (let E4B handle triage) or batch same-tier calls.
	TriageModel string `json:"triage_model"`
	// EscalationModel is tried once when the primary defers (validation fail / low
	// confidence) BEFORE deferring to Opus. Empty = no escalation (defer directly).
	EscalationModel string `json:"escalation_model"`
	// VisionModel is the VLM alias used for the vqa task (multimodal). Empty = no
	// vision route (vqa defers).
	VisionModel string `json:"vision_model,omitempty"`
	// VisionMaxImageBytes caps a single decoded image before it is rejected
	// (guards context/VRAM blowups). 0 = use the loader default.
	VisionMaxImageBytes int `json:"vision_max_image_bytes,omitempty"`
	// Temperature for deterministic structured output (default 0).
	Temperature float64 `json:"temperature"`
	// MaxRetries is how many correction re-prompts before deferring.
	MaxRetries int `json:"max_retries"`
	// ClassifyMinConfidence: classify results below this (self-reported) defer.
	ClassifyMinConfidence float64 `json:"classify_min_confidence"`
	// ConfidenceMarginThreshold: for triage/classify, if the logprob-derived
	// top-2 legal-class margin at the decision token is below this, escalate to a
	// larger tier (catches genuinely torn calls, e.g. eager-YES). 0 disables the
	// logprob gate. Default 0.35.
	ConfidenceMarginThreshold float64 `json:"confidence_margin_threshold"`
	// MaxInputChars caps input length before context-budget trimming.
	MaxInputChars int `json:"max_input_chars"`
	// CachePath / LedgerPath are bbolt files.
	CachePath  string `json:"cache_path"`
	LedgerPath string `json:"ledger_path"`
	// --- self-learning artifacts (written by the offline `calibrate`/`health`/
	// `train-router`/`optimize` jobs; read at pipeline startup). Empty = feature off. ---
	ThresholdsPath    string             `json:"thresholds_path,omitempty"`     // Phase 2: per-task conformal margin thresholds
	TierOverridesPath string             `json:"tier_overrides_path,omitempty"` // Phase 4: health-driven entry-tier bumps + P95 timeouts
	RouterWeightsPath string             `json:"router_weights_path,omitempty"` // Phase 5: logistic entry-tier router
	ConfHeadPath           string         `json:"confhead_path,omitempty"`            // Phase 2: logistic correctness head
	ConfHeadLabelsPath     string         `json:"confhead_labels_path,omitempty"`     // Phase 2: cascade-agreement correctness-label sidecar (classify/triage)
	ConfHeadThresholdsPath string         `json:"confhead_thresholds_path,omitempty"` // Phase 2: per-task conformal p(correct) escalation thresholds (Task 3)
	ConfHeadEnabled        bool           `json:"confhead_enabled,omitempty"`         // Phase 2 Task 4: opt-in — gate ADOPT tasks on the head's p(correct). Default false.
	ExemplarsDir      string             `json:"exemplars_dir,omitempty"`       // Phase 6: few-shot exemplar sidecar + selected pool
	ExemplarShots     int                `json:"exemplar_shots,omitempty"`      // Phase 6: 0 = disabled
	AutoHeal          bool               `json:"auto_heal,omitempty"`           // Phase 7: opt-in autonomous tier reload
	TargetErrorRate   map[string]float64 `json:"target_error_rate,omitempty"`   // Phase 2: per-task α for calibration
	// OpusInputPricePerMTok estimates dollar savings ($ per 1M input tokens).
	OpusInputPricePerMTok float64 `json:"opus_input_price_per_mtok"`
	// RequestTimeoutSec for a single model call.
	RequestTimeoutSec int `json:"request_timeout_sec"`
}

// Default returns a config suitable for the verified E4B-QAT+MTP setup.
func Default() Config {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".local-offload")
	return Config{
		Endpoint:              "http://127.0.0.1:11436",
		CompletionPath:        "/v1/chat/completions", // chat route: server applies the Gemma template; we pass a raw "grammar" field
		Model:                 "offload-e4b",
		TriageModel:           "gemma4-e2b",     // fast tier for triage/classify
		EscalationModel:       "gemma4-26b-a4b", // near-frontier MoE, tried before defer-to-Opus
		VisionModel:           "qwen3vl-4b",     // VLM for the vqa task
		VisionMaxImageBytes:   6000000,          // ~6MB cap per image
		Temperature:               0,
		MaxRetries:                1,
		ClassifyMinConfidence:     0.45,
		ConfidenceMarginThreshold: 0.35,
		MaxInputChars:         24000, // ~6k tokens, well under ctx 8192
		CachePath:             filepath.Join(base, "cache.db"),
		LedgerPath:            filepath.Join(base, "ledger.jsonl"), // append-only JSONL (concurrent read/append)
		ThresholdsPath:        filepath.Join(base, "thresholds.json"),
		TierOverridesPath:     filepath.Join(base, "tier_overrides.json"),
		RouterWeightsPath:     filepath.Join(base, "router-weights.json"),
		ConfHeadPath:           filepath.Join(base, "confhead-weights.json"),
		ConfHeadLabelsPath:     filepath.Join(base, "confhead-labels.jsonl"),
		ConfHeadThresholdsPath: filepath.Join(base, "confhead-thresholds.json"),
		ExemplarsDir:          filepath.Join(base, "exemplars"),
		ExemplarShots:         0, // off until the pool is built + measured
		AutoHeal:              false,
		OpusInputPricePerMTok: 15.0,
		RequestTimeoutSec:     120,
	}
}

// Load merges a JSON config file over the defaults. Missing file => defaults.
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	warnUnknownKeys(b)
	return c, nil
}

// warnUnknownKeys prints a stderr warning for any JSON key that doesn't map to a
// Config field — so a typo like "escalaton_model" surfaces instead of being
// silently ignored. It never fails: the valid fields still load.
func warnUnknownKeys(b []byte) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return
	}
	known := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		name := strings.SplitN(t.Field(i).Tag.Get("json"), ",", 2)[0]
		if name != "" && name != "-" {
			known[name] = true
		}
	}
	for k := range raw {
		if !known[k] {
			fmt.Fprintf(os.Stderr, "warning: unknown config key %q (ignored — typo?)\n", k)
		}
	}
}

// EnsureDirs creates the parent dirs for the store files.
func (c Config) EnsureDirs() error {
	for _, p := range []string{c.CachePath, c.LedgerPath, c.ThresholdsPath, c.RouterWeightsPath, c.TierOverridesPath, c.ConfHeadPath, c.ConfHeadLabelsPath, c.ConfHeadThresholdsPath} {
		if p == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
	}
	if c.ExemplarsDir != "" { // a directory, not a file
		if err := os.MkdirAll(c.ExemplarsDir, 0o755); err != nil {
			return err
		}
	}
	return nil
}
