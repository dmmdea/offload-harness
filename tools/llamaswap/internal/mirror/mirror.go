// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package mirror

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"llamaswap-pp-cli/internal/store"
)

// Witness names the evidence this engine uses to decide that the proxy
// restarted. Recorded per epoch so a later reader knows what the boundary was
// actually based on, rather than trusting the row's existence.
const Witness = "stats.total_requests fence + activity-id identity anchor"

// SchemaVersion stamps every report this package emits.
const SchemaVersion = 1

// Epoch states, mirroring the CHECK constraint in internal/store/domain.go.
const (
	StateOpen    = "open"
	StateUnknown = "unknown"
	StateSealed  = "sealed"
)

// Seal reasons.
const (
	SealRestartMidFetch      = "restart_midfetch"
	SealCounterRegression    = "restart_counter_regression"
	SealIdentityMismatch     = "restart_identity_mismatch"
	SealIDRegression         = "restart_id_regression"
	SealManual               = "manual_seal"
	SealSuperseded           = "superseded_by_new_epoch"
	PostPollTailUncomputable = "unknowable"
)

// Engine mirrors the proxy's in-memory activity ring and event stream into the
// domain tables, maintaining the epoch lifecycle described in
// internal/store/domain.go.
type Engine struct {
	DB     *sql.DB
	Client *Client

	// PageLimit is the activity page size. MaxPages bounds a single pass.
	PageLimit int
	MaxPages  int
	// EventWindow is how long a pass drains /api/events. Zero disables it.
	EventWindow time.Duration
	// FenceRetries bounds re-polls after a mid-fetch restart.
	FenceRetries int

	Now func() time.Time
}

// EpochReport is the per-epoch section of a sync report.
type EpochReport struct {
	EpochID          int64  `json:"epoch_id"`
	State            string `json:"state"`
	Witness          string `json:"witness"`
	FirstSeenAt      string `json:"first_seen_at"`
	LastSeenAt       string `json:"last_seen_at"`
	SealReason       string `json:"seal_reason,omitempty"`
	Rows             int    `json:"rows_mirrored"`
	CensoredRows     int    `json:"censored_rows"`
	IDsDense         bool   `json:"ids_dense"`
	MaxActivityID    int64  `json:"max_activity_id"`
	TotalRequestsEnd int64  `json:"total_requests_last"`
	// LossEvicted is nil when id density was violated: a hole in the id space
	// makes the arithmetic unsound, and an unsound number is worse than none.
	LossEvicted *int64 `json:"loss_evicted"`
	LossPrepoll *int64 `json:"loss_prepoll"`
	Notes       string `json:"notes,omitempty"`
}

// Report is one mirror pass.
type Report struct {
	SchemaVersion int           `json:"schema_version"`
	BaseURL       string        `json:"base_url"`
	RanAt         string        `json:"ran_at"`
	Epochs        []EpochReport `json:"epochs"`
	RowsMirrored  int           `json:"rows_mirrored"`
	RowsCensored  int           `json:"rows_censored"`
	SealedEpochs  []int64       `json:"sealed_epochs"`
	OpenEpochID   int64         `json:"open_epoch_id"`
	FenceRetries  int           `json:"fence_retries"`
	EventsDrained int           `json:"events_drained"`
	SwapEvents    int           `json:"swap_events_recorded"`
	// PostPollTail is NEVER a number. Requests served after our last successful
	// poll and before a restart left no trace anywhere: the ring is gone and
	// the counter is zeroed. Reporting 0 there would be a fabrication.
	PostPollTail string `json:"post_poll_tail"`
	// EpochCountIsLowerBound is always true: two restarts between two polls are
	// indistinguishable from one.
	EpochCountIsLowerBound bool     `json:"epoch_count_is_lower_bound"`
	Warnings               []string `json:"warnings,omitempty"`
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) pageLimit() int {
	if e.PageLimit > 0 {
		return e.PageLimit
	}
	return 200
}

func (e *Engine) maxPages() int {
	if e.MaxPages > 0 {
		return e.MaxPages
	}
	return 200
}

func (e *Engine) fenceRetries() int {
	if e.FenceRetries > 0 {
		return e.FenceRetries
	}
	return 3
}

