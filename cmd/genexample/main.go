// Command genexample regenerates config.example.json from config.Default()
// (LO-17: the committed example had drifted — escalation_model still said
// gemma4-26b-a4b while the code default was qwythos, and ~20 newer keys were
// missing entirely). It emits EVERY json-tagged Config field in struct order,
// so the example is the complete, current key inventory. Home-dir prefixes are
// rewritten to portable "~/" paths (config.Load expands them since LO-4).
//
// Run from the repo root: `go generate .` (wired in main.go) or
// `go run ./cmd/genexample`.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/dmmdea/local-offload/internal/config"
)

func main() {
	out, err := render()
	if err != nil {
		fmt.Fprintln(os.Stderr, "genexample:", err)
		os.Exit(1)
	}
	if err := os.WriteFile("config.example.json", out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "genexample:", err)
		os.Exit(1)
	}
	fmt.Println("wrote config.example.json")
}

// render serializes config.Default() field-by-field (struct order preserved,
// omitempty ignored so zero-valued keys still appear as documentation).
func render() ([]byte, error) {
	def := config.Default()
	home, _ := os.UserHomeDir()
	v := reflect.ValueOf(def)
	tp := v.Type()
	var b bytes.Buffer
	b.WriteString("{\n")
	first := true
	for i := 0; i < tp.NumField(); i++ {
		tag := strings.SplitN(tp.Field(i).Tag.Get("json"), ",", 2)[0]
		if tag == "" || tag == "-" {
			continue
		}
		val := v.Field(i).Interface()
		if s, ok := val.(string); ok && home != "" {
			val = tildify(s, home)
		}
		enc, err := json.Marshal(val)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", tag, err)
		}
		if !first {
			b.WriteString(",\n")
		}
		first = false
		fmt.Fprintf(&b, "  %q: %s", tag, enc)
	}
	b.WriteString("\n}\n")
	return b.Bytes(), nil
}

// tildify rewrites an absolute path under home to the portable "~/" form with
// forward slashes, so the committed example carries no machine-specific dirs.
func tildify(p, home string) string {
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) || strings.HasPrefix(p, home+"/") {
		return "~/" + filepath.ToSlash(p[len(home)+1:])
	}
	return p
}
