package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// forecastRunner runs one request in-process; *pipeline.Pipeline satisfies it.
type forecastRunner interface {
	Run(ctx context.Context, req core.Request) core.Result
}

// forecastRunFunc places one forecast request by route. It is the seam handleForecast and the CLI
// verb call through: Server.forecastRun (nil in production) resolves to forecastPlace, and a test
// injects a fake so the handler is exercisable without a seat or a fleet node.
type forecastRunFunc func(ctx context.Context, cfg config.Config, runner forecastRunner, req core.Request, route string) core.Result

// forecastPlace is the production placement: local and auto run the request on this box's pipeline,
// remote is refused because no fleet caller is wired. The fleet caller replaces this one function.
func forecastPlace(ctx context.Context, _ config.Config, runner forecastRunner, req core.Request, route string) core.Result {
	switch strings.ToLower(strings.TrimSpace(route)) {
	case "", "local", "auto":
		return runner.Run(ctx, req)
	case "remote":
		res := core.Deferf("route remote needs a fleet node serving forecast and none is reachable from this build", "", core.Meta{})
		res.DeferClass = core.DeferClassConfig
		return res
	}
	return forecastBadInputResult(fmt.Sprintf("unrecognized route %q; use local, auto or remote", route))
}

// forecastRouteSchema is the `route` property of offload_forecast. The default is auto: a box that
// binds the forecast model runs it at once, and a box that does not (or whose card is held) is
// served by a fleet node, because every node serves the same model and a spill costs no quality.
const forecastRouteSchema = `"route":{"type":"string","enum":["local","auto","remote"],"description":"where the forecast runs. auto (the DEFAULT): this box's own forecast model when it binds one and its card is free, otherwise a fleet node that serves forecast. local: only this box's model; without one the call defers as config. remote: force a fleet node; with none eligible it defers with defer_class capacity (config when no delegate_remotes are configured) and never touches the local GPU. The series is read on THIS box and travels with the job; a large result file is always written on THIS box. meta.node / meta.placement say where it ran"}`

// Limits of the file inputs and of an inline answer.
const (
	forecastMaxFileBytes  = 128 << 20 // series_path files larger than this are refused
	forecastInlineNumbers = 2000      // a result with more numbers than this is written to a file
	forecastEdgeSteps     = 8         // median steps kept at each end of an inline summary
)

// ForecastArgs are the offload_forecast arguments, shared by the MCP door and the CLI verb.
type ForecastArgs struct {
	Series                json.RawMessage `json:"series"`
	SeriesPath            string          `json:"series_path"`
	TargetColumns         []string        `json:"target_columns"`
	PastCovariateColumns  []string        `json:"past_covariate_columns"`
	KnownCovariateColumns []string        `json:"known_covariate_columns"`
	PredictionLength      int             `json:"prediction_length"`
	Quantiles             []float64       `json:"quantiles"`
	PastCovariates        json.RawMessage `json:"past_covariates"`
	KnownCovariates       json.RawMessage `json:"known_covariates"`
	OutPath               string          `json:"out_path"`
	Route                 string          `json:"route"`
	Select                []string        `json:"select"`
}

// forecastPayloadKeys are the keys the forecast server takes; a .json series file that is an object may
// carry any of them.
var forecastPayloadKeys = map[string]bool{
	"series": true, "prediction_length": true, "quantiles": true,
	"past_covariates": true, "known_covariates": true,
	"categorical_past_covariates": true, "categorical_known_covariates": true,
	"symmetric_averaging": true, "clip_nonnegative": true,
}

// RunForecast is the whole offload_forecast call: build the payload from the arguments, place the
// request by route (run nil is the production placement), spill a large answer to a file, and
// project select. A payload that cannot be built is a contract defer, never an error.
func RunForecast(ctx context.Context, cfg config.Config, runner forecastRunner, run forecastRunFunc, a ForecastArgs, door string) core.Result {
	input, err := BuildForecastPayload(a)
	if err != nil {
		return forecastBadInputResult(err.Error())
	}
	route := strings.TrimSpace(a.Route)
	if route == "" {
		route = "auto"
	}
	if run == nil {
		run = forecastPlace
	}
	res := run(ctx, cfg, runner, core.Request{Task: core.TaskForecast, Door: door, Input: input}, route)
	if res.OK && len(res.Data) > 0 {
		res = spillForecast(cfg, res, a.OutPath)
	}
	if len(a.Select) > 0 {
		res.Data = core.ProjectFields(res.Data, a.Select)
	}
	return res
}

// forecastBadInputResult is a call that cannot be run as given: the contract class with the
// bad_input error class, the same shape the pipeline files for the server's own 400.
func forecastBadInputResult(reason string) core.Result {
	res := core.Deferf(reason, "", core.Meta{ErrClass: core.ErrClassBadInput})
	res.DeferClass = core.DeferClassContract
	return res
}