type epochRow struct {
	ID                int64
	Witness           string
	FirstSeenAt       string
	LastSeenAt        string
	State             string
	SealReason        sql.NullString
	MaxActivityID     sql.NullInt64
	TotalRequestsLast sql.NullInt64
	IDsDense          bool
	LossEvicted       sql.NullInt64
	LossPrepoll       sql.NullInt64
	Notes             sql.NullString
}

// Sync runs one mirror pass: read-verify-read fenced activity fetch, epoch
// resolution, row upsert, loss accounting, and a bounded event drain.
func (e *Engine) Sync(ctx context.Context) (*Report, error) {
	if err := store.EnsureDomainSchema(ctx, e.DB); err != nil {
		return nil, fmt.Errorf("ensure domain schema: %w", err)
	}
	rep := &Report{
		SchemaVersion:          SchemaVersion,
		BaseURL:                e.Client.BaseURL,
		RanAt:                  e.now().UTC().Format(time.RFC3339),
		PostPollTail:           PostPollTailUncomputable,
		EpochCountIsLowerBound: true,
	}

	var rows []ActivityRow
	var stats Stats
	for attempt := 0; ; attempt++ {
		before, err := e.Client.Stats(ctx)
		if err != nil {
			return nil, fmt.Errorf("read restart witness (pre-fetch): %w", err)
		}
		fetched, err := e.fetchAllActivity(ctx)
		if err != nil {
			return nil, err
		}
		after, err := e.Client.Stats(ctx)
		if err != nil {
			return nil, fmt.Errorf("read restart witness (post-fetch): %w", err)
		}
		// THE FENCE. A cumulative counter can only go down by being zeroed,
		// which only a restart does. If it dropped while we were paging, the
		// batch straddles two different servers and MUST be discarded whole —
		// splicing it would silently merge two id spaces into one epoch.
		if after.TotalRequests < before.TotalRequests {
			rep.FenceRetries++
			if sealed, err := e.sealOpenEpoch(ctx, SealRestartMidFetch); err != nil {
				return nil, err
			} else if sealed > 0 {
				rep.SealedEpochs = append(rep.SealedEpochs, sealed)
			}
			if attempt+1 >= e.fenceRetries() {
				rep.Warnings = append(rep.Warnings,
					fmt.Sprintf("restart witness regressed on %d consecutive fetches; batch discarded each time, nothing mirrored this pass", rep.FenceRetries))
				rows = nil
				stats = after
				break
			}
			continue
		}
		rows = fetched
		stats = after
		break
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	cur, err := e.loadOpenEpoch(ctx)
	if err != nil {
		return nil, err
	}
	if cur != nil {
		restarted, reason, err := e.detectRestart(ctx, cur, rows, stats)
		if err != nil {
			return nil, err
		}
		if restarted {
			if err := e.sealEpoch(ctx, cur.ID, reason); err != nil {
				return nil, err
			}
			rep.SealedEpochs = append(rep.SealedEpochs, cur.ID)
			cur = nil
		}
	}

	if cur == nil && len(rows) > 0 {
		cur, err = e.openEpoch(ctx, rows, stats)
		if err != nil {
			return nil, err
		}
	}

	if cur != nil && len(rows) > 0 {
		mirrored, err := e.applyBatch(ctx, cur, rows, stats)
		if err != nil {
			return nil, err
		}
		rep.RowsMirrored = mirrored
	}

	if e.EventWindow > 0 {
		drained, recorded, err := e.drainEvents(ctx, cur)
		if err != nil {
			rep.Warnings = append(rep.Warnings, "event drain: "+err.Error())
		}
		rep.EventsDrained = drained
		rep.SwapEvents = recorded
	}

	if err := e.fillReport(ctx, rep); err != nil {
		return nil, err
	}
	return rep, nil
}

// SealNow force-seals the open epoch (the `sync --seal` verb). No network read:
// the operator is asserting the boundary, so the engine records that provenance
// rather than inventing evidence it does not have.
func (e *Engine) SealNow(ctx context.Context) (*Report, error) {
	if err := store.EnsureDomainSchema(ctx, e.DB); err != nil {
		return nil, fmt.Errorf("ensure domain schema: %w", err)
	}
	rep := &Report{
		SchemaVersion:          SchemaVersion,
		BaseURL:                e.Client.BaseURL,
		RanAt:                  e.now().UTC().Format(time.RFC3339),
		PostPollTail:           PostPollTailUncomputable,
		EpochCountIsLowerBound: true,
	}
	sealed, err := e.sealOpenEpoch(ctx, SealManual)
	if err != nil {
		return nil, err
	}
	if sealed > 0 {
		rep.SealedEpochs = append(rep.SealedEpochs, sealed)
	} else {
		rep.Warnings = append(rep.Warnings, "no open epoch to seal")
	}
	if err := e.fillReport(ctx, rep); err != nil {
		return nil, err
	}
	return rep, nil
}

// fetchAllActivity pages the ring ASCENDING by id. Ascending matters: the
// density assertion and the loss arithmetic both need the complete visible id
// range, and a descending walk would have to be reversed anyway.
func (e *Engine) fetchAllActivity(ctx context.Context) ([]ActivityRow, error) {
	limit := e.pageLimit()
	var all []ActivityRow
	seen := map[int64]bool{}
	for page := 1; page <= e.maxPages(); page++ {
		p, err := e.Client.Activity(ctx, ActivityOpts{Page: page, Limit: limit, Sort: "id", Order: "asc"})
		if err != nil {
			return nil, fmt.Errorf("fetch activity page %d: %w", page, err)
		}
		for _, row := range p.Data {
			if seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			all = append(all, row)
		}
		if len(p.Data) == 0 || len(p.Data) < limit {
			break
		}
		if p.TotalPages > 0 && page >= p.TotalPages {
			break
		}
	}
	return all, nil
}

func (e *Engine) loadOpenEpoch(ctx context.Context) (*epochRow, error) {
	row := e.DB.QueryRowContext(ctx, `
		SELECT epoch_id, witness, first_seen_at, last_seen_at, state, seal_reason,
		       max_activity_id, total_requests_last, ids_dense, loss_evicted, loss_prepoll, notes
		  FROM epochs
		 WHERE state IN ('open','unknown')
		 ORDER BY epoch_id DESC LIMIT 1`)
	var r epochRow
	var dense int
	err := row.Scan(&r.ID, &r.Witness, &r.FirstSeenAt, &r.LastSeenAt, &r.State, &r.SealReason,
		&r.MaxActivityID, &r.TotalRequestsLast, &dense, &r.LossEvicted, &r.LossPrepoll, &r.Notes)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load open epoch: %w", err)
	}
	r.IDsDense = dense != 0
	return &r, nil
}

