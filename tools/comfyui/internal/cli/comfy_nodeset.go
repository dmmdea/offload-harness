// Node-set identity — the reproducibility half that server identity misses.
//
// NOT generated — markerless on purpose, so `printing-press generate --force`
// preserves it. Do not add the generated-file marker.
//
// THE HOLE THIS FILLS. `provenance` already reports which SERVER made a file:
// ComfyUI version, python/torch versions, argv, devices. Two runs can agree on
// every one of those and still not be the same environment, because a custom
// node pack was installed, upgraded, or removed in between — and a custom pack
// is exactly the thing that changes what a class_type MEANS. Without the node
// set, "same server, different result" has no explanation to point at.
//
// WHAT IS CAPTURED. The set of node classes the server offered, digested to a
// stable hash, plus the custom-node packs those classes came from (via
// /object_info's python_module — see comfy_deps.go). Capture and report only:
// nothing here installs, removes, or pins anything. Restoring a node set is a
// lifecycle-manager job that comfy-cli already owns, and doing it from here
// would mutate the node install behind an operator's back.
//
// WHERE IT COMES FROM. The already-resolved object_info — the local cache by
// default. No extra network call is added to the submit path, and the source
// and cache age are recorded alongside the digest so a reader can weigh it
// rather than mistake a cached fingerprint for a live one.

package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"comfyui-pp-cli/internal/comfy/slots"
)

// comfyNodeSetPack is one pack inside a captured node set.
type comfyNodeSetPack struct {
	Pack       string `json:"pack"`
	Kind       string `json:"kind"`
	ClassCount int    `json:"class_count"`
}

// comfyNodeSetIdentity is the captured fingerprint of a server's node set.
type comfyNodeSetIdentity struct {
	ID             string             `json:"id"`
	ComfyUIVersion string             `json:"comfyui_version,omitempty"`
	ClassCount     int                `json:"class_count"`
	PackCount      int                `json:"pack_count"`
	Packs          []comfyNodeSetPack `json:"packs"`
	// ClassDigest is a hash of the sorted class-name list alone. It changes
	// when ANY class appears or disappears, including a core class added by a
	// ComfyUI upgrade — so it detects drift that the pack list cannot.
	ClassDigest string `json:"class_digest"`
	// Source records where the schema was read from ("cache", "live server",
	// "file:<path>"), because a cached fingerprint describes the last sync,
	// not necessarily the run. Never omitted: an unlabelled digest invites
	// exactly the wrong conclusion.
	Source string `json:"source"`
}

// comfyBuildNodeSetIdentity digests a resolved schema into a node-set identity.
//
// Pure: no clock, no filesystem, no network, so the digest is fully testable
// against a fixture schema. Returns an identity with an empty ID when the
// schema is empty — an empty node set is "we could not look", not "the server
// offers nothing", and giving it a hash would let every unfingerprintable run
// collide on one shared id.
func comfyBuildNodeSetIdentity(schema slots.Schema, comfyUIVersion, source string) comfyNodeSetIdentity {
	identity := comfyNodeSetIdentity{
		ComfyUIVersion: comfyUIVersion,
		ClassCount:     len(schema),
		Packs:          []comfyNodeSetPack{},
		Source:         source,
	}
	if len(schema) == 0 {
		return identity
	}

	classes := make([]string, 0, len(schema))
	packClasses := map[string]int{}
	packKind := map[string]string{}
	for classType, spec := range schema {
		classes = append(classes, classType)
		pack, kind := comfyClassifyModule(spec.PythonModule)
		packClasses[pack]++
		packKind[pack] = kind
	}
	sort.Strings(classes)

	classDigest := sha256.Sum256([]byte(strings.Join(classes, "\n")))
	identity.ClassDigest = hex.EncodeToString(classDigest[:])

	packNames := make([]string, 0, len(packClasses))
	for name := range packClasses {
		packNames = append(packNames, name)
	}
	sort.Strings(packNames)
	for _, name := range packNames {
		identity.Packs = append(identity.Packs, comfyNodeSetPack{
			Pack:       name,
			Kind:       packKind[name],
			ClassCount: packClasses[name],
		})
	}
	identity.PackCount = len(identity.Packs)

	// The id covers the ComfyUI version too: the same class list served by a
	// different ComfyUI build is a different environment, because a core
	// class can change behaviour without changing its name.
	keyParts := make([]string, 0, len(identity.Packs)+2)
	keyParts = append(keyParts, "comfyui="+comfyUIVersion, "classes="+identity.ClassDigest)
	for _, p := range identity.Packs {
		keyParts = append(keyParts, fmt.Sprintf("pack=%s:%s:%d", p.Kind, p.Pack, p.ClassCount))
	}
	sum := sha256.Sum256([]byte(strings.Join(keyParts, "\n")))
	identity.ID = hex.EncodeToString(sum[:])[:16]
	return identity
}

