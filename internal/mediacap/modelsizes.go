package mediacap

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// KnownSize returns the byte size the installer recorded for a model file at
// download time, read from $OFFLOAD_HOME/installed.json (schema 2:
// components.<key>.{version,digest,size,source,installed_at}). ok is false when
// no manifest exists, it does not parse, or no component records a size for
// that file name — the overwhelming majority of ComfyUI-class weights on the
// fleet are sourced ad hoc from a peer box rather than this installer, and for
// those doctor keeps the exists-only verdict: it must never fabricate a size.
//
// resolveBinding uses it to catch a file that is only PARTWAY copied into place:
// a bare os.Stat reports FOUND the instant a same-named file of ANY size exists
// in the right class directory, which is exactly what happened on <node-b>
// during the 2026-09-23 media-route remediation — doctor printed
// `animate_character: OK CONFIGURED` while a 16.65 GB unet was still ~70%
// written (register F-38). INCOMPLETE means the file is SMALLER than the size
// recorded when it was downloaded; a larger file (a newer, hand-placed build of
// the same name) is never flagged.
//
// There is deliberately no literal size table in this package: a table here
// would be a second pin store that rots on every model update (ADR 0099 — floors
// never pins). This is also deliberately NOT a hash: comparing a size is one
// already-open os.Stat's Size() field, free; hashing a multi-GB file on every
// doctor run is the opposite of what doctor is for (F-38).
func KnownSize(name string) (int64, bool) {
	raw, err := os.ReadFile(installedManifestPath())
	if err != nil {
		return 0, false
	}
	// Tolerant by construction: unknown fields, a missing "components" (schema 1)
	// or a malformed row all read as "nothing recorded", never an error.
	var m struct {
		Components map[string]struct {
			Size   int64  `json:"size"`
			Source string `json:"source"`
			Dest   string `json:"dest"`
			File   string `json:"file"`
		} `json:"components"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return 0, false
	}
	for _, c := range m.Components {
		if c.Size <= 0 {
			continue
		}
		for _, ref := range []string{c.File, c.Dest, c.Source} {
			if ref != "" && path.Base(strings.ReplaceAll(ref, `\`, "/")) == name {
				return c.Size, true
			}
		}
	}
	return 0, false
}

// installedManifestPath is $OFFLOAD_HOME/installed.json, OFFLOAD_HOME defaulting
// to ~/offload-stack exactly like the installer (mirrors fleetnode.InstalledJSONPath,
// which this package cannot import: fleetnode already imports mediacap).
func installedManifestPath() string {
	home := os.Getenv("OFFLOAD_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(userHome, "offload-stack")
	}
	return filepath.Join(home, "installed.json")
}
