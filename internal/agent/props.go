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
	"sort"
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
//
// Known limitation (reviewed, accepted): the 3s timeout bounds the CLIENT'S
// wait; if an external eviction races the probe on a shared box, llama-swap
// may continue the spawn server-side after this client disconnects. The
// residual effect is a warm seat nobody asked for — one load's GPU-seconds,
// no wrong data — and the race needs a concurrent eviction inside the
// milliseconds between a run finishing and its probe, which the 5-min-TTL
// eviction policy makes narrow. This client cannot prove llama-swap's
// disconnect handling either way; claiming more would be asserting another
// process's behavior from this one's timeout.
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
	// ONE deadline for the WHOLE probe, not one per HTTP call. seatPinClient's
	// timeout bounds a single Do(), so once this function could issue more than
	// one request (the vLLM fallback below) the file's stated contract — pinning
	// never costs more than seatPinClient.Timeout — would have quietly become
	// "up to 3x that", on the SYNCHRONOUS path that gates every agent task's
	// returned result (pipeline/agenttask.go). A shared deadline keeps the
	// promise for both engines: the llama.cpp path is unchanged (one call, same
	// bound) and the vLLM path spends the SAME budget across its calls instead
	// of a fresh one each. A caller's own shorter deadline still wins.
	ctx, cancel := context.WithTimeout(ctx, seatPinClient.Timeout)
	defer cancel()
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
		// /props is llama.cpp's endpoint; vLLM does not serve it and answers
		// 404 (verified live against vLLM 0.28.0, 2026-09-08). Before this
		// fallback that made seat_config_* permanently ABSENT on every vLLM
		// seat, which is not a cosmetic gap: an unpinned row cannot enter a
		// paired experiment, so two vLLM arms could not be told apart by the
		// record at all. Measured consequence: a 4B-vs-9B seat comparison on
		// the A2 was published with the model, the engine build and the served
		// window all unrecorded, and the conclusion rested on arms that
		// differed in ways nothing captured.
		//
		// ONLY 404 falls through. A llama.cpp seat answering 500/503 — mid
		// crash, overloaded, evicted — is a llama.cpp seat having a bad
		// moment, not a vLLM seat: chasing it with two more GETs would spend
		// this probe's budget on a request path that cannot answer, on exactly
		// the run where the seat is already struggling. Every non-404 keeps
		// the pre-existing behaviour, one request and an honest absent pin.
		if resp.StatusCode == http.StatusNotFound {
			return probeVLLMSeatPin(ctx, b, model)
		}
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
	// The basis names EVERY hashed field (review finding: a basis that omits
	// hashed fields lets two configs show different hashes over an identical
	// basis line, defeating its "what changed?" purpose).
	mods := make([]string, 0, len(basis.Modalities))
	for k, v := range basis.Modalities {
		if v {
			mods = append(mods, k)
		}
	}
	sort.Strings(mods)
	return SeatPin{
		SHA256: hex.EncodeToString(sum[:]),
		Basis: strings.TrimSpace(fmt.Sprintf("%s %s %s n_ctx=%d slots=%d temp=%g top_k=%d top_p=%g min_p=%g rf=%s ric=%t cf=%s smp=%s mod=%s tmpl=%s",
			basis.BuildInfo, path.Base(strings.ReplaceAll(basis.ModelPath, `\`, "/")), basis.ModelFtype,
			basis.NCtx, basis.TotalSlots, basis.Temperature, basis.TopK, basis.TopP, basis.MinP,
			orUnset(basis.ReasoningFormat), basis.ReasoningInContent, orUnset(basis.ChatFormat),
			orUnset(strings.Join(basis.Samplers, ",")), orUnset(strings.Join(mods, ",")), orUnset(short))),
	}, true
}

// vllmPinBasis is the CLOSED field set a vLLM seat's pin is hashed over. It is
// a SEPARATE struct from seatPinBasis on purpose: the two engines publish
// disjoint metadata, and reusing seatPinBasis would hash a dozen zero values
// for every vLLM seat — which is precisely how two different configs come to
// show the same hash. `Engine` leads the struct so a vLLM pin can never
// collide with a llama.cpp one even if every other value coincided.
//
// What this pin CANNOT see, stated because a pin is only worth its honesty:
// vLLM publishes no sampler defaults and no --tool-call-parser /
// --reasoning-parser / --gpu-memory-utilization values on ANY endpoint, so two
// seats differing only in those pin identically. Callers must not read a
// matching vllm pin as "identical serving config" — it means "same engine
// version, same weights, same window". Those three are the ones that silently
// differed in the mispaired A2 comparison this fallback exists to prevent.
type vllmPinBasis struct {
	Engine      string `json:"engine"`
	Version     string `json:"version"`
	ModelRoot   string `json:"model_root"`
	ServedName  string `json:"served_name"`
	MaxModelLen int    `json:"max_model_len"`
}

// probeVLLMSeatPin builds a seat pin from the two metadata endpoints a vLLM
// OpenAI server does serve: GET /version (engine build) and GET /v1/models
// (the resolved checkpoint path in `root`, plus `max_model_len`). Verified
// live against vLLM 0.28.0 on 2026-09-08: /props 404s, both of these answer
// 200.
//
// It applies the same refusal discipline as the llama.cpp path — every
// discriminator is REQUIRED, and a missing one yields no pin rather than a pin
// hashed over empty strings. ok=false on any transport, status or decode
// failure.
func probeVLLMSeatPin(ctx context.Context, b, model string) (SeatPin, bool) {
	up := b + "/upstream/" + url.PathEscape(model)

	get := func(path string) ([]byte, bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, up+path, nil)
		if err != nil {
			return nil, false
		}
		resp, err := seatPinClient.Do(req)
		if err != nil {
			return nil, false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, false
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return nil, false
		}
		return body, true
	}

	verBody, ok := get("/version")
	if !ok {
		return SeatPin{}, false
	}
	var ver struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(verBody, &ver); err != nil || ver.Version == "" {
		return SeatPin{}, false
	}

	modBody, ok := get("/v1/models")
	if !ok {
		return SeatPin{}, false
	}
	var models struct {
		Data []struct {
			ID          string `json:"id"`
			Root        string `json:"root"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.Unmarshal(modBody, &models); err != nil || len(models.Data) == 0 {
		return SeatPin{}, false
	}
	// Pick the entry this seat actually serves. A vLLM server started with
	// several --served-model-name aliases lists one entry per alias, so
	// matching by name keeps the pin attributed to the name the run used;
	// falling back to the sole entry covers a seat whose llama-swap alias
	// differs from its vLLM served name. With several entries and no match,
	// refuse rather than pin an arbitrary one.
	idx := -1
	for i, m := range models.Data {
		if strings.EqualFold(m.ID, model) {
			idx = i
			break
		}
	}
	if idx < 0 {
		if len(models.Data) != 1 {
			return SeatPin{}, false
		}
		idx = 0
	}
	m := models.Data[idx]
	if m.Root == "" || m.MaxModelLen <= 0 {
		return SeatPin{}, false
	}

	basis := vllmPinBasis{
		Engine:      "vllm",
		Version:     ver.Version,
		ModelRoot:   m.Root,
		ServedName:  m.ID,
		MaxModelLen: m.MaxModelLen,
	}
	canonical, err := json.Marshal(basis)
	if err != nil {
		return SeatPin{}, false
	}
	sum := sha256.Sum256(canonical)
	return SeatPin{
		SHA256: hex.EncodeToString(sum[:]),
		Basis: strings.TrimSpace(fmt.Sprintf("vllm %s %s served=%s max_model_len=%d",
			basis.Version, path.Base(strings.ReplaceAll(basis.ModelRoot, `\`, "/")),
			basis.ServedName, basis.MaxModelLen)),
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
