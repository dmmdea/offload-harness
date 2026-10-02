package leakgate

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
)

// The tree scan. The scan set is the tracked files; every path is scanned by
// name whatever its content, and every row of the fail-closed table is a Fatal
// naming the path and the reason (never matched text), unless an exempt row
// bound to the exact path and blob id says otherwise.

// TrackedFile is one row of `git ls-files -z -s`: the mode, the object id and
// the path. The library needs the mode to see symlinks and submodules.
type TrackedFile struct {
	Mode string
	Blob string
	Path string
}

// ParseLsFiles parses the output of `git ls-files -z -s`:
// "<mode> <object> <stage><TAB><path>" records, each ending in a NUL. A
// non-zero stage (an unmerged path) is an error.
func ParseLsFiles(b []byte) ([]TrackedFile, error) {
	var out []TrackedFile
	for len(b) > 0 {
		nul := bytes.IndexByte(b, 0)
		if nul < 0 {
			return nil, errors.New("ls-files output: unterminated record")
		}
		rec := string(b[:nul])
		b = b[nul+1:]
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, errors.New("ls-files output: record without a tab")
		}
		fields := strings.Fields(rec[:tab])
		if len(fields) != 3 {
			return nil, errors.New("ls-files output: malformed record")
		}
		if fields[2] != "0" {
			return nil, errors.New("ls-files output: an unmerged path")
		}
		out = append(out, TrackedFile{Mode: fields[0], Blob: fields[1], Path: rec[tab+1:]})
	}
	return out, nil
}

// FS is how the scan reaches file contents: the work tree, the blobs of a
// commit, or memory in a test.
//
// Read returns the file's size and, when size <= max, its bytes. A larger file
// is not read (data is nil and size says how large it is). A missing file is an
// error satisfying errors.Is(err, fs.ErrNotExist).
type FS interface {
	Read(path string, max int64) (data []byte, size int64, err error)
}

// DirFS reads tracked paths (slash-separated, relative) from a work tree.
type DirFS struct{ Root string }

// Read implements FS. A path that is absolute, contains "..", or uses a
// backslash is refused, and so is anything but a regular file.
func (d DirFS) Read(path string, max int64) ([]byte, int64, error) {
	if !fs.ValidPath(path) || path == "." || strings.Contains(path, "\\") {
		return nil, 0, fmt.Errorf("invalid tracked path")
	}
	full := filepath.Join(d.Root, filepath.FromSlash(path))
	info, err := os.Lstat(full)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("not a regular file")
	}
	if info.Size() > max {
		return nil, info.Size(), nil
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, 0, err
	}
	return data, int64(len(data)), nil
}

// ScanOptions configure ScanTree.
type ScanOptions struct {
	// Required: a tracked path missing from the work tree, and an empty file
	// set, are fatal (a clean checkout is expected).
	Required bool
	// Shapes also applies the keyless shape rules.
	Shapes bool
	// Exempt rows exempt a file from one fail-closed rule.
	Exempt []ExemptRow
	// Allow rows allow one finding each (a keyed matcher only).
	Allow []AllowRow
	// Workers is the number of scanning goroutines (default: the CPU count).
	Workers int
	// MaxFile is the size limit in bytes (default 16 MiB).
	MaxFile int64
	// Filter, when set, restricts the scan to the paths it accepts; the rest are
	// not scanned and not reported (they still count in Report.Total).
	Filter func(path string) bool
}

// Fatal is a fail-closed record: a path (empty for a whole-scan condition), a
// line for a run over the limit, and a reason that never carries matched text.
type Fatal struct {
	Path   string
	Line   int
	Reason string
}

// Classes count the inputs where the specified scanner and the reference
// oracle differ; each is a precondition of the parity check (zero, or the
// result is "not comparable").
type Classes struct {
	Runs96        int // runs over 96 characters
	Binaries      int // unknown binary files (name scanned only)
	PNGNonStd     int // PNG files with a chunk other than IHDR, tEXt, IDAT, IEND
	PNGAfterIEND  int // PNG files with bytes after IEND
	PNGCompressed int // PNG files with compressed text
	PNGNoIEND     int // PNG files that could not be walked to IEND
	Oversize      int
	Symlink       int
	Submodule     int
}

