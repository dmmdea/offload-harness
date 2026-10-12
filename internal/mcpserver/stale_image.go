package mcpserver

// stale_image.go: offload_status's process block (D-36).
//
// A rename-swap deploy replaces the installed binary while every MCP server already running keeps the image it
// started from, so a session can write rows with old behaviour for days and nothing says so. The block names the
// image this process runs and compares it with the file now at the path it started from (STUB, red-first).

// holdStartup takes the startup identity Run serves under (STUB).
func (s *Server) holdStartup() {}

// statusProcess builds the process block, or nil when the Server holds no startup identity (STUB).
func (s *Server) statusProcess() any { return nil }

// hashInstalledImage is the seam the installed file is hashed through (STUB).
var hashInstalledImage = func(path string) (string, error) { return "", nil }
