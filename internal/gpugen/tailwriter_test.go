package gpugen

// Fix 1 (SP3 follow-up review): child output capture was unbounded — both
// cmd.CombinedOutput() (the legacy/no-footprint path) and runSampled's
// bytes.Buffer accumulate the child's ENTIRE stdout+stderr in RAM, even
// though the error tail only ever shows the last 400 bytes. A runaway CLI
// spewing GBs over a 40-minute timeout_sec window would balloon the node's
// memory. tailWriter fixes this: a fixed-capacity io.Writer that keeps only
// the LAST N bytes written (tail semantics), tracking the total ever written
// so a "truncated" marker can be surfaced when more arrived than it could
// hold.
//
// These tests were written BEFORE internal/gpugen/tailwriter.go existed
// (write-test-first): with only this file present, `go test ./internal/gpugen/...`
// fails to COMPILE (tailWriter/newTailWriter/tailWriterCap/runCombined all
// undefined) — that compile failure is the RED state for this fix. Adding
// tailwriter.go and wiring the two call sites in gpugen.go turns it GREEN.

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// TestTailWriterCapConstant locks the documented 256 KiB cap.
func TestTailWriterCapConstant(t *testing.T) {
	const want = 256 * 1024
	if tailWriterCap != want {
		t.Fatalf("tailWriterCap = %d, want %d (256 KiB)", tailWriterCap, want)
	}
}

// TestTailWriterUnderCapByteIdentical (case a): a single write under the cap
// is retained byte-for-byte, with no truncation marker.
func TestTailWriterUnderCapByteIdentical(t *testing.T) {
	w := newTailWriter(64)
	input := []byte("hello world, this is well under the cap")
	if len(input) >= 64 {
		t.Fatalf("test fixture must be under cap, got %d bytes", len(input))
	}
	n, err := w.Write(input)
	if err != nil || n != len(input) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(input))
	}
	if !bytes.Equal(w.Contents(), input) {
		t.Fatalf("Contents() = %q, want %q (byte-identical under cap)", w.Contents(), input)
	}
	if w.Truncated() {
		t.Fatal("Truncated() must be false when total written is under the cap")
	}
	if w.Total() != int64(len(input)) {
		t.Fatalf("Total() = %d, want %d", w.Total(), len(input))
	}
	if got := w.String(); got != string(input) {
		t.Fatalf("String() = %q, want the plain contents with no marker", got)
	}
}

