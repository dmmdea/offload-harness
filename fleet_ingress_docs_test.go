package main

import (
	"os"
	"strings"
	"testing"
)

// docSpan returns the text of a doc file from the first occurrence of from up to the first
// occurrence of to after it (to == "" means the end of that line). Line endings are normalized:
// a Windows checkout may hold CRLF.
func docSpan(t *testing.T, path, from, to string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	i := strings.Index(text, from)
	if i < 0 {
		t.Fatalf("%s: %q not found", path, from)
	}
	end := len(text)
	stop := "\n"
	if to != "" {
		stop = to
	}
	if j := strings.Index(text[i:], stop); j >= 0 {
		end = i + j
	}
	return text[i:end]
}

// The ingress guard judges the Host against EVERY tailnet zone the config lists (`tailnet_suffix`
// and `tailnet_suffixes`), so the operator pages that explain the Host rule must name both keys.
// A page that says "the tailnet zone" or "its own tailnet_suffix" tells a reader with a shared-in
// node that enforce will refuse a node it admits.
func TestFleetIngressGuardDocsNameEveryTailnetZoneKey(t *testing.T) {
	row := docSpan(t, "docs/OPERATOR-GUIDE.md", "| `fleet_ingress_guard` |", "")
	if !strings.Contains(row, "`tailnet_suffixes`") {
		t.Errorf("the OPERATOR-GUIDE row for fleet_ingress_guard does not name tailnet_suffixes:\n%s", row)
	}

	section := docSpan(t, "docs/systems/fleet-node.md", "**Ingress guard (register SF-16", "The **media** contract is unauthenticated")
	if !strings.Contains(section, "`tailnet_suffixes`") {
		t.Errorf("the fleet-node.md ingress guard section does not name tailnet_suffixes")
	}
	if strings.Contains(section, "as its own `tailnet_suffix`") {
		t.Errorf("the fleet-node.md ingress guard section still says a shared-in node needs its zone as its own tailnet_suffix; it is listed in tailnet_suffixes")
	}
}

// docNeeds fails for each fragment the doc span lacks.
func docNeeds(t *testing.T, where, span string, frags ...string) {
	t.Helper()
	for _, f := range frags {
		if !strings.Contains(span, f) {
			t.Errorf("%s does not mention %q", where, f)
		}
	}
}

// The guard sits in front of three listeners, so the operator pages for each of them say so:
// a page that documents `--serve` or the fleet node without the key tells a reader that
// enforce will never refuse them. The pages also say what the guard does NOT do and which
// callers it surprises, because the default (warn) serves every request it would refuse.
func TestFleetIngressGuardIsDocumentedWhereTheListenersAre(t *testing.T) {
	t.Run("OPERATOR-GUIDE --serve section", func(t *testing.T) {
		serve := docSpan(t, "docs/OPERATOR-GUIDE.md", "### Server mode (`--serve`)", "### Capability flags")
		docNeeds(t, "the OPERATOR-GUIDE --serve section", serve,
			`(model="offload-e4b", fleet_ingress_guard=warn)`, // what serveOpenAI prints
			"`fleet_ingress_guard`", "`enforce`", "Content-Type: application/json", "Origin")
	})
	t.Run("FLEET-NODE.md config row", func(t *testing.T) {
		node := docSpan(t, "docs/FLEET-NODE.md", "| `fleet_ingress_guard` |", "")
		docNeeds(t, "the FLEET-NODE.md fleet_ingress_guard config row", node, "`warn`", "`enforce`", "`tailnet_suffixes`")
	})
	t.Run("FLEET-NODE.md binding guidance", func(t *testing.T) {
		binding := docSpan(t, "docs/FLEET-NODE.md", "## Binding guidance", "## Footprints")
		docNeeds(t, "the FLEET-NODE.md binding guidance", binding, "`fleet_ingress_guard`", "not authentication", "ingress guard:")
	})
	t.Run("coding-agent.md", func(t *testing.T) {
		iface := docSpan(t, "docs/systems/coding-agent.md", "## Interfaces and entry points", "## Dependencies")
		docNeeds(t, "the coding-agent.md interfaces", iface, "`fleet_ingress_guard`", "Content-Type")
		invariants := docSpan(t, "docs/systems/coding-agent.md", "## Invariants and assumptions", "## Error handling")
		docNeeds(t, "the coding-agent.md invariants", invariants, "`fleet_ingress_guard`", "`warn`")
	})
	t.Run("fleet-node.md edges before enforce", func(t *testing.T) {
		edges := docSpan(t, "docs/systems/fleet-node.md", "Before setting `enforce`, know", "This guard is not authentication")
		docNeeds(t, "the fleet-node.md list of edges before enforce", edges,
			"`local-agent --serve`", "host.docker.internal", "`tailnet_suffixes`", "`fleet_ingress_guard`")
	})
	t.Run("fleet-node.md ingress guard section", func(t *testing.T) {
		section := docSpan(t, "docs/systems/fleet-node.md", "**Ingress guard (register SF-16", "The **media** contract is unauthenticated")
		docNeeds(t, "the fleet-node.md ingress guard section", section,
			"ingress guard: <mode>", "doctor", // the mode is printed at start and in doctor
			"5 minutes", "64", // the warn log's window and a peer's share
			"does not name the tailnet zones") // the refusal body keeps the operator's zones out of a rebound page's reach
	})
	t.Run("OPERATOR-GUIDE config row and troubleshooting row", func(t *testing.T) {
		guide := docSpan(t, "docs/OPERATOR-GUIDE.md", "| `fleet_ingress_guard` |", "")
		docNeeds(t, "the OPERATOR-GUIDE fleet_ingress_guard row", guide, "5 minute", "ingress guard:")
		row := docSpan(t, "docs/OPERATOR-GUIDE.md", "| HTTP `421` / `403` / `415` ", "")
		docNeeds(t, "the OPERATOR-GUIDE troubleshooting row for the guard's refusals", row, "`fleet_ingress_guard`", "`warn`")
	})
}
