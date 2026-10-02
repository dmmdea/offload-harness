package leakgate

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"
)

func pngChunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(data)))
	b.Write(n[:])
	b.WriteString(typ)
	b.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc.Sum32())
	b.Write(c[:])
	return b.Bytes()
}

func buildPNG(chunks ...[]byte) []byte {
	out := append([]byte{}, pngSignature...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

func ihdrChunk() []byte {
	return pngChunk("IHDR", []byte{0, 0, 0, 1, 0, 0, 0, 1, 8, 2, 0, 0, 0})
}

func idatChunk(payload string) []byte { return pngChunk("IDAT", []byte(payload)) }
func iendChunk() []byte               { return pngChunk("IEND", nil) }
func textChunk(keyword, text string) []byte {
	return pngChunk("tEXt", []byte(keyword+"\x00"+text))
}

func deflate(s string) []byte {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func ztxtChunk(keyword, text string) []byte {
	body := append([]byte(keyword+"\x00\x00"), deflate(text)...)
	return pngChunk("zTXt", body)
}

func itxtChunk(keyword string, compressed bool, text string) []byte {
	body := []byte(keyword + "\x00")
	data := []byte(text)
	if compressed {
		body = append(body, 1, 0)
		data = deflate(text)
	} else {
		body = append(body, 0, 0)
	}
	body = append(body, []byte("en\x00translated\x00")...)
	return pngChunk("iTXt", append(body, data...))
}

func joined(info PNGInfo) string {
	parts := make([]string, len(info.Segments))
	for i, s := range info.Segments {
		parts[i] = string(s)
	}
	return strings.Join(parts, "\n")
}

func TestPNGPlainFileScansTextChunksNotPixels(t *testing.T) {
	p := buildPNG(ihdrChunk(), textChunk("Comment", "made by zorblax"), idatChunk("zorblax in pixels"), iendChunk())
	if !IsPNG(p) {
		t.Fatal("IsPNG false for a valid signature")
	}
	info, err := ParsePNG(p)
	if err != nil {
		t.Fatal(err)
	}
	text := joined(info)
	if !strings.Contains(text, "made by zorblax") {
		t.Errorf("the tEXt body is not among the segments: %q", text)
	}
	if strings.Count(text, "zorblax") != 1 {
		t.Errorf("pixel data (IDAT) must not be scanned: %q", text)
	}
	if info.NonStd || info.AfterIEND || info.CompressedText || info.NoIEND {
		t.Errorf("a standard PNG carries no divergence flag: %+v", info)
	}
}

func TestPNGCompressedTextIsInflated(t *testing.T) {
	p := buildPNG(ihdrChunk(), ztxtChunk("Comment", "zlib zorblax"), itxtChunk("Title", true, "intl zorblax"), itxtChunk("Plain", false, "raw zorblax"), idatChunk(""), iendChunk())
	info, err := ParsePNG(p)
	if err != nil {
		t.Fatal(err)
	}
	text := joined(info)
	if strings.Count(text, "zorblax") != 3 {
		t.Errorf("want the three text chunks inflated and scanned: %q", text)
	}
	if !info.CompressedText || !info.NonStd {
		t.Errorf("flags: %+v", info)
	}
}

func TestPNGBytesAfterIENDAreScanned(t *testing.T) {
	p := append(buildPNG(ihdrChunk(), idatChunk(""), iendChunk()), []byte("trailer zorblax")...)
	info, err := ParsePNG(p)
	if err != nil {
		t.Fatal(err)
	}
	if !info.AfterIEND || !strings.Contains(joined(info), "trailer zorblax") {
		t.Errorf("bytes after IEND not scanned: %+v %q", info, joined(info))
	}
}

func TestPNGUnknownChunksAreScanned(t *testing.T) {
	p := buildPNG(ihdrChunk(), pngChunk("pHYs", []byte("phys zorblax")), idatChunk(""), iendChunk())
	info, err := ParsePNG(p)
	if err != nil {
		t.Fatal(err)
	}
	if !info.NonStd || !strings.Contains(joined(info), "phys zorblax") {
		t.Errorf("an ancillary chunk body was not scanned: %+v", info)
	}
}

// TestPNGSegmentOrder pins the line-numbering convention: the text chunks come
// first, joined by a newline in file order (so their line numbers equal what a
// scan of only the text chunks gives), then every other chunk body, then the
// bytes after IEND.
func TestPNGSegmentOrder(t *testing.T) {
	p := append(buildPNG(ihdrChunk(), textChunk("A", "first"), pngChunk("pHYs", []byte("other")), textChunk("B", "second"), idatChunk(""), iendChunk()), []byte("tail")...)
	info, err := ParsePNG(p)
	if err != nil {
		t.Fatal(err)
	}
	segs := make([]string, len(info.Segments))
	for i, s := range info.Segments {
		segs[i] = string(s)
	}
	if len(segs) < 5 || segs[0] != "A\x00first" || segs[1] != "B\x00second" {
		t.Fatalf("segments = %q", segs)
	}
	if segs[len(segs)-1] != "tail" {
		t.Errorf("the bytes after IEND are not last: %q", segs)
	}
	text := joined(info)
	if !strings.Contains(text, "other") {
		t.Errorf("the unknown chunk is missing: %q", text)
	}
}

func TestPNGFatalShapes(t *testing.T) {
	good := buildPNG(ihdrChunk(), textChunk("k", "v"), idatChunk("pixels"), iendChunk())
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"missing IEND", buildPNG(ihdrChunk(), textChunk("k", "v"), idatChunk("pixels")), "IEND"},
		{"truncated inside a chunk", good[:len(good)-20], "overflows"},
		{"length overflows the file", append(append([]byte{}, pngSignature...), 0xff, 0xff, 0xff, 0xf0, 'I', 'H', 'D', 'R', 1, 2, 3), "overflows"},
		{"signature only", append([]byte{}, pngSignature...), "IEND"},
		{"corrupt compressed text", buildPNG(ihdrChunk(), pngChunk("zTXt", []byte("k\x00\x00not a zlib stream")), iendChunk()), "compressed"},
		{"compressed text over the cap", buildPNG(ihdrChunk(), ztxtChunk("k", strings.Repeat("a b ", 5<<20)), iendChunk()), "cap"},
	}
	for _, c := range cases {
		_, err := ParsePNG(c.data)
		if err == nil {
			t.Errorf("%s: no error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
	}
	if IsPNG(good[:7]) || IsPNG([]byte("not a png at all")) || IsPNG(nil) {
		t.Error("IsPNG accepted a bad signature")
	}
}

// TestPNGInflateCapIsExact: the cap is 16 MiB of inflated text; one byte under
// is fine, one byte over is fatal.
func TestPNGInflateCapIsExact(t *testing.T) {
	const cap16 = 16 << 20
	under := buildPNG(ihdrChunk(), ztxtChunk("k", strings.Repeat("ab ", (cap16-8)/3)), iendChunk())
	if _, err := ParsePNG(under); err != nil {
		t.Errorf("under the cap: %v", err)
	}
	over := buildPNG(ihdrChunk(), ztxtChunk("k", strings.Repeat("ab ", cap16/3+2)), iendChunk())
	if _, err := ParsePNG(over); err == nil {
		t.Error("over the cap: no error")
	}
}
