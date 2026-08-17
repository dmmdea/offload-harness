// Package mediahash content-addresses a media file for cache-key purposes.
//
// # Why this exists (T2-A2)
//
// The harness's media cache keys were built from the file's PATH plus, for audio,
// its size and mtime. Three consequences, in severity order:
//
//  1. **A false HIT is possible.** Replace a file at the same path — a re-encoded
//     video, a re-recorded take — and a key built from the path alone matches. The
//     cache then serves the OLD file's transcript or description as if it were the
//     new one. Audio's size+mtime narrows this but does not close it (a same-size
//     replacement with a preserved mtime collides).
//  2. Copy an identical file to a second path and it always MISSES, which is
//     exactly the reuse an artifact cache exists to capture.
//  3. `touch` a file with unchanged content and audio misses.
//
// The image path already did this correctly (`"img:"+sha256hex(loaded bytes)`).
// This package brings audio and video to the same standard.
//
// # Full hash by default, and why
//
// Hashing is cheap relative to what it guards: SHA256 runs at ~1.2 GB/s on this
// class of machine, while the calls being cached (whisper transcription, ffmpeg
// frame sampling plus a VLM pass) take seconds to minutes. Paying ~1 s on a 1 GB
// video to make its identity exact is not a trade-off worth agonising over.
//
// A SAMPLED mode exists for pathological sizes, but it is OPT-IN and never the
// default, because its failure mode is the one this package was written to
// eliminate: two large files that happen to agree on size and on the sampled
// windows would collide and serve each other's results. That is a wrong answer,
// not a slow one. When it is enabled the mode is recorded IN the digest string,
// so a sampled digest and a full digest of the same file can never be mistaken
// for one another.
package mediahash

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// sampleWindow is how many bytes are read from each of the head, middle and tail
// in sampled mode.
const sampleWindow = 8 << 20 // 8 MiB

// Digest returns a stable content identity for the file at path.
//
// maxFullBytes selects the mode:
//
//	<= 0  → always hash the whole file (the default, and the only mode with an
//	        exact identity guarantee).
//	>  0  → files larger than this use a sampled digest instead.
//
// The returned string always names its own mode, so digests taken under
// different settings cannot collide. On any error the caller receives a digest
// that encodes the FAILURE rather than a zero value: a file that cannot be read
// must not silently share a key with every other unreadable file, and must not
// look like a successful hash of empty content.
func Digest(path string, maxFullBytes int64) string {
	if path == "" {
		return "media:none"
	}
	fi, err := os.Stat(path)
	if err != nil {
		// Distinct per path AND per error, so an unreadable file neither collides
		// with another unreadable file nor is mistaken for a hashed one. It also
		// means a transient failure does not permanently poison a key: once the
		// file is readable the digest changes to the real one.
		return "media:staterr:" + shortHex(path+"\x00"+err.Error())
	}
	if fi.IsDir() {
		return "media:isdir:" + shortHex(path)
	}
	size := fi.Size()
	if maxFullBytes > 0 && size > maxFullBytes {
		d, serr := sampled(path, size)
		if serr != nil {
			return "media:readerr:" + shortHex(path+"\x00"+serr.Error())
		}
		return fmt.Sprintf("media:sampled:sz=%d:%s", size, d)
	}
	d, herr := full(path)
	if herr != nil {
		return "media:readerr:" + shortHex(path+"\x00"+herr.Error())
	}
	return fmt.Sprintf("media:sha256:sz=%d:%s", size, d)
}

func full(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sampled hashes the size plus three fixed windows (head, middle, tail).
//
// The size is folded in FIRST so two files differing only in length can never
// agree, which removes the most common near-miss. The remaining exposure — two
// same-size files identical across all three windows — is real and is why this
// mode is opt-in.
func sampled(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	fmt.Fprintf(h, "size=%d\x00", size)
	offsets := []int64{0, (size - sampleWindow) / 2, size - sampleWindow}
	buf := make([]byte, sampleWindow)
	for _, off := range offsets {
		if off < 0 {
			off = 0
		}
		n, rerr := f.ReadAt(buf, off)
		// io.EOF with n > 0 is a short final window, not a failure.
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return "", rerr
		}
		if n <= 0 {
			continue
		}
		fmt.Fprintf(h, "off=%d\x00", off)
		h.Write(buf[:n])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func shortHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}
