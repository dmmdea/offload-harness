// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command (implemented scaffold).
// pp:data-source live

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The probe inputs are constants. A calibrated baseline is only meaningful
// against a byte-identical input, so these must never be parameterized.
const (
	verifyProbeEmbedText  = "llama-swap keep-set probe: the embedder must answer the same way tomorrow as it did today."
	verifyProbeRerankQ    = "Which document explains how the keep-set is protected?"
	verifyProbeRerankDocA = "The keep-set is read from the CLI's own config, never from the server's ttl field, and unload refuses its members."
	verifyProbeRerankDocB = "Sliding-window layers cap their KV cache at the window size instead of the full context length."
)

// verifyCosineFloor is the similarity a healthy embedder must clear against
// its stored baseline vector. An embedder restarted without --pooling mean
// still answers, still returns 768 numbers, and still looks healthy to a
// roster count - but the numbers are DIFFERENT. This floor is what catches
// that class.
const verifyCosineFloor = 0.999

// verifyRerankTolerance is the absolute score drift allowed for the reranker.
const verifyRerankTolerance = 0.05

type verifyKeepsetMember struct {
	Name      string  `json:"name"`
	Resolved  string  `json:"resolved_id,omitempty"`
	InRoster  bool    `json:"in_roster"`
	Loaded    bool    `json:"loaded"`
	Answered  bool    `json:"answered"`
	Call      string  `json:"call,omitempty"`
	LatencyMS float64 `json:"latency_ms,omitempty"`
	Detail    string  `json:"detail,omitempty"`
}

type verifySeatHealth struct {
	Model  string  `json:"model"`
	OK     bool    `json:"ok"`
	Status int     `json:"http_status,omitempty"`
	MS     float64 `json:"latency_ms"`
	Detail string  `json:"detail,omitempty"`
}

