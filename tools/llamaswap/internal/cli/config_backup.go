// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// pp:data-source local

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/lsconfig"
)

// BackupIndexName is the sidecar index appended to on every backup. It is the
// content-addressed counterpart to the filename: filenames are labels, this
// file records the hashes.
const BackupIndexName = "backups-index.json"

// backupIndexEntry is one row of the sidecar index.
type backupIndexEntry struct {
	Sha256      string `json:"sha256"`
	File        string `json:"file"`
	Label       string `json:"label,omitempty"`
	CreatedAt   string `json:"created_at"`
	SourcePath  string `json:"source_path"`
	SourceMtime string `json:"source_mtime"`
	SourceSize  int64  `json:"source_size"`
}

type backupResult struct {
	SchemaVersion string `json:"schema_version"`
	Source        string `json:"source"`
	SourceSha     string `json:"source_sha256"`
	SourceMtime   string `json:"source_mtime"`
	Dir           string `json:"dir"`
	File          string `json:"file,omitempty"`
	IndexFile     string `json:"index_file"`
	Written       bool   `json:"written"`
	// Dedup is set when the live content is already archived under some other
	// filename. No second copy is written; the existing one is named.
	Dedup   bool     `json:"dedup"`
	DedupOf []string `json:"dedup_of,omitempty"`
	// OrphanBackups are existing backups byte-identical to the LIVE file.
	// Finding one means a change was prepared and never applied (or a backup
	// was taken after the fact). Either way, intent and reality disagree.
	OrphanBackups []string `json:"orphan_backups,omitempty"`
	CorpusSources int      `json:"corpus_sources"`
	Notes         []string `json:"notes,omitempty"`
}

const backupSchemaVersion = "backup/1"

var labelSanitizer = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func newConfigBackupCmd(flags *rootFlags) *cobra.Command {
	var (
		label     string
		dir       string
		configArg string
	)

	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Copy the live config to a content-addressed backup and record it in a sidecar index; report dedup and orphan backups.",
		Long: "Take a content-addressed backup of the live llama-swap config.\n\n" +
			"The new file is named <sha10>-<label>.yaml and a row is appended to " + BackupIndexName + ",\n" +
			"recording the sha256, the label, the creation time, and the SOURCE file's\n" +
			"mtime. That combination is what makes the archive trustworthy: a filename is\n" +
			"a label an operator typed and can get wrong (dated backups on real corpora\n" +
			"routinely disagree with their own mtimes), while a content hash cannot.\n\n" +
			"Two findings come free with every run:\n" +
			"  dedup  — the live content is already archived under a different name, so\n" +
			"           nothing is written and the existing copy is named instead.\n" +
			"  orphan — an EXISTING backup is byte-identical to the live file. That means\n" +
			"           a change was staged and never applied, or the backup was taken\n" +
			"           after the change rather than before it. Reported as a fact.\n\n" +
			"This command NEVER modifies the config it copies, and never overwrites an\n" +
			"existing backup: a colliding sha means that content is already archived.",
		Example: "  llamaswap-pp-cli config backup --label pre-reranker-gpu\n" +
			"  llamaswap-pp-cli config backup --dry-run --json\n" +
			"  llamaswap-pp-cli config backup --dir ./config-archive",
		Annotations: map[string]string{
			// Deliberately NOT mcp:read-only: this command creates files.
			"mcp:local-write":     "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,2,%d", ExitConfigInvalid),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			src, err := resolveConfigPath([]string{configArg}, 0)
			if err != nil {
				return err
			}
			res, err := runBackup(src, dir, label, !verifyOrDryRun(flags))
			if err != nil {
				return err
			}
			if wantsJSON(out, flags) {
				return printJSONFiltered(out, res, flags)
			}
			printBackupHuman(cmd, res)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "human label for the backup filename (e.g. pre-reranker-gpu); filenames are labels only, the sha is the identity")
	cmd.Flags().StringVar(&dir, "dir", "", "directory to write the backup into (default: alongside the config, matching the existing convention)")
	cmd.Flags().StringVar(&configArg, "config-file", "", "llama-swap YAML to back up (default: the live config)")
	return cmd
}

// verifyOrDryRun reports whether side effects must be suppressed. Both gates
// are checked: the printing-press verifier's env var (so a verify pass never
// writes a file into an operator's config directory) and --dry-run.
func verifyOrDryRun(flags *rootFlags) bool {
	return cliutilIsVerifyEnv() || dryRunOK(flags)
}

