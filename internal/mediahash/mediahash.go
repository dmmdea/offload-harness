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
// Hashing is cheap RELATIVE TO WHAT IT GUARDS, which is the only comparison that
// matters here: every caller already reads the same file through ffmpeg before
// hashing it, and the model pass that follows takes seconds to minutes. The cost
// is a cold file READ, so it is I/O-bound rather than SHA-bound — on a
// network- or Drive-backed mount it tracks that device's throughput, not memory
// bandwidth. No absolute rate is claimed here because none has been measured on
// the volumes this fleet actually stores media on.
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

// Digest returns a stable content identity for the file at path, or an error.
//
// maxFullBytes selects the mode:
//
//	<= 0  → always hash the whole file (the default, and the only mode with an
//	        exact identity guarantee).
//	>  0  → files larger than this use a sampled digest instead.
//
// The returned string always names its own mode, so digests taken under
// different settings cannot collide.
//
// # Failure MUST be an error, never a digest
//
// An earlier version returned a synthetic string like `media:staterr:<hash of
// path+error>` so callers never had to branch. That was wrong in the most
// damaging possible way: the caller used it as a cache key, so a transient read
// failure wrote a durable entry under a key derived from the PATH — exactly the
// path-keyed identity this package exists to abolish. Two different files that
// hit the same error at the same path then served each other's results.
//
// So the contract is: no identity, no key. A caller that cannot obtain a digest
// must bypass the cache entirely — compute the answer, return it, store nothing.
func Digest(path string, maxFullBytes int64) (string, error) {
	if path == "" {
		return "", errors.New("mediahash: empty path")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("mediahash: stat: %w", err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("mediahash: %s is a directory", path)
	}
	size := fi.Size()
	if maxFullBytes > 0 && size > maxFullBytes {
		d, serr := sampled(path, size)
		if serr != nil {
			return "", fmt.Errorf("mediahash: sampled read: %w", serr)
		}
		return fmt.Sprintf("media:sampled:sz=%d:%s", size, d), nil
	}
	d, herr := full(path)
	if herr != nil {
		return "", fmt.Errorf("mediahash: read: %w", herr)
	}
	return fmt.Sprintf("media:sha256:sz=%d:%s", size, d), nil
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
	// De-duplicated. For a file at or under one window all three offsets clamp to
	// 0, and the previous version then read and hashed the SAME bytes three
	// times — making sampled mode cost 3x the I/O of the full hash it exists to
	// avoid, for identical exactness. Any maxFullBytes below 8 MiB hit that range.
	offsets := dedupeOffsets(size)
	buf := make([]byte, sampleWindow)
	for _, off := range offsets {
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

// dedupeOffsets returns the distinct, ascending window starts for a file of the
// given size: head, middle, tail — collapsed when they coincide.
func dedupeOffsets(size int64) []int64 {
	raw := []int64{0, (size - sampleWindow) / 2, size - sampleWindow}
	out := make([]int64, 0, 3)
	for _, off := range raw {
		if off < 0 {
			off = 0
		}
		dup := false
		for _, seen := range out {
			if seen == off {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, off)
		}
	}
	return out
}
