package leakgate

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

// The synthetic list. The names are made up (they occur nowhere in the tree)
// and are mapped onto the classes of the real list: a long name (sub), a
// glued-phrase form (sub), a case-sensitive word (subcs), short names that
// letters may extend (word), numbers and model codes (exact), words that only
// count together (phrase), and one-letter drive phrases. No test in this
// package spells a name from the operator's list: the keyed gate scans test
// sources like every other file (7.2, rule 7).
const synthList = "" +
	"sub\tzorblax\n" +
	"sub\tgribble\n" +
	"sub\tplughxyzzy\n" +
	"sub\tgruelamp\n" +
	"sub\tkfrotzplover\n" +
	"subcs\tfroodtoken\n" +
	"word\tquuxel\n" +
	"word\tfrobnic\n" +
	"exact\t8128\n" +
	"exact\t33k\n" +
	"phrase\tplugh xyzzy\n" +
	"phrase\tgrue lamp\n" +
	"phrase\tj frotz\n" +
	"phrase\tk frotz plover\n"

// hexKey builds a 64-hex key at run time. It is never a literal, and no test in
// this package reads a key from the environment.
func hexKey(pair string) string { return strings.Repeat(pair, 32) }

func keyBytes(pair string) []byte {
	k, err := DecodeKey(hexKey(pair))
	if err != nil {
		panic(err)
	}
	return k
}

func synthEntries(t testing.TB) []Entry {
	t.Helper()
	es, err := ParseList([]byte(synthList))
	if err != nil {
		t.Fatalf("synthetic list does not parse: %v", err)
	}
	return es
}

func plainMatcher(t testing.TB) *Matcher {
	t.Helper()
	m, err := NewPlainMatcher(WithCanary(synthEntries(t)))
	if err != nil {
		t.Fatalf("plain matcher: %v", err)
	}
	return m
}

func digestMatcher(t testing.TB, pair string, allow []AllowRow, exempt []ExemptRow) (*Matcher, *DigestFile) {
	t.Helper()
	key := keyBytes(pair)
	df, err := BuildDigest(key, synthEntries(t), allow, exempt)
	if err != nil {
		t.Fatalf("build digest: %v", err)
	}
	m, err := NewDigestMatcher(df, key)
	if err != nil {
		t.Fatalf("digest matcher: %v", err)
	}
	return m, df
}

// memFile is one entry of the in-memory file system. size, when set, overrides
// len(data) (a file that is reported large without allocating it).
type memFile struct {
	data []byte
	err  error
	size int64
}

// memFS is the in-memory FS of the fail-closed tests: unreadable files, deleted
// files and large files are all expressible without touching a disk.
type memFS map[string]memFile

func (m memFS) Read(path string, max int64) ([]byte, int64, error) {
	f, ok := m[path]
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	if f.err != nil {
		return nil, 0, f.err
	}
	size := int64(len(f.data))
	if f.size > 0 {
		size = f.size
	}
	if size > max {
		return nil, size, nil
	}
	return f.data, size, nil
}

func file(data string) memFile { return memFile{data: []byte(data)} }

// tracked builds the file list of a memory tree, in sorted order.
func tracked(fsys memFS, modes map[string]string, blobs map[string]string) []TrackedFile {
	var out []TrackedFile
	for p := range fsys {
		mode := "100644"
		if m, ok := modes[p]; ok {
			mode = m
		}
		blob := blobs[p]
		if blob == "" {
			blob = strings.Repeat("0", 40)
		}
		out = append(out, TrackedFile{Mode: mode, Blob: blob, Path: p})
	}
	sortTracked(out)
	return out
}

func sortTracked(f []TrackedFile) {
	for i := 1; i < len(f); i++ {
		for j := i; j > 0 && f[j].Path < f[j-1].Path; j-- {
			f[j], f[j-1] = f[j-1], f[j]
		}
	}
}

// flagged reports whether the matcher or a shape rule flags text, and the
// sorted distinct labels.
func flagged(m *Matcher, text string) (bool, []string) {
	fs, _ := m.ScanText("x.md", []byte(text))
	fs = append(fs, ShapeFindings("x.md", []byte(text))...)
	seen := map[string]bool{}
	var labels []string
	for _, f := range fs {
		l := f.Label()
		if !seen[l] {
			seen[l] = true
			labels = append(labels, l)
		}
	}
	return len(labels) > 0, labels
}

func fatalReasons(r Report) []string {
	var out []string
	for _, f := range r.Fatals {
		out = append(out, f.Reason)
	}
	return out
}

func hasFatal(r Report, path, substr string) bool {
	for _, f := range r.Fatals {
		if f.Path == path && strings.Contains(f.Reason, substr) {
			return true
		}
	}
	return false
}

func lines(findings []Finding) []int {
	var out []int
	for _, f := range findings {
		out = append(out, f.Line)
	}
	return out
}

func equalBytes(a, b []byte) bool { return bytes.Equal(a, b) }
