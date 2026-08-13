// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package mirror_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"llamaswap-pp-cli/internal/fakeswap"
	"llamaswap-pp-cli/internal/mirror"
	"llamaswap-pp-cli/internal/store"
)

func newTestEngine(t *testing.T, fs *fakeswap.Server) (*mirror.Engine, *sql.DB) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &mirror.Engine{
		DB:        db.DB(),
		Client:    mirror.NewClient(fs.URL(), 10*time.Second),
		PageLimit: 25,
	}, db.DB()
}

func epochState(t *testing.T, db *sql.DB, id int64) (string, string, int, int, sql.NullInt64, int) {
	t.Helper()
	var state, reason string
	var dense int
	var lossEvicted sql.NullInt64
	err := db.QueryRow(`SELECT state, COALESCE(seal_reason,''), ids_dense, loss_evicted FROM epochs WHERE epoch_id=?`, id).
		Scan(&state, &reason, &dense, &lossEvicted)
	if err != nil {
		t.Fatalf("read epoch %d: %v", id, err)
	}
	var rows, censored int
	if err := db.QueryRow(`SELECT COUNT(*) FROM requests WHERE epoch_id=?`, id).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM requests WHERE epoch_id=? AND censored=1`, id).Scan(&censored); err != nil {
		t.Fatalf("count censored: %v", err)
	}
	return state, reason, rows, censored, lossEvicted, dense
}

// TestAcceptanceA_RestartRacesPastCursor is the case that motivated the whole
// epoch contract: the new server's ids race PAST the old cursor, so both the id
// space and the cumulative counter look like ordinary forward progress. Only
// content fingerprints at overlapping ids reveal that these are two different
// servers. A miss here means 80 rows from server B get spliced into server A's
// epoch and every later statistic is silently wrong.
func TestAcceptanceA_RestartRacesPastCursor(t *testing.T) {
	fs := fakeswap.New()
	defer fs.Close()
	ctx := context.Background()

	// Epoch 1: ids 1..50 on gemma-4-e4b; the last two are still in flight.
	for i := 1; i <= 48; i++ {
		fs.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 120)
	}
	fs.AddInFlight("gemma-4-e4b", "/v1/chat/completions")
	fs.AddInFlight("gemma-4-e4b", "/v1/chat/completions")

	eng, db := newTestEngine(t, fs)
	rep1, err := eng.Sync(ctx)
	if err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	if rep1.RowsMirrored != 50 || len(rep1.Epochs) != 1 || rep1.Epochs[0].State != mirror.StateOpen {
		t.Fatalf("sync 1 report = %+v", rep1)
	}

	// RESTART, and the new server races past 50 before we poll again.
	fs.ResetEpoch()
	for i := 1; i <= 80; i++ {
		fs.AddActivity("embeddinggemma", "/v1/embeddings", 200, 18)
	}
	if got := fs.TotalRequests(); got != 80 {
		t.Fatalf("precondition: counter should be 80 (> the old 50), got %d", got)
	}

	rep2, err := eng.Sync(ctx)
	if err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if len(rep2.Epochs) != 2 {
		t.Fatalf("want 2 epochs after a restart, got %d: %+v", len(rep2.Epochs), rep2.Epochs)
	}

	state1, reason1, rows1, censored1, _, _ := epochState(t, db, rep2.Epochs[0].EpochID)
	if state1 != mirror.StateSealed {
		t.Fatalf("epoch 1 state = %q, want sealed", state1)
	}
	if reason1 != mirror.SealIdentityMismatch {
		t.Fatalf("epoch 1 seal_reason = %q, want %q", reason1, mirror.SealIdentityMismatch)
	}
	if rows1 != 50 {
		t.Fatalf("epoch 1 rows = %d, want 50", rows1)
	}
	if censored1 != 2 {
		t.Fatalf("epoch 1 censored = %d, want 2 (the in-flight rows, marked censored not failed)", censored1)
	}

	state2, _, rows2, censored2, _, _ := epochState(t, db, rep2.Epochs[1].EpochID)
	if state2 != mirror.StateOpen || rows2 != 80 || censored2 != 0 {
		t.Fatalf("epoch 2: state=%q rows=%d censored=%d, want open/80/0", state2, rows2, censored2)
	}

	// ZERO SPLICED ROWS: each epoch must contain exactly one server's traffic.
	assertSingleModel(t, db, rep2.Epochs[0].EpochID, "gemma-4-e4b")
	assertSingleModel(t, db, rep2.Epochs[1].EpochID, "embeddinggemma")

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 130 {
		t.Fatalf("total mirrored rows = %d, want 130 (50 + 80, none overwritten)", total)
	}
	if rep2.PostPollTail != mirror.PostPollTailUncomputable {
		t.Fatalf("post_poll_tail = %q, want %q", rep2.PostPollTail, mirror.PostPollTailUncomputable)
	}
	if !rep2.EpochCountIsLowerBound {
		t.Fatal("epoch count must be reported as a lower bound")
	}
}

func assertSingleModel(t *testing.T, db *sql.DB, epochID int64, want string) {
	t.Helper()
	rows, err := db.Query(`SELECT DISTINCT model FROM requests WHERE epoch_id=?`, epochID)
	if err != nil {
		t.Fatalf("distinct models: %v", err)
	}
	defer rows.Close()
	var models []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatalf("scan: %v", err)
		}
		models = append(models, m)
	}
	if len(models) != 1 || models[0] != want {
		t.Fatalf("epoch %d contains models %v, want exactly [%s] (spliced rows detected)", epochID, models, want)
	}
}

// TestAcceptanceA_CounterRegressionSealsToo covers the other restart shape: the
// new server has served FEWER requests than the old one, so the fence catches it
// on the counter alone.
func TestAcceptanceA_CounterRegressionSealsToo(t *testing.T) {
	fs := fakeswap.New()
	defer fs.Close()
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		fs.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 100)
	}
	eng, db := newTestEngine(t, fs)
	if _, err := eng.Sync(ctx); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	fs.ResetEpoch()
	for i := 0; i < 5; i++ {
		fs.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 100)
	}
	rep, err := eng.Sync(ctx)
	if err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if len(rep.Epochs) != 2 {
		t.Fatalf("want 2 epochs, got %d", len(rep.Epochs))
	}
	_, reason, _, _, _, _ := epochState(t, db, rep.Epochs[0].EpochID)
	if reason != mirror.SealCounterRegression {
		t.Fatalf("seal_reason = %q, want %q", reason, mirror.SealCounterRegression)
	}
	// A service_lifecycle row must exist so keepset audit can attribute
	// evictions around the restart.
	var lifecycle int
	if err := db.QueryRow(`SELECT COUNT(*) FROM service_lifecycle WHERE event='restart_detected'`).Scan(&lifecycle); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	if lifecycle != 1 {
		t.Fatalf("service_lifecycle restart rows = %d, want 1", lifecycle)
	}
}

// TestAcceptanceB_EvictionLossIsExactWhileDense: 10 rows are served and rolled
// out of the ring between two syncs. The count is knowable exactly because the
// id space stayed dense, and it must be exactly 10 — not 0, not the 60 rows
// actually evicted (50 of which we had already mirrored).
func TestAcceptanceB_EvictionLossIsExactWhileDense(t *testing.T) {
	fs := fakeswap.New()
	defer fs.Close()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		fs.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 100)
	}
	eng, db := newTestEngine(t, fs)
	if _, err := eng.Sync(ctx); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	// 40 more requests, then the ring drops everything up to id 60: ids 51..60
	// were served and are gone before we ever saw them.
	for i := 0; i < 40; i++ {
		fs.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 100)
	}
	fs.EvictOldest(60)
	if ids := fs.VisibleIDs(); ids[0] != 61 {
		t.Fatalf("precondition: oldest visible id = %d, want 61", ids[0])
	}

	rep, err := eng.Sync(ctx)
	if err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if len(rep.Epochs) != 1 {
		t.Fatalf("eviction is NOT a restart; want 1 epoch, got %d", len(rep.Epochs))
	}
	_, _, _, _, lossEvicted, dense := epochState(t, db, rep.Epochs[0].EpochID)
	if dense != 1 {
		t.Fatal("ids should still be dense after ordinary ring eviction")
	}
	if !lossEvicted.Valid || lossEvicted.Int64 != 10 {
		t.Fatalf("loss_evicted = %v, want exactly 10", lossEvicted)
	}
}

// TestAcceptanceC_DensityViolationRefusesToGuess: with a hole in the id space
// the eviction arithmetic is unsound, so ids_dense flips to 0 and loss_evicted
// becomes NULL. A fabricated number here would be worse than no number.
func TestAcceptanceC_DensityViolationYieldsNullLoss(t *testing.T) {
	fs := fakeswap.New()
	defer fs.Close()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		fs.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 100)
	}
	eng, db := newTestEngine(t, fs)
	if _, err := eng.Sync(ctx); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	for i := 0; i < 10; i++ {
		fs.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 100)
	}
	fs.EvictID(55) // hole in the middle of the visible range

	rep, err := eng.Sync(ctx)
	if err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	_, _, _, _, lossEvicted, dense := epochState(t, db, rep.Epochs[0].EpochID)
	if dense != 0 {
		t.Fatal("ids_dense must be 0 after a gap appears in the id range")
	}
	if lossEvicted.Valid {
		t.Fatalf("loss_evicted = %d, want NULL (not computable)", lossEvicted.Int64)
	}
	if rep.Epochs[0].LossEvicted != nil {
		t.Fatalf("report loss_evicted = %d, want null", *rep.Epochs[0].LossEvicted)
	}
}

// TestPrepollLossIsCounterDerived: the ring's oldest visible id is not 1, so
// earlier requests were evicted before this CLI ever polled.
func TestPrepollLossIsCounterDerived(t *testing.T) {
	fs := fakeswap.New()
	defer fs.Close()
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		fs.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 100)
	}
	fs.EvictOldest(12)
	eng, _ := newTestEngine(t, fs)
	rep, err := eng.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if rep.Epochs[0].LossPrepoll == nil || *rep.Epochs[0].LossPrepoll != 12 {
		t.Fatalf("loss_prepoll = %v, want 12", rep.Epochs[0].LossPrepoll)
	}
}

// TestSealNowMarksNonTerminalRowsCensored covers `sync --seal`.
func TestSealNowMarksNonTerminalRowsCensored(t *testing.T) {
	fs := fakeswap.New()
	defer fs.Close()
	ctx := context.Background()
	fs.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 100)
	fs.AddInFlight("gemma-4-e4b", "/v1/chat/completions")
	eng, db := newTestEngine(t, fs)
	if _, err := eng.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	rep, err := eng.SealNow(ctx)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	state, reason, rows, censored, _, _ := epochState(t, db, rep.Epochs[0].EpochID)
	if state != mirror.StateSealed || reason != mirror.SealManual {
		t.Fatalf("state=%q reason=%q, want sealed/%s", state, reason, mirror.SealManual)
	}
	if rows != 2 || censored != 1 {
		t.Fatalf("rows=%d censored=%d, want 2/1", rows, censored)
	}
}

// TestEventDrainRecordsSwapEventsIdempotently: /api/events replays the whole
// buffered log on every connect, so a second drain must add nothing.
func TestEventDrainRecordsSwapEventsIdempotently(t *testing.T) {
	fs := fakeswap.New()
	defer fs.Close()
	ctx := context.Background()
	fs.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 100)
	fs.PushEvent(`{"type":"logData","data":"[INFO] matrix: model=gemma-4-e4b starting (no models running)\n[INFO] <gemma-4-e4b> Health check passed on http://127.0.0.1:9203/health"}`)

	eng, db := newTestEngine(t, fs)
	eng.EventWindow = 400 * time.Millisecond
	rep, err := eng.Sync(ctx)
	if err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	if rep.SwapEvents != 2 {
		t.Fatalf("swap events recorded = %d, want 2 (loading + ready)", rep.SwapEvents)
	}
	rep2, err := eng.Sync(ctx)
	if err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if rep2.SwapEvents != 0 {
		t.Fatalf("replayed frames must not duplicate rows, recorded %d more", rep2.SwapEvents)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM swap_events`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("swap_events rows = %d, want 2", n)
	}
	var cold sql.NullInt64
	if err := db.QueryRow(`SELECT cold_load_ms FROM swap_events WHERE event='ready'`).Scan(&cold); err != nil {
		t.Fatalf("cold load: %v", err)
	}
	if !cold.Valid {
		t.Fatal("a loading/ready pair must yield a cold_load_ms")
	}
}