// TestTailWriterMultipleWritesUnderCapConcatenate: several small writes below
// the cap simply concatenate, matching io.Writer's normal contract (and
// bytes.Buffer's observable behavior, which this replaces).
func TestTailWriterMultipleWritesUnderCapConcatenate(t *testing.T) {
	w := newTailWriter(4096)
	var want bytes.Buffer
	for _, p := range []string{"alpha-", "beta-", "gamma"} {
		want.WriteString(p)
		if _, err := w.Write([]byte(p)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if !bytes.Equal(w.Contents(), want.Bytes()) {
		t.Fatalf("Contents() = %q, want %q", w.Contents(), want.Bytes())
	}
	if w.Truncated() {
		t.Fatal("Truncated() must be false — total is under the cap")
	}
}

// TestTailWriterOverCapKeepsLastCapBytes (case b): 1 MiB written in odd-sized
// (777-byte) chunks against a small injected cap. Asserts:
//   - len(Contents()) == cap
//   - Contents() == the last `cap` bytes of the full input (tail semantics,
//     not "first cap bytes" or an arbitrary window)
//   - a "truncated: total X bytes" marker appears in the formatted String()
//   - memory-boundedness STRUCTURALLY: cap(buf) (the Go slice capacity, i.e.
//     the writer's actual backing array) never exceeds the configured
//     capacity, even after 1 MiB was pushed through it in small pieces — this
//     is the direct rebuttal of the finding ("the underlying buffer is not
//     bounded").
func TestTailWriterOverCapKeepsLastCapBytes(t *testing.T) {
	const capacity = 4096
	const chunkSize = 777    // odd size, does not divide the cap or the total evenly
	const totalTarget = 1 << 20 // 1 MiB

	w := newTailWriter(capacity)
	var full []byte
	n := 0
	for len(full) < totalTarget {
		b := byte('A' + (n % 26))
		chunk := bytes.Repeat([]byte{b}, chunkSize)
		full = append(full, chunk...)
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("Write chunk %d: %v", n, err)
		}
		n++
	}

	got := w.Contents()
	if len(got) != capacity {
		t.Fatalf("len(Contents()) = %d, want %d (the cap)", len(got), capacity)
	}
	want := full[len(full)-capacity:]
	if !bytes.Equal(got, want) {
		t.Fatal("Contents() does not equal the last `capacity` bytes of the input (tail semantics)")
	}
	if !w.Truncated() {
		t.Fatal("Truncated() must be true once total written exceeds the cap")
	}
	if w.Total() != int64(len(full)) {
		t.Fatalf("Total() = %d, want %d (total bytes ever written, uncapped)", w.Total(), len(full))
	}

	formatted := w.String()
	if !strings.Contains(formatted, "truncated") {
		t.Fatalf("String() = %q, want it to contain a truncation marker", formatted)
	}
	if !strings.Contains(formatted, fmt.Sprintf("%d", len(full))) {
		t.Fatalf("String() = %q, want the marker to name the total bytes written (%d)", formatted, len(full))
	}

	// The structural memory-boundedness assertion: the writer's own backing
	// array capacity must never have grown past `capacity`, regardless of how
	// much (or in how many pieces) was written through it.
	if c := cap(w.buf); c > capacity {
		t.Fatalf("cap(buf) = %d, want <= %d (capacity) — the writer's backing array grew unbounded, exactly the finding this fix addresses", c, capacity)
	}
}

// TestTailWriterWriteLargerThanCapInOneShot: a SINGLE write bigger than the
// whole cap must still leave only its own tail resident (not, say, silently
// dropped or doubled).
func TestTailWriterWriteLargerThanCapInOneShot(t *testing.T) {
	const capacity = 16
	w := newTailWriter(capacity)
	input := []byte("this-single-write-is-longer-than-sixteen-bytes")
	if _, err := w.Write(input); err != nil {
		t.Fatalf("Write: %v", err)
	}
	want := input[len(input)-capacity:]
	if !bytes.Equal(w.Contents(), want) {
		t.Fatalf("Contents() = %q, want %q (tail of the single oversized write)", w.Contents(), want)
	}
	if cap(w.buf) > capacity {
		t.Fatalf("cap(buf) = %d, want <= %d", cap(w.buf), capacity)
	}
}

// TestTailWriterWriteExactlyAtCapBoundary: a SINGLE write whose length is
// EXACTLY the cap (neither over nor under) must hit the `n >= w.cap` branch
// in Write (the same branch TestTailWriterWriteLargerThanCapInOneShot
// exercises for n > cap) and retain the WHOLE write byte-for-byte — the
// boundary case that branch's `>=` (not `>`) is written to cover.
func TestTailWriterWriteExactlyAtCapBoundary(t *testing.T) {
	const capacity = 16
	w := newTailWriter(capacity)
	input := []byte("exactly-16-bytes") // len == 16 == capacity, not one more or less
	if len(input) != capacity {
		t.Fatalf("test fixture must be exactly %d bytes, got %d", capacity, len(input))
	}
	n, err := w.Write(input)
	if err != nil || n != len(input) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(input))
	}
	if !bytes.Equal(w.Contents(), input) {
		t.Fatalf("Contents() = %q, want %q (the whole exactly-at-cap write retained)", w.Contents(), input)
	}
	if w.Truncated() {
		t.Fatal("Truncated() must be false — total written equals the cap, not more than it")
	}
	if cap(w.buf) > capacity {
		t.Fatalf("cap(buf) = %d, want <= %d", cap(w.buf), capacity)
	}
}

