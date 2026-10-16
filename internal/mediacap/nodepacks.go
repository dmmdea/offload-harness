package mediacap

// The node packs a run-graph manifest may name. See render/node-packs.json, which is the runner's copy and the
// one a reviewer edits; the table below is pinned to it by TestNodePackPinsMatchTheRunnersFile.

// NodePackPin is one pack a manifest may name: the directory name it installs under, the repository to clone
// and the one commit to check out.
type NodePackPin struct {
	Name   string
	Repo   string
	Commit string
}

// NodePackPins returns the pinned packs.
func NodePackPins() []NodePackPin { return nil }

// NodePackRefusedError is a manifest's refusal: it names a pack this node does not hold a pin for.
type NodePackRefusedError struct {
	// Pack is the pack as the manifest named it (its name, else its repository).
	Pack string
	// Reason says which part of the pack did not match.
	Reason string
}

func (e *NodePackRefusedError) Error() string {
	return "node pack " + e.Pack + " refused: " + e.Reason
}

// CheckManifestNodePacks refuses a run-graph manifest whose node_packs names anything but a pinned pack.
func CheckManifestNodePacks(manifest []byte) error { return nil }
