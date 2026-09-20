package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/servingtmpl"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// TestVulkanAndCPUTiersRenderOnLinux: until 2026-09-20 the vulkan and cpu backends had
// ONLY Windows templates, so a Linux AMD box carrying a MEASURED tier (amd-gcn, measured
// 2026-07-17) could not be installed at all — install.sh died at "no serving template
// for linux/vulkan" on binxarn (Ryzen 5 5625U). A tier is a hardware class; the OS it
// boots must not decide whether it exists. Every tier on these two backends must render
// on BOTH operating systems, seats included, or this goes red before an installer does.
func TestVulkanAndCPUTiersRenderOnLinux(t *testing.T) {
	seedProfiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	portable := map[string]bool{"vulkan": true, "cpu": true}
	checked := 0
	for id, sp := range doc.Profiles {
		if !portable[sp.Backend] {
			continue
		}
		for _, goos := range []string{"linux", "windows"} {
			tmpl, err := templateFor(goos, sp.Backend)
			if err != nil {
				t.Errorf("tier %s: %v", id, err)
				continue
			}
			rendered, err := servingtmpl.Render(tmpl, renderParams(sp, goos))
			if err != nil {
				t.Errorf("tier %s (%s/%s): render failed: %v", id, goos, sp.Backend, err)
				continue
			}
			// The seats the tier declares must be IN the rendered config, not refused:
			// the whole point of a Linux template is that the AMD box keeps its
			// vision/STT capability, not just its cascade.
			for _, s := range sp.MediaSeats {
				if !strings.Contains(rendered, "\n  "+s.Name+":") {
					t.Errorf("tier %s (%s/%s): declared seat %q missing from the rendered config", id, goos, sp.Backend, s.Name)
				}
			}
			if goos == "linux" && sp.Backend == "vulkan" && !strings.Contains(rendered, "GGML_VK_VISIBLE_DEVICES=0") {
				t.Errorf("tier %s (linux/vulkan): the single-device pin GGML_VK_VISIBLE_DEVICES=0 is missing", id)
			}
			if sp.Backend == "cpu" && strings.Contains(rendered, "--n-gpu-layers") {
				t.Errorf("tier %s (%s/cpu): a CPU template must not offload layers", id, goos)
			}
			_ = seedProfiles[id]
			checked++
		}
	}
	if checked < 6 {
		t.Fatalf("expected at least 3 tiers x 2 OSes on the vulkan/cpu backends, checked %d", checked)
	}
}
