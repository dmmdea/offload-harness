// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package lsconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// configLikeName matches a filename that could be a llama-swap config copy.
// The pattern is EXTENSION-TOLERANT on purpose: a real operator corpus
// contains llama-swap.yaml.pre-matrix and backup-2026-07-26/llama-swap.yaml
// alongside the obvious backup-*.yaml, and a glob-only discovery silently
// misses exactly the copies that predate the naming convention.
var configLikeName = regexp.MustCompile(`(?i)\.ya?ml($|[.\-_])`)

// labelDatePattern extracts a YYYY-MM-DD from a filename. The result is a
// LABEL, never evidence: a backup named ...2026-08-06... whose mtime is
// 2026-08-05 was named by hand after the fact. mtime and the content hash are
// the truth; the label is a hint about intent.
var labelDatePattern = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})`)

// maxCorpusFileBytes bounds what discovery will hash. A llama-swap config is
// tens of KB; anything megabyte-scale in the same directory is a binary, a
// log, or a zip that happened to match on name.
const maxCorpusFileBytes = 4 << 20

// Source is one discovered historical config copy.
type Source struct {
	Path    string    `json:"path"`
	Rel     string    `json:"rel"`
	Label   string    `json:"label"`
	Sha256  string    `json:"sha256"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	// IsLive marks the file the operator's service actually reads.
	IsLive bool `json:"is_live"`
	// Flat marks a source sitting directly in the config's own directory
	// (as opposed to a dated backup subdirectory).
	Flat bool `json:"flat"`
	// LabelDate is the date parsed from the FILENAME (never the directory —
	// a directory date describes the batch, not the file).
	LabelDate string `json:"label_date,omitempty"`
	// LabelDateMismatch is true when LabelDate disagrees with the mtime's
	// local calendar date.
	LabelDateMismatch bool `json:"label_date_mismatch,omitempty"`
	// MTimeDate is the mtime's local calendar date, for reporting.
	MTimeDate string `json:"mtime_date"`

	raw []byte
}

// Bytes returns the source's content, read once during discovery.
func (s *Source) Bytes() []byte { return s.raw }

// IdenticalPair names two sources with the same content hash.
type IdenticalPair struct {
	Sha256 string   `json:"sha256"`
	Paths  []string `json:"paths"`
}

// Corpus is the full discovered history around one config file.
type Corpus struct {
	ConfigPath string `json:"config_path"`
	Dir        string `json:"dir"`
	// Live is the source entry for ConfigPath itself, if it was readable.
	Live *Source `json:"live,omitempty"`
	// Historical is every discovered copy EXCEPT the live file, mtime-ascending.
	Historical []*Source `json:"historical"`
	// FlatHistorical is the subset of Historical sitting in Dir itself.
	FlatHistorical []*Source `json:"-"`
	// DistinctFlatStates counts unique content hashes among FlatHistorical.
	DistinctFlatStates int `json:"distinct_flat_states"`
	// IdenticalPairs groups FlatHistorical members sharing a content hash.
	IdenticalPairs []IdenticalPair `json:"identical_pairs"`
	// OrphanBackups are historical copies byte-identical to the LIVE file: a
	// change that was staged and never applied, or a backup taken after the
	// fact. Either way the operator's intent and the running config disagree.
	OrphanBackups []string `json:"orphan_backups"`
	// LabelMismatches are historical sources whose filename date disagrees
	// with their mtime date.
	LabelMismatches []*Source `json:"label_mismatches"`
	// Skipped records candidates rejected during discovery, with the reason,
	// so a surprising count is diagnosable instead of mysterious.
	Skipped []string `json:"skipped,omitempty"`
}

// DiscoverOptions tunes corpus discovery.
type DiscoverOptions struct {
	// Root defaults to the config's own directory.
	Root string
	// MaxDepth bounds recursion below Root. 0 means unlimited.
	MaxDepth int
	// RequireModelsBlock rejects a name-matching file that does not parse as a
	// llama-swap config (no top-level models: mapping). Default true; this is
	// what keeps an unrelated docker-compose.yaml out of the chronology.
	RequireModelsBlock *bool
}

