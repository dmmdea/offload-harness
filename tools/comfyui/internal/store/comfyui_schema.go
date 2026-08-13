// ComfyUI domain schema (Priority 0).
//
// NOT generated — hand-written and preserved across `printing-press generate --force`.
// Do not add the generated-file marker to this file.
//
// WHY THIS EXISTS AT ALL. The generated `resources` table is a blob store keyed by an
// extractable id. ComfyUI's endpoints are not CRUD: `/history` returns ONE object keyed by
// prompt_id, so the generic sync lands the entire render history in a single row
// (`resources[id='history']`). Every question that actually drove this tool — which arm OOM'd,
// which runs were slowest, what did that error say — is unanswerable against a blob.
//
// WHY A DURABLE STORE IS THE WHOLE POINT. ComfyUI's history is `self.history = {}` in RAM
// (execution.py), FIFO-evicted at MAXIMUM_HISTORY_SIZE=10000, and destroyed on every restart.
// Memory-knob experiments are precisely what force those restarts, so the server cannot retain
// the data an experiment needs. Nothing in the ecosystem keeps it either.
//
// DESIGN RULES ENCODED HERE, each from a real defect:
//   - Devices are keyed by UUID, never index: torch's cuda:N ordering is the INVERSE of
//     nvidia-smi's on this box, so an index is not an identity.
//   - `run_log_slice` deliberately has NO duration column. Timing may only come from
//     execution_start/execution_success timestamps; scraping the log text once produced a
//     false "+49% regression".
//   - `shape_sha` (seed-stripped) groups runs that share a performance shape; `graph_sha`
//     (exact) is what the submit lease dedupes on.
//   - `options_count = 0` on a recognised COMBO means the model CLASS is unregistered
//     (a missing extra_model_paths.yaml key) — NOT that a file is missing.
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ComfyUISchemaVersion tracks the domain schema independently of the generated
// StoreSchemaVersion, so a printing-press regeneration cannot silently roll it back.
const ComfyUISchemaVersion = 1

const comfyUIUserVersionKey = "comfyui_schema_version"

