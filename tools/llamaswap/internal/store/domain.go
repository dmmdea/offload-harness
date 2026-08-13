package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Domain schema for llama-swap operational history. Additive to the generated
// migration chain: commands call EnsureDomainSchema lazily instead of touching
// the generated Migrate path, so a printing-press regen never conflicts.
//
// Table ownership (build waves — do not write to tables you don't own):
//
//	spine (wave A):   epochs, requests, swap_events, unload_provenance, service_lifecycle
//	config (wave B):  seat_config_history, bindings_audit
//	measure (wave C): bench_runs, vram_snapshots, ctx_probes, probe_baselines
//
// Epoch semantics (binding, from the roast Logician contract):
//   - epochs.state is one of 'open' | 'unknown' | 'sealed'. UNKNOWN means polls
//     are failing — absence of evidence seals nothing.
//   - Sealing an epoch marks its non-terminal request rows censored=1 (neither
//     completed nor failed; long requests are likelier to be censored — biased).
//   - Loss is THREE quantities, never one: loss_evicted (exact iff activity ids
//     are dense — assert density continuously, degrade to NULL when violated),
//     loss_prepoll (derivable only from a cumulative counter), and the
//     post-last-poll tail, which is UNCOMPUTABLE and must never be reported as 0.
//   - Epoch count is a lower bound (two restarts between polls are one boundary).
func EnsureDomainSchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS epochs (
			epoch_id INTEGER PRIMARY KEY AUTOINCREMENT,
			witness TEXT NOT NULL,
			first_seen_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('open','unknown','sealed')),
			seal_reason TEXT,
			max_activity_id INTEGER,
			total_requests_last INTEGER,
			ids_dense INTEGER NOT NULL DEFAULT 1,
			loss_evicted INTEGER,
			loss_prepoll INTEGER,
			notes TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS requests (
			epoch_id INTEGER NOT NULL,
			activity_id INTEGER NOT NULL,
			ts TEXT,
			model TEXT,
			req_path TEXT,
			status INTEGER,
			cache_tokens INTEGER, input_tokens INTEGER, output_tokens INTEGER,
			draft_tokens INTEGER, draft_acc_tokens INTEGER,
			prompt_per_second REAL, tokens_per_second REAL,
			duration_ms INTEGER,
			censored INTEGER NOT NULL DEFAULT 0,
			raw JSON,
			PRIMARY KEY (epoch_id, activity_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_model_ts ON requests(model, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_status ON requests(status)`,
		`CREATE TABLE IF NOT EXISTS swap_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			epoch_id INTEGER,
			ts TEXT NOT NULL,
			model TEXT NOT NULL,
			event TEXT NOT NULL CHECK(event IN ('loading','ready','unloading','unloaded','failed')),
			cold_load_ms INTEGER,
			source TEXT NOT NULL,
			details JSON
		)`,
		`CREATE INDEX IF NOT EXISTS idx_swap_events_model ON swap_events(model, ts)`,
		`CREATE TABLE IF NOT EXISTS unload_provenance (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			model TEXT NOT NULL,
			caller TEXT NOT NULL,
			drained INTEGER,
			forced INTEGER NOT NULL DEFAULT 0,
			result TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS service_lifecycle (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			event TEXT NOT NULL,
			version TEXT,
			details TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS seat_config_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_file TEXT NOT NULL,
			content_sha TEXT NOT NULL,
			file_mtime TEXT,
			model TEXT NOT NULL,
			cmd_sha TEXT,
			full_cmd TEXT,
			comment_block TEXT,
			first_seen_at TEXT NOT NULL,
			UNIQUE(content_sha, model)
		)`,
		`CREATE TABLE IF NOT EXISTS bindings_audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			harness_config_sha TEXT,
			key TEXT NOT NULL,
			model TEXT,
			resolved_ok INTEGER NOT NULL,
			note TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS bench_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			model TEXT NOT NULL,
			config_sha TEXT,
			llamaswap_version TEXT,
			build_info TEXT,
			pp_median REAL, tg_median REAL, tg_max REAL,
			prompt_n INTEGER,
			cold_load_ms INTEGER,
			vram_delta_mib INTEGER,
			gpu_uuid TEXT,
			clean_state INTEGER,
			runs INTEGER, max_tokens INTEGER,
			concurrency INTEGER NOT NULL DEFAULT 1,
			notes TEXT,
			kv_depth INTEGER NOT NULL DEFAULT 0,
			kv_depth_observed INTEGER,
			pp_mean REAL, pp_stddev REAL,
			tg_mean REAL, tg_stddev REAL,
			cache_hit_rate REAL,
			comparability_sha TEXT,
			comparability_key TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS vram_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			gpu_uuid TEXT NOT NULL,
			gpu_role TEXT,
			model TEXT,
			ctx INTEGER,
			baseline_mib INTEGER,
			after_mib INTEGER,
			delta_mib INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS ctx_probes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			model TEXT NOT NULL,
			n_ctx_live INTEGER,
			prompt_label TEXT,
			real_tokens INTEGER,
			room INTEGER,
			verdict TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS probe_baselines (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL CHECK(kind IN ('embed','rerank')),
			model TEXT NOT NULL,
			input_sha TEXT NOT NULL,
			expected JSON NOT NULL,
			tolerance REAL NOT NULL,
			created_at TEXT NOT NULL,
			UNIQUE(kind, model, input_sha)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return ensureDomainColumns(ctx, db)
}

// domainAddedColumns are columns added to a domain table AFTER it shipped.
// CREATE TABLE IF NOT EXISTS is a no-op on a store that already has the
// table, so a new column reaches an existing database only through ALTER.
// Every entry must be nullable or carry a DEFAULT: SQLite cannot add a NOT
// NULL column without one, and back-filling a measurement column with a
// fabricated value is worse than leaving it NULL.
var domainAddedColumns = []struct{ table, column, ddl string }{
	// wave LS-1: per-depth bench rows with dispersion and a comparability key.
	{"bench_runs", "kv_depth", "INTEGER NOT NULL DEFAULT 0"},
	{"bench_runs", "kv_depth_observed", "INTEGER"},
	{"bench_runs", "pp_mean", "REAL"},
	{"bench_runs", "pp_stddev", "REAL"},
	{"bench_runs", "tg_mean", "REAL"},
	{"bench_runs", "tg_stddev", "REAL"},
	{"bench_runs", "cache_hit_rate", "REAL"},
	{"bench_runs", "comparability_sha", "TEXT"},
	{"bench_runs", "comparability_key", "TEXT"},
}

// ensureDomainColumns applies the additive column migrations idempotently by
// reading the live table shape first. Relying on the "duplicate column name"
// error string would couple the migration to a driver message.
func ensureDomainColumns(ctx context.Context, db *sql.DB) error {
	existing := map[string]map[string]bool{}
	for _, c := range domainAddedColumns {
		if _, ok := existing[c.table]; ok {
			continue
		}
		cols, err := tableColumns(ctx, db, c.table)
		if err != nil {
			return err
		}
		existing[c.table] = cols
	}
	for _, c := range domainAddedColumns {
		if existing[c.table][c.column] {
			continue
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.ddl)); err != nil {
			return fmt.Errorf("adding %s.%s: %w", c.table, c.column, err)
		}
		existing[c.table][c.column] = true
	}
	return nil
}

func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("reading %s columns: %w", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			return nil, fmt.Errorf("scanning %s columns: %w", table, err)
		}
		out[name] = true
	}
	return out, rows.Err()
}
