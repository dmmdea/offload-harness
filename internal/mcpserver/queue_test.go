package mcpserver

// The MCP side of the ticket queue (P0 plan S2; ADR 0083): the image door's window, the place a queued call keeps in the box's
// own line, the tool text that tells a caller not to loop, and the media_queue block of offload_status.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediaremote"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// fakeKeeper records the calls the door's runner forwards to the place keeper.
type fakeKeeper struct {
	left  []core.Request
	kept  []string
	drops []string
}

func (f *fakeKeeper) LeaveMediaPlace(_ context.Context, req core.Request, since time.Time) (core.MediaPlace, error) {
	f.left = append(f.left, req)
	return core.MediaPlace{Token: "tk-fake00000001", Since: since}, nil
}
func (f *fakeKeeper) KeepMediaPlace(token string) bool { f.kept = append(f.kept, token); return true }
func (f *fakeKeeper) DropMediaPlace(token string)      { f.drops = append(f.drops, token) }

// The ticket queue keeps a queued call's place through the runner the door hands it: the pipeline's, or a seam, and never
// silently nothing on a server that has a pipeline.
func TestTheMediaDoorsForwardThePlaceToThePipeline(t *testing.T) {
	s := New(nil)
	f := &fakeKeeper{}
	s.placeHook = f
	req := core.Request{Task: core.TaskGenerateImage, Input: "a red door", Params: map[string]any{"family": "qwen-image-2.1"}}
	since := time.Now().Add(-time.Minute)
	pl, err := core.LeavePlace(context.Background(), runTaskAs{s}, req, since)
	if err != nil || pl.Token != "tk-fake00000001" || !pl.Since.Equal(since) {
		t.Fatalf("the place comes back as the keeper gave it: %+v %v", pl, err)
	}
	if len(f.left) != 1 || f.left[0].Params["family"] != "qwen-image-2.1" {
		t.Fatalf("the keeper was asked about the door's request: %+v", f.left)
	}
	if !core.KeepPlace(runTaskAs{s}, "tk-fake00000001") || len(f.kept) != 1 {
		t.Errorf("keeping reaches the keeper: %v", f.kept)
	}
	core.DropPlace(runTaskAs{s}, "tk-fake00000001")
	if len(f.drops) != 1 {
		t.Errorf("dropping reaches the keeper: %v", f.drops)
	}

	// A server with no pipeline keeps none; a server with one asks it.
	bare := New(nil)
	if _, err := core.LeavePlace(context.Background(), runTaskAs{bare}, req, time.Time{}); !errors.Is(err, core.ErrNoMediaPlace) {
		t.Errorf("no pipeline, no place: %v", err)
	}
	if core.KeepPlace(runTaskAs{bare}, "tk-fake00000001") {
		t.Error("no pipeline keeps nothing")
	}
	core.DropPlace(runTaskAs{bare}, "tk-fake00000001") // harmless
	real := New(pipeline.New(config.Config{}, nil, nil, nil))
	video := core.Request{Task: core.TaskGenerateVideo, Input: "a slow pan"}
	_, err = core.LeavePlace(context.Background(), runTaskAs{real}, video, time.Time{})
	if !errors.Is(err, core.ErrNoMediaPlace) || !strings.Contains(err.Error(), "only image generation") {
		t.Errorf("the pipeline's own answer must come back (a video call keeps no place, and says why): %v", err)
	}
}

// wait_s is the window the door waits for its ticket: absent is the machine's gpu_wait_ms, 0 answers after the ticket's first look,
// and a large value is capped.
func TestWaitSMapsToTheWindow(t *testing.T) {
	ptr := func(n int) *int { return &n }
	for _, tc := range []struct {
		name string
		in   *int
		want time.Duration
	}{
		{"absent: gpu_wait_ms", nil, 0},
		{"zero: after the first look", ptr(0), mediaremote.AtOnce},
		{"negative: the same", ptr(-5), mediaremote.AtOnce},
		{"seconds", ptr(45), 45 * time.Second},
		{"capped", ptr(100000), maxWaitS * time.Second},
	} {
		if got := waitWindow(tc.in); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The tool text tells a caller what the answer means and not to loop: wait_s is advertised, the waiter_token is described as
// the ticket (and as the place it brings along), and the route text no longer says a call with a token is never moved.
func TestTheImageToolAdvertisesTheTicketAndWaitS(t *testing.T) {
	var raw []byte
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name == "offload_generate_image" {
			raw, _ = json.Marshal(tool.InputSchema)
		}
	}
	if raw == nil {
		t.Fatal("offload_generate_image is not advertised")
	}
	var schema struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if w := schema.Properties["wait_s"]; w.Type != "integer" || !strings.Contains(w.Description, "answers 'queued'") {
		t.Errorf("wait_s: %+v", w)
	}
	for _, want := range []string{"mq-", "do NOT call again", "collect the result", "tk-"} {
		if !strings.Contains(schema.Properties["waiter_token"].Description, want) {
			t.Errorf("waiter_token must say %q: %s", want, schema.Properties["waiter_token"].Description)
		}
	}
	route := schema.Properties["route"].Description
	if strings.Contains(route, "is never moved") || strings.Contains(route, "on THIS machine only") {
		t.Errorf("the route text still describes the S1 behaviour: %s", route)
	}
	for _, want := range []string{"TICKET", "ADR 0083", "mq-"} {
		if !strings.Contains(route, want) {
			t.Errorf("the route text must say %q: %s", want, route)
		}
	}
}

// offload_status lists the machine's tickets as media_queue[], and names nothing when there are none.
func TestStatusMediaQueueListsTheTickets(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	s := New(pipeline.New(cfg, nil, nil, nil))
	empty := s.statusPayload(context.Background(), cfg, "media_queue")
	if v, ok := empty["media_queue"].(map[string]any); !ok || len(v) != 0 {
		t.Fatalf("asked for by name with no ticket, the block is empty: %#v", empty)
	}

	dir := filepath.Join(cfg.StateDir, "media-queue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ticket := map[string]any{"id": "mq-0123456789ab", "req_key": "k", "created_ms": time.Now().Add(-90 * time.Second).UnixMilli(),
		"owner": map[string]any{"pid": os.Getpid()}, "heartbeat_ms": time.Now().UnixMilli(), "task": "generate_image", "family": "qwen-image-2.1-fast",
		"state": "waiting", "scanned": true, "position": 2, "local_token": "tk-abc12345"}
	b, _ := json.Marshal(ticket)
	if err := os.WriteFile(filepath.Join(dir, "mq-0123456789ab.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	got := s.statusPayload(context.Background(), cfg, "media_queue")
	rows, ok := got["media_queue"].([]mediaremote.QueueRow)
	if !ok || len(rows) != 1 {
		t.Fatalf("one ticket listed: %#v", got)
	}
	r := rows[0]
	if r.Ticket != "mq-0123456789ab" || r.State != "waiting" || r.Position != 2 || !r.OwnerAlive || !r.Place || r.Family != "qwen-image-2.1-fast" || r.AgeSec < 60 {
		t.Errorf("row = %+v", r)
	}
}
