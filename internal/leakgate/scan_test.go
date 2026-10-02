package leakgate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"
)

var (
	utf8BOM = []byte{0xEF, 0xBB, 0xBF}
	blobA   = strings.Repeat("a", 40)
	blobB   = strings.Repeat("b", 40)
)

func utf16Bytes(s string, bigEndian bool, bom bool) []byte {
	var out []byte
	put := func(u uint16) {
		if bigEndian {
			out = append(out, byte(u>>8), byte(u))
		} else {
			out = append(out, byte(u), byte(u>>8))
		}
	}
	if bom {
		put(0xFEFF)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		put(u)
	}
	return out
}

func scanOf(t *testing.T, m *Matcher, fsys memFS, modes, blobs map[string]string, opts ScanOptions) Report {
	t.Helper()
	return ScanTree(m, tracked(fsys, modes, blobs), fsys, opts)
}

func findingKeys(r Report) []string {
	var out []string
	for _, f := range r.Findings {
		p := f.Path
		if f.Name {
			p += " (name)"
		}
		out = append(out, p+":"+itoa(f.Line)+":"+itoa(f.Col)+" "+f.Label())
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestScanTreeFindsTokensAcrossContentTypes(t *testing.T) {
	m := plainMatcher(t)
	fsys := memFS{
		"docs/a.md":     file("hello\nzorblax here\n"),
		"u16le.txt":     {data: utf16Bytes("line\nzorblax", false, true)},
		"u16be.txt":     {data: utf16Bytes("zorblax", true, true)},
		"bom.txt":       {data: append(append([]byte{}, utf8BOM...), []byte("zorblax")...)},
		"img/tag.png":   {data: buildPNG(ihdrChunk(), textChunk("c", "by zorblax"), idatChunk("zorblax"), iendChunk())},
		"clean.md":      file("nothing to see\n"),
		"crlf.md":       file("a\r\nb zorblax\r\n"),
		"escaped.json":  file("{\"k\": \"x\\nfrobnic\"}\n"),
		"folded.md":     file("Zorbl" + string(rune(0x00E1)) + "x\n"),
		"deep/Plugh.md": file("plugh xyzzy\n"),
	}
	r := scanOf(t, m, fsys, nil, nil, ScanOptions{})
	if len(r.Fatals) != 0 {
		t.Fatalf("unexpected fatals: %+v", r.Fatals)
	}
	got := findingKeys(r)
	want := []string{
		"bom.txt:1:1 sub:zorblax",
		"crlf.md:2:3 sub:zorblax",
		"deep/Plugh.md:1:1 phrase:plugh xyzzy",
		"docs/a.md:2:1 sub:zorblax",
		"escaped.json:1:11 word:frobnic",
		"folded.md:1:1 sub:zorblax",
		"img/tag.png:1:6 sub:zorblax",
		"u16be.txt:1:1 sub:zorblax",
		"u16le.txt:2:1 sub:zorblax",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings\n got %q\nwant %q", got, want)
	}
	if r.Scanned != len(fsys) || r.Total != len(fsys) {
		t.Errorf("scanned %d of %d, want %d of %d", r.Scanned, r.Total, len(fsys), len(fsys))
	}
}

func TestScanTreeNameScanCoversEveryTrackedPath(t *testing.T) {
	m := plainMatcher(t)
	fsys := memFS{
		"docs/zorblax-notes.md": file("clean body"),
		"bin/zorblax.dat":       {data: []byte("\x00\x01\x02 binary")},
		"gone/gribble.md":       {err: fs.ErrNotExist},
		"link/quuxel":           file("target"),
		"sub/frobnic":           file("x"),
	}
	modes := map[string]string{"link/quuxel": "120000", "sub/frobnic": "160000"}
	r := scanOf(t, m, fsys, modes, nil, ScanOptions{})
	got := findingKeys(r)
	want := []string{
		"bin/zorblax.dat (name):1:5 sub:zorblax",
		"docs/zorblax-notes.md (name):1:6 sub:zorblax",
		"gone/gribble.md (name):1:6 sub:gribble",
		"link/quuxel (name):1:6 word:quuxel",
		"sub/frobnic (name):1:5 word:frobnic",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("name findings\n got %q\nwant %q", got, want)
	}
	for _, f := range r.Findings {
		if !f.Name {
			t.Errorf("finding %+v is not marked as a name finding", f)
		}
	}
}

// TestScanTreeFailClosedRows is section 5.4 against the in-memory file system:
// every row is a Fatal naming the path and the reason, never matched text.
func TestScanTreeFailClosedRows(t *testing.T) {
	m := plainMatcher(t)
	type row struct {
		name   string
		path   string
		file   memFile
		mode   string
		opts   ScanOptions
		want   string // substring of the Fatal reason; empty = no Fatal
		class  func(Classes) int
		scanOK bool // the file's body must still be scanned for findings
	}
	big := make([]byte, 17<<20)
	for i := range big {
		big[i] = 'a'
		if i%50 == 49 {
			big[i] = ' '
		}
	}
	rows := []row{
		{name: "unreadable file", path: "a.md", file: memFile{err: errors.New("permission denied")}, want: "unreadable"},
		{name: "deleted but indexed, not required", path: "a.md", file: memFile{err: fs.ErrNotExist}},
		{name: "deleted but indexed, required", path: "a.md", file: memFile{err: fs.ErrNotExist}, opts: ScanOptions{Required: true}, want: "deleted"},
		{name: "unknown binary", path: "a.bin", file: memFile{data: []byte("ab\x00cd")}, want: "binary", class: func(c Classes) int { return c.Binaries }},
		{name: "unknown binary with a NUL after the sniff window is text", path: "a.bin", file: memFile{data: append([]byte(strings.Repeat("x ", 4000)), 0)}},
		{name: "oversize", path: "big.txt", file: memFile{data: big}, want: "oversize", class: func(c Classes) int { return c.Oversize }},
		{name: "a 100-character run", path: "r.md", file: file("fine\n" + strings.Repeat("q", 100) + "\n"), want: "run over 96", class: func(c Classes) int { return c.Runs96 }, scanOK: true},
		{name: "a 96-character run is within the limit", path: "r.md", file: file(strings.Repeat("q", 96))},
		{name: "symlink", path: "l", file: file("target"), mode: "120000", want: "symlink", class: func(c Classes) int { return c.Symlink }},
		{name: "submodule", path: "s", file: memFile{err: errors.New("is a directory")}, mode: "160000", want: "submodule", class: func(c Classes) int { return c.Submodule }},
		{name: "png with a bad signature", path: "x.png", file: file("plain text posing as an image"), want: "signature"},
		{name: "png without IEND", path: "x.png", file: memFile{data: buildPNG(ihdrChunk(), idatChunk("p"))}, want: "IEND", class: func(c Classes) int { return c.PNGNoIEND }},
		{name: "png with a truncated chunk", path: "x.png", file: memFile{data: buildPNG(ihdrChunk(), idatChunk("p"), iendChunk())[:40]}, want: "overflows", class: func(c Classes) int { return c.PNGNoIEND }},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			fsys := memFS{r.path: r.file}
			modes := map[string]string{}
			if r.mode != "" {
				modes[r.path] = r.mode
			}
			rep := scanOf(t, m, fsys, modes, nil, r.opts)
			if r.want == "" {
				if len(rep.Fatals) != 0 {
					t.Fatalf("unexpected fatals: %+v", rep.Fatals)
				}
				return
			}
			if !hasFatal(rep, r.path, r.want) {
				t.Fatalf("no fatal on %q mentioning %q; fatals = %+v", r.path, r.want, rep.Fatals)
			}
			if r.class != nil && r.class(rep.Classes) != 1 {
				t.Errorf("class counter = %d, want 1 (%+v)", r.class(rep.Classes), rep.Classes)
			}
			for _, f := range rep.Fatals {
				if strings.Contains(strings.ToLower(f.Reason), "zorblax") {
					t.Errorf("a fatal carries matched text: %q", f.Reason)
				}
			}
		})
	}
}