// comfyRecordNodeSet upserts the identity and links it to a run.
//
// FAIL-OPEN BY DESIGN. Provenance capture must never be able to fail a render
// that already succeeded: every error is returned for the caller to log, and
// the caller records the run regardless. A missing fingerprint degrades
// provenance to what it reported before this existed.
func comfyRecordNodeSet(ctx context.Context, db *sql.DB, promptID string, identity comfyNodeSetIdentity) error {
	if db == nil || identity.ID == "" || strings.TrimSpace(promptID) == "" {
		return nil
	}
	packsJSON, err := json.Marshal(identity.Packs)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO node_set (id, comfyui_version, class_count, pack_count, packs_json, class_digest, source)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET last_seen = CURRENT_TIMESTAMP`,
		identity.ID, identity.ComfyUIVersion, identity.ClassCount, identity.PackCount,
		string(packsJSON), identity.ClassDigest, identity.Source,
	); err != nil {
		return fmt.Errorf("recording node set: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO run_node_set (prompt_id, node_set_id) VALUES (?, ?)
		 ON CONFLICT(prompt_id) DO UPDATE SET node_set_id = excluded.node_set_id`,
		promptID, identity.ID,
	); err != nil {
		return fmt.Errorf("linking run to node set: %w", err)
	}
	return nil
}

// comfyLoadNodeSetForRun reads back the node set captured for one run.
//
// Returns ok=false when the run predates node-set capture, which is the normal
// case for every run recorded before this shipped. Callers report that as "not
// captured" rather than as an error — an old run genuinely has no fingerprint
// and inventing one would be a lie about reproducibility.
func comfyLoadNodeSetForRun(ctx context.Context, db *sql.DB, promptID string) (comfyNodeSetIdentity, bool) {
	if db == nil || strings.TrimSpace(promptID) == "" {
		return comfyNodeSetIdentity{}, false
	}
	var (
		identity  comfyNodeSetIdentity
		packsJSON sql.NullString
		version   sql.NullString
		digest    sql.NullString
		source    sql.NullString
	)
	err := db.QueryRowContext(ctx,
		`SELECT ns.id, COALESCE(ns.comfyui_version,''), COALESCE(ns.class_count,0), COALESCE(ns.pack_count,0),
		        ns.packs_json, ns.class_digest, ns.source
		 FROM run_node_set rns JOIN node_set ns ON ns.id = rns.node_set_id
		 WHERE rns.prompt_id = ?`,
		promptID,
	).Scan(&identity.ID, &version, &identity.ClassCount, &identity.PackCount, &packsJSON, &digest, &source)
	if err != nil {
		return comfyNodeSetIdentity{}, false
	}
	identity.ComfyUIVersion = version.String
	identity.ClassDigest = digest.String
	identity.Source = source.String
	identity.Packs = []comfyNodeSetPack{}
	if packsJSON.Valid && packsJSON.String != "" {
		_ = json.Unmarshal([]byte(packsJSON.String), &identity.Packs)
	}
	return identity, true
}

// comfyCaptureNodeSetForRun resolves the node schema the cheap way — through
// the same cache `validate` reads, adding no network call to the submit path —
// digests it, and records it against the run.
//
// Every failure is swallowed into a returned error the caller logs as a
// warning. Capture is a bonus on top of a successful submit and must never
// turn one into a failure.
func comfyCaptureNodeSetForRun(ctx context.Context, flags *rootFlags, db *sql.DB, promptID, comfyUIVersion string) error {
	resolved, err := slotsResolveObjectInfo(ctx, flags, "")
	if err != nil {
		return err
	}
	if len(resolved.schema) == 0 {
		return nil
	}
	source := resolved.source
	if resolved.fromCache {
		source = fmt.Sprintf("%s (%s)", resolved.source, slotsCacheAge(resolved.syncedAt))
	}
	identity := comfyBuildNodeSetIdentity(resolved.schema, comfyUIVersion, source)
	return comfyRecordNodeSet(ctx, db, promptID, identity)
}
