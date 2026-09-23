package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// bomCfg writes body behind a UTF-8 byte-order mark, the way Windows PowerShell 5.1's
// Set-Content/Out-File -Encoding UTF8 saves every file.
func bomCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, append([]byte{0xEF, 0xBB, 0xBF}, body...), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadAcceptsAUTF8BOM is the OptiPlex regression (2026-09-23): a config saved from
// PowerShell 5.1 starts with a BOM, encoding/json refused it, and a run-graph ran on
// built-in defaults. The file must load as written, from Load and from LoadWithSource.
func TestLoadAcceptsAUTF8BOM(t *testing.T) {
	p := bomCfg(t, `{"model":"bom-model","videogen_width":1234}`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("a BOM-prefixed config must load: %v", err)
	}
	if cfg.Model != "bom-model" || cfg.VideoGenWidth != 1234 {
		t.Fatalf("the file's settings must be in effect, got model=%q videogen_width=%d", cfg.Model, cfg.VideoGenWidth)
	}
	cfg, src := LoadWithSource(p)
	if !src.Loaded() || cfg.Model != "bom-model" {
		t.Fatalf("LoadWithSource must report the BOM file as loaded: src=%+v model=%q", src, cfg.Model)
	}
	if !bytes.Equal(StripBOM([]byte("\xEF\xBB\xBF{}")), []byte("{}")) || !bytes.Equal(StripBOM([]byte("{}")), []byte("{}")) {
		t.Fatal("StripBOM must drop exactly one leading BOM and leave other input alone")
	}
}

// TestUnparseableConfigIsDefaultsAndSaysSo: a file that does not decode is a ParseError,
// the value is the plain defaults (a TYPE error used to leave a half-read file, with no
// expansion or validation, that was neither), and both disclosures say NOTHING from the
// file is in effect — never the validation wording "the file's other settings ARE in
// effect", which is what an operator was told on the OptiPlex while it ran on defaults.
func TestUnparseableConfigIsDefaultsAndSaysSo(t *testing.T) {
	for name, body := range map[string]string{
		"syntax error": `{"model":"x",}`,
		"type error":   `{"model":"from-the-file","videogen_width":"wide"}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := writeShapeCfg(t, body)
			cfg, src := LoadWithSource(p)
			if !IsParseError(src.LoadErr) {
				t.Fatalf("LoadErr = %v, want a ParseError", src.LoadErr)
			}
			if !reflect.DeepEqual(cfg, Default()) {
				t.Fatalf("an unparseable file must yield exactly the built-in defaults; model=%q", cfg.Model)
			}
			var w bytes.Buffer
			if !WarnOnDefaults(src, &w) {
				t.Fatal("an unparseable config must warn")
			}
			msg := w.String()
			for _, want := range []string{"could NOT BE PARSED", "NOTHING from this file is in effect", "BUILT-IN DEFAULTS", p} {
				if !strings.Contains(msg, want) {
					t.Errorf("warning must say %q:\n%s", want, msg)
				}
			}
			if strings.Contains(msg, "ARE in effect") {
				t.Errorf("warning must not claim the file's settings are in effect:\n%s", msg)
			}
			line := SourceLine(src)
			if !strings.Contains(line, "BUILT-IN DEFAULTS") || !strings.Contains(line, p) || !strings.Contains(line, "could not be parsed") {
				t.Errorf("doctor's config line must say defaults, name the file and why: %q", line)
			}
			if LoadFailure(src.LoadErr) != "could not be parsed" || !strings.Contains(src.LoadErr.Error(), "NOTHING from the file is in effect") {
				t.Errorf("the error itself must carry the truth for surfaces that print it verbatim: %v", src.LoadErr)
			}
		})
	}
	// A validation refusal keeps its own (true) wording: the file's settings ARE in effect.
	p := writeShapeCfg(t, `{"endpoint":"http://127.0.0.1:9"}`)
	_, src := LoadWithSource(p)
	if src.LoadErr == nil || IsParseError(src.LoadErr) {
		t.Fatalf("precondition: a :9 endpoint is a validation failure, got %v", src.LoadErr)
	}
	var w bytes.Buffer
	WarnOnDefaults(src, &w)
	if !strings.Contains(w.String(), "ARE in effect") || LoadFailure(src.LoadErr) != "failed validation" {
		t.Fatalf("a validation failure keeps its wording:\n%s", w.String())
	}
}
