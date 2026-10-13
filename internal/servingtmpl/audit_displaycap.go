// audit_displaycap.go - the display-cap-unseen note (seam: behaviour-free until implemented).
package servingtmpl

import "github.com/dmmdea/offload-harness/internal/gpuprobe"

// HasProxyEntry reports whether any model entry of a llama-swap config is a `proxy:` seat (seam).
func HasProxyEntry(text string) bool { return false }

// AuditDisplayCapUnseen is the display-cap-unseen note (seam).
func AuditDisplayCapUnseen(text string, cards []gpuprobe.Card) []Violation { return nil }

// unseenCard is a display card of the reading (seam).
type unseenCard struct {
	Index    int
	UUID     string
	Name     string
	TotalGiB float64
}

// unseenPin is an entry pinned onto a display card (seam).
type unseenPin struct {
	Name string
	Card int
}

// unseenReport is what the audit found (seam).
type unseenReport struct {
	Cards    []unseenCard
	Pinned   []unseenPin
	Proxy    []string
	Unpinned []string
	Unmapped []string
	Unknown  bool
}

// unseenEntries classifies the entries of a file against a card table (seam).
func unseenEntries(text string, cards []gpuprobe.Card) (unseenReport, bool) {
	return unseenReport{}, true
}
