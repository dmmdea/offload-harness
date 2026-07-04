package contextbudget

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTrimNoCutBelowCap: input at/below the cap passes through untouched.
func TestTrimNoCutBelowCap(t *testing.T) {
	in := "señal única de prueba"
	out, trimmed := Trim(in, len(in))
	if trimmed || out != in {
		t.Fatalf("no trim expected: trimmed=%v out=%q", trimmed, out)
	}
	out, trimmed = Trim(in, 0)
	if trimmed || out != in {
		t.Fatalf("maxChars<=0 must be a no-op: trimmed=%v", trimmed)
	}
}

// TestTrimRuneSafeHeadBoundary (LO-13): a 2-byte Spanish rune (á/ñ) straddling
// the HEAD cut point must not be split — the cut backs off to the boundary
// and the result stays valid UTF-8 within the cap.
func TestTrimRuneSafeHeadBoundary(t *testing.T) {
	// keep = max - marker; head = keep*2/3. Fill the head zone with ñ so the
	// cut lands mid-rune for at least one of the probed caps.
	body := strings.Repeat("ñ", 3000) // 6000 bytes of 2-byte runes
	for max := 500; max < 520; max++ {
		out, trimmed := Trim(body, max)
		if !trimmed {
			t.Fatalf("cap %d must trim", max)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("cap %d: output is not valid UTF-8 (split rune at head cut)", max)
		}
		if len(out) > max {
			t.Fatalf("cap %d: output %d bytes exceeds cap", max, len(out))
		}
	}
}

// TestTrimRuneSafeTailBoundary (LO-13): the TAIL cut must start on a rune
// boundary — a suffix beginning with a continuation byte is mojibake.
func TestTrimRuneSafeTailBoundary(t *testing.T) {
	head := strings.Repeat("a", 4000)
	tail := strings.Repeat("á", 2000) // the tail zone is all 2-byte runes
	for max := 500; max < 520; max++ {
		out, trimmed := Trim(head+tail, max)
		if !trimmed {
			t.Fatalf("cap %d must trim", max)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("cap %d: output is not valid UTF-8 (split rune at tail cut)", max)
		}
	}
}

// TestTrimDegenerateCapRuneSafe: the degenerate hard head-cut (keep<200) must
// also back off a split rune.
func TestTrimDegenerateCapRuneSafe(t *testing.T) {
	in := strings.Repeat("ñ", 300)
	for max := 201; max < 206; max++ { // marker is ~54 bytes; keep<200 territory
		out, trimmed := Trim(in, max)
		if !trimmed {
			t.Fatalf("cap %d must trim", max)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("cap %d: degenerate cut split a rune", max)
		}
		if len(out) > max {
			t.Fatalf("cap %d: %d bytes exceeds cap", max, len(out))
		}
	}
}

// TestTrimKeepsHeadAndTailContent: the elision marker sits between genuine
// head and tail content (behavior unchanged by the rune-safety fix).
func TestTrimKeepsHeadAndTailContent(t *testing.T) {
	in := "INICIO " + strings.Repeat("x", 30000) + " FINAL"
	out, trimmed := Trim(in, 1000)
	if !trimmed {
		t.Fatal("must trim")
	}
	if !strings.HasPrefix(out, "INICIO ") {
		t.Fatalf("head lost: %q", out[:20])
	}
	if !strings.HasSuffix(out, " FINAL") {
		t.Fatalf("tail lost: %q", out[len(out)-20:])
	}
	if !strings.Contains(out, "[...content elided") {
		t.Fatal("marker missing")
	}
}

// TestIsTrivial: unchanged gate.
func TestIsTrivial(t *testing.T) {
	if !IsTrivial("  hola  ") {
		t.Fatal("short input must be trivial")
	}
	if IsTrivial("this is long enough") {
		t.Fatal("long input must not be trivial")
	}
}
