package agent

// Log-collapse rung (stub: behaviour lands in the next commit).

const (
	logsHeadLines = 8
	logsTailLines = 4
	logsMinRun    = 3
	logsMinChars  = 400
	logsMaxChars  = 4 << 20
	logsPrefix    = "[log-collapse: original "
)

func logsKey(line string) string { return line }

func logsKeep(line string) bool { return false }

func isLogCollapsed(content string) bool { return false }

var logsCollapseImpl = collapseLogs

func collapseLogs(content string) (string, bool) { return content, false }

func logCollapse(content string) (string, bool) { return content, false }

func logsRung(out []Msg, from, to, budget int, pinned map[string]bool) {}
