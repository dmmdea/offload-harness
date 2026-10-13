// audit_displaycap.go - the display-cap-unseen note (seam: behaviour-free until implemented).
package servingtmpl

import "github.com/dmmdea/offload-harness/internal/gpuprobe"

// HasProxyEntry reports whether any model entry of a llama-swap config is a `proxy:` seat (seam).
func HasProxyEntry(text string) bool { return false }

// AuditDisplayCapUnseen is the display-cap-unseen note (seam).
func AuditDisplayCapUnseen(text string, cards []gpuprobe.Card) []Violation { return nil }