// Report is the result of a tree scan.
type Report struct {
	Findings []Finding
	Fatals   []Fatal
	Scanned  int // files scanned (after the filter)
	Total    int // tracked files
	Missing  int // tracked paths missing from the work tree (name scanned only)
	Classes  Classes
}

const (
	defaultMaxFile = 16 << 20
	sniffWindow    = 8000
)

type exemptKey struct{ path, why string }

type fileResult struct {
	findings []Finding
	fatals   []Fatal
	classes  Classes
	missing  bool
}

func (c *Classes) add(o Classes) {
	c.Runs96 += o.Runs96
	c.Binaries += o.Binaries
	c.PNGNonStd += o.PNGNonStd
	c.PNGAfterIEND += o.PNGAfterIEND
	c.PNGCompressed += o.PNGCompressed
	c.PNGNoIEND += o.PNGNoIEND
	c.Oversize += o.Oversize
	c.Symlink += o.Symlink
	c.Submodule += o.Submodule
}

type allowKey struct{ where, path, d string }

// ScanTree scans a set of tracked files. m may be nil: with Shapes set that is
// the keyless mode (shape rules and the fail-closed rows only, no name scan).
func ScanTree(m *Matcher, files []TrackedFile, fsys FS, opts ScanOptions) Report {
	rep := Report{Total: len(files)}
	if m == nil && !opts.Shapes {
		rep.Fatals = append(rep.Fatals, Fatal{Reason: "nothing to scan for: no matcher and no shape rules"})
		return rep
	}
	if len(opts.Allow) > 0 && (m == nil || m.key == nil) {
		rep.Fatals = append(rep.Fatals, Fatal{Reason: "allow rows need a keyed matcher"})
		return rep
	}
	if len(files) == 0 && opts.Required {
		rep.Fatals = append(rep.Fatals, Fatal{Reason: "gate went blind: no tracked files"})
		return rep
	}
	max := opts.MaxFile
	if max <= 0 {
		max = defaultMaxFile
	}
	exempt := map[exemptKey]string{}
	for _, x := range opts.Exempt {
		exempt[exemptKey{x.Path, x.Why}] = x.Blob
	}
	allow := map[allowKey]struct{}{}
	for _, a := range opts.Allow {
		raw, err := hex2raw(a.D)
		if err != nil {
			rep.Fatals = append(rep.Fatals, Fatal{Reason: "allow row digest is not hex"})
			return rep
		}
		allow[allowKey{a.Where, a.Path, raw}] = struct{}{}
	}

	var todo []TrackedFile
	for _, tf := range files {
		if opts.Filter == nil || opts.Filter(tf.Path) {
			todo = append(todo, tf)
		}
	}
	rep.Scanned = len(todo)
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > len(todo) {
		workers = len(todo)
	}
	results := make([]fileResult, len(todo))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var sc *Scanner
			if m != nil {
				sc = m.NewScanner()
			}
			for i := range next {
				results[i] = scanFile(sc, todo[i], fsys, max, exempt, allow, opts)
			}
		}()
	}
	for i := range todo {
		next <- i
	}
	close(next)
	wg.Wait()

	for _, r := range results {
		rep.Findings = append(rep.Findings, r.findings...)
		rep.Fatals = append(rep.Fatals, r.fatals...)
		rep.Classes.add(r.classes)
		if r.missing {
			rep.Missing++
		}
	}
	sortFindings(rep.Findings)
	sort.SliceStable(rep.Fatals, func(i, j int) bool {
		a, b := rep.Fatals[i], rep.Fatals[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Reason < b.Reason
	})
	return rep
}

func hex2raw(h string) (string, error) {
	if len(h)%2 != 0 {
		return "", errors.New("odd length")
	}
	b := make([]byte, len(h)/2)
	for i := range b {
		hi, lo := hexVal(h[2*i]), hexVal(h[2*i+1])
		if hi < 0 || lo < 0 {
			return "", errors.New("not hex")
		}
		b[i] = byte(hi<<4 | lo)
	}
	return string(b), nil
}

