// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored shared plumbing for the measurement command family
// (gguf, vram, fit, ctx, bench, scratch, gate, build check, verify).
// Not a command: no pp:data-source marker.

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/config"
	"llamaswap-pp-cli/internal/measure"
	"llamaswap-pp-cli/internal/store"
)

// mcLoopback is the ONLY spelling of the local proxy host used by this
// command family. "localhost" resolves to ::1 first on this box and the
// llama-swap listener is IPv4-only, so every such call eats a ~21 second
// connect stall before falling back. A measurement command that stalls 21s
// measures the resolver, not the model.
const mcLoopback = "127.0.0.1"

// registerMeasureCommands wires the measurement family into the root command
// without editing generated root.go. Registration is additive and
// name-guarded, so a generator refresh cannot double-add or clobber.
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		addNovelCommandIfAbsent(root, newMeasureGgufCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureVramCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureFitCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureCtxCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureBenchCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureScratchCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureGateCmd(flags))
		addNovelCommandIfAbsent(root, newMeasureBuildCmd(flags))
	})
}

// mcBaseURL returns the proxy base URL with localhost rewritten to the IPv4
// loopback literal.
func mcBaseURL(flags *rootFlags) (string, error) {
	path := ""
	if flags != nil {
		path = flags.configPath
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", configErr(err)
	}
	return mcNormalizeHost(cfg.BaseURL), nil
}

func mcNormalizeHost(raw string) string {
	out := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	out = strings.ReplaceAll(out, "//localhost:", "//"+mcLoopback+":")
	out = strings.ReplaceAll(out, "//[::1]:", "//"+mcLoopback+":")
	return out
}

// mcTimeout raises the request deadline for commands that can legitimately
// wait on a multi-GB model load, unless the operator set --timeout
// explicitly (in which case their number wins).
func mcTimeout(cmd *cobra.Command, flags *rootFlags, floor time.Duration) time.Duration {
	if cmd != nil {
		if f := cmd.Flags().Lookup("timeout"); f != nil && f.Changed {
			return flags.timeout
		}
	}
	if flags != nil && flags.timeout > floor {
		return flags.timeout
	}
	return floor
}

type mcHTTPError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *mcHTTPError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 400 {
		body = body[:400] + "..."
	}
	return fmt.Sprintf("HTTP %d from %s %s: %s", e.Status, e.Method, e.Path, body)
}

// mcClassify maps a transport or HTTP failure onto the wave's typed exit
// codes so unattended callers branch on the code, not on a message.
func mcClassify(err error) error {
	if err == nil {
		return nil
	}
	var he *mcHTTPError
	if As(err, &he) {
		switch {
		case he.Status == http.StatusNotFound:
			return notFoundErr(err)
		case he.Status >= 500:
			return &cliError{code: ExitUpstream5xx, err: err}
		}
		return apiErr(err)
	}
	var ne net.Error
	if As(err, &ne) || isNetworkError(err) {
		return &cliError{code: ExitServerUnreachable, err: fmt.Errorf(
			"%w\nhint: is llama-swap listening on %s? `llamaswap-pp-cli server health` checks it", err, mcLoopback)}
	}
	return apiErr(err)
}

func mcDo(ctx context.Context, flags *rootFlags, method, path string, body any, timeout time.Duration) ([]byte, int, error) {
	base, err := mcBaseURL(flags)
	if err != nil {
		return nil, 0, err
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, resp.StatusCode, &mcHTTPError{Status: resp.StatusCode, Method: method, Path: path, Body: string(data)}
	}
	return data, resp.StatusCode, nil
}

