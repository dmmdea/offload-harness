// props.go — the SERVER-SIDE half of experiment config pinning (A1, Tier 2 of
// the 2026-08-21 Phase 2 re-aim). ProbeSeatPin reduces a seat's live
// llama-server /props answer to a stable hash + a human-readable basis, so a
// paired cross-seat run can REFUSE to compare rows produced under different
// serving configs.
//
// Scope honesty — what this pin does and does not buy: it detects drift in
// the SERVED state (weights file, quant, build, context window, server-side
// sampler defaults, chat template). It cannot see the harness's own request
// construction (per-call temperature, the re-pack's enable_thinking:false,
// profile toolsets) — that half is code, pinned separately by
// buildinfo.Version + buildinfo.BuildSHA256 on the same wire result. The
// defect class that invalidated the 2026-08-17 corpus (enable_thinking/GBNF
// empty content) lived on the REQUEST side; naming this hash alone as the fix
// for that class would be false, which is why both halves ship together.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// SeatPin is a seat's serving-config fingerprint. SHA256 is what pairing
// logic compares; Basis is the short human-readable summary an operator reads
// when two hashes differ ("what changed?").
type SeatPin struct {
	SHA256 string
	Basis  string
}

// seatPinBasis is the CLOSED field set the hash is computed over, extracted
// from /props. Closed and named — hashing the raw /props body would churn the
// pin on fields that cannot affect an answer (slot state, ui settings,
// timings) and on key-order accidents. Struct field order fixes the canonical
// JSON, so the hash is stable by construction.
//
// `seed` is deliberately ABSENT: it is a per-request quantity (the server
// merely reports a default the harness always overrides), and pinning it
// would make two identical configs unpairable over a value that never
// governed a run. Sampler defaults ARE included even though the agent loop
// overrides temperature per request — for the lanes that DON'T override, a
// changed server default is exactly the drift this exists to catch; for the
// lanes that do, the override is code, pinned by the build hash.
type seatPinBasis struct {
	BuildInfo          string          `json:"build_info"`
	ModelPath          string          `json:"model_path"`
	ModelFtype         string          `json:"model_ftype"`
	NCtx               int             `json:"n_ctx"`
	TotalSlots         int             `json:"total_slots"`
	Temperature        float64         `json:"temperature"`
	TopK               int             `json:"top_k"`
	TopP               float64         `json:"top_p"`
	MinP               float64         `json:"min_p"`
	ReasoningFormat    string          `json:"reasoning_format"`
	ReasoningInContent bool            `json:"reasoning_in_content"`
	ChatFormat         string          `json:"chat_format"`
	Samplers           []string        `json:"samplers"`
	ChatTemplateSHA256 string          `json:"chat_template_sha256"`
	Modalities         map[string]bool `json:"modalities"`
}

// seatPinClient: short timeout ON PURPOSE. The probe runs right after a run
// completed, when the seat is resident and /props answers in milliseconds. If
// the seat was evicted in between, llama-swap would COLD-START it to answer —
// minutes of load as a telemetry side effect — so the probe gives up long
// before that completes and the pin stays absent (unknown), which is the
// honest outcome for "the state I meant to pin is already gone".
var seatPinClient = &http.Client{Timeout: 3 * time.Second}

