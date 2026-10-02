package tierdocs

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// ADR 0049 Amendment 5 (register A-122b) made max_num_batched_tokens the third lever the ampere-16 co-residency
// guard pins, beside the utilization and the sequence count: the engine's activation workspace scales with it. The
// page that states a seat's operating point therefore states all three, for the lane seat and for the extra
// seats, and says nothing for a seat that declares no budget (a "0" would read as a measured zero).
func TestSeatTablesStateTheBatchBudget(t *testing.T) {
	lane := laneSpec()
	lane.MaxBatchedTokens = 2048
	extra := extraSpec()
	extra.MaxBatchedTokens = 4096
	page := renderTier("t", Profile{CtxSize: 8192, VLLMSeat: lane, ExtraVLLMSeats: []vllmseat.Spec{extra}, Layers: layers()}, nil)

	laneRow := "| max_num_batched_tokens | 2048 | the engine's per-step token budget (`--max-num-batched-tokens`); with the sequence count it sizes the workspace the profiled share must cover, so it is a co-residency lever beside utilization |"
	extraRow := "| max_num_batched_tokens | 4096 | the engine's per-step token budget (`--max-num-batched-tokens`) |"
	li, ei, section := strings.Index(page, laneRow), strings.Index(page, extraRow), strings.Index(page, "### Extra vLLM seats")
	if li < 0 || ei < 0 || section < 0 {
		t.Fatalf("the page lost a batch-budget row (lane %d, extra %d, extra-seats section %d):\n%s", li, ei, section, page)
	}
	if !(li < section && section < ei) {
		t.Errorf("the lane seat's row must sit in the Agent seat table (before the extra seats) and the extra seat's in its own (after): lane %d, section %d, extra %d", li, section, ei)
	}

	// A seat that declares no budget gets no row: laneSpec and extraSpec leave it unset.
	none := renderTier("t", Profile{CtxSize: 8192, VLLMSeat: laneSpec(), ExtraVLLMSeats: []vllmseat.Spec{extraSpec()}, Layers: layers()}, nil)
	if strings.Contains(none, "max_num_batched_tokens") {
		t.Errorf("a seat with no declared batch budget printed a row for it:\n%s", none)
	}
}
