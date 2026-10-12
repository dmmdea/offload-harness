package vllmseat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// H-33. A cache-server chunk is the engine's unified KV block in tokens (784 for the
// Qwen3.8-27B hybrid with fp16 KV, 1568 with fp8, which doubles it), and the engine's
// per-step token budget (--max-num-batched-tokens) must cover at least one block.
// Validate checked chunk_size > 0 and nothing else, so a seat could be authored, bound
// to a store through `install vllm-seat --config`, and rendered with a batch smaller
// than the chunk it was registered with. The block is a model-and-dtype property known
// only as measured pairs, not a Spec field, and ChunkSize is declared to EQUAL it, so
// for a store-backed seat "batch >= chunk" is the checkable form of "batch >= block".

// batchBelowChunk is the one mutation every refusal test applies to a store-backed seat.
func batchBelowChunk(s Spec) Spec {
	s.MaxBatchedTokens = 1024
	if s.CacheServer != nil {
		s.CacheServer.ChunkSize = 1568
	}
	return s
}

func TestASeatWhoseBatchIsSmallerThanTheCacheChunkIsRefused(t *testing.T) {
	extra := extraSeat()
	extra.CacheServer = &CacheServer{Address: "/mnt/kvcache/x", ChunkSize: 1568, KeyPrefix: "x-gen1"}
	for name, check := range map[string]func(t *testing.T) error{
		"lane seat": func(t *testing.T) error {
			return batchBelowChunk(flagship()).Validate("blackwell-2x16")
		},
		"extra seat": func(t *testing.T) error {
			return batchBelowChunk(extra).ValidateExtra("ampere-16")
		},
		// The render path re-validates AFTER a per-seat binding replaced the tier's
		// chunk, which is the only place a bound chunk meets the seat's own batch.
		"render": func(t *testing.T) error {
			_, err := batchBelowChunk(pinned()).Artifacts(wslTemplatesDir(), wslRT())
			return err
		},
		"one token under": func(t *testing.T) error {
			s := flagship()
			s.MaxBatchedTokens = s.CacheServer.ChunkSize - 1
			return s.Validate("blackwell-2x16")
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := check(t)
			if err == nil {
				t.Fatal("a store-backed seat whose max_num_batched_tokens is below the cache chunk_size must be refused; Validate returned nil")
			}
			for _, want := range []string{"max_num_batched_tokens", "chunk_size 1568", "cache"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal should contain %q; got %v", want, err)
				}
			}
		})
	}
	// The numbers are the message: the author must see both, not a generic complaint.
	err := batchBelowChunk(flagship()).Validate("blackwell-2x16")
	if err == nil || !strings.Contains(err.Error(), "max_num_batched_tokens 1024") {
		t.Errorf("the refusal should name max_num_batched_tokens 1024; got %v", err)
	}
}

// Equal is the boundary the rule must allow: one full block per step is the smallest
// batch that can hold a block.
func TestASeatWhoseBatchEqualsTheChunkIsAccepted(t *testing.T) {
	s := flagship()
	s.MaxBatchedTokens = s.CacheServer.ChunkSize
	if err := s.Validate("blackwell-2x16"); err != nil {
		t.Fatalf("a batch equal to the chunk must validate: %v", err)
	}
	p := pinned()
	p.MaxBatchedTokens = p.CacheServer.ChunkSize
	if _, err := p.Artifacts(wslTemplatesDir(), wslRT()); err != nil {
		t.Fatalf("a batch equal to the chunk must render: %v", err)
	}
}

// Without a store there is no chunk, so there is nothing to compare: a storeless seat's
// batch is its own operating point (the shipped storeless seats run 2048 and 4096). A
// storeless fp8 seat with a small batch therefore stays unguarded, which is a named
// limit of this rule, not a promise: the block is not a Spec field.
func TestAStorelessSeatIsNotJudgedByTheChunk(t *testing.T) {
	s := ref()
	s.MaxBatchedTokens = 64
	if err := s.Validate("ampere-16"); err != nil {
		t.Fatalf("a storeless seat has no chunk to be smaller than: %v", err)
	}
	s.StorelessReason = "measured: no store beside the embedder"
	if err := s.Validate("ampere-16"); err != nil {
		t.Fatalf("a storeless seat with a recorded reason has no chunk either: %v", err)
	}
}

// An undeclared batch (0) is not a number to compare. The rule judges two DECLARED
// values; whether a seat must declare a batch at all is a different question.
func TestASeatThatDeclaresNoBatchIsNotJudgedByTheChunk(t *testing.T) {
	s := flagship()
	s.MaxBatchedTokens = 0
	if err := s.Validate("blackwell-2x16"); err != nil {
		t.Fatalf("an undeclared max_num_batched_tokens is not below the chunk: %v", err)
	}
}

// The two shipped store-backed seats, read from the committed tier table: fp8 KV
// doubles the hybrid unified block (784 -> 1568), and the measured batch is 3135, one
// token under two blocks. This pins the pairs, it does not derive them.
func TestTheMeasuredFP8SeatsKeepTheirChunk(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			VLLMSeat *Spec  `json:"vllm_seat"`
			Extra    []Spec `json:"extra_vllm_seats"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, tier := range []string{"blackwell-2x16", "blackwell-3x16"} {
		p, ok := doc.Profiles[tier]
		if !ok || p.VLLMSeat == nil {
			t.Fatalf("tier %s declares no vllm_seat in profiles.json", tier)
		}
		s := *p.VLLMSeat
		if s.CacheServer == nil {
			t.Fatalf("tier %s: the measured fp8 seat is bound to a cache server", tier)
		}
		if got := strings.ToLower(s.KVCacheDtype); !strings.HasPrefix(got, "fp8") {
			t.Errorf("tier %s: kv_cache_dtype = %q, the measured pair is fp8", tier, s.KVCacheDtype)
		}
		if s.CacheServer.ChunkSize != 1568 {
			t.Errorf("tier %s: chunk_size = %d, the measured fp8 unified block is 1568 (fp16 is 784)", tier, s.CacheServer.ChunkSize)
		}
		if s.MaxBatchedTokens != 3135 {
			t.Errorf("tier %s: max_num_batched_tokens = %d, the measured operating point is 3135", tier, s.MaxBatchedTokens)
		}
		if s.MaxBatchedTokens < s.CacheServer.ChunkSize {
			t.Errorf("tier %s: batch %d is below chunk %d", tier, s.MaxBatchedTokens, s.CacheServer.ChunkSize)
		}
		if err := s.Validate(tier); err != nil {
			t.Errorf("tier %s: the shipped seat must validate: %v", tier, err)
		}
	}
	// Every other shipped store-backed seat obeys the same inequality (today none
	// besides the two above exists; this keeps the next one honest).
	var tiers []string
	for tier := range doc.Profiles {
		tiers = append(tiers, tier)
	}
	sort.Strings(tiers)
	for _, tier := range tiers {
		p := doc.Profiles[tier]
		seats := append([]Spec(nil), p.Extra...)
		if p.VLLMSeat != nil {
			seats = append(seats, *p.VLLMSeat)
		}
		for _, s := range seats {
			if s.CacheServer != nil && s.MaxBatchedTokens > 0 && s.MaxBatchedTokens < s.CacheServer.ChunkSize {
				t.Errorf("tier %s seat %s: batch %d is below the cache chunk %d", tier, s.ID, s.MaxBatchedTokens, s.CacheServer.ChunkSize)
			}
		}
	}
}
