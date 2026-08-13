package store

import (
	"context"
	"database/sql"
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
			notes TEXT
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
	return nil
}