// detectRestart decides whether the fetched batch came from a DIFFERENT server
// process than the open epoch. Two independent witnesses, because either alone
// has a hole:
//
//  1. Counter regression. total_requests only decreases by being zeroed.
//     Catches a restart whose new traffic has not yet passed the old count.
//  2. Identity mismatch at overlapping ids. A restart that races PAST the old
//     cursor (old max 50, new server already at 80) leaves the counter higher
//     and the ids higher — both look like normal progress. But id 50 now names
//     a different request, so its fingerprint differs from the mirrored one.
//     This is the case that would otherwise splice two servers into one epoch.
//
// A third, cheaper signal (ids went backwards with no overlap at all) covers a
// restart into a ring whose visible window sits entirely below the old cursor.
func (e *Engine) detectRestart(ctx context.Context, cur *epochRow, rows []ActivityRow, stats Stats) (bool, string, error) {
	if cur.TotalRequestsLast.Valid && stats.TotalRequests < cur.TotalRequestsLast.Int64 {
		return true, SealCounterRegression, nil
	}
	if len(rows) == 0 {
		return false, "", nil
	}
	minFresh := rows[0].ID
	maxFresh := rows[len(rows)-1].ID

	stored, err := e.storedFingerprints(ctx, cur.ID, minFresh, maxFresh)
	if err != nil {
		return false, "", err
	}
	overlap := 0
	for _, r := range rows {
		fp, ok := stored[r.ID]
		if !ok {
			continue
		}
		overlap++
		if fp != r.Fingerprint() {
			return true, SealIdentityMismatch, nil
		}
	}
	if overlap == 0 && cur.MaxActivityID.Valid && cur.MaxActivityID.Int64 > 0 && minFresh <= cur.MaxActivityID.Int64 {
		// The ring's whole visible window sits at or below a cursor we already
		// passed, yet none of those ids match what we mirrored: the id space
		// was rebuilt.
		return true, SealIDRegression, nil
	}
	return false, "", nil
}

