package config

import (
	"fmt"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// TailnetZones is every tailnet DNS zone this config names: tailnet_suffix first (the
// operator's own tailnet), then tailnet_suffixes in the order written (a tailnet that
// shared a node in). Each is normalized by netguard.ParseZone, blank slots are skipped
// and duplicates collapse, so the same zone spelled twice is one zone.
//
// A malformed entry is an error that names its key and index, never a silent drop: a
// zone that does not take effect fails closed in the gate while reading, to the person
// who typed it, as if it had. This is the list Load installs into netguard before any
// endpoint is vetted, and the list EndpointWarnings judges delegate_remotes against, so
// the load-time check and the doctor row read one answer.
func (c Config) TailnetZones() ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(z string) {
		if z != "" && !seen[z] {
			seen[z] = true
			out = append(out, z)
		}
	}
	z, err := netguard.ParseZone("tailnet_suffix", c.TailnetSuffix)
	if err != nil {
		return nil, fmt.Errorf("tailnet_suffix: %w", err)
	}
	add(z)
	for i, s := range c.TailnetSuffixes {
		z, err := netguard.ParseZone(fmt.Sprintf("tailnet_suffixes[%d]", i), s)
		if err != nil {
			return nil, err
		}
		add(z)
	}
	return out, nil
}
