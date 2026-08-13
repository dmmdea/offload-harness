// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package fakeswap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && resp.StatusCode < 400 {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

func TestActivityPaginatesAscendingAndDescending(t *testing.T) {
	s := New()
	defer s.Close()
	for i := 0; i < 25; i++ {
		s.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 120)
	}
	var page struct {
		Data       []Activity `json:"data"`
		Page       int        `json:"page"`
		Total      int        `json:"total"`
		TotalPages int        `json:"total_pages"`
	}
	getJSON(t, s.URL()+"/api/metrics/activity?limit=10&page=1&sort=id&order=asc", &page)
	if page.Total != 25 || page.TotalPages != 3 {
		t.Fatalf("total=%d total_pages=%d, want 25/3", page.Total, page.TotalPages)
	}
	if len(page.Data) != 10 || page.Data[0].ID != 1 || page.Data[9].ID != 10 {
		t.Fatalf("asc page 1 = %v", page.Data)
	}
	getJSON(t, s.URL()+"/api/metrics/activity?limit=10", &page)
	if page.Data[0].ID != 25 {
		t.Fatalf("default order should be newest-first, got first id %d", page.Data[0].ID)
	}
}

func TestResetEpochRestartsIDsAndZeroesCounters(t *testing.T) {
	s := New()
	defer s.Close()
	for i := 0; i < 5; i++ {
		s.AddActivity("m", "/v1/embeddings", 200, 10)
	}
	if got := s.TotalRequests(); got != 5 {
		t.Fatalf("total_requests = %d, want 5", got)
	}
	s.ResetEpoch()
	if got := s.TotalRequests(); got != 0 {
		t.Fatalf("total_requests after reset = %d, want 0", got)
	}
	id := s.AddActivity("m", "/v1/embeddings", 200, 10)
	if id != 1 {
		t.Fatalf("first id after reset = %d, want 1 (ids must restart)", id)
	}
}

func TestEvictOldestAndEvictIDShapeTheRing(t *testing.T) {
	s := New()
	defer s.Close()
	for i := 0; i < 10; i++ {
		s.AddActivity("m", "/v1/embeddings", 200, 10)
	}
	s.EvictOldest(4)
	ids := s.VisibleIDs()
	if len(ids) != 6 || ids[0] != 5 {
		t.Fatalf("after EvictOldest(4) ids = %v", ids)
	}
	if s.TotalRequests() != 10 {
		t.Fatalf("eviction must not touch the cumulative counter, got %d", s.TotalRequests())
	}
	s.EvictID(7)
	ids = s.VisibleIDs()
	for _, id := range ids {
		if id == 7 {
			t.Fatalf("id 7 still visible: %v", ids)
		}
	}
}

func TestRunningReportsTTLZeroForResidentSeat(t *testing.T) {
	s := New()
	defer s.Close()
	s.AddModel(Model{ID: "embeddinggemma", Aliases: []string{"text-embedding", "local-embed"}, ConfigTTL: -1})
	s.RunningIDs("embeddinggemma")
	var out struct {
		Running []struct {
			Model string `json:"model"`
			TTL   int    `json:"ttl"`
		} `json:"running"`
	}
	getJSON(t, s.URL()+"/running", &out)
	if len(out.Running) != 1 || out.Running[0].TTL != 0 {
		t.Fatalf("/running must reproduce the verified ttl:0 lie for a ttl:-1 seat, got %+v", out.Running)
	}
}

func TestSlotModes(t *testing.T) {
	s := New()
	defer s.Close()
	s.AddModel(Model{ID: "m"})
	cases := map[SlotMode]int{
		SlotIdle:       200,
		SlotProcessing: 200,
		SlotStatus404:  404,
		SlotStatus500:  500,
	}
	for mode, want := range cases {
		s.SetSlots("m", mode)
		got := getJSON(t, s.URL()+"/upstream/m/slots", nil)
		if got != want {
			t.Fatalf("mode %s: status %d, want %d", mode, got, want)
		}
	}
}

func TestUnloadCallsAreRecorded(t *testing.T) {
	s := New()
	defer s.Close()
	s.AddModel(Model{ID: "m"})
	s.RunningIDs("m")
	resp, err := http.Post(s.URL()+"/api/models/unload/m", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	calls := s.UnloadCalls()
	if len(calls) != 1 || calls[0].Model != "m" {
		t.Fatalf("unload calls = %+v", calls)
	}
	var out struct {
		Running []any `json:"running"`
	}
	getJSON(t, s.URL()+"/running", &out)
	if len(out.Running) != 0 {
		t.Fatalf("unload should clear the model from /running, got %v", out.Running)
	}
}

func TestEventsReplaysQueuedFrames(t *testing.T) {
	s := New()
	defer s.Close()
	s.PushEvent(fmt.Sprintf(`{"type":"logData","data":%q}`, "[INFO] matrix: model=gemma-4-e4b starting (no models running)"))
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 128)
	n, _ := resp.Body.Read(buf)
	if n == 0 {
		t.Fatal("expected at least one SSE frame")
	}
}