// TestScanTreeReadsEveryPNGTextClass: a name in a compressed text chunk, in an
// international text chunk (compressed or not) or after IEND is found, and each
// class is counted for the parity check.
func TestScanTreeReadsEveryPNGTextClass(t *testing.T) {
	m := plainMatcher(t)
	fsys := memFS{
		"z.png":     {data: buildPNG(ihdrChunk(), ztxtChunk("c", "by zorblax"), idatChunk(""), iendChunk())},
		"i.png":     {data: buildPNG(ihdrChunk(), itxtChunk("c", true, "by zorblax"), idatChunk(""), iendChunk())},
		"u.png":     {data: buildPNG(ihdrChunk(), itxtChunk("c", false, "by zorblax"), idatChunk(""), iendChunk())},
		"after.png": {data: append(buildPNG(ihdrChunk(), idatChunk(""), iendChunk()), []byte("trailer zorblax")...)},
		"plain.png": {data: buildPNG(ihdrChunk(), textChunk("c", "by zorblax"), idatChunk("zorblax"), iendChunk())},
		"pix.png":   {data: buildPNG(ihdrChunk(), idatChunk("zorblax"), iendChunk())},
	}
	rep := scanOf(t, m, fsys, nil, nil, ScanOptions{})
	if len(rep.Fatals) != 0 {
		t.Fatalf("fatals: %+v", rep.Fatals)
	}
	got := map[string]int{}
	for _, f := range rep.Findings {
		got[f.Path]++
	}
	want := map[string]int{"z.png": 1, "i.png": 1, "u.png": 1, "after.png": 1, "plain.png": 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings per file = %v, want %v (pixel data is out of scope)", got, want)
	}
	if c := rep.Classes; c.PNGNonStd != 3 || c.PNGCompressed != 2 || c.PNGAfterIEND != 1 || c.PNGNoIEND != 0 {
		t.Errorf("classes = %+v", c)
	}
}