// exemption looks up the exempt row for (path, why): exempt when the row's blob
// is the file's, stale when a row for the pair exists with another blob.
func exemption(rows map[exemptKey]string, tf TrackedFile, why string) (exempt bool, stale bool) {
	blob, found := rows[exemptKey{tf.Path, why}]
	if !found {
		return false, false
	}
	return blob == tf.Blob, blob != tf.Blob
}

func staleReason(path string) string {
	return "exempt row is stale for " + path + ": regenerate the digest file"
}

func scanFile(sc *Scanner, tf TrackedFile, fsys FS, max int64, exempt map[exemptKey]string, allow map[allowKey]struct{}, opts ScanOptions) fileResult {
	var res fileResult
	fatal := func(line int, reason string) {
		res.fatals = append(res.fatals, Fatal{Path: tf.Path, Line: line, Reason: reason})
	}
	// failClosed reports a Fatal unless an exempt row covers the file; a stale
	// row says so instead of the generic reason.
	failClosed := func(why, reason string) {
		ok, stale := exemption(exempt, tf, why)
		switch {
		case ok:
		case stale:
			fatal(0, staleReason(tf.Path))
		default:
			fatal(0, reason)
		}
	}
	keep := func(h hit, name bool) {
		if len(allow) > 0 {
			where := "body"
			if name {
				where = "name"
			}
			if _, ok := allow[allowKey{where, tf.Path, sc.allowDigest(h.chunk)}]; ok {
				return
			}
		}
		res.findings = append(res.findings, sc.toFinding(h, tf.Path, name))
	}

	// the name scan covers every tracked path, whatever its content.
	if sc != nil {
		hits, long := sc.scanText(nameBytes(tf.Path), len(allow) > 0)
		for _, h := range hits {
			keep(h, true)
		}
		for _, line := range long {
			res.classes.Runs96++
			if ok, stale := exemption(exempt, tf, WhyLongRun); !ok {
				if stale {
					fatal(0, staleReason(tf.Path))
				} else {
					fatal(line, "run over 96 characters in the path")
				}
			}
		}
	}

	switch tf.Mode {
	case "120000":
		res.classes.Symlink++
		failClosed(WhySymlink, "tracked symlink (not read, not exempt)")
		return res
	case "160000":
		res.classes.Submodule++
		failClosed(WhySubmodule, "tracked submodule (not read, not exempt)")
		return res
	}

	data, size, err := fsys.Read(tf.Path, max)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			res.missing = true
			if opts.Required {
				fatal(0, "tracked path missing from the work tree (deleted but still indexed)")
			}
		case errors.Is(err, fs.ErrPermission):
			fatal(0, "unreadable file: permission denied")
		default:
			fatal(0, "unreadable file: read error")
		}
		return res
	}
	if size > max {
		res.classes.Oversize++
		failClosed(WhyOversize, "oversize file (over 16 MiB)")
		return res
	}

	body := data
	isPNG := false
	var raw []byte // set for a byte-order-marked file: its raw bytes are scanned too
	invalid := false
	switch {
	case hasUTF16BOM(body):
		raw = data
		var ok bool
		if body, ok = decodeUTF16(body); !ok {
			// A mark with a payload that is not UTF-16 (an odd length, an unpaired
			// surrogate) is no text the decoder can read: fail closed like any
			// other unknown binary. Valid or not, the raw bytes are scanned as
			// well, because a plain ASCII file with a stray mark decodes to other
			// scripts and would hide its names.
			invalid = true
			res.classes.Binaries++
			failClosed(WhyBinary, "byte-order mark but not valid UTF-16 (the raw bytes were scanned)")
		}
	case IsPNG(body):
		isPNG = true
		info, perr := ParsePNG(body)
		if info.NonStd {
			res.classes.PNGNonStd++
		}
		if info.AfterIEND {
			res.classes.PNGAfterIEND++
		}
		if info.CompressedText {
			res.classes.PNGCompressed++
		}
		if info.NoIEND {
			res.classes.PNGNoIEND++
		}
		if perr != nil {
			fatal(0, perr.Error())
		}
		body = bytes.Join(info.Segments, []byte{'\n'})
	case strings.HasSuffix(strings.ToLower(tf.Path), ".png"):
		fatal(0, ErrPNGSignature.Error())
	}
	if !isPNG && !invalid && bytes.IndexByte(body[:min(len(body), sniffWindow)], 0) >= 0 {
		res.classes.Binaries++
		if !strings.HasSuffix(strings.ToLower(tf.Path), ".png") {
			failClosed(WhyBinary, "unknown binary (a NUL in the first 8000 bytes; the name was scanned)")
		}
		return res
	}
	if !isPNG {
		body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	}

	// scanBody runs the matcher (and the shape rules) over one view of the file.
	scanBody := func(b []byte) {
		if sc != nil {
			hits, long := sc.scanText(b, len(allow) > 0)
			for _, h := range hits {
				keep(h, false)
			}
			if len(long) > 0 {
				res.classes.Runs96 += len(long)
				if ok, stale := exemption(exempt, tf, WhyLongRun); !ok {
					if stale {
						fatal(0, staleReason(tf.Path))
					} else {
						last := 0
						for _, line := range long {
							if line != last {
								fatal(line, "run over 96 characters")
								last = line
							}
						}
					}
				}
			}
		}
		if opts.Shapes {
			for _, f := range ShapeFindings(tf.Path, b) {
				res.findings = append(res.findings, f)
			}
		}
	}
	scanBody(body)
	if raw != nil {
		// The two views of one file can report the same token twice.
		first := len(res.findings)
		nFatals := len(res.fatals)
		scanBody(raw)
		res.findings = dedupeFrom(res.findings, first)
		res.fatals = dedupeFatalsFrom(res.fatals, nFatals)
	}
	return res
}

