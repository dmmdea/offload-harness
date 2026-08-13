// ComfyUI domain repository (Priority 0).
//
// NOT generated — hand-written and preserved across regeneration.
//
// The canonicalisation helpers here are the load-bearing part. Two hashes are computed for
// every graph:
//
//	GraphSHA  — exact content identity. The submit lease dedupes on this, which is what makes
//	            the "wrapper resubmitted and burned 30 GPU-minutes" failure structurally
//	            impossible rather than merely documented.
//	ShapeSHA  — seed and volatile widgets stripped. Two runs differing only by seed share a
//	            performance shape, so a regression guard can compare like with like.
//
// Both are computed over a canonical serialisation (keys sorted, whitespace normalised) so
// that a semantically identical graph always hashes identically regardless of key order.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// volatileInputs are widget names excluded from ShapeSHA. Each changes per-run without
// changing the work performed, so including them would put every run in its own shape group
// and make the regression guard useless.
var volatileInputs = map[string]bool{
	"seed":                   true,
	"noise_seed":             true,
	"control_after_generate": true,
	"filename_prefix":        true,
}

// APINode is one node of a ComfyUI API-format graph.
type APINode struct {
	ClassType string                 `json:"class_type"`
	Inputs    map[string]interface{} `json:"inputs"`
	Meta      map[string]interface{} `json:"_meta,omitempty"`
}

// APIGraph is a ComfyUI API-format graph: node id -> node.
type APIGraph map[string]APINode