// comfyUIMigrations are idempotent. Order matters only where a table references another.
var comfyUIMigrations = []string{
	// ---- core spine -------------------------------------------------------

	// One distinct server identity. argv is part of the identity because a memory-knob
	// experiment changes argv, and arms legitimately span restarts with different argv.
	`CREATE TABLE IF NOT EXISTS server (
		id TEXT PRIMARY KEY,
		comfyui_version TEXT,
		frontend_version TEXT,
		python_version TEXT,
		torch_version TEXT,
		argv_json JSON,
		features_json JSON,
		first_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_seen DATETIME DEFAULT CURRENT_TIMESTAMP
	)`,

	// Keyed by UUID. comfy_index and nvidia_index are RECORDED but never used as identity:
	// they disagree on this box (torch cuda:0 = the 5070 Ti = nvidia-smi index 1).
	`CREATE TABLE IF NOT EXISTS device (
		uuid TEXT PRIMARY KEY,
		server_id TEXT NOT NULL,
		comfy_index INTEGER,
		nvidia_index INTEGER,
		name TEXT,
		vram_total INTEGER,
		pcie_link_width INTEGER,
		first_seen DATETIME DEFAULT CURRENT_TIMESTAMP
	)`,

	// One submission attempt — the spine. duration_ms is GENERATED so it can never be
	// written from anything but the two authoritative timestamps.
	`CREATE TABLE IF NOT EXISTS run (
		prompt_id TEXT PRIMARY KEY,
		name TEXT UNIQUE,
		graph_sha TEXT,
		shape_sha TEXT,
		server_id TEXT,
		object_info_snapshot_id INTEGER,
		submitted_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		state TEXT NOT NULL DEFAULT 'submitted',
		exit_class TEXT,
		execution_start_ms INTEGER,
		execution_success_ms INTEGER,
		duration_ms INTEGER GENERATED ALWAYS AS (
			CASE WHEN execution_success_ms IS NOT NULL AND execution_start_ms IS NOT NULL
			     THEN execution_success_ms - execution_start_ms END
		) VIRTUAL,
		queue_depth_at_submit INTEGER,
		batch_id TEXT,
		exp_arm_id INTEGER,
		node_errors_json JSON,
		error_node_id TEXT,
		error_node_type TEXT,
		error_exception_type TEXT,
		error_exception_message TEXT,
		error_traceback_tail TEXT,
		lease_pid INTEGER,
		lease_created_at DATETIME,
		lease_heartbeat_at DATETIME,
		completeness TEXT NOT NULL DEFAULT 'full',
		argv_json JSON
	)`,
	`CREATE INDEX IF NOT EXISTS idx_run_shape ON run(shape_sha)`,
	`CREATE INDEX IF NOT EXISTS idx_run_graph ON run(graph_sha)`,
	`CREATE INDEX IF NOT EXISTS idx_run_state ON run(state)`,
	`CREATE INDEX IF NOT EXISTS idx_run_batch ON run(batch_id)`,
	`CREATE INDEX IF NOT EXISTS idx_run_submitted ON run(submitted_at)`,

	// Immutable, content-addressed API graph. shape_sha256 strips seed and volatile widgets
	// so two runs that differ only by seed share a performance shape.
	`CREATE TABLE IF NOT EXISTS graph (
		sha256 TEXT PRIMARY KEY,
		shape_sha256 TEXT,
		api_json JSON NOT NULL,
		node_count INTEGER,
		class_histogram_json JSON,
		template_id TEXT,
		flatten_report_json JSON,
		first_seen DATETIME DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE INDEX IF NOT EXISTS idx_graph_shape ON graph(shape_sha256)`,

	// One slot mutation. A run reconstructs from template + patch chain, which is what makes
	// replay possible after the original argv and history are gone.
	`CREATE TABLE IF NOT EXISTS patch (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		graph_in_sha TEXT NOT NULL,
		graph_out_sha TEXT NOT NULL,
		address TEXT NOT NULL,
		node_id TEXT NOT NULL,
		expected_class TEXT NOT NULL,
		old_value JSON,
		new_value JSON,
		applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE INDEX IF NOT EXISTS idx_patch_in ON patch(graph_in_sha)`,

	// ---- measurement ------------------------------------------------------

	// Node-level durations are transient websocket events addressed to the submitting client
	// and are persisted by nothing on the server. Only the submitter can capture them.
	`CREATE TABLE IF NOT EXISTS run_node_timing (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL,
		node_id TEXT NOT NULL,
		class_type TEXT,
		started_ms INTEGER,
		ended_ms INTEGER,
		cached INTEGER NOT NULL DEFAULT 0,
		order_index INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_node_timing_run ON run_node_timing(run_id)`,

	// SAMPLED, and named so. A sample is not a peak; callers must not present it as one.
	`CREATE TABLE IF NOT EXISTS run_vram_sample (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL,
		t_ms INTEGER NOT NULL,
		device_uuid TEXT NOT NULL,
		vram_free INTEGER,
		vram_used INTEGER,
		executing_node_id TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_vram_run ON run_vram_sample(run_id)`,

	// NO duration column here, by design: timing must never leak in from log text.
	`CREATE TABLE IF NOT EXISTS run_log_slice (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		t_iso TEXT,
		text TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_logslice_run ON run_log_slice(run_id)`,

	// Probed media properties live here because fps/duration/has_audio are properties of the
	// produced FILE and appear in no ComfyUI endpoint — they are what a cross-model
	// comparison actually needs.
	`CREATE TABLE IF NOT EXISTS output (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL,
		node_id TEXT,
		output_key TEXT,
		filename TEXT NOT NULL,
		subfolder TEXT,
		type TEXT,
		abs_path TEXT,
		bytes INTEGER,
		width INTEGER,
		height INTEGER,
		fps REAL,
		duration_s REAL,
		has_audio INTEGER,
		probed_at DATETIME
	)`,
	`CREATE INDEX IF NOT EXISTS idx_output_run ON output(run_id)`,
	`CREATE INDEX IF NOT EXISTS idx_output_filename ON output(filename)`,

	// LoadImage takes a bare filename and /history records only that filename, so once the
	// input dir is cleaned every archived run referencing it becomes unreproducible.
	// Content-addressing the staged input is what keeps months-old comparisons valid.
	`CREATE TABLE IF NOT EXISTS input_asset (
		content_sha256 TEXT PRIMARY KEY,
		comfy_filename TEXT NOT NULL,
		host_path TEXT,
		bytes INTEGER,
		staged_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`,

	// ---- schema & catalog drift ------------------------------------------

	// Inserted ONLY when the content hash changes. /object_info is a live-only view with no
	// history; a custom-node update silently rewrites a node's inputs otherwise.
	`CREATE TABLE IF NOT EXISTS object_info_snapshot (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		server_id TEXT,
		fetched_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		content_sha256 TEXT NOT NULL UNIQUE,
		blob BLOB
	)`,

	`CREATE TABLE IF NOT EXISTS node_class (
		snapshot_id INTEGER NOT NULL,
		class_type TEXT NOT NULL,
		display_name TEXT,
		category TEXT,
		description TEXT,
		aliases_json JSON,
		output_types_json JSON,
		deprecated INTEGER NOT NULL DEFAULT 0,
		api_node INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (snapshot_id, class_type)
	)`,

	// spec_shape records whether the COMBO was legacy (options at index 0) or v3 (options at
	// index 1). A reader that assumes one shape mis-reads ~42% of inputs on this box.
	`CREATE TABLE IF NOT EXISTS node_input (
		snapshot_id INTEGER NOT NULL,
		class_type TEXT NOT NULL,
		name TEXT NOT NULL,
		kind TEXT,
		type_name TEXT,
		spec_shape TEXT,
		options_json JSON,
		options_count INTEGER,
		upload_kind TEXT,
		default_json JSON,
		min_value REAL,
		max_value REAL,
		step_value REAL,
		tooltip TEXT,
		PRIMARY KEY (snapshot_id, class_type, name)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_node_input_class ON node_input(class_type)`,

	`CREATE TABLE IF NOT EXISTS model_folder (
		folder_key TEXT PRIMARY KEY,
		paths_json JSON,
		last_seen DATETIME DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS model_file (
		folder_key TEXT NOT NULL,
		filename TEXT NOT NULL,
		size_bytes INTEGER,
		mtime DATETIME,
		first_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (folder_key, filename)
	)`,

	`CREATE TABLE IF NOT EXISTS template (
		id TEXT PRIMARY KEY,
		title TEXT,
		description TEXT,
		tags_json JSON,
		models_json JSON,
		media_type TEXT,
		source TEXT,
		asset_sha256 TEXT,
		ui_json_path TEXT,
		subgraph_count INTEGER,
		nested INTEGER NOT NULL DEFAULT 0,
		first_seen DATETIME DEFAULT CURRENT_TIMESTAMP
	)`,

	// exp_arm carries its OWN server_id: arms legitimately span restarts with different argv,
	// which is exactly why the experiment cannot live in the server's RAM history.
	`CREATE TABLE IF NOT EXISTS experiment (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		spec_json JSON,
		notes TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS exp_arm (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id INTEGER NOT NULL,
		label TEXT,
		server_id TEXT,
		vars_json JSON,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE INDEX IF NOT EXISTS idx_exparm_exp ON exp_arm(experiment_id)`,

	// A negative-result corpus. An OOM's execution_error lives in a RAM dict until the next
	// restart — and the restart is usually caused by the OOM.
	`CREATE TABLE IF NOT EXISTS failure_fingerprint (
		fingerprint TEXT PRIMARY KEY,
		exception_type TEXT,
		normalised_message TEXT,
		node_class TEXT,
		first_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
		occurrences INTEGER NOT NULL DEFAULT 1,
		resolved_by_run_id TEXT
	)`,

	`CREATE TABLE IF NOT EXISTS comfy_kv (
		k TEXT PRIMARY KEY,
		v TEXT
	)`,

	// One distinct NODE SET: which node classes and custom-node packs the
	// server offered. This is the half of reproducibility that `server` does
	// not cover — server records the binary's identity (version, argv), and
	// two runs can share all of that while a custom pack was installed,
	// upgraded, or removed between them, which silently changes what a graph
	// means. Capture only; nothing here restores anything.
	//
	// id is a content hash, so an unchanged node set is recorded once no
	// matter how many runs reference it.
	`CREATE TABLE IF NOT EXISTS node_set (
		id TEXT PRIMARY KEY,
		comfyui_version TEXT,
		class_count INTEGER,
		pack_count INTEGER,
		packs_json JSON,
		class_digest TEXT,
		source TEXT,
		captured_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_seen DATETIME DEFAULT CURRENT_TIMESTAMP
	)`,

	// run -> node_set as a LINK TABLE rather than a column on run.
	// MigrateComfyUI replays CREATE TABLE IF NOT EXISTS on every open, which
	// creates new tables on an existing database but does NOT add a column to
	// an existing one. A new column would therefore exist only in databases
	// created after this release, and every older store would fail the SELECT.
	// A separate table is the shape that migrates cleanly with no ALTER and no
	// schema-version bump.
	`CREATE TABLE IF NOT EXISTS run_node_set (
		prompt_id TEXT PRIMARY KEY,
		node_set_id TEXT NOT NULL,
		captured_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`,
}