// --- call-site integration test: exercises the ACTUAL production wiring
// (runCombined for the legacy/no-footprint path, runSampled for the
// footprint/sampled path) against a REAL child process that writes more than
// the production 256 KiB cap to stdout, in many odd-sized chunks — proving
// both replaced call sites (gpugen.go:189 CombinedOutput-style,
// gpugen.go:211-214 runSampled's bytes.Buffer) actually route the child's
// output through a bounded tailWriter, not just that the type works in
// isolation. This is also the test used for the mutation-verify step (see
// the report): temporarily making ONE call site ignore the given writer (in
// favor of its own local bytes.Buffer, i.e. literally un-doing the fix at
// that site) makes exactly that subtest fail while the other stays green.

const (
	chunkedChunkSize   = 777 // odd size
	chunkedTotalChunks = 400 // 400*777 = 310800 bytes > 262144 (the 256 KiB cap)
)

// chunkedStdoutScript returns a node invocation that writes
// chunkedTotalChunks chunks of chunkedChunkSize bytes to stdout, chunk i
// filled with the byte 'A'+(i%26) — fully deterministic so the Go side can
// replicate the exact expected byte sequence (expectedChunkedOutput).
//
// It must NOT call process.exit(). When stdout is a PIPE (which it always is
// here — the parent captures it), Node writes asynchronously on POSIX, and
// process.exit() discards whatever is still buffered. The child then delivers a
// whole number of chunks and stops: this test failed on Linux CI for a week with
// 62160 and 81585 bytes — exactly 80 and 105 of the 777-byte chunks — while
// passing on Windows, where pipe writes are synchronous and everything got out.
// Letting the event loop drain naturally exits 0 with the full stream flushed.
func chunkedStdoutScript() (exe, script string, args []string) {
	js := fmt.Sprintf(`
for (let i = 0; i < %d; i++) {
  const b = 65 + (i %% 26);
  process.stdout.write(Buffer.alloc(%d, b));
}
`, chunkedTotalChunks, chunkedChunkSize)
	return "node", "-e", []string{js}
}

// expectedChunkedOutput replicates chunkedStdoutScript's output byte-for-byte
// in Go, built from the SAME two constants so the fixture can never drift.
func expectedChunkedOutput() []byte {
	full := make([]byte, 0, chunkedTotalChunks*chunkedChunkSize)
	for i := 0; i < chunkedTotalChunks; i++ {
		b := byte('A' + (i % 26))
		full = append(full, bytes.Repeat([]byte{b}, chunkedChunkSize)...)
	}
	return full
}

func TestCallSitesBoundRealChildOutputOverCap(t *testing.T) {
	requireNode(t)
	full := expectedChunkedOutput()
	if len(full) <= tailWriterCap {
		t.Fatalf("fixture must exceed the production cap: got %d bytes, cap is %d", len(full), tailWriterCap)
	}
	wantTail := full[len(full)-tailWriterCap:]

	cases := []struct {
		name string
		run  func(cmd *exec.Cmd, w *tailWriter) error
	}{
		{"runCombined (legacy/CombinedOutput-style call site)", func(cmd *exec.Cmd, w *tailWriter) error {
			return runCombined(cmd, w)
		}},
		{"runSampled (footprint call site)", func(cmd *exec.Cmd, w *tailWriter) error {
			_, err := runSampled(cmd, w, nil)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exe, script, args := chunkedStdoutScript()
			cmd := exec.Command(exe, append([]string{script}, args...)...)
			tw := newTailWriter(tailWriterCap)
			if err := tc.run(cmd, tw); err != nil {
				t.Fatalf("child failed unexpectedly: %v", err)
			}
			got := tw.Contents()
			if len(got) != tailWriterCap {
				t.Fatalf("len(Contents()) = %d, want %d (the cap) — this call site did not bound the child's real output", len(got), tailWriterCap)
			}
			if !bytes.Equal(got, wantTail) {
				t.Fatal("Contents() does not equal the last `cap` bytes of the child's actual stdout")
			}
			if !tw.Truncated() {
				t.Fatal("Truncated() must be true — the child wrote more than the cap")
			}
			if int(tw.Total()) != len(full) {
				t.Fatalf("Total() = %d, want %d (the child's actual total bytes written)", tw.Total(), len(full))
			}
			if c := cap(tw.buf); c > tailWriterCap {
				t.Fatalf("cap(buf) = %d, want <= %d — this call site let the backing array grow unbounded", c, tailWriterCap)
			}
		})
	}
}
