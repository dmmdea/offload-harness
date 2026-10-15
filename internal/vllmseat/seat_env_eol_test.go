package vllmseat

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// The seat env is the one rendered file BASH reads: seat_fg.sh does `. "$CFG"` inside the
// distro. A CR before each newline is not whitespace there, it is part of the value, so
// SEAT_PP= becomes a one-character string, `[ -n "${SEAT_PP:-}" ]` is true, and a
// tensor-parallel seat starts as a pipeline seat with `--pipeline-parallel-size "\r"`. The
// template is CRLF on a Windows checkout with autocrlf (it had no .gitattributes rule), and
// the renderer copied its bytes as they were. The tests that read the env trim the CR, so
// none of them could see it; these read the bytes.

// crlfTemplates copies a template directory with every line ending forced to CRLF, which is
// what a Windows checkout without an eol rule produces, so the test fails on every machine
// and not only on a checkout that happens to be CRLF.
func crlfTemplates(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		lf := strings.ReplaceAll(string(raw), "\r\n", "\n")
		if err := os.WriteFile(filepath.Join(dst, e.Name()), []byte(strings.ReplaceAll(lf, "\n", "\r\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestWSLSeatEnvIsLFWhateverLineEndingsTheTemplateCheckoutHolds(t *testing.T) {
	for name, dir := range map[string]string{
		"a template checked out CRLF":      crlfTemplates(t, wslTemplatesDir()),
		"the template as this tree has it": wslTemplatesDir(),
	} {
		t.Run(name, func(t *testing.T) {
			s := pinned()
			files, err := s.Artifacts(dir, wslRT())
			if err != nil {
				t.Fatal(err)
			}
			env, ok := files[s.ID+".env"]
			if !ok {
				t.Fatalf("no %s.env rendered; got %v", s.ID, keys(files))
			}
			if n := strings.Count(env, "\r"); n != 0 {
				t.Errorf("the seat env carries %d CR byte(s); bash inside the distro reads each one as part of the value "+
					"(SEAT_PP=<CR> is a non-empty string and turns a tensor-parallel seat into a pipeline seat)", n)
			}
			// The line the CR would have corrupted, byte for byte: an empty SEAT_PP is the
			// launcher's statement "tensor-parallel seat".
			if !strings.Contains(env, "\nSEAT_PP=\n") {
				t.Errorf("SEAT_PP must render as an empty assignment ended by a bare LF:\n%s", env)
			}
		})
	}
}

// The attribute is the other half: the renderer's normalisation covers a rendered copy, and
// the pin keeps the template itself LF in every checkout, so an operator who copies the
// TEMPLATE by hand (the guide still lists it) gets the same bytes.
func TestGitattributesPinsTheSeatEnvTemplateToLF(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	const target = "setup/templates/vllm-seat/windows-wsl/seat.env"
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 2 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if ok, _ := path.Match(f[0], target); !ok {
			continue
		}
		for _, attr := range f[1:] {
			if attr == "eol=lf" {
				return
			}
		}
	}
	t.Errorf(".gitattributes has no rule giving %s eol=lf: bash inside the distro reads a rendered copy of it, "+
		"and a Windows checkout with autocrlf writes CRLF", target)
}