func mcGetJSON(ctx context.Context, flags *rootFlags, path string, timeout time.Duration, out any) error {
	data, _, err := mcDo(ctx, flags, http.MethodGet, path, nil, timeout)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func mcPostJSON(ctx context.Context, flags *rootFlags, path string, body any, timeout time.Duration, out any) error {
	data, _, err := mcDo(ctx, flags, http.MethodPost, path, body, timeout)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// mcSeat is one entry of GET /running: a model that is loaded right now,
// with the exact command line it was started with.
type mcSeat struct {
	Model string `json:"model"`
	State string `json:"state"`
	Cmd   string `json:"cmd"`
	Proxy string `json:"proxy"`
	TTL   int    `json:"ttl"`
	Name  string `json:"name"`
}

type mcRunningEnvelope struct {
	Running []mcSeat `json:"running"`
}

// mcRunning lists loaded seats. Every /upstream probe in this family is
// gated on this list: hitting /upstream/{model}/... for an unloaded model
// makes llama-swap START it, which is a multi-GB side effect disguised as a
// read.
func mcRunning(ctx context.Context, flags *rootFlags, timeout time.Duration) ([]mcSeat, error) {
	var env mcRunningEnvelope
	if err := mcGetJSON(ctx, flags, "/running", timeout, &env); err != nil {
		return nil, err
	}
	return env.Running, nil
}

func mcFindSeat(seats []mcSeat, model string) (mcSeat, bool) {
	for _, s := range seats {
		if strings.EqualFold(s.Model, model) {
			return s, true
		}
	}
	return mcSeat{}, false
}

func mcLoadedNames(seats []mcSeat) []string {
	out := make([]string, 0, len(seats))
	for _, s := range seats {
		out = append(out, s.Model)
	}
	return out
}

// mcRosterEntry is one /v1/models row, including llama-swap's alias metadata.
type mcRosterEntry struct {
	ID   string `json:"id"`
	Meta struct {
		Llamaswap struct {
			Aliases []string `json:"aliases"`
		} `json:"llamaswap"`
	} `json:"meta"`
}

func mcRoster(ctx context.Context, flags *rootFlags, timeout time.Duration) ([]mcRosterEntry, error) {
	var env struct {
		Data []mcRosterEntry `json:"data"`
	}
	if err := mcGetJSON(ctx, flags, "/v1/models", timeout, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// mcResolveAlias maps an alias to its canonical roster id. Unknown names are
// returned unchanged with ok=false so the caller can emit ExitModelNotFound
// with the roster listed.
func mcResolveAlias(roster []mcRosterEntry, name string) (string, bool) {
	for _, e := range roster {
		if strings.EqualFold(e.ID, name) {
			return e.ID, true
		}
	}
	for _, e := range roster {
		for _, a := range e.Meta.Llamaswap.Aliases {
			if strings.EqualFold(a, name) {
				return e.ID, true
			}
		}
	}
	return name, false
}

func mcModelNotFound(name string, roster []mcRosterEntry) error {
	ids := make([]string, 0, len(roster))
	for _, e := range roster {
		ids = append(ids, e.ID)
	}
	return &cliError{code: ExitModelNotFound, err: fmt.Errorf(
		"model %q is not in the roster (ids: %s)", name, strings.Join(ids, ", "))}
}

// ---------------------------------------------------------------------------
// llama-server command-line parsing
// ---------------------------------------------------------------------------

// mcSplitCmd splits a llama-server command line into tokens, honoring double
// quotes so a quoted Windows path with a space stays one token.
func mcSplitCmd(cmd string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range cmd {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t' || r == '\n' || r == '\r') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// mcFlagValue returns the value of the first matching flag, supporting both
// "--flag value" and "--flag=value" spellings.
func mcFlagValue(tokens []string, names ...string) (string, bool) {
	for i, tok := range tokens {
		for _, name := range names {
			if tok == name {
				if i+1 < len(tokens) {
					return tokens[i+1], true
				}
				return "", true
			}
			if strings.HasPrefix(tok, name+"=") {
				return strings.TrimPrefix(tok, name+"="), true
			}
		}
	}
	return "", false
}

func mcFlagInt(tokens []string, names ...string) (int, bool) {
	if v, ok := mcFlagValue(tokens, names...); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n, true
		}
	}
	return 0, false
}

// mcIsLlamaServer reports whether a seat is a llama.cpp server. whisper-server
// seats exist on this roster; GGUF/ctx/fit checks must skip them with an
// honest note instead of false-positiving.
func mcIsLlamaServer(cmd string) bool {
	tokens := mcSplitCmd(cmd)
	if len(tokens) == 0 {
		return false
	}
	exe := strings.ToLower(tokens[0])
	exe = strings.ReplaceAll(exe, "\\", "/")
	if i := strings.LastIndex(exe, "/"); i >= 0 {
		exe = exe[i+1:]
	}
	return strings.HasPrefix(exe, "llama-server")
}

func mcSeatModelPath(cmd string) (string, bool) {
	return mcFlagValue(mcSplitCmd(cmd), "-m", "--model")
}

func mcSeatCtx(cmd string) (int, bool) {
	return mcFlagInt(mcSplitCmd(cmd), "-c", "--ctx-size")
}

func mcSeatCacheTypes(cmd string) (string, string) {
	tokens := mcSplitCmd(cmd)
	k, _ := mcFlagValue(tokens, "--cache-type-k", "-ctk")
	v, _ := mcFlagValue(tokens, "--cache-type-v", "-ctv")
	return k, v
}

func mcSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// CLI-side config extras
// ---------------------------------------------------------------------------

// mcExtras are measurement-family settings read from the CLI's own
// config.json. Kept in a separate struct (parsed from the same file) so the
// generated config type stays generator-owned.
type mcExtras struct {
	// GPURoles maps a GPU UUID to the operator's name for that card.
	GPURoles map[string]string `json:"gpu_roles"`
	// KeepSet is the protected resident set. Sourced from config, never
	// from the server's ttl field, which reports 0 for ttl:-1 models.
	KeepSet []string `json:"keep_set"`
	// ProbeTolerance overrides the default rerank-score tolerance.
	ProbeTolerance float64 `json:"probe_tolerance"`
	// Source records where the values came from for provenance output.
	Source string `json:"-"`
}

func mcLoadExtras(flags *rootFlags) mcExtras {
	out := mcExtras{GPURoles: map[string]string{}}
	path := ""
	if flags != nil {
		path = flags.configPath
	}
	if path == "" {
		if env := os.Getenv("LLAMASWAP_CONFIG"); env != "" {
			path = env
		} else if dir, err := cliutil.ConfigDir(); err == nil {
			path = dir + string(os.PathSeparator) + "config.json"
		}
	}
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var parsed mcExtras
	if err := json.Unmarshal(data, &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not parse measurement settings from %s: %v\n", path, err)
		return out
	}
	if parsed.GPURoles == nil {
		parsed.GPURoles = map[string]string{}
	}
	parsed.Source = path
	return parsed
}

// mcGPUs reads every card with role labels applied.
func mcGPUs(ctx context.Context, flags *rootFlags) ([]measure.GPU, error) {
	return measure.PerUUID(ctx, mcLoadExtras(flags).GPURoles)
}

// ---------------------------------------------------------------------------
// store
// ---------------------------------------------------------------------------

// mcOpenDomainStore opens the local SQLite store and ensures the domain
// tables exist. Persistence is best-effort by design: a measurement that
// cannot be recorded is still a measurement, so callers warn and continue.
func mcOpenDomainStore(ctx context.Context) (*store.Store, error) {
	s, err := store.OpenWithContext(ctx, defaultDBPath("llamaswap-pp-cli"))
	if err != nil {
		return nil, err
	}
	if err := store.EnsureDomainSchema(ctx, s.DB()); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// mcRecord runs fn against the domain store, warning (never failing the
// command) when persistence is unavailable.
func mcRecord(ctx context.Context, label string, fn func(s *store.Store) error) {
	if cliutil.IsVerifyEnv() {
		return
	}
	s, err := mcOpenDomainStore(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s not recorded (store unavailable): %v\n", label, err)
		return
	}
	defer s.Close()
	if err := fn(s); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s not recorded: %v\n", label, err)
	}
}

func mcNow() string { return time.Now().UTC().Format(time.RFC3339) }

// ---------------------------------------------------------------------------
// output
// ---------------------------------------------------------------------------

// mcEmit renders a typed result. Machine callers and redirected output get
// the JSON envelope through the shared pipeline (so --select/--compact/--csv
// behave identically to generated commands); interactive humans and
// --human-friendly get the prose rendering.
func mcEmit(cmd *cobra.Command, flags *rootFlags, payload any, pretty func(w io.Writer)) error {
	w := cmd.OutOrStdout()
	if pretty == nil || flags == nil || wantsMachineOutput(flags) || flags.agent || !wantsHumanTable(w, flags) {
		return printJSONFiltered(w, payload, flags)
	}
	pretty(w)
	return nil
}

// mcVerifyPlanOnly short-circuits a state-mutating command under the
// printing-press verifier: it prints what it would have done and returns
// without touching the GPUs. Distinct from --dry-run so both surfaces work.
func mcVerifyPlanOnly(cmd *cobra.Command, flags *rootFlags, action string, plan map[string]any) (bool, error) {
	if !cliutil.IsVerifyEnv() {
		return false, nil
	}
	if plan == nil {
		plan = map[string]any{}
	}
	plan["verify_mode"] = true
	plan["action"] = action
	plan["would"] = "run " + action + "; GPU state left untouched under PRINTING_PRESS_VERIFY"
	return true, printJSONFiltered(cmd.OutOrStdout(), plan, flags)
}

func mcFmtMiB(mib int) string {
	if mib >= 1024 {
		return fmt.Sprintf("%.2f GiB", float64(mib)/1024)
	}
	return fmt.Sprintf("%d MiB", mib)
}