func (e *Engine) storedFingerprints(ctx context.Context, epochID, minID, maxID int64) (map[int64]string, error) {
	rows, err := e.DB.QueryContext(ctx, `
		SELECT activity_id, COALESCE(ts,''), COALESCE(model,''), COALESCE(req_path,''),
		       COALESCE(status,0), COALESCE(duration_ms,0)
		  FROM requests
		 WHERE epoch_id = ? AND activity_id BETWEEN ? AND ?`, epochID, minID, maxID)
	if err != nil {
		return nil, fmt.Errorf("read stored fingerprints: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var ts, model, path string
		var status int
		var dur int64
		if err := rows.Scan(&id, &ts, &model, &path, &status, &dur); err != nil {
			return nil, err
		}
		out[id] = ActivityRow{
			Timestamp: ts, Model: model, ReqPath: path,
			RespStatusCode: status, DurationMS: dur,
		}.Fingerprint()
	}
	return out, rows.Err()
}

// openEpoch starts a new epoch and records its counter-derived pre-poll loss:
// requests this server already served that had rolled out of the ring before we
// ever looked. It is a lower bound on what we missed, never a total.
func (e *Engine) openEpoch(ctx context.Context, rows []ActivityRow, stats Stats) (*epochRow, error) {
	now := e.now().UTC().Format(time.RFC3339)
	visible := int64(len(rows))
	prepoll := stats.TotalRequests - visible
	if prepoll < 0 {
		prepoll = 0
	}
	notes := ""
	if len(rows) > 0 {
		// Cross-check against the id-derived value. Dense 1-based ids make
		// (minID-1) the same quantity; a disagreement means one of the two
		// assumptions is wrong, and the reader deserves to know.
		idDerived := rows[0].ID - 1
		if idDerived != prepoll {
			notes = fmt.Sprintf("loss_prepoll counter-derived=%d id-derived=%d (disagreement recorded, counter kept)", prepoll, idDerived)
		}
	}
	res, err := e.DB.ExecContext(ctx, `
		INSERT INTO epochs (witness, first_seen_at, last_seen_at, state, ids_dense, loss_prepoll, loss_evicted, total_requests_last, notes)
		VALUES (?, ?, ?, 'open', 1, ?, 0, ?, ?)`,
		Witness, now, now, prepoll, stats.TotalRequests, nullIfEmpty(notes))
	if err != nil {
		return nil, fmt.Errorf("open epoch: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &epochRow{
		ID: id, Witness: Witness, FirstSeenAt: now, LastSeenAt: now,
		State: StateOpen, IDsDense: true,
		LossPrepoll:       sql.NullInt64{Int64: prepoll, Valid: true},
		LossEvicted:       sql.NullInt64{Int64: 0, Valid: true},
		TotalRequestsLast: sql.NullInt64{Int64: stats.TotalRequests, Valid: true},
		Notes:             sql.NullString{String: notes, Valid: notes != ""},
	}, nil
}

// applyBatch upserts the visible rows, asserts id density, and accrues eviction
// loss. Density is asserted EVERY batch, not once: the moment a hole appears the
// eviction arithmetic stops being sound and the epoch's loss_evicted degrades to
// NULL for good, rather than silently continuing to publish a wrong number.
func (e *Engine) applyBatch(ctx context.Context, cur *epochRow, rows []ActivityRow, stats Stats) (int, error) {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO requests (epoch_id, activity_id, ts, model, req_path, status,
		                      cache_tokens, input_tokens, output_tokens, draft_tokens, draft_acc_tokens,
		                      prompt_per_second, tokens_per_second, duration_ms, censored, raw)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?)
		ON CONFLICT(epoch_id, activity_id) DO UPDATE SET
			ts=excluded.ts, model=excluded.model, req_path=excluded.req_path, status=excluded.status,
			cache_tokens=excluded.cache_tokens, input_tokens=excluded.input_tokens,
			output_tokens=excluded.output_tokens, draft_tokens=excluded.draft_tokens,
			draft_acc_tokens=excluded.draft_acc_tokens, prompt_per_second=excluded.prompt_per_second,
			tokens_per_second=excluded.tokens_per_second, duration_ms=excluded.duration_ms,
			raw=excluded.raw`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	count := 0
	for _, r := range rows {
		raw, _ := json.Marshal(r)
		var status any
		if r.Terminal() {
			status = r.RespStatusCode
		} else {
			status = nil
		}
		var dur any
		if r.DurationMS > 0 {
			dur = r.DurationMS
		} else {
			dur = nil
		}
		if _, err := stmt.ExecContext(ctx, cur.ID, r.ID, r.Timestamp, r.Model, r.ReqPath, status,
			r.Tokens.CacheTokens, r.Tokens.InputTokens, r.Tokens.OutputTokens,
			r.Tokens.DraftTokens, r.Tokens.DraftAccTokens,
			r.Tokens.PromptPerSecond, r.Tokens.TokensPerSecond, dur, string(raw)); err != nil {
			return 0, fmt.Errorf("upsert request %d: %w", r.ID, err)
		}
		count++
	}

	minID := rows[0].ID
	maxID := rows[len(rows)-1].ID
	dense := cur.IDsDense && idsAreDense(rows)

	// Eviction loss: rows numbered strictly between our last cursor and the
	// oldest id still visible were served and rolled out unseen.
	lossDelta := int64(0)
	if cur.MaxActivityID.Valid && minID > cur.MaxActivityID.Int64+1 {
		lossDelta = minID - cur.MaxActivityID.Int64 - 1
	}

	now := e.now().UTC().Format(time.RFC3339)
	newMax := maxID
	if cur.MaxActivityID.Valid && cur.MaxActivityID.Int64 > newMax {
		newMax = cur.MaxActivityID.Int64
	}
	if dense {
		if _, err := tx.ExecContext(ctx, `
			UPDATE epochs SET last_seen_at=?, max_activity_id=?, total_requests_last=?,
			                  ids_dense=1, loss_evicted=COALESCE(loss_evicted,0)+?
			 WHERE epoch_id=?`, now, newMax, stats.TotalRequests, lossDelta, cur.ID); err != nil {
			return 0, err
		}
	} else {
		note := "id density violated (a gap appeared in the visible id range); loss_evicted is NOT computable and is recorded as NULL rather than guessed"
		if _, err := tx.ExecContext(ctx, `
			UPDATE epochs SET last_seen_at=?, max_activity_id=?, total_requests_last=?,
			                  ids_dense=0, loss_evicted=NULL,
			                  notes=CASE WHEN notes IS NULL OR notes='' THEN ? ELSE notes END
			 WHERE epoch_id=?`, now, newMax, stats.TotalRequests, note, cur.ID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	cur.MaxActivityID = sql.NullInt64{Int64: newMax, Valid: true}
	cur.IDsDense = dense
	return count, nil
}

func idsAreDense(rows []ActivityRow) bool {
	for i := 1; i < len(rows); i++ {
		if rows[i].ID != rows[i-1].ID+1 {
			return false
		}
	}
	return true
}

func (e *Engine) sealOpenEpoch(ctx context.Context, reason string) (int64, error) {
	cur, err := e.loadOpenEpoch(ctx)
	if err != nil || cur == nil {
		return 0, err
	}
	if err := e.sealEpoch(ctx, cur.ID, reason); err != nil {
		return 0, err
	}
	return cur.ID, nil
}

// sealEpoch closes an epoch. Non-terminal rows become CENSORED — not failed.
// A request still in flight when the process died has NO observable outcome,
// and the censored set is biased toward long requests, so any statistic derived
// from it must say so.
func (e *Engine) sealEpoch(ctx context.Context, epochID int64, reason string) error {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := e.now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `
		UPDATE epochs SET state='sealed', seal_reason=?, last_seen_at=? WHERE epoch_id=?`,
		reason, now, epochID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE requests SET censored=1 WHERE epoch_id=? AND (status IS NULL OR status < 100)`,
		epochID); err != nil {
		return err
	}
	if strings.HasPrefix(reason, "restart") {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO service_lifecycle (ts, event, version, details) VALUES (?,?,?,?)`,
			now, "restart_detected", nil,
			fmt.Sprintf("epoch %d sealed: %s (witness: %s)", epochID, reason, Witness)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// drainEvents records swap lifecycle rows from the SSE stream.
//
// Two payload shapes are handled. The one VERIFIED on this deployment is
// {"type":"logData","data":"<raw log text>"}, whose swap markers are matched
// literally below. A structured model-status frame is also accepted defensively
// (newer builds emit one), but nothing is invented for shapes never observed —
// unknown frames are counted and dropped.
func (e *Engine) drainEvents(ctx context.Context, cur *epochRow) (int, int, error) {
	events, err := e.Client.DrainEvents(ctx, e.EventWindow)
	if err != nil {
		return len(events), 0, err
	}
	var epochID any
	if cur != nil {
		epochID = cur.ID
	}
	recorded := 0
	for _, ev := range events {
		for _, se := range parseSwapEvents(ev) {
			inserted, err := e.insertSwapEvent(ctx, epochID, se)
			if err != nil {
				return len(events), recorded, err
			}
			if inserted {
				recorded++
			}
		}
	}
	return len(events), recorded, nil
}

type swapEvent struct {
	Model   string
	Event   string
	Source  string
	Line    string
	LineSHA string
}

// parseSwapEvents extracts model lifecycle transitions from one SSE frame.
func parseSwapEvents(ev RawEvent) []swapEvent {
	var out []swapEvent
	if ev.Text != "" {
		// logData payloads arrive double-encoded on this deployment: after the
		// envelope unmarshal the buffer still carries literal backslash-n
		// two-byte sequences instead of newlines, so a plain "\n" split sees
		// ONE giant line and the whole replayed log collapses into a single
		// phantom event (verified live: swap_events_recorded stuck at 1).
		// Normalize the escaped form before splitting; real newlines pass
		// through untouched.
		text := strings.ReplaceAll(ev.Text, `\n`, "\n")
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if model, ok := parseLogMarker(line); ok {
				out = append(out, swapEvent{
					Model: model.model, Event: model.event, Source: "log",
					Line: line, LineSHA: shaOf(line),
				})
			}
		}
		return out
	}
	var structured struct {
		Model  string `json:"model"`
		State  string `json:"state"`
		Status string `json:"status"`
	}
	if len(ev.Data) > 0 && json.Unmarshal(ev.Data, &structured) == nil && structured.Model != "" {
		state := structured.State
		if state == "" {
			state = structured.Status
		}
		if mapped, ok := mapModelState(state); ok {
			key := ev.Type + "|" + structured.Model + "|" + mapped + "|" + ev.ReceivedAt.Format(time.RFC3339)
			out = append(out, swapEvent{
				Model: structured.Model, Event: mapped, Source: "events",
				Line: string(ev.Data), LineSHA: shaOf(key),
			})
		}
	}
	return out
}

type logMarker struct{ model, event string }

// parseLogMarker matches the log lines this deployment actually emits. Verified
// live on v249:
//
//	[INFO] matrix: model=embeddinggemma starting (no models running)
//	[INFO] <embeddinggemma> Health check passed on http://127.0.0.1:9201/health
func parseLogMarker(line string) (logMarker, bool) {
	if idx := strings.Index(line, "matrix: model="); idx >= 0 {
		rest := line[idx+len("matrix: model="):]
		model, _, _ := strings.Cut(rest, " ")
		if model != "" && strings.Contains(rest, "starting") {
			return logMarker{model: model, event: "loading"}, true
		}
	}
	if open := strings.Index(line, "<"); open >= 0 {
		if close := strings.Index(line[open:], ">"); close > 1 {
			model := line[open+1 : open+close]
			tail := line[open+close:]
			switch {
			case strings.Contains(tail, "Health check passed"):
				return logMarker{model: model, event: "ready"}, true
			case strings.Contains(tail, "Stopping"), strings.Contains(tail, "unloading"):
				return logMarker{model: model, event: "unloading"}, true
			case strings.Contains(tail, "Stopped"), strings.Contains(tail, "exited"):
				return logMarker{model: model, event: "unloaded"}, true
			}
		}
	}
	return logMarker{}, false
}

func mapModelState(state string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "loading", "starting":
		return "loading", true
	case "ready", "loaded", "running":
		return "ready", true
	case "unloading", "stopping":
		return "unloading", true
	case "unloaded", "stopped":
		return "unloaded", true
	case "failed", "error":
		return "failed", true
	}
	return "", false
}

// insertSwapEvent is idempotent per log line: /api/events replays the entire
// buffered log on every connect, so a naive insert would duplicate the whole
// history on each sync.
func (e *Engine) insertSwapEvent(ctx context.Context, epochID any, se swapEvent) (bool, error) {
	var exists int
	err := e.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM swap_events WHERE model=? AND event=? AND details LIKE ?`,
		se.Model, se.Event, "%\"line_sha\":\""+se.LineSHA+"\"%").Scan(&exists)
	if err != nil {
		return false, err
	}
	if exists > 0 {
		return false, nil
	}
	details, _ := json.Marshal(map[string]string{"line_sha": se.LineSHA, "line": truncate(se.Line, 400)})
	now := e.now().UTC().Format(time.RFC3339)

	// Cold-load duration: pair this 'ready' with the most recent unpaired
	// 'loading' for the same model. The timestamp is our RECEIPT time, not the
	// proxy's — the log payload carries no per-line clock — so it is an upper
	// bound on the real load, recorded as such.
	var coldLoad any
	if se.Event == "ready" {
		var startedAt string
		err := e.DB.QueryRowContext(ctx,
			`SELECT ts FROM swap_events WHERE model=? AND event='loading' ORDER BY id DESC LIMIT 1`,
			se.Model).Scan(&startedAt)
		if err == nil {
			if t0, perr := time.Parse(time.RFC3339, startedAt); perr == nil {
				if ms := e.now().UTC().Sub(t0).Milliseconds(); ms >= 0 {
					coldLoad = ms
				}
			}
		} else if err != sql.ErrNoRows {
			return false, err
		}
	}
	if _, err := e.DB.ExecContext(ctx,
		`INSERT INTO swap_events (epoch_id, ts, model, event, cold_load_ms, source, details) VALUES (?,?,?,?,?,?,?)`,
		epochID, now, se.Model, se.Event, coldLoad, se.Source, string(details)); err != nil {
		return false, err
	}
	return true, nil
}

