// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored spine wiring for the epoch-aware mirror. Not a generated file.
// pp:data-source live

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/cliutil"
	"llamaswap-pp-cli/internal/config"
	"llamaswap-pp-cli/internal/mirror"
	"llamaswap-pp-cli/internal/store"
)

// spineSchemaVersion stamps every novel spine output so a consumer can detect a
// shape change without diffing fields.
const spineSchemaVersion = 1

// spineExitErr attaches one of the typed exit codes from exitcodes.go to an
// error. Novel commands never return bare ints; unattended callers branch on
// these codes and a wrong one is worse than a crash.
func spineExitErr(code int, err error) error { return &cliError{code: code, err: err} }

// spineBaseURL resolves the proxy base URL and forces a loopback ALIAS to its
// literal IP. Resolving a loopback hostname on this platform can stall ~21s on
// an ::1 first attempt before falling back to IPv4; every spine command must
// reach 127.0.0.1 directly. The check is by resolution, not by name, so any
// alias mapped to loopback in the hosts file is normalized too.
func spineBaseURL(flags *rootFlags) (string, error) {
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return "", configErr(err)
	}
	raw := strings.TrimSpace(cfg.BaseURL)
	if raw == "" {
		return mirror.DefaultBaseURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw, nil
	}
	host := u.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return strings.TrimRight(raw, "/"), nil
	}
	ips, lookupErr := net.LookupIP(host)
	if lookupErr != nil || len(ips) == 0 {
		return strings.TrimRight(raw, "/"), nil
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return strings.TrimRight(raw, "/"), nil
		}
	}
	port := u.Port()
	if port != "" {
		u.Host = net.JoinHostPort("127.0.0.1", port)
	} else {
		u.Host = "127.0.0.1"
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// spineClient builds the typed llama-swap client used by every spine command.
func spineClient(flags *rootFlags) (*mirror.Client, error) {
	base, err := spineBaseURL(flags)
	if err != nil {
		return nil, err
	}
	timeout := 30 * time.Second
	if flags != nil && flags.timeout > 0 {
		timeout = flags.timeout
	}
	return mirror.NewClient(base, timeout), nil
}

// spineOpenDB opens the local store and guarantees the domain schema exists.
func spineOpenDB(ctx context.Context, dbPath string) (*store.Store, error) {
	if dbPath == "" {
		dbPath = defaultDBPath("llamaswap-pp-cli")
	}
	db, err := store.OpenWithContext(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening local database: %w", err)
	}
	if err := store.EnsureDomainSchema(ctx, db.DB()); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensuring domain schema: %w", err)
	}
	return db, nil
}

// spineMirrorOptions carries the sync-time mirror knobs.
type spineMirrorOptions struct {
	DBPath      string
	EventWindow time.Duration
	PageLimit   int
	MaxPages    int
}

// spineWouldSyncReport is what a verify/dry-run pass emits: the mirror does no
// network work at all, and says so, instead of half-running against a mock.
type spineWouldSyncReport struct {
	SchemaVersion int    `json:"schema_version"`
	WouldSync     bool   `json:"would_sync"`
	Reason        string `json:"reason"`
	BaseURL       string `json:"base_url"`
	Action        string `json:"action"`
}

func spineWriteWouldSync(cmd *cobra.Command, flags *rootFlags, reason, action string) error {
	base, err := spineBaseURL(flags)
	if err != nil {
		base = mirror.DefaultBaseURL
	}
	rep := spineWouldSyncReport{
		SchemaVersion: spineSchemaVersion,
		WouldSync:     true,
		Reason:        reason,
		BaseURL:       base,
		Action:        action,
	}
	if flags != nil && flags.asJSON {
		return printJSONFiltered(cmd.OutOrStdout(), rep, flags)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "would sync: %s against %s (%s)\n", action, base, reason)
	return err
}

