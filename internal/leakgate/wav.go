package leakgate

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// WAV files hold text beside the samples: LIST/INFO tags, bext, iXML, id3,
// cue labels and notes, and anything an editor appended after the RIFF. The
// walk reads every chunk except the two opaque ones (fmt, the format header,
// and data, the PCM samples; sample data is out of scope, stated), and the
// bytes after the declared RIFF end. Opaque means only what a player reads
// there: bytes of a fmt chunk past the 40-byte header and any data chunk after
// the first are scanned as text, and a long printable run inside the samples
// is a fatal. A chunk id the walk does not know is
// text-bearing, never skipped. A file that cannot be walked to the end of its
// RIFF, or that has no fmt and no data chunk, is a fatal, never a silent pass.

// The walk's fatal conditions.
var (
	ErrWAVSignature = errors.New("wav: bad signature")
	ErrWAVOverflow  = errors.New("wav: chunk length overflows the file")
	ErrWAVNoChunks  = errors.New("wav: missing fmt or data chunk")
	// ErrWAVTextInData: the sample chunk is the one place the walk does not read, so text parked there would ride
	// through unseen. Sample bytes do not form long printable runs; a file that does is refused and needs an exempt row.
	ErrWAVTextInData = errors.New("wav: a run of printable text inside the data chunk")
)

const (
	wavFmtHeaderMax = 40 // bytes of a fmt chunk that are format header (16, 18 or 40)
	wavTextRunMin   = 32 // consecutive printable-ASCII bytes in the sample chunk that count as text
)

// hasPrintableRun reports whether b holds min or more consecutive printable-ASCII bytes (0x20 to 0x7e).
func hasPrintableRun(b []byte, min int) bool {
	run := 0
	for _, c := range b {
		if c >= 0x20 && c <= 0x7e {
			run++
			if run >= min {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// WAVInfo is what the walk found. Segments are the byte ranges to scan, in
// file order: the body of every chunk but fmt and data, then the bytes after
// the declared RIFF end. A scan joins them with a newline.
type WAVInfo struct {
	Segments [][]byte
}

// IsWAV reports whether data starts with a RIFF header of form type WAVE.
func IsWAV(data []byte) bool {
	return len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE"))
}

// ParseWAV walks the chunks of a WAV. It returns what could be extracted even
// when it also returns an error, so a scan can still read what is there before
// it fails.
func ParseWAV(data []byte) (WAVInfo, error) {
	var info WAVInfo
	if !IsWAV(data) {
		return info, ErrWAVSignature
	}
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	// The RIFF size counts the bytes after its own 8-byte header.
	limit := uint64(binary.LittleEndian.Uint32(data[4:8])) + 8
	if limit > uint64(len(data)) {
		fail(ErrWAVOverflow)
		limit = uint64(len(data))
	}
	if limit < 12 {
		fail(ErrWAVOverflow)
		limit = 12
	}
	pos := uint64(12)
	sawFmt, sawData := false, false
	for pos < limit {
		if limit-pos < 8 {
			fail(ErrWAVOverflow)
			info.Segments = append(info.Segments, data[pos:limit])
			pos = limit
			break
		}
		typ := string(data[pos : pos+4])
		n := uint64(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		bodyStart := pos + 8
		bodyEnd := bodyStart + n
		if bodyEnd > limit {
			fail(ErrWAVOverflow)
			// The rest of the declared range is still scanned as it is.
			info.Segments = append(info.Segments, data[bodyStart:limit])
			pos = limit
			break
		}
		switch typ {
		case "fmt ":
			sawFmt = true
			// The widest standard header (WAVE_FORMAT_EXTENSIBLE) is 40 bytes; anything after it is not format data.
			if n > wavFmtHeaderMax {
				info.Segments = append(info.Segments, data[bodyStart+wavFmtHeaderMax:bodyEnd])
			}
		case "data":
			if sawData {
				// A second data chunk is not what a player reads as the samples: it is scanned like any other chunk.
				info.Segments = append(info.Segments, data[bodyStart:bodyEnd])
				break
			}
			sawData = true
			if hasPrintableRun(data[bodyStart:bodyEnd], wavTextRunMin) {
				fail(ErrWAVTextInData)
			}
		default:
			// LIST, bext, iXML, id3 , ID3 , cue , labl, note, and any id not known.
			info.Segments = append(info.Segments, data[bodyStart:bodyEnd])
		}
		pos = bodyEnd + n&1 // chunks are padded to an even length
	}
	if !sawFmt || !sawData {
		fail(ErrWAVNoChunks)
	}
	if pos < uint64(len(data)) {
		info.Segments = append(info.Segments, data[pos:])
	}
	return info, firstErr
}
