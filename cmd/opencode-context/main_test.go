package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/occontext"
	_ "modernc.org/sqlite"
)

// A synthetic two-call session on opencode's real DDL (internal/occontext/testdata).
func fixtureDB(t *testing.T) string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join("..", "..", "internal", "occontext", "testdata", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{string(ddl),
		`INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES ('ses_x','prj','s','/work/example','t','1.18.32',1000,1000)`,
		`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
		 ('msg_1','ses_x',2000,2000,'{"role":"assistant","agent":"build","modelID":"m-seat","providerID":"llamacpp","tokens":{"total":1100,"input":1000,"output":60,"reasoning":40,"cache":{"read":0,"write":0}},"time":{"created":2000,"completed":3000}}'),
		 ('msg_2','ses_x',5000,5000,'{"role":"assistant","agent":"build","modelID":"m-seat","providerID":"llamacpp","tokens":{"total":1300,"input":132,"output":10,"reasoning":10,"cache":{"read":1148,"write":0}},"time":{"created":5000,"completed":6000}}')`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q[:40], err)
		}
	}
	return path
}

func TestRunJSONHonoursTheFlags(t *testing.T) {
	db := fixtureDB(t)
	var out, errOut bytes.Buffer
	args := []string{"--db", db, "--json", "--cache-block", "1148", "--cache-valid-since", "llamacpp/m-seat=1970-01-01T00:00:01Z", "--session", "ses_x"}
	if err := run(args, &out, &errOut); err != nil {
		t.Fatalf("run: %v (%s)", err, errOut.String())
	}
	var r occontext.Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("output is not the JSON report: %v\n%s", err, out.String())
	}
	if len(r.Sessions) != 1 || len(r.Sessions[0].Calls) != 2 {
		t.Fatalf("sessions = %+v", r.Sessions)
	}
	if r.Params.CacheBlock != 1148 || len(r.Params.CacheValidSince) != 1 || r.Params.CacheValidSince[0].Model != "llamacpp/m-seat" {
		t.Fatalf("params = %+v", r.Params)
	}
	if r.Params.TextBytesPerToken != 3.5 || r.Params.ToolBytesPerToken != 2.7 {
		t.Fatalf("default ratios = %+v", r.Params)
	}
	// The first call (t=2 s) is after the 1 s cutoff; both count, 1148 of 2280 cached.
	if r.Cache.ValidCalls != 2 || r.Cache.Cached != 1148 || r.Cache.Prompt != 2280 {
		t.Fatalf("cache = %+v", r.Cache)
	}
	if len(r.Source.Copied) == 0 || r.Source.DB != db {
		t.Fatalf("source = %+v", r.Source)
	}
}

func TestRunTableAndBadFlags(t *testing.T) {
	db := fixtureDB(t)
	var out, errOut bytes.Buffer
	if err := run([]string{"--db", db}, &out, &errOut); err != nil || !strings.Contains(out.String(), "ses_x") {
		t.Fatalf("table run: %v\n%s", err, out.String())
	}
	for _, bad := range [][]string{
		{"--db", db, "--cache-valid-since", "not-a-time"},
		{"--db", db, "--tool-bytes-per-token", "0"},
		{"--db", filepath.Join(t.TempDir(), "absent.db")},
		{"--db", db, "stray"},
	} {
		if err := run(bad, &out, &errOut); err == nil {
			t.Errorf("run(%q) succeeded, want an error", bad)
		}
	}
}
