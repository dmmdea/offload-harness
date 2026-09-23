// Command opencode-context is a read-only context instrument for opencode. It copies
// opencode's session database (plus its -wal and -shm) to a temp dir, reads the copy and
// reports, per session and agent: first-call prompt tokens, per-call prompt / cached /
// output / reasoning tokens, the prefix-cache read ratio, what made the context grow
// between calls, compaction events and time to first token. The measurement contract
// lives in internal/occontext; docs: docs/systems/opencode-integration.md.
//
//	go run ./cmd/opencode-context --last 5 --cache-valid-since 2026-09-22T17:31:00Z --cache-block 1568
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/occontext"
)

// defaultDB is where opencode keeps its session store: the XDG data dir, which opencode
// resolves the same way on every OS (on Windows it is %USERPROFILE%\.local\share).
func defaultDB() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "opencode", "opencode.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db")
}

type cutoffs []occontext.Cutoff

func (c *cutoffs) String() string { return fmt.Sprint(*c) }
func (c *cutoffs) Set(v string) error {
	cut, err := occontext.ParseCutoff(v)
	if err != nil {
		return err
	}
	*c = append(*c, cut)
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "opencode-context:", err)
		}
		os.Exit(2)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("opencode-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		db       = fs.String("db", defaultDB(), "opencode session db; it is COPIED (with -wal and -shm) before anything reads it")
		last     = fs.Int("last", 10, "the N most recent primary sessions, each with its child sessions (0: all)")
		since    = fs.String("since", "", "only sessions created at or after this time (RFC 3339, or local YYYY-MM-DD[ HH:MM])")
		sessions = fs.String("session", "", "comma-separated session ids (each with its children); overrides --last")
		model    = fs.String("model", "", "only calls whose provider/model contains this")
		block    = fs.Int("cache-block", 0, "server prefix-cache block in tokens; cached counts are checked for whole blocks (0: off)")
		toolBPT  = fs.Float64("tool-bytes-per-token", 2.7, "bytes per token for estimating tool output in the growth split")
		textBPT  = fs.Float64("text-bytes-per-token", 3.5, "bytes per token for estimating prose: reasoning a server did not count, and user text")
		titles   = fs.Bool("titles", false, "print session titles (off: the report carries no session content)")
		asJSON   = fs.Bool("json", false, "print the report as JSON")
		cuts     cutoffs
	)
	fs.Var(&cuts, "cache-valid-since", "[MODEL=]TIME: cache figures of calls before TIME are reporting artifacts and leave every ratio (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *db == "" {
		return errors.New("no --db given and no home directory to find opencode's db in")
	}
	if *toolBPT <= 0 || *textBPT <= 0 {
		return errors.New("bytes-per-token ratios must be positive")
	}
	o := occontext.Options{Last: *last, Model: *model, CacheValidSince: cuts, CacheBlock: *block,
		ToolBytesPerToken: *toolBPT, TextBytesPerToken: *textBPT, Titles: *titles}
	if *since != "" {
		t, err := occontext.ParseTime(*since)
		if err != nil {
			return err
		}
		o.Since = t
	}
	if *sessions != "" {
		for _, id := range strings.Split(*sessions, ",") {
			if id = strings.TrimSpace(id); id != "" {
				o.Sessions = append(o.Sessions, id)
			}
		}
		o.Last = 0
	}

	snap, err := occontext.Snapshot(*db)
	if err != nil {
		return err
	}
	defer snap.Close()
	store, err := occontext.Load(snap.DB)
	if err != nil {
		return err
	}
	r := occontext.Analyze(store, o)
	r.Source = snap.Info
	r.Source.UnparsedRows = store.Unparsed
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	occontext.WriteTable(stdout, r)
	return nil
}