// dedupeFrom drops, from the findings after index first, every finding that
// repeats one reported for the same path, line, mode and entry.
func dedupeFrom(fs []Finding, first int) []Finding {
	type key struct {
		name       bool
		line       int
		mode, id   string
		text, path string
	}
	seen := map[key]bool{}
	for _, f := range fs[:first] {
		seen[key{f.Name, f.Line, f.Mode, f.ID, f.Text, f.Path}] = true
	}
	out := fs[:first]
	for _, f := range fs[first:] {
		k := key{f.Name, f.Line, f.Mode, f.ID, f.Text, f.Path}
		if !seen[k] {
			seen[k] = true
			out = append(out, f)
		}
	}
	return out
}

// dedupeFatalsFrom drops, from the fatals after index first, the ones already
// reported for the same path, line and reason.
func dedupeFatalsFrom(fs []Fatal, first int) []Fatal {
	seen := map[Fatal]bool{}
	for _, f := range fs[:first] {
		seen[f] = true
	}
	out := fs[:first]
	for _, f := range fs[first:] {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func hasUTF16BOM(b []byte) bool {
	return len(b) >= 2 && (b[0] == 0xFF && b[1] == 0xFE || b[0] == 0xFE && b[1] == 0xFF)
}

// decodeUTF16 decodes a UTF-16 text with a byte-order mark to UTF-8. ok is false
// when the bytes are not valid UTF-16: an odd payload (a dangling byte, dropped
// from the result), or an unpaired surrogate (which becomes U+FFFD, a separator).
func decodeUTF16(b []byte) (out []byte, ok bool) {
	little := b[0] == 0xFF
	b = b[2:]
	ok = len(b)%2 == 0
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if little {
			units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
		} else {
			units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
		}
	}
	for i := 0; i < len(units); i++ {
		switch u := units[i]; {
		case u >= 0xD800 && u < 0xDC00: // a high surrogate needs a low one next
			if i+1 < len(units) && units[i+1] >= 0xDC00 && units[i+1] < 0xE000 {
				i++
			} else {
				ok = false
			}
		case u >= 0xDC00 && u < 0xE000: // a low surrogate alone
			ok = false
		}
	}
	runes := utf16.Decode(units)
	out = make([]byte, 0, len(runes))
	for _, r := range runes {
		out = utf8.AppendRune(out, r)
	}
	return out, ok
}