// DiscoverCorpus finds every historical copy of a llama-swap config reachable
// from the config's directory, hashes each, and reports the content-level
// facts (distinct states, byte-identical pairs, orphan backups) plus the
// filename-label discrepancies.
//
// Discovery is recursive and extension-tolerant, and it treats FILENAMES AS
// LABELS ONLY.
func DiscoverCorpus(configPath string, opts DiscoverOptions) (*Corpus, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		abs = configPath
	}
	dir := opts.Root
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Dir(abs)
	}
	requireModels := true
	if opts.RequireModelsBlock != nil {
		requireModels = *opts.RequireModelsBlock
	}

	c := &Corpus{ConfigPath: abs, Dir: dir}
	var all []*Source

	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			c.Skipped = append(c.Skipped, fmt.Sprintf("%s: %v", p, err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if opts.MaxDepth > 0 && depthBelow(dir, p) > opts.MaxDepth {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !configLikeName.MatchString(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			c.Skipped = append(c.Skipped, fmt.Sprintf("%s: stat: %v", name, err))
			return nil
		}
		if info.Size() > maxCorpusFileBytes {
			c.Skipped = append(c.Skipped, fmt.Sprintf("%s: %d bytes exceeds corpus cap", name, info.Size()))
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			c.Skipped = append(c.Skipped, fmt.Sprintf("%s: read: %v", name, err))
			return nil
		}
		if requireModels && !looksLikeLlamaSwapConfig(raw) {
			c.Skipped = append(c.Skipped, fmt.Sprintf("%s: no top-level models: block", name))
			return nil
		}
		sum := sha256.Sum256(raw)
		pAbs, _ := filepath.Abs(p)
		src := &Source{
			Path:      pAbs,
			Rel:       RelOrBase(dir, pAbs),
			Label:     name,
			Sha256:    hex.EncodeToString(sum[:]),
			Size:      info.Size(),
			ModTime:   info.ModTime(),
			IsLive:    sameFile(pAbs, abs),
			Flat:      filepath.Dir(pAbs) == filepath.Clean(dir),
			MTimeDate: info.ModTime().Format("2006-01-02"),
			raw:       raw,
		}
		// Label date comes from the BASE NAME only. A dated backup DIRECTORY
		// (backup-2026-07-25/llama-swap.yaml) labels the batch, not the file,
		// and folding it in would manufacture mismatches that say nothing
		// about the file.
		if m := labelDatePattern.FindStringSubmatch(name); m != nil {
			src.LabelDate = m[0]
			src.LabelDateMismatch = src.LabelDate != src.MTimeDate
		}
		all = append(all, src)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, walkErr)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].ModTime.Equal(all[j].ModTime) {
			return all[i].Path < all[j].Path
		}
		return all[i].ModTime.Before(all[j].ModTime)
	})

	for _, s := range all {
		if s.IsLive {
			c.Live = s
			continue
		}
		c.Historical = append(c.Historical, s)
		if s.Flat {
			c.FlatHistorical = append(c.FlatHistorical, s)
		}
		if s.LabelDateMismatch {
			c.LabelMismatches = append(c.LabelMismatches, s)
		}
	}

	byHash := map[string][]*Source{}
	for _, s := range c.FlatHistorical {
		byHash[s.Sha256] = append(byHash[s.Sha256], s)
	}
	c.DistinctFlatStates = len(byHash)
	for _, h := range SortedKeys(byHash) {
		group := byHash[h]
		if len(group) < 2 {
			continue
		}
		pair := IdenticalPair{Sha256: h}
		for _, s := range group {
			pair.Paths = append(pair.Paths, s.Rel)
		}
		sort.Strings(pair.Paths)
		c.IdenticalPairs = append(c.IdenticalPairs, pair)
	}

	if c.Live != nil {
		for _, s := range c.Historical {
			if s.Sha256 == c.Live.Sha256 {
				c.OrphanBackups = append(c.OrphanBackups, s.Rel)
			}
		}
		sort.Strings(c.OrphanBackups)
	}
	return c, nil
}

// Chronology returns the historical sources plus the live file, deduplicated
// by content hash (first occurrence by mtime wins) and mtime-ascending. This
// is the timeline `seat log` walks: consecutive DISTINCT states, not
// consecutive files, because two byte-identical backups are one state.
func (c *Corpus) Chronology() []*Source {
	seen := map[string]bool{}
	var out []*Source
	for _, s := range c.Historical {
		if seen[s.Sha256] {
			continue
		}
		seen[s.Sha256] = true
		out = append(out, s)
	}
	if c.Live != nil && !seen[c.Live.Sha256] {
		out = append(out, c.Live)
	}
	return out
}

// DuplicatesOf returns the other sources sharing a source's content hash.
func (c *Corpus) DuplicatesOf(s *Source) []string {
	var out []string
	for _, o := range c.Historical {
		if o.Sha256 == s.Sha256 && o.Path != s.Path {
			out = append(out, o.Rel)
		}
	}
	if c.Live != nil && c.Live.Sha256 == s.Sha256 && c.Live.Path != s.Path {
		out = append(out, c.Live.Rel)
	}
	sort.Strings(out)
	return out
}

// looksLikeLlamaSwapConfig is a cheap textual sniff — a top-level `models:`
// key at column 0. Cheaper and more forgiving than a full parse (a historical
// copy may predate a schema change), and it is only ever used to REJECT
// obvious non-configs.
func looksLikeLlamaSwapConfig(raw []byte) bool {
	for _, ln := range splitLines(raw) {
		if strings.HasPrefix(ln, "models:") {
			return true
		}
	}
	return false
}

func depthBelow(root, p string) int {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return 0
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return 0
	}
	return strings.Count(rel, "/") + 1
}

func sameFile(a, b string) bool {
	if strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) {
		return true
	}
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return os.SameFile(sa, sb)
}