// fillReport reads back every epoch so the report reflects committed state.
func (e *Engine) fillReport(ctx context.Context, rep *Report) error {
	rows, err := e.DB.QueryContext(ctx, `
		SELECT epoch_id, witness, first_seen_at, last_seen_at, state, COALESCE(seal_reason,''),
		       COALESCE(max_activity_id,0), COALESCE(total_requests_last,0), ids_dense,
		       loss_evicted, loss_prepoll, COALESCE(notes,'')
		  FROM epochs ORDER BY epoch_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var er EpochReport
		var dense int
		var lossEvicted, lossPrepoll sql.NullInt64
		if err := rows.Scan(&er.EpochID, &er.Witness, &er.FirstSeenAt, &er.LastSeenAt, &er.State,
			&er.SealReason, &er.MaxActivityID, &er.TotalRequestsEnd, &dense,
			&lossEvicted, &lossPrepoll, &er.Notes); err != nil {
			return err
		}
		er.IDsDense = dense != 0
		if lossEvicted.Valid {
			v := lossEvicted.Int64
			er.LossEvicted = &v
		}
		if lossPrepoll.Valid {
			v := lossPrepoll.Int64
			er.LossPrepoll = &v
		}
		if err := e.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE epoch_id=?`, er.EpochID).Scan(&er.Rows); err != nil {
			return err
		}
		if err := e.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE epoch_id=? AND censored=1`, er.EpochID).Scan(&er.CensoredRows); err != nil {
			return err
		}
		rep.RowsCensored += er.CensoredRows
		if er.State == StateOpen || er.State == StateUnknown {
			rep.OpenEpochID = er.EpochID
		}
		rep.Epochs = append(rep.Epochs, er)
	}
	return rows.Err()
}

func shaOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