// ProbeSeatPin GETs the llama-swap per-model /props passthrough (upstream
// only — the bare-root /props answers for whatever model happens to be
// loaded, the exact mis-attribution ProbeUpstreamWindow already refuses) and
// returns the seat's pin. ok=false on any transport, status, or decode
// failure: a pin is evidence, and a guess is worse than an absence.
func ProbeSeatPin(ctx context.Context, base, model string) (SeatPin, bool) {
	b := swapclient.BaseURL(base)
	if b == "" {
		return SeatPin{}, false
	}
	u := b + "/upstream/" + url.PathEscape(model) + "/props"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return SeatPin{}, false
	}
	resp, err := seatPinClient.Do(req)
	if err != nil {
		return SeatPin{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SeatPin{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return SeatPin{}, false
	}
	var payload struct {
		DGS struct {
			NCtx   int `json:"n_ctx"`
			Params struct {
				Temperature        float64  `json:"temperature"`
				TopK               int      `json:"top_k"`
				TopP               float64  `json:"top_p"`
				MinP               float64  `json:"min_p"`
				ReasoningFormat    string   `json:"reasoning_format"`
				ReasoningInContent bool     `json:"reasoning_in_content"`
				ChatFormat         string   `json:"chat_format"`
				Samplers           []string `json:"samplers"`
			} `json:"params"`
		} `json:"default_generation_settings"`
		TotalSlots   int             `json:"total_slots"`
		ModelPath    string          `json:"model_path"`
		ModelFtype   string          `json:"model_ftype"`
		BuildInfo    string          `json:"build_info"`
		ChatTemplate string          `json:"chat_template"`
		Modalities   map[string]bool `json:"modalities"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return SeatPin{}, false
	}
	// A llama-swap error envelope ({"error":...,"src":"llama-swap"}) decodes
	// cleanly into all-zero fields. n_ctx==0 is impossible on a real answer
	// (every server has a context), so it is the discriminator between "the
	// seat answered" and "something answered with a shape that is not props".
	if payload.DGS.NCtx <= 0 {
		return SeatPin{}, false
	}
	// build_info and chat_template are REQUIRED discriminators, not optional
	// decoration: an answer missing either (an older llama.cpp build, a proxy
	// mangling the payload) would hash the empty string, and two DIFFERENT
	// builds both missing the field would then produce the SAME pin — a
	// produced value that is wrong, the worse failure mode. No pin beats a
	// pin that can falsely say "same config". Every seat on this fleet
	// reports both (verified live on both node classes).
	// build_info and chat_template are REQUIRED discriminators, not optional
	// decoration: an answer missing either (an older llama.cpp build, a proxy
	// mangling the payload) would hash the empty string, and two DIFFERENT
	// builds both missing the field would then produce the SAME pin — a
	// produced value that is wrong, the worse failure mode. No pin beats a
	// pin that can falsely say "same config". Every seat on this fleet
	// reports both (verified live on both node classes).
	if payload.BuildInfo == "" || payload.ChatTemplate == "" {
		return SeatPin{}, false
	}

	tmplSHA := ""
	if payload.ChatTemplate != "" {
		s := sha256.Sum256([]byte(payload.ChatTemplate))
		tmplSHA = hex.EncodeToString(s[:])
	}
	basis := seatPinBasis{
		BuildInfo:          payload.BuildInfo,
		ModelPath:          payload.ModelPath,
		ModelFtype:         payload.ModelFtype,
		NCtx:               payload.DGS.NCtx,
		TotalSlots:         payload.TotalSlots,
		Temperature:        payload.DGS.Params.Temperature,
		TopK:               payload.DGS.Params.TopK,
		TopP:               payload.DGS.Params.TopP,
		MinP:               payload.DGS.Params.MinP,
		ReasoningFormat:    payload.DGS.Params.ReasoningFormat,
		ReasoningInContent: payload.DGS.Params.ReasoningInContent,
		ChatFormat:         payload.DGS.Params.ChatFormat,
		Samplers:           payload.DGS.Params.Samplers,
		ChatTemplateSHA256: tmplSHA,
		Modalities:         payload.Modalities,
	}
	canonical, err := json.Marshal(basis) // maps marshal key-sorted; struct order is fixed
	if err != nil {
		return SeatPin{}, false
	}
	sum := sha256.Sum256(canonical)

	short := tmplSHA
	if len(short) > 8 {
		short = short[:8]
	}
	return SeatPin{
		SHA256: hex.EncodeToString(sum[:]),
		Basis: strings.TrimSpace(fmt.Sprintf("%s %s %s n_ctx=%d slots=%d temp=%g top_k=%d top_p=%g min_p=%g rf=%s tmpl=%s",
			basis.BuildInfo, path.Base(strings.ReplaceAll(basis.ModelPath, `\`, "/")), basis.ModelFtype,
			basis.NCtx, basis.TotalSlots, basis.Temperature, basis.TopK, basis.TopP, basis.MinP,
			orUnset(basis.ReasoningFormat), orUnset(short))),
	}, true
}

// orUnset keeps the basis line grep-friendly when a field is absent on an
// older llama.cpp build — "rf=" with nothing after it reads as a typo.
func orUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}
