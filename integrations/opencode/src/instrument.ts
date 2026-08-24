// Cross-harness dispatch instrument. Appends one JSONL line per event to the SAME file the
// Claude Code hooks write (~/.claude/state/dispatch-log.jsonl), tagged harness:"opencode",
// so the weekly adherence read covers both harnesses with one arithmetic. Fail-open like
// its Claude Code twin: a broken log channel writes one stderr line and never throws.
import { appendFileSync, mkdirSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

export const DEFAULT_LOG = join(homedir(), ".claude", "state", "dispatch-log.jsonl");

export type DispatchEvent = {
  event: "session" | "readonly_spawn" | "task_reroute" | "task_reroute_failed" | "delegate" | "nudge" | "config_default_applied";
  sid?: string;
  [k: string]: unknown;
};

// Write failures are counted so offload_plugin_status can report "N log writes failed" —
// a dead instrument must be distinguishable from a quiet week. Stats are PER plugin
// instance (passed in by the caller), never a module singleton.
export type InstrumentStats = { writes: number; failures: number; lastError: string | null };
export function newInstrumentStats(): InstrumentStats {
  return { writes: 0, failures: 0, lastError: null };
}

// APPEND-ONLY on this side: the file is shared with the Claude Code hooks, which own its
// 1 MiB rotation. Two processes rotating the same file race each other into data loss, so
// exactly one writer rotates.
export function appendDispatchLog(ev: DispatchEvent, path: string = DEFAULT_LOG, stats: InstrumentStats = newInstrumentStats()): boolean {
  try {
    mkdirSync(dirname(path), { recursive: true });
    const line = { ts: new Date().toISOString(), harness: "opencode", ...ev, sid: String(ev.sid ?? "unknown").slice(0, 8) };
    appendFileSync(path, JSON.stringify(line) + "\n");
    stats.writes++;
    return true;
  } catch (e) {
    stats.failures++;
    stats.lastError = (e as Error)?.message ?? String(e);
    try {
      process.stderr.write(`[opencode-local-offload] dispatch-log write failed: ${stats.lastError}\n`);
    } catch {
      /* ignore */
    }
    return false;
  }
}