// comfyUIFTS is created separately: FTS5 virtual tables are not IF-NOT-EXISTS-safe to
// re-declare with a changed shape, so shape changes bump ComfyUISchemaVersion instead.
var comfyUIFTS = []string{
	`CREATE VIRTUAL TABLE IF NOT EXISTS run_fts USING fts5(
		prompt_id UNINDEXED,
		name,
		prompt_text,
		error_text,
		class_types,
		tokenize = 'unicode61 remove_diacritics 2'
	)`,
	`CREATE VIRTUAL TABLE IF NOT EXISTS node_fts USING fts5(
		class_type,
		display_name,
		category,
		description,
		tokenize = 'unicode61 remove_diacritics 2'
	)`,
}

// MigrateComfyUI creates the ComfyUI domain schema. Idempotent and safe to call on every
// open. Kept separate from the generated Migrate so a regeneration cannot roll it back.
func MigrateComfyUI(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT CAST(v AS INTEGER) FROM comfy_kv WHERE k = ?), 0)`,
		comfyUIUserVersionKey,
	).Scan(&have); err != nil {
		// comfy_kv may not exist yet on a fresh DB; treat that as version 0.
		have = 0
	}
	if have > ComfyUISchemaVersion {
		return fmt.Errorf("comfyui domain schema version %d is newer than this binary supports (%d)", have, ComfyUISchemaVersion)
	}

	for _, stmt := range comfyUIMigrations {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("comfyui migration failed (%.60s...): %w", stmt, err)
		}
	}
	for _, stmt := range comfyUIFTS {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("comfyui fts migration failed (%.60s...): %w", stmt, err)
		}
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO comfy_kv(k, v) VALUES(?, ?)
		 ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		comfyUIUserVersionKey, fmt.Sprintf("%d", ComfyUISchemaVersion),
	); err != nil {
		return fmt.Errorf("recording comfyui schema version: %w", err)
	}
	return nil
}
