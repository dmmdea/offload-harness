package leakgate

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
)

// PNG files hold text beside the pixels: tEXt, zTXt and iTXt chunks, and
// anything an editor appended after IEND. The walk reads every chunk except
// IDAT (pixel data is out of scope, stated), inflates compressed text up to
// 16 MiB, and scans the bytes after IEND. A file that cannot be walked to its
// IEND is a fatal, never a silent pass.

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// maxInflate caps the inflated size of one compressed text chunk.
const maxInflate = 16 << 20

// The walk's fatal conditions.
var (
	ErrPNGSignature = errors.New("png: bad signature")
	ErrPNGOverflow  = errors.New("png: chunk length overflows the file")
	ErrPNGNoIEND    = errors.New("png: missing IEND")
	ErrPNGInflate   = errors.New("png: compressed text over the 16 MiB cap")
	ErrPNGCorrupt   = errors.New("png: corrupt compressed text chunk")
)

// PNGInfo is what the walk found. Segments are the byte ranges to scan, in a
// fixed order: the text chunks first (tEXt, zTXt and iTXt, in file order; the
// keyword is kept and a compressed body is inflated), so that the line numbers
// of a text finding equal those of a scan of the text chunks alone; then the
// bodies of every other chunk but IDAT; then the bytes after IEND. A scan
// joins them with a newline.
type PNGInfo struct {
	Segments       [][]byte
	NonStd         bool // a chunk other than IHDR, tEXt, IDAT and IEND
	AfterIEND      bool // bytes after IEND
	CompressedText bool // a zTXt chunk or a compressed iTXt chunk
	NoIEND         bool // no IEND reached: truncated, or a length that overflows the file
}

// IsPNG reports whether data starts with the PNG signature.
func IsPNG(data []byte) bool {
	return len(data) >= len(pngSignature) && bytes.Equal(data[:len(pngSignature)], pngSignature)
}

func inflateCapped(body []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, ErrPNGCorrupt
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxInflate+1))
	if err != nil {
		return nil, ErrPNGCorrupt
	}
	if len(out) > maxInflate {
		return nil, ErrPNGInflate
	}
	return out, nil
}

// ParsePNG walks the chunks of a PNG. It returns what could be extracted even
// when it also returns an error, so a scan can still read what is there before
// it fails.
func ParsePNG(data []byte) (PNGInfo, error) {
	var info PNGInfo
	if !IsPNG(data) {
		return info, ErrPNGSignature
	}
	var texts, others [][]byte
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	pos := len(pngSignature)
	sawIEND := false
	for pos < len(data) && !sawIEND {
		if len(data)-pos < 8 {
			info.NoIEND = true
			fail(ErrPNGOverflow)
			break
		}
		n := uint64(binary.BigEndian.Uint32(data[pos : pos+4]))
		typ := string(data[pos+4 : pos+8])
		end := uint64(pos) + 8 + n + 4 // the body, then its CRC
		if end > uint64(len(data)) {
			info.NoIEND = true
			fail(ErrPNGOverflow)
			break
		}
		body := data[pos+8 : pos+8+int(n)]
		pos = int(end)
		switch typ {
		case "IDAT":
			// pixel data: out of scope.
		case "IHDR":
			others = append(others, body)
		case "IEND":
			others = append(others, body)
			sawIEND = true
		case "tEXt":
			texts = append(texts, body)
		case "zTXt":
			info.NonStd = true
			info.CompressedText = true
			texts = append(texts, zTXtSegment(body, fail))
		case "iTXt":
			info.NonStd = true
			texts = append(texts, iTXtSegment(body, &info, fail))
		default:
			info.NonStd = true
			others = append(others, body)
		}
	}
	if !sawIEND && !info.NoIEND {
		info.NoIEND = true
		fail(ErrPNGNoIEND)
	}
	info.Segments = append(info.Segments, texts...)
	info.Segments = append(info.Segments, others...)
	if sawIEND && pos < len(data) {
		info.AfterIEND = true
		info.Segments = append(info.Segments, data[pos:])
	}
	return info, firstErr
}

// zTXtSegment is keyword, NUL, then the inflated text. A chunk that cannot be
// read is still scanned as it is, and reported.
func zTXtSegment(body []byte, fail func(error)) []byte {
	k := bytes.IndexByte(body, 0)
	if k < 0 || k+2 > len(body) || body[k+1] != 0 {
		fail(ErrPNGCorrupt)
		return body
	}
	text, err := inflateCapped(body[k+2:])
	if err != nil {
		fail(err)
		return body[:k+1]
	}
	return append(append([]byte(nil), body[:k+1]...), text...)
}

// iTXtSegment is keyword, NUL, flag, method, language tag, NUL, translated
// keyword, NUL, then the text (inflated when the flag says so). The fields
// before the text are scanned as they are.
func iTXtSegment(body []byte, info *PNGInfo, fail func(error)) []byte {
	k := bytes.IndexByte(body, 0)
	if k < 0 || k+3 > len(body) {
		fail(ErrPNGCorrupt)
		return body
	}
	flag := body[k+1]
	rest := body[k+3:]
	p1 := bytes.IndexByte(rest, 0)
	if p1 < 0 {
		fail(ErrPNGCorrupt)
		return body
	}
	p2 := bytes.IndexByte(rest[p1+1:], 0)
	if p2 < 0 {
		fail(ErrPNGCorrupt)
		return body
	}
	head := body[:k+3+p1+1+p2+1]
	text := body[len(head):]
	if flag == 1 {
		info.CompressedText = true
		inflated, err := inflateCapped(text)
		if err != nil {
			fail(err)
			return head
		}
		text = inflated
	}
	return append(append([]byte(nil), head...), text...)
}
