package fleetview

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

func TestHandlerServesPageAndOverview(t *testing.T) {
	p := NewPoller(config.Config{}, nil, time.Second, 5)
	h := NewHandler(p)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	body := rr.Body.String()
	for _, id := range []string{`id="cluster"`, `id="cards"`, `id="jobs"`, `id="errors"`, `fetch('/api/overview')`} {
		if !strings.Contains(body, id) {
			t.Fatalf("page missing %s", id)
		}
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Fatal("page must not reference external resources")
	}
	if !strings.Contains(body, "const pct=tot?100*(1-free/tot):0") {
		t.Fatal("page missing the zero-vram_total_gb guard on the per-device VRAM bar (NaN width when a device row omits/zeroes vram_total_gb)")
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/overview", nil))
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rr.Body.String(), `"nodes"`) {
		t.Fatalf("overview: %d %s", rr.Code, rr.Body.String())
	}
}