// runBackup performs the (optionally suppressed) copy plus the corpus
// findings. write=false computes and reports everything without touching the
// filesystem — which is exactly what makes the orphan-backup finding usable as
// a pure read.
func runBackup(src, dir, label string, write bool) (*backupResult, error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", src, err)
	}
	st, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", src, err)
	}
	f, err := lsconfig.ParseBytes(raw, src, lsconfig.LoadOptions{})
	if err != nil {
		return nil, errConfigInvalid(err)
	}
	srcAbs, _ := filepath.Abs(src)
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Dir(srcAbs)
	}
	dirAbs, _ := filepath.Abs(dir)

	res := &backupResult{
		SchemaVersion: backupSchemaVersion,
		Source:        srcAbs,
		SourceSha:     f.Sha256,
		SourceMtime:   st.ModTime().Format(time.RFC3339),
		Dir:           dirAbs,
		IndexFile:     filepath.Join(dirAbs, BackupIndexName),
	}

	// Two discoveries, because dedup and orphan-detection ask different
	// questions. Orphan detection needs the corpus AROUND THE LIVE FILE (is an
	// existing backup identical to what is running?). Dedup needs everything
	// already archived, which includes a --dir target that is not the config's
	// own directory — without that second walk, `--dir ./archive` writes a
	// duplicate copy on every run and the "content-addressed" promise is void.
	corpus, err := lsconfig.DiscoverCorpus(srcAbs, lsconfig.DiscoverOptions{})
	if err == nil {
		res.CorpusSources = len(corpus.Historical)
		res.OrphanBackups = corpus.OrphanBackups
		markDedup(res, f.Sha256, corpus.Historical)
	} else {
		res.Notes = append(res.Notes, fmt.Sprintf("corpus discovery failed: %v", err))
	}
	if dirAbs != filepath.Dir(srcAbs) {
		if archive, aerr := lsconfig.DiscoverCorpus(srcAbs, lsconfig.DiscoverOptions{Root: dirAbs}); aerr == nil {
			markDedup(res, f.Sha256, archive.Historical)
		}
	}
	sort.Strings(res.DedupOf)

	name := backupFileName(f.Sha256, label)
	target := filepath.Join(dirAbs, name)
	res.File = target

	if res.Dedup {
		res.Notes = append(res.Notes,
			fmt.Sprintf("live content is already archived (%s) — no new copy written; a backup is identified by its sha256, not by how many filenames point at it", strings.Join(res.DedupOf, ", ")))
	}
	if len(res.OrphanBackups) > 0 {
		res.Notes = append(res.Notes,
			fmt.Sprintf("ORPHAN BACKUP: %s is byte-identical to the live config. A backup normally captures the state BEFORE a change; an identical one means the change it was named for was never applied, or the backup was taken after the fact. Compare the backup's label against `config drift` before assuming either", strings.Join(res.OrphanBackups, ", ")))
	}

	if !write {
		res.Notes = append(res.Notes, "no files written (dry-run or verify mode)")
		return res, nil
	}
	if res.Dedup {
		return res, nil
	}
	if err := os.MkdirAll(dirAbs, 0o755); err != nil {
		return nil, fmt.Errorf("create backup dir %s: %w", dirAbs, err)
	}
	// Refuse to overwrite. A colliding path with matching content is already
	// the archive we would have written; a colliding path with different
	// content would mean a sha collision, which is not a case to paper over.
	if _, err := os.Stat(target); err == nil {
		res.Notes = append(res.Notes, fmt.Sprintf("%s already exists — left untouched", target))
		res.Dedup = true
		return res, nil
	}
	if err := os.WriteFile(target, raw, 0o644); err != nil {
		return nil, fmt.Errorf("write backup %s: %w", target, err)
	}
	res.Written = true
	if err := appendBackupIndex(res.IndexFile, backupIndexEntry{
		Sha256:      f.Sha256,
		File:        name,
		Label:       label,
		CreatedAt:   time.Now().Format(time.RFC3339),
		SourcePath:  srcAbs,
		SourceMtime: st.ModTime().Format(time.RFC3339),
		SourceSize:  st.Size(),
	}); err != nil {
		res.Notes = append(res.Notes, fmt.Sprintf("backup written but index update failed: %v", err))
	}
	return res, nil
}

// markDedup records every already-archived source whose content matches sha.
func markDedup(res *backupResult, sha string, sources []*lsconfig.Source) {
	seen := map[string]bool{}
	for _, existing := range res.DedupOf {
		seen[existing] = true
	}
	for _, s := range sources {
		if s.Sha256 != sha || seen[s.Rel] {
			continue
		}
		seen[s.Rel] = true
		res.Dedup = true
		res.DedupOf = append(res.DedupOf, s.Rel)
	}
}

func backupFileName(sha, label string) string {
	short := sha
	if len(short) > 10 {
		short = short[:10]
	}
	label = strings.Trim(labelSanitizer.ReplaceAllString(strings.TrimSpace(label), "-"), "-")
	if label == "" {
		label = "backup"
	}
	return short + "-" + label + ".yaml"
}

// appendBackupIndex rewrites the sidecar index with the new row appended. The
// index is OUR file, created by this command; it is not the config, and
// rewriting it is not the write this package refuses to make.
func appendBackupIndex(path string, entry backupIndexEntry) error {
	var entries []backupIndexEntry
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &entries); err != nil {
			// Never destroy an index we cannot parse — an operator may have
			// hand-edited it, and clobbering it would lose real history.
			return fmt.Errorf("existing %s is not valid JSON; refusing to overwrite it: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		if e.Sha256 == entry.Sha256 && e.File == entry.File {
			return nil
		}
	}
	entries = append(entries, entry)
	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

func printBackupHuman(cmd *cobra.Command, r *backupResult) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s\n", bold("config backup"))
	fmt.Fprintf(out, "  source  %s\n", r.Source)
	fmt.Fprintf(out, "  sha256  %s\n", r.SourceSha)
	fmt.Fprintf(out, "  mtime   %s\n", r.SourceMtime)
	fmt.Fprintf(out, "  dir     %s\n", r.Dir)
	fmt.Fprintf(out, "  corpus  %d historical source(s) discovered\n", r.CorpusSources)
	fmt.Fprintln(out)
	switch {
	case r.Written:
		fmt.Fprintf(out, "%s %s\n", green("WROTE"), r.File)
		fmt.Fprintf(out, "  indexed in %s\n", r.IndexFile)
	case r.Dedup:
		fmt.Fprintf(out, "%s nothing written\n", yellow("DEDUP"))
	default:
		fmt.Fprintf(out, "%s %s\n", yellow("PLANNED"), r.File)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(out, "\n  %s\n", n)
	}
}