func (s *Server) handleForecast(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in ForecastArgs
	if bad := parseArgs(req.Params.Arguments, &in); bad != nil {
		return bad, nil
	}
	// A forecast is quick, but a cold seat loads for ~20 s and a card held by a render can keep the
	// call waiting: with a progress token the client hears an opening notification and a heartbeat.
	stopHeartbeat := s.startHeartbeat(ctx, req, "offload_forecast")
	defer stopHeartbeat()
	return result(RunForecast(ctx, s.p.Cfg(), s.p, s.forecastRun, in, "offload_forecast"))
}

// BuildForecastPayload turns the arguments into the canonical JSON object the forecast server
// takes. Exactly one of Series and SeriesPath names the data; explicit arguments win over keys of
// an object .json file.
func BuildForecastPayload(a ForecastArgs) (string, error) {
	payload := map[string]any{}
	hasSeries := len(a.Series) > 0 && string(a.Series) != "null"
	switch {
	case hasSeries && a.SeriesPath != "":
		return "", errors.New("give series or series_path, not both")
	case !hasSeries && a.SeriesPath == "":
		return "", errors.New("give series (inline numbers) or series_path (a .csv or .json file on this box)")
	case hasSeries:
		if len(a.TargetColumns)+len(a.PastCovariateColumns)+len(a.KnownCovariateColumns) > 0 {
			return "", errors.New("target_columns, past_covariate_columns and known_covariate_columns select CSV columns and need series_path")
		}
		payload["series"] = a.Series
	default:
		if err := loadForecastFile(a, payload); err != nil {
			return "", err
		}
	}

	if a.PredictionLength != 0 {
		payload["prediction_length"] = a.PredictionLength
	}
	h, _ := payload["prediction_length"].(int)
	if f, ok := payload["prediction_length"].(float64); ok {
		h = int(f)
	}
	if h < 1 {
		return "", errors.New("prediction_length is required and must be at least 1")
	}
	if len(a.Quantiles) > 0 {
		for _, q := range a.Quantiles {
			if math.IsNaN(q) || q <= 0 || q >= 1 {
				return "", fmt.Errorf("quantile %v is outside (0, 1)", q)
			}
		}
		payload["quantiles"] = a.Quantiles
	}
	for key, raw := range map[string]json.RawMessage{"past_covariates": a.PastCovariates, "known_covariates": a.KnownCovariates} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		if _, set := payload[key]; set && a.SeriesPath != "" && strings.EqualFold(filepath.Ext(a.SeriesPath), ".csv") {
			return "", fmt.Errorf("%s came from the CSV columns; give the columns or %s, not both", key, key)
		}
		payload[key] = raw
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("the series is not valid JSON: %w", err)
	}
	return string(b), nil
}

// loadForecastFile reads series_path into payload: .json is the series itself (an array) or an
// object with payload keys, .csv is a table (loadForecastCSV).
func loadForecastFile(a ForecastArgs, payload map[string]any) error {
	path := a.SeriesPath
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("series_path: %w", err)
	}
	if st.IsDir() {
		return fmt.Errorf("series_path %q is a directory", path)
	}
	if st.Size() > forecastMaxFileBytes {
		return fmt.Errorf("series_path is %d bytes; the cap is %d", st.Size(), int64(forecastMaxFileBytes))
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		return loadForecastCSV(a, payload)
	case ".json":
		if len(a.TargetColumns)+len(a.PastCovariateColumns)+len(a.KnownCovariateColumns) > 0 {
			return errors.New("target_columns, past_covariate_columns and known_covariate_columns select CSV columns; a .json series file has none")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("series_path: %w", err)
		}
		raw = []byte(strings.TrimPrefix(strings.TrimSpace(string(raw)), utf8BOM))
		if len(raw) == 0 {
			return errors.New("series_path is empty")
		}
		switch raw[0] {
		case '[':
			payload["series"] = json.RawMessage(raw)
		case '{':
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(raw, &obj); err != nil {
				return fmt.Errorf("series_path is not valid JSON: %w", err)
			}
			for k, v := range obj {
				if !forecastPayloadKeys[k] {
					return fmt.Errorf("series_path: %q is not a forecast key (series, prediction_length, quantiles, past_covariates, known_covariates, categorical_past_covariates, categorical_known_covariates, symmetric_averaging, clip_nonnegative)", k)
				}
				if k == "prediction_length" {
					var n int
					if err := json.Unmarshal(v, &n); err != nil {
						return errors.New("series_path: prediction_length must be an integer")
					}
					payload[k] = n
					continue
				}
				payload[k] = v
			}
			if _, ok := payload["series"]; !ok {
				return errors.New("series_path: the object carries no series key")
			}
		default:
			return errors.New("series_path: a .json file holds an array of numbers (the series) or an object with forecast keys")
		}
		return nil
	}
	return fmt.Errorf("series_path %q: only .csv and .json files are read", path)
}

