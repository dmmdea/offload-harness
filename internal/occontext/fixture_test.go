package occontext

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The fixture is SYNTHETIC: testdata/schema.sql is opencode's DDL copied from a real
// opencode.db, and every row below is invented in the JSON shapes opencode 1.18 writes
// (message.data / part.data). No real session content is ever committed.

var t0 = time.Date(2026, 1, 10, 10, 0, 0, 0, time.UTC)

type fixture struct {
	t    *testing.T
	dir  string
	path string
	db   *sql.DB
	seq  int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ddl, err := os.ReadFile(filepath.Join("testdata", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("schema: %v", err)
	}
	f := &fixture{t: t, dir: dir, path: path, db: db}
	t.Cleanup(func() { _ = db.Close() })
	return f
}

func (f *fixture) id(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_%06d", prefix, f.seq)
}

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (f *fixture) session(id, parent, title string, at time.Time) {
	var p any
	if parent != "" {
		p = parent
	}
	f.exec("INSERT INTO session (id, project_id, parent_id, slug, directory, title, version, time_created, time_updated) VALUES (?,?,?,?,?,?,?,?,?)",
		id, "prj_fixture", p, "slug-"+id, "/work/example", title, "1.18.32", ms(at), ms(at))
}

// user writes a user message with the given parts (each a part.data object).
func (f *fixture) user(sid string, at time.Time, parts ...map[string]any) string {
	mid := f.id("msg")
	data := map[string]any{"role": "user", "time": map[string]any{"created": ms(at)}, "agent": "build",
		"model": map[string]any{"providerID": "llamacpp", "modelID": "m-seat"}}
	f.exec("INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)", mid, sid, ms(at), ms(at), mustJSON(data))
	for _, p := range parts {
		f.part(mid, sid, at, p)
	}
	return mid
}

func (f *fixture) part(mid, sid string, at time.Time, p map[string]any) {
	f.exec("INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?,?)",
		f.id("prt"), mid, sid, ms(at), ms(at), mustJSON(p))
}

type call struct {
	agent     string
	model     string
	at        time.Time
	prompt    int // input + cache.read (cache.write is 0 on the servers measured)
	cached    int
	output    int
	reasoning int
	summary   bool
	ttft      time.Duration
	wall      time.Duration
	// reasoningText, when set, is stored as a reasoning part (bytes feed the estimate
	// used when the server reports reasoning 0, as llama.cpp does).
	reasoningText string
	text          string
	tools         []toolOut
	stub          bool // an assistant message with no recorded tokens (subtask launcher, abort)
}

type toolOut struct {
	name   string
	output string
	failed bool
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func synthetic(s string) map[string]any {
	return map[string]any{"type": "text", "text": s, "synthetic": true, "metadata": map[string]any{"compaction_continue": true}}
}

func compactionPart(auto, overflow bool) map[string]any {
	return map[string]any{"type": "compaction", "auto": auto, "overflow": overflow, "tail_start_id": "msg_tail"}
}

// assistant writes one assistant message = one LLM call, with the parts opencode writes
// for a call: step-start, reasoning, text, tool parts, step-finish.
func (f *fixture) assistant(sid string, c call) string {
	f.t.Helper()
	if c.agent == "" {
		c.agent = "build"
	}
	if c.model == "" {
		c.model = "m-seat"
	}
	if c.wall == 0 {
		c.wall = 5 * time.Second
	}
	if c.ttft == 0 {
		c.ttft = time.Second
	}
	mid := f.id("msg")
	tokens := map[string]any{"total": 0, "input": 0, "output": 0, "reasoning": 0, "cache": map[string]any{"write": 0, "read": 0}}
	if !c.stub {
		tokens = map[string]any{
			"total":     c.prompt + c.output + c.reasoning,
			"input":     c.prompt - c.cached,
			"output":    c.output,
			"reasoning": c.reasoning,
			"cache":     map[string]any{"write": 0, "read": c.cached},
		}
	}
	data := map[string]any{
		"parentID": "msg_parent", "role": "assistant", "mode": c.agent, "agent": c.agent,
		"path": map[string]any{"cwd": "/work/example", "root": "/work/example"}, "cost": 0,
		"tokens": tokens, "modelID": c.model, "providerID": "llamacpp",
		"time":   map[string]any{"created": ms(c.at), "completed": ms(c.at.Add(c.wall))},
		"finish": "stop",
	}
	if c.summary {
		data["summary"] = true
	}
	f.exec("INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)", mid, sid, ms(c.at), ms(c.at), mustJSON(data))
	first := c.at.Add(c.ttft)
	if !c.stub {
		f.part(mid, sid, first, map[string]any{"type": "step-start"})
	}
	if c.reasoningText != "" || (c.reasoning > 0 && !c.stub) {
		rt := c.reasoningText
		if rt == "" {
			rt = "synthetic reasoning"
		}
		f.part(mid, sid, first, map[string]any{"type": "reasoning", "text": rt, "time": map[string]any{"start": ms(first), "end": ms(first.Add(time.Second))}})
	}
	if c.text != "" {
		f.part(mid, sid, first, map[string]any{"type": "text", "text": c.text, "time": map[string]any{"start": ms(first), "end": ms(first)}})
	}
	for _, tl := range c.tools {
		state := map[string]any{"status": "completed", "input": map[string]any{}, "output": tl.output, "title": "t",
			"time": map[string]any{"start": ms(first), "end": ms(first)}}
		if tl.failed {
			state = map[string]any{"status": "error", "input": map[string]any{}, "error": tl.output,
				"time": map[string]any{"start": ms(first), "end": ms(first)}}
		}
		f.part(mid, sid, first, map[string]any{"type": "tool", "tool": tl.name, "callID": f.id("call"), "state": state})
	}
	if !c.stub {
		f.part(mid, sid, c.at.Add(c.wall), map[string]any{"type": "step-finish", "reason": "stop", "tokens": tokens, "cost": 0})
	}
	return mid
}

// analyze snapshots the fixture db through the real copy path and analyzes it.
func (f *fixture) analyze(opts Options) *Report {
	f.t.Helper()
	snap, err := Snapshot(f.path)
	if err != nil {
		f.t.Fatalf("snapshot: %v", err)
	}
	defer snap.Close()
	store, err := Load(snap.DB)
	if err != nil {
		f.t.Fatalf("load: %v", err)
	}
	if opts.ToolBytesPerToken == 0 {
		opts.ToolBytesPerToken = 2.7
	}
	if opts.TextBytesPerToken == 0 {
		opts.TextBytesPerToken = 3.9
	}
	r := Analyze(store, opts)
	r.Source = snap.Info
	return r
}

func findSession(t *testing.T, r *Report, id string) SessionReport {
	t.Helper()
	for _, s := range r.Sessions {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %s not in report (have %d sessions)", id, len(r.Sessions))
	return SessionReport{}
}

func findGroup(t *testing.T, r *Report, kind, agent string) GroupReport {
	t.Helper()
	for _, g := range r.Groups {
		if g.Kind == kind && g.Agent == agent {
			return g
		}
	}
	var have []string
	for _, g := range r.Groups {
		have = append(have, g.Kind+"/"+g.Agent)
	}
	t.Fatalf("group %s/%s not in report (have %s)", kind, agent, strings.Join(have, ", "))
	return GroupReport{}
}
