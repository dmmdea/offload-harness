package main

// `local-offload media-recipes [--json]`: what this machine's image (and video, animation, audio) families ARE, as the fleet
// node publishes them in /fleet/health `image_recipes` and `media_recipes`. Two machines render the same image recipe when
// the digests of the family match (and they run the same harness version), which is what `generate-image --batch --spill` and
// the ticket queue compare. Before this verb the only way to read a digest was a curl of /fleet/health with the bearer on each
// machine. The rows come from fleetnode.ImageRecipesWith and fleetnode.MediaRecipes, the functions the node's health uses, so
// the CLI cannot disagree with the node; the one difference is that a CLI waits for each weight file's content fingerprint
// (a cold read of a large file takes a while) where health never waits. Read-only: it loads no model and starts nothing.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// mediaRecipesDoc is the JSON `media-recipes --json` prints: the same member names and row shapes /fleet/health uses.
type mediaRecipesDoc struct {
	HarnessVersion string                        `json:"harness_version"`
	ImageRecipes   []fleetnode.ImageRecipeHealth `json:"image_recipes"`
	MediaRecipes   []fleetnode.MediaRecipeHealth `json:"media_recipes"`
}

func runMediaRecipes(args []string) error {
	fs := flag.NewFlagSet("media-recipes", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print the rows as JSON (the image_recipes and media_recipes members of /fleet/health)")
	_ = fs.Parse(args)
	if err := leftoverArgErr(fs, "media-recipes"); err != nil {
		return err
	}
	cfg := loadCfg(fs)
	doc := mediaRecipesDoc{
		HarnessVersion: version,
		// A blocking read: a one-shot command waits for the fingerprint of each weight file (bounded per file by the
		// fingerprinter, after which a file prints as pending and the digest is empty: run it again).
		ImageRecipes: fleetnode.ImageRecipesWith(cfg, mediacap.SharedFingerprints().Await),
		MediaRecipes: fleetnode.MediaRecipes(cfg),
	}
	if doc.ImageRecipes == nil {
		doc.ImageRecipes = []fleetnode.ImageRecipeHealth{}
	}
	if doc.MediaRecipes == nil {
		doc.MediaRecipes = []fleetnode.MediaRecipeHealth{}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}
	writeMediaRecipes(os.Stdout, doc)
	return nil
}

func writeMediaRecipes(w io.Writer, doc mediaRecipesDoc) {
	fmt.Fprintf(w, "harness %s\n", doc.HarnessVersion)
	if len(doc.ImageRecipes) == 0 {
		fmt.Fprintln(w, "\nimage families: none (no ComfyUI image binding names a checkpoint on this machine)")
	}
	for _, r := range doc.ImageRecipes {
		name := r.Name
		if name == "" {
			name = "(default binding)"
		}
		tag := ""
		if r.Default {
			tag = " (default)"
		}
		fmt.Fprintf(w, "\nimage family %s%s\n", name, tag)
		if r.Digest != "" {
			fmt.Fprintf(w, "  digest  %s\n", r.Digest)
		} else {
			fmt.Fprintln(w, "  digest  (none: a file below has no content fingerprint yet, so this is not an identity)")
		}
		fmt.Fprintf(w, "  engine  %s  graph %s\n", r.Engine, r.Graph)
		for _, f := range r.Files {
			fmt.Fprintf(w, "  file    %-10s %s  bytes=%d  fingerprint=%s\n", f.Role, f.Name, f.Bytes, fpWord(f.Fingerprint))
		}
		if gaps := r.Unfingerprinted(); len(gaps) > 0 {
			fmt.Fprintf(w, "  unfingerprinted  %v\n", gaps)
		}
		if r.Steps > 0 || r.Sampler != "" {
			fmt.Fprintf(w, "  sampling steps=%d cfg=%g sampler=%s scheduler=%s\n", r.Steps, r.CFG, r.Sampler, r.Scheduler)
		}
	}
	for _, r := range doc.MediaRecipes {
		tag := ""
		if r.Default {
			tag = " (default)"
		}
		fmt.Fprintf(w, "\n%s family %s%s  engine %s  (sizes only: no digest, never an identity)\n", r.Lane, r.Name, tag, r.Engine)
		for _, f := range r.Files {
			fmt.Fprintf(w, "  file    %-10s %s  bytes=%d\n", f.Role, f.Name, f.Bytes)
		}
	}
}

// fpWord shortens a sha256 fingerprint for a line of text; --json carries it whole.
func fpWord(fp string) string {
	if fp == "" {
		return "(none)"
	}
	if len(fp) > 16 {
		return fp[:16] + "..."
	}
	return fp
}
