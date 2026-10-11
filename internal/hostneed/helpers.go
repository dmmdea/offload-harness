package hostneed

import (
	"sort"
	"strings"
)

// RenderHelpers is the file name of every render helper a lease's wrapped command is recognised by
// (ParseRenderCall), sorted. A message that names them (the line `gpu reserve` prints when it declares
// the media class default, F71) reads this list, so it cannot drift from the rule that recognises them.
func RenderHelpers() []string {
	names := make([]string, 0, len(helperRoute))
	for name := range helperRoute {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RunsOwnGraph says the call POSTS THE CALLER'S OWN WORKFLOW (`--graph <file>`): what such a call loads is
// whatever the graph names, which the lease cannot read, so it is sized by the media class default and not
// as the helper's default family (Estimate). The hint `gpu reserve` prints when it declares that default
// reads this same rule, so the two cannot disagree about which calls are graph calls.
func (c RenderCall) RunsOwnGraph() bool {
	return strings.TrimSpace(c.Flags["graph"]) != ""
}
