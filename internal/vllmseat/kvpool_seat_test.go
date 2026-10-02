package vllmseat

import (
	"strings"
	"testing"
)

// execLine is the run script's `exec vllm serve ...` command as ONE logical line: the
// backslash-continued physical lines joined, and nothing from the comments above it. A
// substring match over the whole script would pass on the header comment that explains the
// flag, which is the failure the gate exists to catch (the flag reaching the engine).
func execLine(t *testing.T, run string) string {
	t.Helper()
	lines := strings.Split(strings.ReplaceAll(run, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), "exec vllm serve") {
			continue
		}
		joined := strings.TrimSpace(l)
		for strings.HasSuffix(joined, `\`) && i+1 < len(lines) {
			i++
			joined = strings.TrimSuffix(joined, `\`) + " " + strings.TrimSpace(lines[i])
		}
		return joined
	}
	t.Fatalf("the run script has no `exec vllm serve` line:\n%s", run)
	return ""
}

// ampere16LaneKVPool is the 1.42 GiB KV pool the ampere-16 lane seat is started with (the
// pool vLLM 0.29.0 sized on a warm start, 45,472 tokens; ADR 0049 Amendment 6, register
// A-129): the number the seat's table declares and the launch line must carry.
const ampere16LaneKVPool int64 = 1524713390

// TestLinuxSeatRendersItsFixedKVPool: a util-sized KV pool is measured during startup
// profiling, device-wide, so a cold compile cache or another model loading in that window
// shrinks it below one request and the start fails (register A-129). The lane seat is
// therefore started with a FIXED pool, and the linux-systemd run script has to carry it:
// until the A-119 seed only the windows-wsl launch rendered --kv-cache-memory-bytes, and the
// validator refused the field here, so the ampere-16 reference box (a Linux unit) could not
// declare the pin at all.
func TestLinuxSeatRendersItsFixedKVPool(t *testing.T) {
	s := ref()
	s.ModelPath = "/hf/hub/models--RedHatAI--Qwen3.5-4B-quantized.w4a16/snapshots/deadbeef"
	s.KVCacheMemoryBytes = ampere16LaneKVPool
	if err := s.Validate("ampere-16"); err != nil {
		t.Fatalf("a pinned pool on the linux-systemd launch must validate: %v", err)
	}
	files, err := s.Artifacts(templatesDir(), rt())
	if err != nil {
		t.Fatal(err)
	}
	line := execLine(t, files["vllm-seat-run.sh"])
	if !strings.Contains(line, " --kv-cache-memory-bytes 1524713390 ") {
		t.Errorf("the engine's launch line does not carry the pinned pool:\n%s", line)
	}
	if n := strings.Count(line, "--kv-cache-memory-bytes"); n != 1 {
		t.Errorf("the pool flag appears %d times on the launch line, want exactly 1:\n%s", n, line)
	}
	// vLLM ignores the utilization once the pool is pinned, but the flag stays on the line: it is
	// still the value a reader of the unit and a re-measurement start from.
	if !strings.Contains(line, "--gpu-memory-utilization 0.65") {
		t.Errorf("the pinned launch line lost --gpu-memory-utilization:\n%s", line)
	}
	for _, f := range files {
		if strings.Contains(f, "__") {
			t.Error("a rendered artifact still holds a token")
		}
	}
}

// TestLinuxSeatWithoutAPoolRendersNoKVPoolFlag is the other direction: a seat that declares no
// pool is util-sized, exactly as every seat was before the field reached the linux launch, and
// the flag must not appear (an unconditional flag would pin every Linux seat to 0 bytes).
func TestLinuxSeatWithoutAPoolRendersNoKVPoolFlag(t *testing.T) {
	s := ref()
	s.ModelPath = "/hf/hub/models--RedHatAI--Qwen3.5-4B-quantized.w4a16/snapshots/deadbeef"
	files, err := s.Artifacts(templatesDir(), rt())
	if err != nil {
		t.Fatal(err)
	}
	line := execLine(t, files["vllm-seat-run.sh"])
	if strings.Contains(line, "kv-cache-memory-bytes") {
		t.Errorf("a seat with no pinned pool rendered the flag:\n%s", line)
	}
	if !strings.Contains(line, "--gpu-memory-utilization 0.65") {
		t.Errorf("an unpinned seat must stay util-sized:\n%s", line)
	}
}

// The pool is the one launch-line field the linux template now renders besides the
// historical set; a pipeline is still windows-wsl only, and a negative pool is still
// nonsense on either launch.
func TestKVPoolValidationAcrossLaunches(t *testing.T) {
	s := ref()
	s.KVCacheMemoryBytes = -1
	if err := s.Validate("t"); err == nil || !strings.Contains(err.Error(), "kv_cache_memory_bytes") {
		t.Errorf("a negative pool must be refused by name, got %v", err)
	}
	p := ref()
	p.KVCacheMemoryBytes = ampere16LaneKVPool
	p.Device, p.PipelineParallel, p.LayerPartition = "0,1", 2, "32,32"
	err := p.Validate("t")
	if err == nil {
		t.Fatal("a pipeline on the linux-systemd launch must still be refused: its run script renders no pipeline flags")
	}
	if !strings.Contains(err.Error(), "pipeline_parallel") {
		t.Errorf("the refusal must name pipeline_parallel, got %v", err)
	}
	// ... and the refusal is about the pipeline alone: the same seat without it validates with the pool.
	p.Device, p.PipelineParallel, p.LayerPartition = "", 0, ""
	if err := p.Validate("t"); err != nil {
		t.Errorf("the pool on its own must validate on linux-systemd: %v", err)
	}
}