// canonicalJSON renders v with map keys sorted at every level, so hashing is order-stable.
// encoding/json already sorts map[string]T keys, but interface{} values decoded from JSON
// are map[string]interface{}, which it also sorts — so a round-trip through Marshal is
// sufficient and avoids hand-rolling a serialiser that could disagree with it.
func canonicalJSON(v interface{}) ([]byte, error) {
	// Round-trip to normalise number formatting and drop any struct-vs-map asymmetry.
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// GraphSHA is the exact content identity of an API graph.
func GraphSHA(g APIGraph) (string, error) {
	b, err := canonicalJSON(g)
	if err != nil {
		return "", fmt.Errorf("canonicalising graph: %w", err)
	}
	return sha256Hex(b), nil
}

// ShapeSHA is the identity of a graph with volatile inputs stripped. Runs sharing a ShapeSHA
// did the same work and are legitimately comparable on duration.
func ShapeSHA(g APIGraph) (string, error) {
	stripped := make(APIGraph, len(g))
	for id, node := range g {
		inputs := make(map[string]interface{}, len(node.Inputs))
		for k, v := range node.Inputs {
			if volatileInputs[k] {
				continue
			}
			inputs[k] = v
		}
		stripped[id] = APINode{ClassType: node.ClassType, Inputs: inputs}
	}
	b, err := canonicalJSON(stripped)
	if err != nil {
		return "", fmt.Errorf("canonicalising shape: %w", err)
	}
	return sha256Hex(b), nil
}

// ClassHistogram counts node classes in a graph — a cheap fingerprint for "what kind of work
// is this", used to group and to explain a shape at a glance.
func ClassHistogram(g APIGraph) map[string]int {
	h := make(map[string]int)
	for _, node := range g {
		h[node.ClassType]++
	}
	return h
}

// SortedClassTypes returns the distinct class types, sorted — for FTS indexing.
func SortedClassTypes(g APIGraph) []string {
	seen := make(map[string]bool)
	for _, node := range g {
		seen[node.ClassType] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// UpsertGraph stores a graph content-addressed and returns its GraphSHA. Storing the same
// graph twice is a no-op, so a replay never duplicates.
func UpsertGraph(ctx context.Context, db *sql.DB, g APIGraph, templateID string, flattenReport []byte) (string, error) {
	gsha, err := GraphSHA(g)
	if err != nil {
		return "", err
	}
	ssha, err := ShapeSHA(g)
	if err != nil {
		return "", err
	}
	apiJSON, err := canonicalJSON(g)
	if err != nil {
		return "", err
	}
	hist, err := json.Marshal(ClassHistogram(g))
	if err != nil {
		return "", err
	}
	var report interface{}
	if len(flattenReport) > 0 {
		report = string(flattenReport)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO graph (sha256, shape_sha256, api_json, node_count, class_histogram_json, template_id, flatten_report_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(sha256) DO NOTHING`,
		gsha, ssha, string(apiJSON), len(g), string(hist), nullIfEmpty(templateID), report,
	); err != nil {
		return "", fmt.Errorf("storing graph: %w", err)
	}
	return gsha, nil
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// RunRow is the durable record of one submission attempt.
type RunRow struct {
	PromptID     string
	Name         string
	GraphSHA     string
	ShapeSHA     string
	ServerID     string
	State        string
	ExitClass    string
	StartMS      sql.NullInt64
	SuccessMS    sql.NullInt64
	DurationMS   sql.NullInt64
	Completeness string
	NodeErrors   sql.NullString
	ErrorNodeID  sql.NullString
	ErrorExcType sql.NullString
	ErrorExcMsg  sql.NullString
	BatchID      sql.NullString
}

// InsertRun records a submission. The prompt_id is client-minted BEFORE the POST, so a lost
// reply is recoverable by lookup rather than by guessing which job was ours.
func InsertRun(ctx context.Context, db *sql.DB, r RunRow) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO run (prompt_id, name, graph_sha, shape_sha, server_id, state, completeness, batch_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(prompt_id) DO NOTHING`,
		r.PromptID, nullIfEmpty(r.Name), nullIfEmpty(r.GraphSHA), nullIfEmpty(r.ShapeSHA),
		nullIfEmpty(r.ServerID), defaultStr(r.State, "submitted"), defaultStr(r.Completeness, "full"),
		r.BatchID,
	)
	if err != nil {
		return fmt.Errorf("inserting run: %w", err)
	}
	return nil
}

func defaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// SetRunTiming records the ONLY authoritative duration source: ComfyUI's own
// execution_start / execution_success timestamps from /history.
//
// Timestamps arrive as epoch seconds or milliseconds depending on path; normalise by
// magnitude. A value below this threshold cannot be milliseconds since 1970 for any date
// after 1973, so it must be seconds.
const msThreshold = 100_000_000_000

// NormaliseEpochMS converts an epoch timestamp of unknown unit to milliseconds.
func NormaliseEpochMS(v float64) int64 {
	if v <= 0 {
		return 0
	}
	if v < msThreshold {
		return int64(v * 1000)
	}
	return int64(v)
}

// SetRunTiming writes the authoritative timestamps. It REFUSES a success-before-start pair
// rather than storing a negative duration, because a silently wrong duration is worse than a
// missing one — that is the class of defect that produced a false "+49% regression".
func SetRunTiming(ctx context.Context, db *sql.DB, promptID string, startMS, successMS int64) error {
	if startMS > 0 && successMS > 0 && successMS < startMS {
		return fmt.Errorf("refusing timing for %s: success_ms %d precedes start_ms %d", promptID, successMS, startMS)
	}
	_, err := db.ExecContext(ctx,
		`UPDATE run SET execution_start_ms = NULLIF(?, 0), execution_success_ms = NULLIF(?, 0) WHERE prompt_id = ?`,
		startMS, successMS, promptID)
	if err != nil {
		return fmt.Errorf("setting run timing: %w", err)
	}
	return nil
}

// FindActiveRunByGraphSHA backs the idempotent-attach lease: before submitting, ask whether
// this exact graph is already in flight. ComfyUI dedupes nothing, so without this every
// re-invocation mints a second 20-minute render.
func FindActiveRunByGraphSHA(ctx context.Context, db *sql.DB, graphSHA string) (string, bool, error) {
	var promptID string
	err := db.QueryRowContext(ctx,
		`SELECT prompt_id FROM run
		  WHERE graph_sha = ? AND state IN ('submitted','running','completed-outputs-pending')
		  ORDER BY submitted_at DESC LIMIT 1`,
		graphSHA).Scan(&promptID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("looking up active run: %w", err)
	}
	return promptID, true, nil
}

// SetRunState transitions a run and optionally records its exit class.
func SetRunState(ctx context.Context, db *sql.DB, promptID, state, exitClass string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE run SET state = ?, exit_class = COALESCE(NULLIF(?, ''), exit_class) WHERE prompt_id = ?`,
		state, exitClass, promptID)
	if err != nil {
		return fmt.Errorf("setting run state: %w", err)
	}
	return nil
}

// ShapeStats summarises the historical duration distribution for one performance shape.
// This is what the passive regression guard compares a new run against — a distribution of
// prior runs, never a log line.
type ShapeStats struct {
	ShapeSHA string
	N        int
	MeanMS   float64
	StdDevMS float64
	MinMS    int64
	MaxMS    int64
}

// ShapeStatsFor computes the duration distribution for a shape over completed runs.
func ShapeStatsFor(ctx context.Context, db *sql.DB, shapeSHA string) (ShapeStats, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT execution_success_ms - execution_start_ms AS d
		   FROM run
		  WHERE shape_sha = ? AND state = 'completed'
		    AND execution_start_ms IS NOT NULL AND execution_success_ms IS NOT NULL`,
		shapeSHA)
	if err != nil {
		return ShapeStats{}, fmt.Errorf("querying shape stats: %w", err)
	}
	defer rows.Close()

	st := ShapeStats{ShapeSHA: shapeSHA}
	var vals []int64
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			return ShapeStats{}, err
		}
		vals = append(vals, d)
	}
	if err := rows.Err(); err != nil {
		return ShapeStats{}, err
	}
	if len(vals) == 0 {
		return st, nil
	}
	st.N = len(vals)
	st.MinMS, st.MaxMS = vals[0], vals[0]
	var sum float64
	for _, v := range vals {
		sum += float64(v)
		if v < st.MinMS {
			st.MinMS = v
		}
		if v > st.MaxMS {
			st.MaxMS = v
		}
	}
	st.MeanMS = sum / float64(len(vals))
	if len(vals) > 1 {
		var sq float64
		for _, v := range vals {
			d := float64(v) - st.MeanMS
			sq += d * d
		}
		// Sample standard deviation (n-1): with a handful of runs the population form
		// understates spread and would over-flag the regression guard.
		st.StdDevMS = sqrt(sq / float64(len(vals)-1))
	}
	return st, nil
}

// sqrt avoids importing math for one call in a package that otherwise has no float deps.
func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 20; i++ {
		z = (z + x/z) / 2
	}
	return z
}

