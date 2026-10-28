package update

import (
	"fmt"
	"io"
	"strings"
)

// PrintReports writes the generation table and, under it, what each component did.
func PrintReports(w io.Writer, reports []Report, opts Options) {
	if opts.ResolveOnly {
		for _, r := range reports {
			if r.Candidate == nil {
				fmt.Fprintf(w, "%s: %s %s\n", r.Component, r.Action, r.Detail)
				continue
			}
			c := r.Candidate
			fmt.Fprintf(w, "%s: %s (%s, channel %s) from %s\n", c.Component, c.ID, orDash(c.Variant), orDash(c.Channel), c.Source)
			for _, a := range c.Assets {
				fmt.Fprintf(w, "  %-52s %-72s %5d MiB\n", a.Name, a.Digest, a.Size>>20)
			}
			for _, n := range c.Notes {
				fmt.Fprintf(w, "  note: %s\n", n)
			}
		}
		return
	}
	rows := [][]string{{"COMPONENT", "INSTALLED", "PREVIOUS", "RESOLVED", "ACTION"}}
	for _, r := range reports {
		rows = append(rows, []string{r.Component, orDash(r.Installed), orDash(r.Previous), orDash(r.Resolved), r.Action})
	}
	widths := make([]int, 5)
	for _, row := range rows {
		for i, c := range row {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	for _, row := range rows {
		var b strings.Builder
		for i, c := range row {
			fmt.Fprintf(&b, "%-*s  ", widths[i], c)
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	for _, r := range reports {
		if r.Detail != "" {
			fmt.Fprintf(w, "%s: %s\n", r.Component, r.Detail)
		}
		for _, n := range r.Notes {
			fmt.Fprintf(w, "  %s\n", n)
		}
	}
	for _, r := range reports {
		if r.Action == "updated" || r.Action == "installed" || r.Action == "rollback" {
			fmt.Fprintln(w, "note: a running MCP server keeps the old harness image until it is restarted (ADR 0085); this verb replaces engines and the llama-swap daemon, not that process.")
			break
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