func TestScanTreeLongRunFatalNamesTheLine(t *testing.T) {
	m := plainMatcher(t)
	fsys := memFS{"r.md": file("ok\nzorblax\n" + strings.Repeat("q", 97) + "\nlast\n")}
	rep := scanOf(t, m, fsys, nil, nil, ScanOptions{})
	if len(rep.Fatals) != 1 || rep.Fatals[0].Path != "r.md" || rep.Fatals[0].Line != 3 {
		t.Fatalf("fatals = %+v, want one on r.md:3", rep.Fatals)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Line != 2 {
		t.Errorf("the rest of the file is still scanned: %+v", rep.Findings)
	}
}

func TestScanTreeMissingFileIsCountedAndNamedOnly(t *testing.T) {
	m := plainMatcher(t)
	fsys := memFS{"gone.md": {err: fs.ErrNotExist}, "here.md": file("zorblax")}
	rep := scanOf(t, m, fsys, nil, nil, ScanOptions{})
	if rep.Missing != 1 || len(rep.Fatals) != 0 {
		t.Errorf("missing = %d fatals = %+v", rep.Missing, rep.Fatals)
	}
	rep = scanOf(t, m, fsys, nil, nil, ScanOptions{Required: true})
	if rep.Missing != 1 || !hasFatal(rep, "gone.md", "deleted") {
		t.Errorf("required: missing = %d fatals = %+v", rep.Missing, rep.Fatals)
	}
}

func TestScanTreeExemptRows(t *testing.T) {
	m := plainMatcher(t)
	fsys := memFS{
		"fonts/a.woff2": {data: []byte("wOF2\x00\x01\x02 binary")},
		"big.txt":       {data: make([]byte, 17<<20)},
		"run.md":        file(strings.Repeat("q", 100)),
		"link":          file("target"),
		"mod":           {err: errors.New("dir")},
	}
	for i := range fsys["big.txt"].data {
		fsys["big.txt"].data[i] = ' '
	}
	modes := map[string]string{"link": "120000", "mod": "160000"}
	blobs := map[string]string{"fonts/a.woff2": blobA, "big.txt": blobA, "run.md": blobA, "link": blobA, "mod": blobA}
	exempt := []ExemptRow{
		{Path: "fonts/a.woff2", Blob: blobA, Why: "binary"},
		{Path: "big.txt", Blob: blobA, Why: "oversize"},
		{Path: "run.md", Blob: blobA, Why: "longrun"},
		{Path: "link", Blob: blobA, Why: "symlink"},
		{Path: "mod", Blob: blobA, Why: "submodule"},
	}
	rep := scanOf(t, m, fsys, modes, blobs, ScanOptions{Exempt: exempt})
	if len(rep.Fatals) != 0 {
		t.Fatalf("exempt files still fatal: %+v", rep.Fatals)
	}
	if c := rep.Classes; c.Binaries != 1 || c.Oversize != 1 || c.Runs96 != 1 || c.Symlink != 1 || c.Submodule != 1 {
		t.Errorf("exempt files are still counted in the classes: %+v", c)
	}
	// A row for the wrong reason does not exempt.
	wrong := []ExemptRow{{Path: "fonts/a.woff2", Blob: blobA, Why: "oversize"}}
	rep = scanOf(t, m, memFS{"fonts/a.woff2": fsys["fonts/a.woff2"]}, nil, map[string]string{"fonts/a.woff2": blobA}, ScanOptions{Exempt: wrong})
	if !hasFatal(rep, "fonts/a.woff2", "binary") {
		t.Errorf("a row for another reason exempted the file: %+v", rep.Fatals)
	}
	// A stale row (the blob moved) says exactly that.
	stale := []ExemptRow{{Path: "fonts/a.woff2", Blob: blobB, Why: "binary"}}
	rep = scanOf(t, m, memFS{"fonts/a.woff2": fsys["fonts/a.woff2"]}, nil, map[string]string{"fonts/a.woff2": blobA}, ScanOptions{Exempt: stale})
	if !hasFatal(rep, "fonts/a.woff2", "exempt row is stale for fonts/a.woff2: regenerate the digest file") {
		t.Errorf("stale exempt row: %+v", rep.Fatals)
	}
}

func TestScanTreeBlindWhenRequired(t *testing.T) {
	m := plainMatcher(t)
	rep := ScanTree(m, nil, memFS{}, ScanOptions{Required: true})
	if len(rep.Fatals) != 1 || !strings.Contains(rep.Fatals[0].Reason, "gate went blind") {
		t.Fatalf("empty file set under required: %+v", rep.Fatals)
	}
	rep = ScanTree(m, nil, memFS{}, ScanOptions{})
	if len(rep.Fatals) != 0 {
		t.Fatalf("empty file set, not required: %+v", rep.Fatals)
	}
	// A filter that removes every file is not "blind": the list was the point.
	rep = ScanTree(m, []TrackedFile{{Mode: "100644", Blob: blobA, Path: "a.md"}}, memFS{"a.md": file("x")}, ScanOptions{Required: true, Filter: func(string) bool { return false }})
	if len(rep.Fatals) != 0 || rep.Scanned != 0 || rep.Total != 1 {
		t.Fatalf("filtered-out tree: %+v", rep)
	}
}

func TestScanTreeFilterAndCounts(t *testing.T) {
	m := plainMatcher(t)
	fsys := memFS{"a.md": file("zorblax"), "b.md": file("zorblax"), "c.bin": {data: []byte("\x00")}}
	rep := scanOf(t, m, fsys, nil, nil, ScanOptions{Filter: func(p string) bool { return p == "a.md" }})
	if rep.Scanned != 1 || rep.Total != 3 || len(rep.Findings) != 1 || rep.Findings[0].Path != "a.md" || len(rep.Fatals) != 0 {
		t.Fatalf("%+v", rep)
	}
}

func TestScanTreeIsDeterministicAcrossWorkers(t *testing.T) {
	m := plainMatcher(t)
	fsys := memFS{}
	for i := 0; i < 60; i++ {
		name := "d" + itoa(i%7) + "/f" + itoa(i) + ".md"
		fsys[name] = file("line\nzorblax and Quuxel\n" + strings.Repeat("x ", i))
	}
	one := scanOf(t, m, fsys, nil, nil, ScanOptions{Workers: 1})
	many := scanOf(t, m, fsys, nil, nil, ScanOptions{Workers: 8})
	if !reflect.DeepEqual(findingKeys(one), findingKeys(many)) || len(one.Findings) != 120 {
		t.Fatalf("worker counts disagree: %d vs %d findings", len(one.Findings), len(many.Findings))
	}
	if !sort.StringsAreSorted(findingKeys(one)) {
		t.Errorf("findings are not sorted")
	}
}

func TestScanTreeShapesAndKeylessMode(t *testing.T) {
	fsys := memFS{"a.md": file("pin " + gpu("9b1c42e7") + " here\n")}
	m := plainMatcher(t)
	rep := scanOf(t, m, fsys, nil, nil, ScanOptions{})
	if len(rep.Findings) != 0 {
		t.Errorf("shape rules ran although Shapes is off: %+v", rep.Findings)
	}
	rep = scanOf(t, m, fsys, nil, nil, ScanOptions{Shapes: true})
	if got := findingKeys(rep); !reflect.DeepEqual(got, []string{"a.md:1:5 shape:gpu-uuid"}) {
		t.Errorf("with shapes: %q", got)
	}
	// Keyless mode: no matcher at all, shape rules only (and no name scan).
	rep = scanOf(t, nil, memFS{"a.md": file("pin " + gpu("9b1c42e7") + " here\nzorblax"), "z.bin": {data: []byte("\x00")}}, nil, nil, ScanOptions{Shapes: true})
	if got := findingKeys(rep); !reflect.DeepEqual(got, []string{"a.md:1:5 shape:gpu-uuid"}) {
		t.Errorf("keyless: %q", got)
	}
	if !hasFatal(rep, "z.bin", "binary") {
		t.Errorf("keyless mode must keep the fail-closed rows: %+v", rep.Fatals)
	}
	rep = scanOf(t, nil, memFS{"a.md": file("x")}, nil, nil, ScanOptions{})
	if len(rep.Fatals) != 1 {
		t.Errorf("no matcher and no shapes scans nothing and must say so: %+v", rep)
	}
}

func TestScanTreeAllowRowsDropOnlyTheirOwnFinding(t *testing.T) {
	key := keyBytes("ab")
	m, _ := digestMatcher(t, "ab", nil, nil)
	fsys := memFS{
		"docs/x.md":            file("see Zorblax-Tool.md here\nand zorblax plain\n"),
		"other/x.md":           file("see Zorblax-Tool.md here\n"),
		"docs/zorblax-name.md": file("clean"),
	}
	opts := ScanOptions{Allow: []AllowRow{
		{Where: "body", Path: "docs/x.md", D: AllowDigest(key, "zorblax-tool.md")},
		{Where: "name", Path: "docs/zorblax-name.md", D: AllowDigest(key, "docs/zorblax-name.md")},
	}}
	rep := scanOf(t, m, fsys, nil, nil, opts)
	got := findingKeys(rep)
	// the chunk of a name finding is the run of [A-Za-z0-9_.-] around it: "zorblax-name.md".
	if len(got) < 2 {
		t.Fatalf("findings = %q", got)
	}
	has := func(prefix string) bool {
		for _, g := range got {
			if strings.HasPrefix(g, prefix) {
				return true
			}
		}
		return false
	}
	if has("docs/x.md:1:") {
		t.Errorf("the allowed finding was not dropped: %q", got)
	}
	if !has("docs/x.md:2:") {
		t.Errorf("an allow row dropped a different finding in the same file: %q", got)
	}
	if !has("other/x.md:1:") {
		t.Errorf("an allow row for one path dropped a finding in another: %q", got)
	}
	// A body allow row never allows a name finding and vice versa.
	if !has("docs/zorblax-name.md (name)") {
		t.Errorf("a name allow with the wrong chunk dropped the finding: %q", got)
	}
	opts.Allow[1].D = AllowDigest(key, "zorblax-name.md")
	rep = scanOf(t, m, fsys, nil, nil, opts)
	for _, g := range findingKeys(rep) {
		if strings.HasPrefix(g, "docs/zorblax-name.md (name)") {
			t.Errorf("the name allow row did not apply: %q", g)
		}
	}
	// Plaintext matchers hold no key and cannot evaluate allow rows.
	rep = scanOf(t, plainMatcher(t), fsys, nil, nil, opts)
	if len(rep.Fatals) == 0 {
		t.Errorf("allow rows with a keyless matcher must be fatal, not ignored")
	}
}

// TestDigestScanEqualsPlaintextScan: the digest-mode scanner and the plaintext
// scanner report the same findings over one in-memory tree (the oracle has no
// digest mode, so this is where that equality is proven).
func TestDigestScanEqualsPlaintextScan(t *testing.T) {
	plain := plainMatcher(t)
	digest, df := digestMatcher(t, "ab", nil, nil)
	key := keyBytes("ab")
	fsys := memFS{
		"a.md":            file("zorblax\nGribbleDevices and gribble_x\n"),
		"b.md":            file("a Quuxel box; \\nfrobnic; \\x0aquuxel; %2Fzorblax; Plugh%20Xyzzy; PlughXyzzy\n"),
		"c.md":            file("J:\\Frotz\\x /mnt/k/frotz/plover K:/Frotz%20Plover %j frotz\n"),
		"d.md":            file("wsl --distribution froodtoken; FroodToken product; 8128 and 38128 and 33K and 33kx\n"),
		"e.md":            file("Zorbl" + string(rune(0x00E1)) + "x and Quu" + string(rune(0x200B)) + "xel and \\u007aorblax\n"),
		"f.txt":           {data: utf16Bytes("zorblax\nplugh xyzzy", false, true)},
		"g.png":           {data: buildPNG(ihdrChunk(), textChunk("c", "grue lamp"), idatChunk("zorblax"), iendChunk())},
		"h/zorblax-x.md":  file("clean"),
		"i.md":            file("nothing here at all\n"),
		"j.md":            file("pin " + gpu("9b1c42e7") + " zorblax\n" + strings.Repeat("q", 100)),
		"k.bin":           {data: []byte("\x00zorblax")},
		"l/FrobnicWidget": file(CanaryText()),
	}
	opts := ScanOptions{Shapes: true}
	pr := scanOf(t, plain, fsys, nil, nil, opts)
	dr := scanOf(t, digest, fsys, nil, nil, opts)
	if len(pr.Findings) < 20 {
		t.Fatalf("the fixture tree is too quiet: %d findings", len(pr.Findings))
	}
	// Map each plaintext entry to the id its digest carries.
	ids := map[string]string{}
	for _, e := range WithCanary(synthEntries(t)) {
		ids[string(e.Mode)+":"+e.Text] = EntryDigest(key, e).D[:8]
	}
	norm := func(r Report, plainMode bool) []string {
		var out []string
		for _, f := range r.Findings {
			id := f.ID
			switch {
			case f.Mode == "shape":
				id = f.Text
			case plainMode:
				id = ids[f.Mode+":"+f.Text]
				if id == "" {
					t.Fatalf("plaintext finding %q has no digest id", f.Label())
				}
			}
			out = append(out, f.Path+boolStr(f.Name)+":"+itoa(f.Line)+":"+itoa(f.Col)+" "+f.Mode+" "+id)
		}
		sort.Strings(out)
		return out
	}
	if a, b := norm(pr, true), norm(dr, false); !reflect.DeepEqual(a, b) {
		t.Fatalf("digest-mode findings differ from plaintext-mode findings\nplain  %q\ndigest %q", a, b)
	}
	if !reflect.DeepEqual(fatalReasons(pr), fatalReasons(dr)) || pr.Classes != dr.Classes || pr.Scanned != dr.Scanned {
		t.Errorf("fatals/classes differ: %+v vs %+v", pr, dr)
	}
	_ = df
}

func boolStr(b bool) string {
	if b {
		return "(name)"
	}
	return ""
}

func TestParseLsFiles(t *testing.T) {
	out := "100644 " + blobA + " 0\tdocs/a.md\x00" +
		"120000 " + blobB + " 0\tlink\x00" +
		"160000 " + blobA + " 0\tsub\x00" +
		"100755 " + blobB + " 0\tdir with space/b\tc.sh\x00"
	got, err := ParseLsFiles([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []TrackedFile{
		{Mode: "100644", Blob: blobA, Path: "docs/a.md"},
		{Mode: "120000", Blob: blobB, Path: "link"},
		{Mode: "160000", Blob: blobA, Path: "sub"},
		{Mode: "100755", Blob: blobB, Path: "dir with space/b\tc.sh"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if got, err := ParseLsFiles(nil); err != nil || len(got) != 0 {
		t.Errorf("empty input: %v %v", got, err)
	}
	for _, bad := range []string{
		"100644 " + blobA + " 1\tconflict.md\x00",
		"100644 " + blobA + "\tno-stage\x00",
		"garbage\x00",
		"100644 " + blobA + " 0\x00",
		"100644 " + blobA + " 0\tmissing-terminator",
	} {
		if _, err := ParseLsFiles([]byte(bad)); err == nil {
			t.Errorf("ParseLsFiles(%q) accepted a malformed record", bad)
		}
	}
}

func TestDirFS(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := DirFS{Root: dir}
	data, size, err := d.Read("sub/a.txt", 100)
	if err != nil || string(data) != "hello" || size != 5 {
		t.Fatalf("read: %q %d %v", data, size, err)
	}
	data, size, err = d.Read("sub/a.txt", 4)
	if err != nil || data != nil || size != 5 {
		t.Fatalf("over the limit must return the size and no data: %q %d %v", data, size, err)
	}
	if _, _, err = d.Read("sub/missing.txt", 100); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, _, err = d.Read("sub", 100); err == nil {
		t.Fatal("a directory read must be an error")
	}
	if _, _, err = d.Read("../outside", 100); err == nil {
		t.Fatal("a path escaping the root must be an error")
	}
	if _, _, err = d.Read("/abs/olute", 100); err == nil {
		t.Fatal("an absolute path must be an error")
	}
}

func TestFileListGrammar(t *testing.T) {
	text := "# a comment\r\n" +
		"\r\n" +
		"docs/a.md\r\n" +
		"  docs/b.md  \n" +
		"?docs/new.md\n" +
		"adr/old.md=>adr/new.md\n" +
		"x.md # trailing comments are not part of the grammar\n"
	specs, err := ParseFileList(text)
	if err != nil {
		t.Fatal(err)
	}
	// a hash after the first non-blank character is part of the path: only a
	// line that starts with one is a comment.
	if len(specs) != 5 {
		t.Fatalf("specs = %+v", specs)
	}
	if specs[0].Path != "docs/a.md" || specs[1].Path != "docs/b.md" || !specs[2].Optional || specs[2].Path != "docs/new.md" ||
		specs[3].Old != "adr/old.md" || specs[3].New != "adr/new.md" {
		t.Fatalf("specs = %+v", specs)
	}
	for _, bad := range []string{
		"a.md\na.md\n",
		"a.md\n?a.md\n",
		"a.md\nb.md=>a.md\n",
		"b.md=>b.md\n",
		"=>b.md\n",
		"a.md=>\n",
		"a=>b=>c\n",
		"?a.md=>b.md\n",
		"?\n",
		"/abs/a.md\n",
		"../up.md\n",
		"a\\b.md\n",
	} {
		if _, err := ParseFileList(bad); err == nil {
			t.Errorf("ParseFileList(%q) accepted a malformed list", bad)
		}
	}
}

func TestResolveFileList(t *testing.T) {
	tracked := map[string]bool{"a.md": true, "b.md": true, "old.md": true}
	mk := func(text string) []FileSpec {
		s, err := ParseFileList(text)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	got, err := ResolveFileList(mk("a.md\n?absent.md\n?b.md\nold.md=>new.md\n"), tracked)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a.md": true, "b.md": true, "old.md": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved = %v", got)
	}
	tracked2 := map[string]bool{"a.md": true, "new.md": true}
	got, err = ResolveFileList(mk("old.md=>new.md\n"), tracked2)
	if err != nil || !reflect.DeepEqual(got, map[string]bool{"new.md": true}) {
		t.Fatalf("rename after the chunk: %v %v", got, err)
	}
	for _, c := range []struct{ name, list string }{
		{"stale path", "gone.md\n"},
		{"rename with both sides present", "a.md=>b.md\n"},
		{"rename with neither side present", "x.md=>y.md\n"},
	} {
		if _, err := ResolveFileList(mk(c.list), tracked); err == nil {
			t.Errorf("%s: no error (a stale list must never be a vacuous pass)", c.name)
		}
	}
}

// TestScanTreeBOMThatIsNotUTF16IsNeverSilentlySkipped: a file that starts with a
// byte-order mark but is not valid UTF-16 (an odd payload, an unpaired
// surrogate) used to be "decoded" into noise and scanned as such, so a listed
// name in its raw bytes passed unseen. The raw bytes are scanned too, and a file
// that does not decode is a Fatal (exempt only by path and blob, like any other
// unknown binary). Valid UTF-16 with a mark is still decoded and scanned.
func TestScanTreeBOMThatIsNotUTF16IsNeverSilentlySkipped(t *testing.T) {
	m := plainMatcher(t)
	bom := []byte{0xFF, 0xFE}
	cat := func(parts ...[]byte) []byte {
		var out []byte
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	u16 := func(units ...uint16) []byte {
		var out []byte
		for _, u := range units {
			out = append(out, byte(u), byte(u>>8))
		}
		return out
	}
	// "zor" + a lone high surrogate + "blax": decoded, the name is split by the
	// replacement character, so only the Fatal can flag it.
	split := u16('z', 'o', 'r', 0xD800, 'b', 'l', 'a', 'x')
	rows := []struct {
		name      string
		data      []byte
		wantFatal bool
		wantFind  bool
	}{
		{"odd payload, ASCII name, non-text bytes", cat(bom, []byte("zorblax"), []byte{0x00, 0x01}), true, true},
		{"BOM then plain ASCII text", cat(bom, []byte("zorblax")), true, true},
		{"even payload of plain ASCII text decodes to other scripts", cat(bom, []byte("zorblax!")), false, true},
		{"unpaired surrogate splits the name", cat(bom, split), true, false},
		{"big-endian mark with an odd payload", cat([]byte{0xFE, 0xFF}, []byte("zorblax")), true, true},
		{"valid little-endian text", utf16Bytes("a zorblax b", false, true), false, true},
		{"valid big-endian text", utf16Bytes("a zorblax b", true, true), false, true},
		{"little-endian mark over a big-endian payload", cat(bom, utf16Bytes("a zorblax b", true, false)), false, true},
		{"big-endian mark over a little-endian payload", cat([]byte{0xFE, 0xFF}, utf16Bytes("a zorblax b", false, false)), false, true},
		{"valid little-endian text, nothing listed", utf16Bytes("nothing here", false, true), false, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			rep := scanOf(t, m, memFS{"t.txt": {data: r.data}}, nil, nil, ScanOptions{})
			if gotFatal := len(rep.Fatals) > 0; gotFatal != r.wantFatal {
				t.Errorf("fatal = %v (%+v), want %v", gotFatal, rep.Fatals, r.wantFatal)
			}
			if gotFind := len(rep.Findings) > 0; gotFind != r.wantFind {
				t.Errorf("findings = %v (%v), want %v", gotFind, findingKeys(rep), r.wantFind)
			}
		})
	}
	// an exempt row bound to the blob accepts the file as it does any other
	// binary; the finding in its raw bytes is still reported.
	bad := cat(bom, []byte("zorblax"))
	exempt := []ExemptRow{{Path: "t.txt", Blob: blobA, Why: WhyBinary}}
	fsys := memFS{"t.txt": {data: bad}}
	rep := scanOf(t, m, fsys, nil, map[string]string{"t.txt": blobA}, ScanOptions{Exempt: exempt})
	if len(rep.Fatals) != 0 || len(rep.Findings) == 0 {
		t.Errorf("exempt BOM file: fatals %+v findings %v", rep.Fatals, findingKeys(rep))
	}
	rep = scanOf(t, m, fsys, nil, map[string]string{"t.txt": blobB}, ScanOptions{Exempt: exempt})
	if !hasFatal(rep, "t.txt", "stale") {
		t.Errorf("a stale exempt row must say so: %+v", rep.Fatals)
	}
	// nothing the scan reports carries matched text.
	rep = scanOf(t, m, memFS{"t.txt": {data: bad}}, nil, nil, ScanOptions{})
	for _, f := range rep.Fatals {
		if strings.Contains(strings.ToLower(f.Reason), "zorblax") {
			t.Errorf("a fatal carries matched text: %q", f.Reason)
		}
	}
}