// ComboShape distinguishes where a COMBO's options live in the /object_info tuple.
type ComboShape string

const (
	ComboLegacy ComboShape = "legacy" // options at index 0
	ComboV3     ComboShape = "v3"     // options at index 1, under {"options": [...]}
	ComboNone   ComboShape = "primitive"
)

// ParseComboOptions reads a node input spec from /object_info and returns its options plus
// which shape they were found in.
//
// This is the single highest-value correctness helper in the CLI. ComfyUI ships BOTH shapes
// simultaneously — on this box 480 of 880 inputs are v3 and 400 are legacy — and a reader
// that assumes one shape silently reports "no options" for ~42% of inputs, which then reads
// as a missing model file. An EMPTY option list on a recognised COMBO means the model CLASS
// is unregistered (a missing extra_model_paths.yaml key), NOT that a file is absent.
func ParseComboOptions(spec interface{}) ([]string, ComboShape) {
	arr, ok := spec.([]interface{})
	if !ok || len(arr) == 0 {
		return nil, ComboNone
	}

	// v3: index 1 is an object carrying {"options": [...]}.
	if len(arr) > 1 {
		if meta, ok := arr[1].(map[string]interface{}); ok {
			if raw, ok := meta["options"]; ok {
				if opts, ok := raw.([]interface{}); ok {
					return toStrings(opts), ComboV3
				}
			}
		}
	}

	// legacy: index 0 is itself the option list.
	if opts, ok := arr[0].([]interface{}); ok {
		return toStrings(opts), ComboLegacy
	}

	return nil, ComboNone
}

func toStrings(in []interface{}) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ModelVisibility separates the four causes every other tool conflates.
type ModelVisibility string

const (
	ModelVisible           ModelVisibility = "visible"            // present and offered by the loader
	ModelClassUnregistered ModelVisibility = "class-unregistered" // COMBO exists but has ZERO options
	ModelNotListed         ModelVisibility = "not-listed"         // options exist, this file is not among them
	ModelNoSuchInput       ModelVisibility = "no-such-input"      // the input is not a COMBO at all
)

// ClassifyModelVisibility answers "why can't ComfyUI see my model?" honestly.
//
// The distinction that matters: an empty options list is NOT "file missing". It means the
// model CLASS has no registered folder — on this box that was a missing
// `latent_upscale_models` key in extra_model_paths.yaml, which surfaced only as the opaque
// validation error `value not in list: ... not in []`.
func ClassifyModelVisibility(spec interface{}, filename string) (ModelVisibility, []string) {
	opts, shape := ParseComboOptions(spec)
	if shape == ComboNone {
		return ModelNoSuchInput, nil
	}
	if len(opts) == 0 {
		return ModelClassUnregistered, nil
	}
	for _, o := range opts {
		if o == filename {
			return ModelVisible, opts
		}
	}
	return ModelNotListed, opts
}
