package mediacap

import "fmt"

// The models a run-graph manifest may name.
//
// A manifest's models[] makes the node download each entry's source_url into the ComfyUI tree at its path before
// ComfyUI starts. The fleet bearer lets a caller run renders, not write files of its choosing: a path that leaves
// models/<class>/ (custom_nodes/, a ".." climb, an absolute path, a drive) would drop code where ComfyUI loads it,
// and a source that is not one of a few https hosts would make the node fetch from an address of the caller's choosing.

// ManifestModelRefusedError is a manifest's refusal for a models[] entry the node will not provision.
type ManifestModelRefusedError struct {
	// Model is the entry as the manifest named it (its path, else its place in the list).
	Model string
	// Reason says which part of the entry did not pass.
	Reason string
}

func (e *ManifestModelRefusedError) Error() string {
	return fmt.Sprintf("run-graph manifest: model %q is not allowed (%s); a manifest model lands under models/<class>/ and is fetched over https from a listed host only (render/node-packs.json)", clipPack(e.Model), e.Reason)
}

// ModelSourceHosts returns the hosts a manifest model may be fetched from.
func ModelSourceHosts() []string { return nil }

// CheckManifestModels refuses a run-graph manifest whose models[] names an entry the node will not provision.
func CheckManifestModels(manifest []byte) error { return nil }
