package leakgate

import (
	"reflect"
	"strings"
	"testing"
)

// This file is scanned by the keyless shape rule like every other file in the
// tree, so every positive input keeps "GPU-" apart from the hex digits and the
// UNC prefix apart from the distro name (7.2, rule 7). Negatives may be written
// whole because they never flag.

func shapeLabels(src string) []string {
	var out []string
	for _, f := range ShapeFindings("x.md", []byte(src)) {
		out = append(out, f.Label())
	}
	return out
}

func TestShapeGPUHeads(t *testing.T) {
	flag := []struct{ name, src string }{
		{"full id", gpu("9b1c42e7") + "-5d3a-4f08-91ce-77aa00bb11cc"},
		{"eight-digit pin, lower", gpu("9b1c42e7")},
		{"eight-digit pin, upper", gpu("9B1C42E7")},
		{"twelve-digit head", gpu("9b1c42e75d3a")},
		{"eight decimal digits", gpu("12345678")},
		{"four digits with three dots", gpu("9b1c") + "..."},
		{"four digits with the ellipsis character", gpu("9b1c") + string(rune(0x2026))},
		{"four digits with two groups", gpu("9b1c") + "-5d3a-4f08"},
		{"seven digits with an ellipsis", gpu("9b1c42e") + "..."},
		{"two groups then a delimiter", gpu("9b1c") + "-5d3a-4f08,"},
		{"first four uniform, second four not", gpu("1111aaab")},
		{"first four not uniform", gpu("1112aaaa")},
	}
	for _, c := range flag {
		if got := shapeLabels(c.src); !reflect.DeepEqual(got, []string{"shape:gpu-uuid"}) {
			t.Errorf("%s: %q -> %v, want one gpu-uuid finding", c.name, c.src, got)
		}
	}
	pass := []struct{ name, src string }{
		{"placeholder one", "GPU-1111aaaa-2222-3333-4444-555566667777"},
		{"placeholder two", "GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000"},
		{"placeholder zeros", "GPU-00000000-0000-0000-0000-000000000000"},
		{"placeholder letters", "GPU-aaaaaaaa"},
		{"short placeholder", "GPU-aaaa"},
		{"short placeholder digits", "GPU-1111"},
		{"model A100", "GPU-A100"},
		{"model 4090", "GPU-4090"},
		{"model 5070 with a prefix", "dual-GPU-5070"},
		{"model H100", "GPU-H100"},
		{"model A100-80GB", "GPU-A100-80GB"},
		{"model with a year", "GPU-4090-2024"},
		{"four digits alone", "GPU-1234"},
		{"six digits, one group", "GPU-123456-2024"},
		{"accelerated", "GPU-accelerated"},
		{"effective", "GPU-effective"},
		{"added", "GPU-added"},
		{"cafe", "GPU-cafe"},
		{"cafe with an ellipsis, no digit", "GPU-cafe..."},
		{"deadlock", "GPU-deadlock"},
		{"faced", "GPU-faced"},
		{"env", "GPU-env"},
		{"a word glued to a digit run", "GPU-accelerated2"},
		{"lower-case gpu is not the driver's form", "gpu-" + "9b1c42e7"},
		{"no head at all", "GPU- 9b1c42e7"},
		{"end of input", "GPU-"},
		{"seven digits without an ellipsis", gpu("9b1c42e")},
		{"hex run followed by a non-hex letter", gpu("9b1c42e7") + "g"},
		{"hex run followed by a non-hex letter, long", gpu("9b1c42e75d3a") + "x"},
	}
	for _, c := range pass {
		if got := shapeLabels(c.src); len(got) != 0 {
			t.Errorf("%s: %q -> %v, want no finding", c.name, c.src, got)
		}
	}
}

func TestShapeLinesAndColumns(t *testing.T) {
	src := "one\ntwo " + gpu("9b1c42e7") + "\nthree\nfour " + gpu("9b1c") + "... five\n" + gpu("12345678")
	fs := ShapeFindings("a.md", []byte(src))
	var lines []int
	for _, f := range fs {
		lines = append(lines, f.Line)
		if f.Path != "a.md" || f.Mode != "shape" || f.Col < 1 {
			t.Errorf("finding fields: %+v", f)
		}
	}
	if !reflect.DeepEqual(lines, []int{2, 4, 5}) {
		t.Fatalf("lines = %v, want [2 4 5]", lines)
	}
	if fs[0].Col != 5 {
		t.Errorf("column of the first finding = %d, want 5 (1-based byte offset of the G)", fs[0].Col)
	}
}

