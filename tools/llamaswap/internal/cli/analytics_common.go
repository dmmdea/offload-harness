// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Novel analytics support (wave D): shared primitives for the residency and
// saturation verbs. Both read the local mirror ONLY and print — they never edit
// groups, ttl, or placement.
//
// pp:data-source local
//
// Why a shared file: residency and saturation both reconstruct request timing
// and both must state their coverage the same way. Duplicating either would let
// the two verbs quietly disagree about the same slice of history (the roast
// Expansionist's point). The interval-direction fact below is the load-bearing
// one: it was WRONG in the first design and is pinned here once.

package cli

import (
	"context"
	"database/sql"
	"os"
	"time"

	"llamaswap-pp-cli/internal/mirror"
	"llamaswap-pp-cli/internal/store"
)

// yamlConfigPath resolves the llama-swap config the same way the keep-set loader
// and ps do: the LLAMASWAP_YAML env var, then the documented default if it
// exists, else empty (caller renders "unknown", never a guess).
func yamlConfigPath() string {
	if p := os.Getenv(mirror.EnvYAMLPath); p != "" {
		return p
	}
	if _, err := os.Stat(mirror.DefaultYAMLPath); err == nil {
		return mirror.DefaultYAMLPath
	}
	return ""
}

// analyticsSchemaVersion is the wire version for the residency/saturation JSON.
// Separate from spineSchemaVersion (swaps/keepset): these verbs can evolve their
// shape without forcing a bump on the older reports.
const analyticsSchemaVersion = 1

// mirrorRequest is one mirrored request row, already time-parsed.
//
// TS is the request's COMPLETION time and DurationMS spans dispatch→completion
// (verified against llama-swap upstream source: metrics are recorded AFTER
// next.ServeHTTP returns, and the duration timer starts just before dispatch —
// so a cold load is included). The request therefore occupied the seat over the
// half-open interval [TS - DurationMS, TS], NOT [TS, TS + DurationMS]. Every
// consumer must use Start()/End() below rather than re-deriving the interval, so
// the direction is decided in exactly one place.
type mirrorRequest struct {
	EpochID  int64
	Model    string
	TS       time.Time
	Duration time.Duration
	Status   int
	ReqPath  string
	InTok    sql.NullInt64
	OutTok   sql.NullInt64
}

// Start is the reconstructed dispatch time. End is the completion time (TS).
func (r mirrorRequest) Start() time.Time { return r.TS.Add(-r.Duration) }
func (r mirrorRequest) End() time.Time   { return r.TS }

// loadMirrorRequests reads request rows (optionally since a cutoff), newest
// ordering by ts then activity_id for a stable sweep. A row with an unparseable
// ts is skipped, not guessed.
func loadMirrorRequests(ctx context.Context, db *store.Store, since time.Time, model string) ([]mirrorRequest, error) {
	q := `SELECT epoch_id, COALESCE(model,''), ts, COALESCE(duration_ms,0), COALESCE(status,0),
	             COALESCE(req_path,''), input_tokens, output_tokens
	        FROM requests`
	var conds []string
	var args []any
	if !since.IsZero() {
		conds = append(conds, `ts >= ?`)
		args = append(args, since.UTC().Format(time.RFC3339))
	}
	if model != "" {
		conds = append(conds, `model = ?`)
		args = append(args, model)
	}
	for i, c := range conds {
		if i == 0 {
			q += ` WHERE ` + c
		} else {
			q += ` AND ` + c
		}
	}
	q += ` ORDER BY ts, activity_id`
	rows, err := db.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mirrorRequest
	for rows.Next() {
		var r mirrorRequest
		var ts string
		var durMS int64
		if err := rows.Scan(&r.EpochID, &r.Model, &ts, &durMS, &r.Status, &r.ReqPath, &r.InTok, &r.OutTok); err != nil {
			return nil, err
		}
		t, perr := time.Parse(time.RFC3339, ts)
		if perr != nil {
			continue
		}
		r.TS = t
		r.Duration = time.Duration(durMS) * time.Millisecond
		out = append(out, r)
	}
	return out, rows.Err()
}