// loadForecastCSV reads a table into payload. The first row is the header. Target columns become
// the series (one column: a single series; several: independent series), past covariates the
// covariate rows over the context, known covariates the rows over context plus horizon. Trailing
// rows whose every target cell is empty are the horizon: they must number prediction_length and
// they carry the known covariates' future values. A column that is not numeric (a date, an id) is
// ignored unless named.
func loadForecastCSV(a ForecastArgs, payload map[string]any) error {
	f, err := os.Open(a.SeriesPath)
	if err != nil {
		return fmt.Errorf("series_path: %w", err)
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.TrimLeadingSpace = true
	header, err := rd.Read()
	if err != nil {
		return fmt.Errorf("series_path: reading the header row: %w", err)
	}
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], utf8BOM)
	}
	col := map[string]int{}
	for i, h := range header {
		h = strings.TrimSpace(h)
		header[i] = h
		if _, dup := col[h]; !dup && h != "" {
			col[h] = i
		}
	}
	var rows [][]string
	for {
		rec, err := rd.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("series_path: %w", err)
		}
		rows = append(rows, rec)
	}
	if len(rows) == 0 {
		return errors.New("series_path: the CSV has a header and no data rows")
	}

	named := func(label string, names []string) ([]int, error) {
		idx := make([]int, 0, len(names))
		for _, n := range names {
			i, ok := col[strings.TrimSpace(n)]
			if !ok {
				return nil, fmt.Errorf("%s: no column %q in the CSV header (%s)", label, n, strings.Join(header, ", "))
			}
			idx = append(idx, i)
		}
		return idx, nil
	}
	pastIdx, err := named("past_covariate_columns", a.PastCovariateColumns)
	if err != nil {
		return err
	}
	knownIdx, err := named("known_covariate_columns", a.KnownCovariateColumns)
	if err != nil {
		return err
	}
	var targetIdx []int
	if len(a.TargetColumns) > 0 {
		if targetIdx, err = named("target_columns", a.TargetColumns); err != nil {
			return err
		}
	} else {
		taken := map[int]bool{}
		for _, i := range append(append([]int{}, pastIdx...), knownIdx...) {
			taken[i] = true
		}
		for i := range header {
			if !taken[i] && csvColumnNumeric(rows, i) {
				targetIdx = append(targetIdx, i)
			}
		}
		if len(targetIdx) == 0 {
			return errors.New("target_columns: the CSV has no numeric column to forecast; name the columns")
		}
	}

	read := func(label string, idx []int) ([][]*float64, error) {
		out := make([][]*float64, len(idx))
		for k, c := range idx {
			out[k] = make([]*float64, len(rows))
			for r, rec := range rows {
				v, err := csvCell(rec, c)
				if err != nil {
					return nil, fmt.Errorf("%s %q, data row %d: %w", label, header[c], r+1, err)
				}
				out[k][r] = v
			}
		}
		return out, nil
	}
	targets, err := read("target column", targetIdx)
	if err != nil {
		return err
	}
	// The horizon: trailing rows with every target cell empty.
	n := len(rows)
	horizon := 0
	for r := n - 1; r >= 0; r-- {
		empty := true
		for _, t := range targets {
			if t[r] != nil {
				empty = false
				break
			}
		}
		if !empty {
			break
		}
		horizon++
	}
	ctxLen := n - horizon
	if ctxLen == 0 {
		return errors.New("series_path: every target cell is empty")
	}
	h := a.PredictionLength
	if h == 0 {
		if v, ok := payload["prediction_length"].(int); ok {
			h = v
		}
	}
	if horizon > 0 && horizon != h {
		return fmt.Errorf("series_path: %d trailing rows have every target cell empty (the horizon) but prediction_length is %d", horizon, h)
	}
	if len(knownIdx) > 0 && horizon != h {
		return fmt.Errorf("known_covariate_columns need the future values: the CSV must end with prediction_length (%d) rows whose target cells are empty, found %d", h, horizon)
	}

	series := make([][]*float64, len(targets))
	for k, t := range targets {
		series[k] = t[:ctxLen]
	}
	if len(series) == 1 {
		payload["series"] = series[0]
	} else {
		payload["series"] = series
	}
	if len(pastIdx) > 0 {
		past, err := read("past covariate column", pastIdx)
		if err != nil {
			return err
		}
		for k := range past {
			past[k] = past[k][:ctxLen]
		}
		payload["past_covariates"] = forecastCovariates(past, len(series))
	}
	if len(knownIdx) > 0 {
		known, err := read("known covariate column", knownIdx)
		if err != nil {
			return err
		}
		payload["known_covariates"] = forecastCovariates(known, len(series))
	}
	return nil
}

