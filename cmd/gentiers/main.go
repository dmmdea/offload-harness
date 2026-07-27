// Command gentiers writes docs/tiers/ from setup/templates/profiles.json.
// The rendering lives in internal/tierdocs so the staleness test can call it
// directly; this binary only puts the bytes on disk.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dmmdea/offload-harness/internal/tierdocs"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	files, err := tierdocs.Render(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gentiers:", err)
		os.Exit(1)
	}
	for path, body := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "gentiers:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "gentiers:", err)
			os.Exit(1)
		}
	}
	fmt.Println("gentiers: wrote", len(files), "files under", filepath.Join(root, "docs", "tiers"))
}