// analyticsCoverage is the honesty block both verbs print. It answers "how much
// of the truth is this computed from" — the same discipline swaps.go's coverage
// struct enforces, plus the two holes this data actually has:
//   - SecondResolutionTS: llama-swap stamps activity in whole seconds, so any
//     sub-second concurrency is unreconstructable. saturation says so instead of
//     printing a depth number that is really a timestamp artifact.
//   - WalkCapped / BelowWatermark: rows the mirror never captured (a --max-pages
//     walk that hit its cap, or activity that existed before this CLI's first
//     poll of an epoch — loss_prepoll). Per-request analytics under-count by
//     exactly this, so it is a coverage field, never silent.
type analyticsCoverage struct {
	RequestRows      int      `json:"request_rows"`
	WindowStart      string   `json:"window_start,omitempty"`
	WindowEnd        string   `json:"window_end,omitempty"`
	Epochs           int      `json:"epochs"`
	PrepollLoss      int64    `json:"prepoll_loss"`
	EvictedLoss      *int64   `json:"evicted_loss"`
	EpochsWithHole   int      `json:"epochs_with_unknown_loss"`
	CoveragePct      *float64 `json:"coverage_pct"`
	SecondResolution bool     `json:"second_resolution_ts"`
	Sampled          bool     `json:"sampled"`
	Caveats          []string `json:"caveats"`
}

// fillAnalyticsCoverage populates the coverage block from the epochs table and
// the loaded request window. CoveragePct is mirrored-rows over
// (mirrored + prepoll + evicted) across the covered epochs — nil when a hole
// makes the denominator unsound rather than a fabricated 100%.
func fillAnalyticsCoverage(ctx context.Context, db *store.Store, reqs []mirrorRequest) (analyticsCoverage, error) {
	c := analyticsCoverage{Sampled: true, SecondResolution: true, RequestRows: len(reqs)}
	if len(reqs) > 0 {
		c.WindowStart = reqs[0].TS.UTC().Format(time.RFC3339)
		c.WindowEnd = reqs[len(reqs)-1].TS.UTC().Format(time.RFC3339)
	}
	var prepoll, evicted int64
	var evictedSound = true
	rows, err := db.DB().QueryContext(ctx, `SELECT COALESCE(loss_prepoll,0), loss_evicted, ids_dense FROM epochs`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var lp int64
		var le sql.NullInt64
		var dense int
		if err := rows.Scan(&lp, &le, &dense); err != nil {
			return c, err
		}
		c.Epochs++
		prepoll += lp
		if le.Valid {
			evicted += le.Int64
		} else {
			evictedSound = false
		}
		if !le.Valid || dense == 0 {
			c.EpochsWithHole++
		}
	}
	if err := rows.Err(); err != nil {
		return c, err
	}
	c.PrepollLoss = prepoll
	if evictedSound {
		c.EvictedLoss = &evicted
	}
	// Coverage percentage only when the denominator is sound.
	if evictedSound && len(reqs) > 0 {
		total := float64(len(reqs)) + float64(prepoll) + float64(evicted)
		if total > 0 {
			pct := float64(len(reqs)) / total * 100
			pct = float64(int(pct*100+0.5)) / 100
			c.CoveragePct = &pct
		}
	}
	c.Caveats = append(c.Caveats,
		"mirror timestamps are whole-second resolution: sub-second concurrency cannot be reconstructed, so in-flight depth is not reported.",
		"SAMPLED: covers rows this CLI mirrored. Activity dropped before the first poll of an epoch (prepoll_loss) or evicted from the server before a sync is absent.")
	if prepoll > 0 {
		c.Caveats = append(c.Caveats, "prepoll_loss > 0: some activity existed before this CLI first polled the open epoch; per-model counts are lower bounds. A full re-sync from activity id 1 may recover it.")
	}
	if c.EpochsWithHole > 0 {
		c.Caveats = append(c.Caveats, "one or more epochs have unknown eviction loss (id density broke); coverage_pct is null rather than a fabricated number.")
	}
	return c, nil
}

// analyticsYAMLSeats reads the llama-swap YAML READ-ONLY and returns a map from
// every addressable name (id AND each alias) to its seat, so a request logged
// under an alias still resolves to its TTL. An unreadable config yields an empty
// map; callers render "unknown", never a default TTL.
func analyticsYAMLSeats() (map[string]mirror.YAMLSeat, []string) {
	byName := map[string]mirror.YAMLSeat{}
	path := yamlConfigPath()
	if path == "" {
		return byName, []string{"no llama-swap YAML found (" + mirror.EnvYAMLPath + " unset and no default): TTLs render as unknown"}
	}
	seats, err := mirror.ParseYAMLSeats(path)
	if err != nil {
		return byName, []string{"llama-swap YAML not readable (" + err.Error() + "): TTLs render as unknown"}
	}
	for _, s := range seats {
		byName[s.ID] = s
		for _, a := range s.Aliases {
			byName[a] = s
		}
	}
	return byName, nil
}

// analyticsPercentile returns the q-quantile of a sorted int64 slice, nearest-
// rank. Shared with swaps.go's spinePercentile semantics but kept local so a
// change to one report cannot silently move the other.
func analyticsPercentile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := int(float64(len(sorted))*q+0.999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
