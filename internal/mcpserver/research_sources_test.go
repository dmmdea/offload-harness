package mcpserver

// offload_research's text-source adapters (YouTube subtitles through a configured yt-dlp, RSS/Atom): the tool
// takes the caller's subtitle languages, and the fetch is handed the configured binary path and a scratch
// root. The adapters themselves are internal/research's tests; this pins the wiring and the tool surface.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/research"
)

func TestResearchHandsTheFetchTheSubtitleAdapterSettings(t *testing.T) {
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.StateDir = home
	cfg.LedgerPath = home + "/ledger.jsonl"
	cfg.AgentDelegationEnabled = true
	cfg.YtDlpPath = "/opt/tools/yt-dlp"
	s := New(pipeline.New(cfg, nil, nil, nil))
	var got research.Options
	s.researchFetch = func(_ context.Context, urls []string, opt research.Options) []research.Fetched {
		got = opt
		out := make([]research.Fetched, len(urls))
		for i, u := range urls {
			out[i] = research.Fetched{URL: u, Err: "stub"}
		}
		return out
	}
	if _, err := s.handleResearch(context.Background(), callReq(`{"goal":"g","urls":["https://youtu.be/dQw4w9WgXcQ"],"sub_langs":"es.*,en","route":"local"}`)); err != nil {
		t.Fatal(err)
	}
	if got.YtDlpPath != "/opt/tools/yt-dlp" || got.SubLangs != "es.*,en" || got.ScratchRoot != cfg.EffectiveResearchScratchDir() {
		t.Fatalf("options = %+v", got)
	}
	// with no key configured the adapter stays off
	cfg.YtDlpPath = ""
	s2 := New(pipeline.New(cfg, nil, nil, nil))
	s2.researchFetch = s.researchFetch
	if _, err := s2.handleResearch(context.Background(), callReq(`{"goal":"g","urls":["https://youtu.be/dQw4w9WgXcQ"],"route":"local"}`)); err != nil {
		t.Fatal(err)
	}
	if got.YtDlpPath != "" || got.SubLangs != "" {
		t.Fatalf("adapter not off by default: %+v", got)
	}
}

func TestResearchToolSchemaTakesSubLangsAndNamesTheSources(t *testing.T) {
	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	found := false
	for _, tool := range listTools(t, cfg) {
		if tool.Name != "offload_research" {
			continue
		}
		found = true
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		p, ok := schema.Properties["sub_langs"]
		if !ok || p.Type != "string" || !strings.Contains(p.Description, "en.*") {
			t.Fatalf("sub_langs = %+v", p)
		}
		for _, want := range []string{"YouTube", "RSS", "Atom"} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("tool description does not mention %s", want)
			}
		}
	}
	if !found {
		t.Fatal("offload_research not registered")
	}
}