type verifyProbeResult struct {
	Kind       string   `json:"kind"`
	Model      string   `json:"model"`
	Mode       string   `json:"mode"`
	InputSHA   string   `json:"input_sha256"`
	Pass       bool     `json:"pass"`
	Measured   float64  `json:"measured"`
	Expected   float64  `json:"expected"`
	Threshold  float64  `json:"threshold"`
	Metric     string   `json:"metric"`
	Dims       int      `json:"dims,omitempty"`
	VectorSHA  string   `json:"vector_sha256,omitempty"`
	BaselineAt string   `json:"baseline_recorded_at,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	// Uninitialized marks a probe that could not assert because no baseline
	// exists yet. This is a setup gap, not a degradation — it must never be
	// reported with the DEGRADED exit code, or a fresh install looks like a
	// broken memory stack.
	Uninitialized bool `json:"uninitialized,omitempty"`
}

type verifyReport struct {
	SchemaVersion int    `json:"schema_version"`
	RanAt         string `json:"ran_at"`
	Mode          string `json:"mode"`

	RosterCount  int    `json:"roster_count,omitempty"`
	ExpectModels int    `json:"expect_models,omitempty"`
	RosterOK     *bool  `json:"roster_ok,omitempty"`
	RosterDetail string `json:"roster_detail,omitempty"`

	Keepset     []verifyKeepsetMember `json:"keepset,omitempty"`
	KeepsetOK   *bool                 `json:"keepset_ok,omitempty"`
	SeatHealth  []verifySeatHealth    `json:"probe_each,omitempty"`
	Probes      []verifyProbeResult   `json:"probes,omitempty"`
	ProbesOK    *bool                 `json:"probes_ok,omitempty"`
	LoadedSeats []string              `json:"loaded_seats"`
	Notes       []string              `json:"notes,omitempty"`
	Warnings    []string              `json:"warnings,omitempty"`
}

func newNovelVerifyCmd(flags *rootFlags) *cobra.Command {
	var (
		flagProbe       bool
		flagInit        bool
		flagExpect      int
		flagKeepset     string
		flagProbeEach   bool
		flagEmbedModel  string
		flagRerankModel string
		flagTolerance   float64
		flagCosineMin   float64
	)

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Post-restart verification: roster count, keep-set ANSWERING, and calibrated memory-stack probes",
		Long: `Two halves, both of which exist because counting was not enough.

Roster half (default):
  --expect-models N   assert /v1/models lists exactly N models
  --keepset a,b       assert each member is in /running AND answers a real
                      call - an embedder that is listed but returns an error
                      is a silent outage
  --probe-each        cheap health probe per LOADED seat (never unloaded ones:
                      /upstream auto-starts what it touches)

Probe half (--probe):
  --probe --init      embed a fixed string on the embedder and rerank a fixed
                      pair on the reranker, and record the results as the
                      calibrated baseline
  --probe             re-run them and assert cosine >= 0.999 against the stored
                      vector and |score delta| <= tolerance

The probe half catches the class roster counting cannot see: a model that
answers, but answers DIFFERENTLY - the embedder restarted without its pooling
flag being the canonical example. Exits 26 with the measured-vs-expected
numbers when that happens.`,
		Example: `  llamaswap-pp-cli verify --expect-models 15 --keepset embeddinggemma,bge-reranker-v2-m3 --probe-each
  llamaswap-pp-cli verify --probe --init
  llamaswap-pp-cli verify --probe --json`,
		Annotations: map[string]string{
			"mcp:read-only":        "true",
			"pp:typed-exit-codes":  "4=proxy unreachable, 24=roster count mismatch, 26=keep-set degraded or probe outside tolerance",
			"pp:measurement-owner": "wave-c",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "verify")
			}
			if err := validateDataSourceStrategy(flags, "live"); err != nil {
				return usageErr(err)
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			timeout := mcTimeout(cmd, flags, 2*time.Minute)

			doProbe := flagProbe || flagInit
			doRoster := !doProbe || flagExpect > 0 || flagKeepset != "" || flagProbeEach

			report := &verifyReport{SchemaVersion: 1, RanAt: mcNow()}
			switch {
			case doProbe && doRoster:
				report.Mode = "roster+probe"
			case doProbe:
				report.Mode = "probe"
			default:
				report.Mode = "roster"
			}

			roster, err := mcRoster(ctx, flags, timeout)
			if err != nil {
				return mcClassify(err)
			}
			seats, err := mcRunning(ctx, flags, timeout)
			if err != nil {
				return mcClassify(err)
			}
			report.LoadedSeats = mcLoadedNames(seats)

			extras := mcLoadExtras(flags)
			failedRoster, failedKeepset, failedProbe := false, false, false
			probeDegraded, probeUninitialized := false, false

			if doRoster {
				report.RosterCount, report.ExpectModels = len(roster), flagExpect
				if flagExpect > 0 {
					ok := len(roster) == flagExpect
					report.RosterOK = &ok
					if !ok {
						failedRoster = true
						report.RosterDetail = fmt.Sprintf("expected %d models, /v1/models lists %d", flagExpect, len(roster))
					} else {
						report.RosterDetail = fmt.Sprintf("%d models registered, as expected", len(roster))
					}
				} else {
					report.RosterDetail = fmt.Sprintf("%d models registered (no --expect-models assertion)", len(roster))
				}

				members := verifySplitCSV(flagKeepset)
				if len(members) == 0 {
					members = extras.KeepSet
					if len(members) > 0 {
						report.Notes = append(report.Notes, "keep-set taken from the CLI config (never from the server's ttl field, which reports 0 for ttl:-1 models)")
					}
				}
				if len(members) > 0 {
					ok := true
					for _, m := range members {
						row := verifyKeepsetCheck(ctx, flags, roster, seats, m, flagEmbedModel, flagRerankModel, timeout)
						if !row.Answered {
							ok = false
						}
						report.Keepset = append(report.Keepset, row)
					}
					report.KeepsetOK = &ok
					failedKeepset = !ok
				}

				if flagProbeEach {
					for _, seat := range seats {
						report.SeatHealth = append(report.SeatHealth, verifySeatProbe(ctx, flags, seat.Model, timeout))
					}
					report.Notes = append(report.Notes, "--probe-each covered LOADED seats only: /upstream auto-starts whatever it touches, so probing the roster would load it")
				}
			}

			if doProbe {
				results, warnings := verifyRunProbes(ctx, flags, roster, flagInit, flagEmbedModel, flagRerankModel, flagCosineMin, flagTolerance, timeout)
				report.Probes = results
				report.Warnings = append(report.Warnings, warnings...)
				ok := true
				for _, p := range results {
					if !p.Pass {
						ok = false
						if p.Uninitialized {
							probeUninitialized = true
						} else {
							probeDegraded = true
						}
					}
				}
				report.ProbesOK = &ok
				failedProbe = !ok
			}

			if err := mcEmit(cmd, flags, report, func(w io.Writer) { verifyPrint(w, report) }); err != nil {
				return err
			}
			switch {
			case failedProbe && probeDegraded:
				return &cliError{code: ExitProbeFailed, err: fmt.Errorf("%s", verifyProbeFailureSummary(report))}
			case failedProbe && probeUninitialized:
				// A missing baseline is a setup gap, never a degradation:
				// exit 24 (config-invalid class) with the setup command, so
				// a fresh install is distinguishable from a broken stack.
				return &cliError{code: ExitConfigInvalid, err: fmt.Errorf("probe baselines not initialized — run `verify --probe --init` first")}
			case failedKeepset:
				return &cliError{code: ExitProbeFailed, err: fmt.Errorf("a keep-set member is listed but not answering: %s", verifyKeepsetFailureSummary(report))}
			case failedRoster:
				return &cliError{code: ExitConfigInvalid, err: fmt.Errorf("%s", report.RosterDetail)}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagProbe, "probe", false, "Run the calibrated memory-stack probes against their stored baselines")
	cmd.Flags().BoolVar(&flagInit, "init", false, "With --probe: record the current results as the baseline instead of asserting against it")
	cmd.Flags().IntVar(&flagExpect, "expect-models", 0, "Assert /v1/models lists exactly this many models")
	cmd.Flags().StringVar(&flagKeepset, "keepset", "", "Comma-separated keep-set members that must be loaded AND answering (default: the CLI config's keep_set)")
	cmd.Flags().BoolVar(&flagProbeEach, "probe-each", false, "Health-probe every LOADED seat")
	cmd.Flags().StringVar(&flagEmbedModel, "embed-model", "embeddinggemma", "Embedding model id or alias used by the probes")
	cmd.Flags().StringVar(&flagRerankModel, "rerank-model", "bge-reranker-v2-m3", "Reranker model id or alias used by the probes")
	cmd.Flags().Float64Var(&flagTolerance, "tolerance", verifyRerankTolerance, "Absolute rerank-score drift allowed against the baseline")
	cmd.Flags().Float64Var(&flagCosineMin, "cosine-min", verifyCosineFloor, "Minimum cosine similarity against the baseline embedding")
	return cmd
}

func verifySplitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// verifyKeepsetCheck asserts presence AND a real answer. "Listed" is not
// "working": the outage this catches had the model in /running the whole time.
func verifyKeepsetCheck(ctx context.Context, flags *rootFlags, roster []mcRosterEntry, seats []mcSeat,
	name, embedModel, rerankModel string, timeout time.Duration) verifyKeepsetMember {

	row := verifyKeepsetMember{Name: name}
	resolved, known := mcResolveAlias(roster, name)
	row.InRoster, row.Resolved = known, resolved
	if !known {
		row.Detail = "not in /v1/models (checked against ids and aliases)"
		return row
	}
	_, loaded := mcFindSeat(seats, resolved)
	row.Loaded = loaded
	if !loaded {
		row.Detail = "in the roster but NOT resident: a keep-set member that is not loaded is already the outage"
		return row
	}

	embedID, _ := mcResolveAlias(roster, embedModel)
	rerankID, _ := mcResolveAlias(roster, rerankModel)
	start := time.Now()
	switch resolved {
	case rerankID:
		row.Call = "POST /v1/rerank"
		_, err := verifyRerank(ctx, flags, resolved, timeout)
		row.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			row.Detail = "rerank call failed: " + err.Error()
			return row
		}
	case embedID:
		row.Call = "POST /v1/embeddings"
		_, err := verifyEmbed(ctx, flags, resolved, verifyProbeEmbedText, timeout)
		row.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			row.Detail = "embedding call failed: " + err.Error()
			return row
		}
	default:
		// Not one of the two known-shape members: fall back to the upstream
		// health endpoint, which is honest about being weaker evidence.
		row.Call = "GET /upstream/" + resolved + "/health"
		h := verifySeatProbe(ctx, flags, resolved, timeout)
		row.LatencyMS = h.MS
		if !h.OK {
			row.Detail = "health probe failed: " + h.Detail
			return row
		}
		row.Detail = "answered a health probe (weaker evidence than a real inference call)"
	}
	row.Answered = true
	return row
}

func verifySeatProbe(ctx context.Context, flags *rootFlags, model string, timeout time.Duration) verifySeatHealth {
	out := verifySeatHealth{Model: model}
	start := time.Now()
	data, status, err := mcDo(ctx, flags, http.MethodGet, "/upstream/"+model+"/health", nil, timeout)
	out.MS = float64(time.Since(start).Microseconds()) / 1000
	out.Status = status
	if err != nil {
		out.Detail = err.Error()
		return out
	}
	out.OK = true
	out.Detail = strings.TrimSpace(truncate(string(data), 120))
	return out
}

func verifyEmbed(ctx context.Context, flags *rootFlags, model, text string, timeout time.Duration) ([]float64, error) {
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := mcPostJSON(ctx, flags, "/v1/embeddings", map[string]any{"model": model, "input": text}, timeout, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("the embedder returned no vector")
	}
	return out.Data[0].Embedding, nil
}

func verifyRerank(ctx context.Context, flags *rootFlags, model string, timeout time.Duration) (float64, error) {
	body := map[string]any{
		"model":     model,
		"query":     verifyProbeRerankQ,
		"documents": []string{verifyProbeRerankDocA, verifyProbeRerankDocB},
		"top_n":     2,
	}
	var out struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	err := mcPostJSON(ctx, flags, "/v1/rerank", body, timeout, &out)
	if err != nil {
		var he *mcHTTPError
		if As(err, &he) && he.Status == http.StatusNotFound {
			err = mcPostJSON(ctx, flags, "/rerank", body, timeout, &out)
		}
		if err != nil {
			return 0, err
		}
	}
	for _, r := range out.Results {
		if r.Index == 0 {
			return r.RelevanceScore, nil
		}
	}
	if len(out.Results) > 0 {
		return out.Results[0].RelevanceScore, nil
	}
	return 0, fmt.Errorf("the reranker returned no results")
}

func verifyRunProbes(ctx context.Context, flags *rootFlags, roster []mcRosterEntry, initMode bool,
	embedModel, rerankModel string, cosineMin, tolerance float64, timeout time.Duration) ([]verifyProbeResult, []string) {

	var results []verifyProbeResult
	var warnings []string

	embedID, known := mcResolveAlias(roster, embedModel)
	if !known {
		warnings = append(warnings, fmt.Sprintf("embed probe skipped: %q is not in the roster", embedModel))
	} else {
		results = append(results, verifyEmbedProbe(ctx, flags, embedID, initMode, cosineMin, timeout))
	}
	rerankID, known := mcResolveAlias(roster, rerankModel)
	if !known {
		warnings = append(warnings, fmt.Sprintf("rerank probe skipped: %q is not in the roster", rerankModel))
	} else {
		results = append(results, verifyRerankProbe(ctx, flags, rerankID, initMode, tolerance, timeout))
	}
	return results, warnings
}

type verifyEmbedBaseline struct {
	Dims      int       `json:"dims"`
	VectorSHA string    `json:"vector_sha256"`
	Vector    []float64 `json:"vector"`
}

type verifyRerankBaseline struct {
	Score float64 `json:"score"`
	Index int     `json:"index"`
}

func verifyEmbedProbe(ctx context.Context, flags *rootFlags, model string, initMode bool, cosineMin float64, timeout time.Duration) verifyProbeResult {
	res := verifyProbeResult{
		Kind: "embed", Model: model, Metric: "cosine similarity vs stored baseline vector",
		Threshold: cosineMin, InputSHA: verifySHA(verifyProbeEmbedText), Mode: "assert",
	}
	if initMode {
		res.Mode = "init"
	}
	vec, err := verifyEmbed(ctx, flags, model, verifyProbeEmbedText, timeout)
	if err != nil {
		res.Detail = "embedding call failed: " + err.Error()
		return res
	}
	res.Dims = len(vec)
	res.VectorSHA = verifyVectorSHA(vec)

	if initMode {
		payload := verifyEmbedBaseline{Dims: len(vec), VectorSHA: res.VectorSHA, Vector: vec}
		if err := verifyStoreBaseline(ctx, "embed", model, res.InputSHA, payload, cosineMin); err != nil {
			res.Detail = "baseline not stored: " + err.Error()
			return res
		}
		res.Pass, res.Measured, res.Expected = true, 1, 1
		res.Detail = fmt.Sprintf("baseline recorded: %d dims, vector sha256 %s", len(vec), res.VectorSHA[:16])
		return res
	}

	var stored verifyEmbedBaseline
	createdAt, err := verifyLoadBaseline(ctx, "embed", model, res.InputSHA, &stored)
	if err != nil {
		res.Uninitialized = true
		res.Detail = "no baseline on file for this model+input; run `verify --probe --init` first (" + err.Error() + ")"
		return res
	}
	res.BaselineAt = createdAt
	if len(stored.Vector) != len(vec) {
		res.Detail = fmt.Sprintf("DIMENSION CHANGE: baseline has %d dims, the model now returns %d", len(stored.Vector), len(vec))
		return res
	}
	cos := verifyCosine(stored.Vector, vec)
	res.Measured, res.Expected = cos, 1
	res.Pass = cos >= cosineMin
	if res.Pass {
		res.Detail = fmt.Sprintf("cosine %.6f vs baseline (floor %.4f); vector sha %s", cos, cosineMin, res.VectorSHA[:16])
	} else {
		res.Detail = fmt.Sprintf("DEGRADED: cosine %.6f is below the %.4f floor. The model answered, but not the same way it did at baseline "+
			"(%s) - the dropped-pooling-flag class. Baseline vector sha %s, now %s",
			cos, cosineMin, createdAt, stored.VectorSHA[:16], res.VectorSHA[:16])
	}
	return res
}

func verifyRerankProbe(ctx context.Context, flags *rootFlags, model string, initMode bool, tolerance float64, timeout time.Duration) verifyProbeResult {
	res := verifyProbeResult{
		Kind: "rerank", Model: model, Metric: "absolute score delta vs stored baseline",
		Threshold: tolerance, InputSHA: verifySHA(verifyProbeRerankQ + "\x00" + verifyProbeRerankDocA + "\x00" + verifyProbeRerankDocB),
		Mode: "assert",
	}
	if initMode {
		res.Mode = "init"
	}
	score, err := verifyRerank(ctx, flags, model, timeout)
	if err != nil {
		res.Detail = "rerank call failed: " + err.Error()
		return res
	}
	res.Measured = score

	if initMode {
		if err := verifyStoreBaseline(ctx, "rerank", model, res.InputSHA, verifyRerankBaseline{Score: score, Index: 0}, tolerance); err != nil {
			res.Detail = "baseline not stored: " + err.Error()
			return res
		}
		res.Pass, res.Expected = true, score
		res.Detail = fmt.Sprintf("baseline recorded: score %.6f for document 0", score)
		return res
	}

	var stored verifyRerankBaseline
	createdAt, err := verifyLoadBaseline(ctx, "rerank", model, res.InputSHA, &stored)
	if err != nil {
		res.Uninitialized = true
		res.Detail = "no baseline on file for this model+input; run `verify --probe --init` first (" + err.Error() + ")"
		return res
	}
	res.BaselineAt, res.Expected = createdAt, stored.Score
	delta := math.Abs(score - stored.Score)
	res.Pass = delta <= tolerance
	if res.Pass {
		res.Detail = fmt.Sprintf("score %.6f vs baseline %.6f (delta %.6f, tolerance %.4f)", score, stored.Score, delta, tolerance)
	} else {
		res.Detail = fmt.Sprintf("DEGRADED: score %.6f vs baseline %.6f recorded %s - delta %.6f exceeds tolerance %.4f",
			score, stored.Score, createdAt, delta, tolerance)
	}
	return res
}

func verifyStoreBaseline(ctx context.Context, kind, model, inputSHA string, payload any, tolerance float64) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s, err := mcOpenDomainStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	_, err = s.DB().ExecContext(ctx,
		`INSERT INTO probe_baselines (kind, model, input_sha, expected, tolerance, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(kind, model, input_sha) DO UPDATE SET
		   expected = excluded.expected, tolerance = excluded.tolerance, created_at = excluded.created_at`,
		kind, model, inputSHA, string(raw), tolerance, mcNow())
	return err
}

func verifyLoadBaseline(ctx context.Context, kind, model, inputSHA string, out any) (string, error) {
	s, err := mcOpenDomainStore(ctx)
	if err != nil {
		return "", err
	}
	defer s.Close()
	var expected, createdAt string
	err = s.DB().QueryRowContext(ctx,
		`SELECT expected, created_at FROM probe_baselines WHERE kind = ? AND model = ? AND input_sha = ?`,
		kind, model, inputSHA).Scan(&expected, &createdAt)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal([]byte(expected), out); err != nil {
		return createdAt, err
	}
	return createdAt, nil
}

func verifyCosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func verifySHA(s string) string { return mcSHA256(s) }

// verifyVectorSHA hashes a canonical fixed-precision serialization of the
// vector. Fixed precision on purpose: a float printed at full precision
// differs across builds for reasons that have nothing to do with the model.
func verifyVectorSHA(v []float64) string {
	h := sha256.New()
	for _, f := range v {
		fmt.Fprintf(h, "%.6f;", f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func verifyProbeFailureSummary(r *verifyReport) string {
	var parts []string
	for _, p := range r.Probes {
		if !p.Pass {
			parts = append(parts, fmt.Sprintf("%s/%s: measured %.6f, expected %.6f (%s)", p.Kind, p.Model, p.Measured, p.Expected, p.Detail))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func verifyKeepsetFailureSummary(r *verifyReport) string {
	var parts []string
	for _, m := range r.Keepset {
		if !m.Answered {
			parts = append(parts, fmt.Sprintf("%s (%s)", m.Name, m.Detail))
		}
	}
	return strings.Join(parts, "; ")
}

func verifyPrint(w io.Writer, r *verifyReport) {
	fmt.Fprintf(w, "%s  (%s, %s)\n", bold("verify"), r.Mode, r.RanAt)
	if r.RosterDetail != "" {
		mark := " "
		if r.RosterOK != nil {
			mark = verifyMark(*r.RosterOK)
		}
		fmt.Fprintf(w, "  %s roster        %s\n", mark, r.RosterDetail)
	}
	for _, m := range r.Keepset {
		fmt.Fprintf(w, "  %s keep-set      %-24s loaded=%v answered=%v %s %s\n",
			verifyMark(m.Answered), m.Name, m.Loaded, m.Answered, m.Call, m.Detail)
	}
	for _, h := range r.SeatHealth {
		fmt.Fprintf(w, "  %s seat health   %-24s %.0f ms %s\n", verifyMark(h.OK), h.Model, h.MS, truncate(h.Detail, 60))
	}
	for _, p := range r.Probes {
		fmt.Fprintf(w, "  %s probe %-7s %-24s %s\n", verifyMark(p.Pass), p.Kind, p.Model, p.Detail)
	}
	if len(r.LoadedSeats) > 0 {
		fmt.Fprintf(w, "  loaded seats    %s\n", strings.Join(r.LoadedSeats, ", "))
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note            %s\n", n)
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "  %s %s\n", yellow("warning:"), warn)
	}
}

func verifyMark(ok bool) string {
	if ok {
		return green("PASS")
	}
	return red("FAIL")
}