// forecastCovariates shapes C covariate rows for B series: (C, T) for one series, (B, C, T) with the
// same rows beside each series for several.
func forecastCovariates(rows [][]*float64, series int) any {
	if series == 1 {
		return rows
	}
	out := make([][][]*float64, series)
	for i := range out {
		out[i] = rows
	}
	return out
}

// csvColumnNumeric reports whether column c holds at least one number and nothing else but empty
// cells: the test that leaves a date or an id column out of the default targets.
func csvColumnNumeric(rows [][]string, c int) bool {
	seen := false
	for _, rec := range rows {
		v, err := csvCell(rec, c)
		if err != nil {
			return false
		}
		if v != nil {
			seen = true
		}
	}
	return seen
}

// csvCell parses one cell: an empty cell or NA/NaN/null/none is a missing value (nil), a number is
// a number, anything else (and an infinity) is an error.
func csvCell(rec []string, c int) (*float64, error) {
	if c >= len(rec) {
		return nil, nil
	}
	s := strings.TrimSpace(rec[c])
	switch strings.ToLower(s) {
	case "", "na", "n/a", "nan", "null", "none":
		return nil, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not a number", s)
	}
	if math.IsNaN(v) {
		return nil, nil
	}
	if math.IsInf(v, 0) {
		return nil, fmt.Errorf("%q is not a finite number", s)
	}
	return &v, nil
}

// spillForecast writes a large answer to a file and keeps a summary inline: shape, levels,
// result_path and the first and last forecastEdgeSteps median steps of every series. A result
// within forecastInlineNumbers numbers comes back as it is.
func spillForecast(cfg config.Config, res core.Result, outPath string) core.Result {
	var obj map[string]json.RawMessage
	if json.Unmarshal(res.Data, &obj) != nil {
		return res
	}
	var median any
	var quantiles map[string]any
	_ = json.Unmarshal(obj["median"], &median)
	_ = json.Unmarshal(obj["quantiles"], &quantiles)
	count := countNumbers(median)
	for _, q := range quantiles {
		count += countNumbers(q)
	}
	if count <= forecastInlineNumbers {
		return res
	}

	path := strings.TrimSpace(outPath)
	if path == "" {
		sum := sha256.Sum256(res.Data)
		path = filepath.Join(cfg.MediaDir, "forecast-"+hex.EncodeToString(sum[:])[:8]+".json")
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return forecastWriteFailed(res, count, err)
	}
	if err := os.WriteFile(path, res.Data, 0o644); err != nil {
		return forecastWriteFailed(res, count, err)
	}

	summary := map[string]any{}
	for k, v := range obj {
		if k != "median" && k != "quantiles" {
			summary[k] = v
		}
	}
	summary["result_path"] = path
	summary["median_head"] = edgeSteps(median, true)
	summary["median_tail"] = edgeSteps(median, false)
	b, err := json.Marshal(summary)
	if err != nil {
		return forecastWriteFailed(res, count, err)
	}
	res.Data = b
	return res
}

// forecastWriteFailed is a large answer that cannot be written where it should go. It is a
// capacity defer on this box (the disk), and the forecast itself is lost with it.
func forecastWriteFailed(res core.Result, count int, err error) core.Result {
	out := core.Deferf(fmt.Sprintf("the forecast has %d numbers, too many to return inline, and writing the result file failed: %v", count, err), "", res.Meta)
	out.DeferClass = core.DeferClassCapacity
	return out
}

// countNumbers counts the numeric leaves (null included, as the missing steps they are) of a
// decoded JSON value.
func countNumbers(v any) int {
	switch t := v.(type) {
	case []any:
		n := 0
		for _, e := range t {
			n += countNumbers(e)
		}
		return n
	case float64, nil:
		return 1
	}
	return 0
}

// edgeSteps keeps the first (or last) forecastEdgeSteps steps of every series of a decoded median:
// the innermost arrays are the time axis, so the nesting of the input's rank is kept.
func edgeSteps(v any, head bool) any {
	a, ok := v.([]any)
	if !ok {
		return v
	}
	if len(a) > 0 {
		if _, nested := a[0].([]any); nested {
			out := make([]any, len(a))
			for i, e := range a {
				out[i] = edgeSteps(e, head)
			}
			return out
		}
	}
	k := forecastEdgeSteps
	if k > len(a) {
		k = len(a)
	}
	if head {
		return a[:k]
	}
	return a[len(a)-k:]
}

// utf8BOM is the byte order mark a spreadsheet export puts before the first cell.
var utf8BOM = string(rune(0xFEFF))