// spineRunMirror executes one mirror pass and renders the epoch/loss report.
func spineRunMirror(cmd *cobra.Command, flags *rootFlags, opts spineMirrorOptions) error {
	if cliutil.IsVerifyEnv() {
		return spineWriteWouldSync(cmd, flags, "PRINTING_PRESS_VERIFY=1: no network reads, no epoch writes", "mirror activity + drain events")
	}
	if dryRunOK(flags) {
		return spineWriteWouldSync(cmd, flags, "--dry-run", "mirror activity + drain events")
	}
	ctx := cmd.Context()
	db, err := spineOpenDB(ctx, opts.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	client, err := spineClient(flags)
	if err != nil {
		return err
	}
	eng := &mirror.Engine{
		DB:          db.DB(),
		Client:      client,
		EventWindow: opts.EventWindow,
		PageLimit:   opts.PageLimit,
		MaxPages:    opts.MaxPages,
	}
	rep, err := eng.Sync(ctx)
	if err != nil {
		return spineExitErr(ExitServerUnreachable, fmt.Errorf("mirror sync: %w", err))
	}
	return spineWriteMirrorReport(cmd, flags, rep)
}

// spineRunSeal force-seals the open epoch without touching the network.
func spineRunSeal(cmd *cobra.Command, flags *rootFlags, dbPath string) error {
	if cliutil.IsVerifyEnv() {
		return spineWriteWouldSync(cmd, flags, "PRINTING_PRESS_VERIFY=1: no network reads, no epoch writes", "seal the open epoch")
	}
	if dryRunOK(flags) {
		return spineWriteWouldSync(cmd, flags, "--dry-run", "seal the open epoch")
	}
	ctx := cmd.Context()
	db, err := spineOpenDB(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	client, err := spineClient(flags)
	if err != nil {
		return err
	}
	eng := &mirror.Engine{DB: db.DB(), Client: client}
	rep, err := eng.SealNow(ctx)
	if err != nil {
		return err
	}
	return spineWriteMirrorReport(cmd, flags, rep)
}

// spineRunWatch is a FOREGROUND poll loop. It does not daemonize, fork, or
// install a scheduled task — an unattended watcher is a house anti-feature.
// Ctrl-C returns cleanly with the last report already printed.
func spineRunWatch(cmd *cobra.Command, flags *rootFlags, opts spineMirrorOptions, interval time.Duration) error {
	if cliutil.IsVerifyEnv() {
		return spineWriteWouldSync(cmd, flags, "PRINTING_PRESS_VERIFY=1: no network reads, no epoch writes", "poll the mirror in the foreground")
	}
	if dryRunOK(flags) {
		return spineWriteWouldSync(cmd, flags, "--dry-run", "poll the mirror in the foreground")
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()

	watchCmd := *cmd
	watchCmd.SetContext(ctx)
	for {
		if err := spineRunMirror(&watchCmd, flags, opts); err != nil {
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "watch: %v\n", err)
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(cmd.ErrOrStderr(), "watch: interrupted; stopping cleanly")
			return nil
		case <-time.After(interval):
		}
	}
	return nil
}

// spineWriteMirrorReport renders a mirror report as JSON or a human summary.
//
// The human rendering states three things the numbers alone cannot: that the
// epoch count is a lower bound, that the post-poll tail is unknowable, and that
// a NULL loss means "not computable", never zero.
func spineWriteMirrorReport(cmd *cobra.Command, flags *rootFlags, rep *mirror.Report) error {
	if flags != nil && (flags.asJSON || flags.csv || flags.quiet || flags.plain) {
		return printJSONFiltered(cmd.OutOrStdout(), rep, flags)
	}
	if !isTerminal(cmd.OutOrStdout()) {
		return printJSONFiltered(cmd.OutOrStdout(), rep, flags)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "mirror: %d rows this pass, %d censored total, %d epoch(s) known\n",
		rep.RowsMirrored, rep.RowsCensored, len(rep.Epochs))
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "EPOCH\tSTATE\tROWS\tCENSORED\tMAX-ID\tIDS-DENSE\tLOSS-EVICTED\tLOSS-PREPOLL\tSEAL-REASON")
	for _, e := range rep.Epochs {
		fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%d\t%v\t%s\t%s\t%s\n",
			e.EpochID, e.State, e.Rows, e.CensoredRows, e.MaxActivityID, e.IDsDense,
			spineLossText(e.LossEvicted), spineLossText(e.LossPrepoll), orDash(e.SealReason))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "post-poll tail: %s (requests served after the last poll and before a restart leave no trace; never reported as a number)\n", rep.PostPollTail)
	fmt.Fprintln(w, "epoch count is a LOWER BOUND: two restarts between two polls are indistinguishable from one.")
	for _, warn := range rep.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warn)
	}
	return nil
}

// spineLossText renders a nullable loss. A NULL is "unknown", never 0: a
// fabricated zero is exactly the failure this whole accounting exists to avoid.
func spineLossText(v *int64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d", *v)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// spineWriteJSONLine is a small helper for NDJSON side-channel events.
func spineWriteJSONLine(w io.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintln(w, string(b))
}