func TestShapeOverlappingPrefixesAreFound(t *testing.T) {
	// a failed head must not hide the head that follows it.
	src := "GPU-GPU-" + "9b1c42e7"
	if got := shapeLabels(src); !reflect.DeepEqual(got, []string{"shape:gpu-uuid"}) {
		t.Fatalf("%q -> %v", src, got)
	}
}

func TestShapeWSLDistros(t *testing.T) {
	flag := []string{
		unc("\\", "scratchbox", "\\x"),
		unc("/", "scratchbox", "/x"),
		unc("/", "Ubuntu-"+"Lab", "/x"),
		"\\\\WSL$\\" + "scratchbox" + "\\x",
		"//WSL.LocalHost/" + "scratchbox" + "/x",
		"\\\\wsl$\\" + "scratchbox",
		unc("/", "Ubuntu-24.4", "/x"),
		unc("/", "Debian-lab", "/x"),
		unc("/", "docker-desktop-extra", "/x"),
		unc("/", "alpine3", "/x"),
	}
	for _, src := range flag {
		if got := shapeLabels(src); !reflect.DeepEqual(got, []string{"shape:wsl-unc"}) {
			t.Errorf("%q -> %v, want one wsl-unc finding", src, got)
		}
	}
	pass := []string{
		"//wsl.localhost/<distro>/x",
		"//wsl.localhost/<name>/x",
		"//wsl.localhost/distro/x",
		"//wsl.localhost/DISTRO/x",
		"//wsl.localhost/Ubuntu/x",
		"//wsl.localhost/ubuntu/x",
		"//wsl.localhost/Ubuntu-22.04/x",
		"//wsl.localhost/Debian/x",
		"//wsl.localhost/kali-linux/x",
		"//wsl.localhost/Alpine/x",
		"//wsl.localhost/docker-desktop/x",
		"//wsl.localhost/docker-desktop-data/x",
		"\\\\wsl$\\Ubuntu-24.04\\home",
		"wsl.localhost",
		"wsl.localhost/",
		"see wsl.localhost/\"quoted\"",
		"a UNC prefix such as wsl.localhost/ ends a sentence",
	}
	for _, src := range pass {
		if got := shapeLabels(src); len(got) != 0 {
			t.Errorf("%q -> %v, want no finding", src, got)
		}
	}
}

func TestShapeWSLCaptureStopsAtDelimiters(t *testing.T) {
	for _, tail := range []string{"/x", "\\x", " x", "\"x", "'x", "`x", ")x", "\tx", "\nx"} {
		src := "//wsl.localhost/" + "scratchbox" + tail
		if got := shapeLabels(src); len(got) != 1 {
			t.Errorf("tail %q: %v, want one finding", tail, got)
		}
	}
	// the capture is the first component only: a public distro followed by a
	// real-looking directory is still public.
	if got := shapeLabels("//wsl.localhost/Ubuntu/scratchbox"); len(got) != 0 {
		t.Errorf("public distro followed by a directory flagged: %v", got)
	}
}

func TestShapeWSLLineNumbers(t *testing.T) {
	src := "a\nb\n" + unc("/", "scratchbox", "/x") + "\n"
	fs := ShapeFindings("x", []byte(src))
	if len(fs) != 1 || fs[0].Line != 3 {
		t.Fatalf("findings = %+v, want one on line 3", fs)
	}
}

func TestShapeLabelNeverCarriesTheMatchedText(t *testing.T) {
	for _, src := range []string{gpu("9b1c42e7"), unc("/", "scratchbox", "/x")} {
		for _, f := range ShapeFindings("x", []byte(src)) {
			s := f.String()
			if strings.Contains(s, "9b1c42e7") || strings.Contains(s, "scratchbox") {
				t.Errorf("shape finding output carries matched text: %q", s)
			}
		}
	}
}
