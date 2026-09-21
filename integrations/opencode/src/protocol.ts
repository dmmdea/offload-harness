// The plan-time surface: injected into every turn's system prompt via
// experimental.chat.system.transform. Claude Code has no equivalent lever — its rule file
// sits in the cached prefix and its hooks fire after a parallel burst is already composed.
// Here the protocol is present in the generation that composes the dispatch. Kept compact:
// this text is paid every turn.
// Both protocol variants carry this marker; the system transform de-duplicates on it. A variant
// without it was injected twice per turn (caught by "injects the protocol once").
export const PROTOCOL_MARKER = "(house protocol, always on)";

export function protocolText(mcp: string, offloadAgent: string, primaryTools: "tier1" | "all" = "all"): string {
  const t = (name: string) => `${mcp}_${name}`;
  if (primaryTools === "tier1") return tier1ProtocolText(mcp, offloadAgent);
  return [
    `# local-offload harness — three-lane dispatch (house protocol, always on)`,
    `Free local lanes are wired as MCP tools (prefix \`${mcp}_\`) and as the \`${offloadAgent}\` subagent. UNDER-use is the measured failure mode; use them by default.`,
    `At every 2+-subagent moment, BEFORE composing any task call:`,
    `1. Partition legs. DELEGABLE = read-only over local files, bounded (≲6 named files), answer fits a flat schema. CLAUDE-ONLY only via a NAMED exception on that leg: needs web/network · writes/executes · judgment (review, design, architecture) · over seat capacity. "It's research, it all needs web" is the recorded failure pattern.`,
    `2. ≥2 delegable legs → ONE \`${t("agent_delegate")}\` call with route:"spread" (subtask 0 = local seat, subtask 1 = first fleet node — the guaranteed pair). Exactly 1 → \`${t("agent_run")}\`. Hand seats FILES (context_paths / named paths), never a search problem. Read-only subagent legs may also be sent as task subagent_type "${offloadAgent}" (free local seat).`,
    `3. Issue the spread call and the Claude-only task calls in the SAME message — all lanes run concurrently.`,
    `4. Verify the pair landed: read results[].placement / reason (and retried_on); a contract with no output_schema or over-size silently deals local. summary.infrastructure>0 = broken node, not model failure. {deferred:true} is normal — do the leg yourself.`,
    `Tier 1 (mechanical text, single shot): ${t("offload_summarize")} · ${t("offload_classify")} · ${t("offload_extract")} · ${t("offload_triage")}. Vision: ${t("offload_vqa")} · ${t("offload_ocr")} · ${t("offload_transcribe")} · ${t("offload_extract_image")} · ${t("offload_assess_image")} · ${t("offload_video_describe")}. Media: ${t("offload_generate_image")} · ${t("offload_edit_image")} · ${t("offload_inpaint_image")} · ${t("offload_upscale_image")} · ${t("offload_generate_video")} · ${t("offload_generate_audio")} · ${t("offload_generate_svg")} · ${t("offload_run_graph")} · ${t("offload_media")} (generic media entry). Roster/health: ${t("offload_status")} (call first when unsure). Cloud (only remote surface, opt-in): ${t("offload_nim")}.`,
    `Contract checklist for ${t("agent_delegate")}: self-contained goal · context_paths under read_root · output_schema (flat properties map, REQUIRED for a remote seat) · acceptance that tests CONTENT (contains:/regex:/min_items:), never nonempty: alone · route:"spread" for 2+, auto for 1.`,
    `Web research is a harness lane too: \`${t("offload_research")}\` {goal, urls≤12} fetches public pages DELEGATOR-side and the seats digest them (route spread). Search for URLs, then hand them over — "needs the web" is not a reason for a cloud subagent.`,
    `Measured limits: keep judgment, brand/voice and architecture here; offload the READING, keep the DECIDING. A local seat's output is not load-bearing until you spot-check the consequential parts.`,
  ].join("\n");
}

export function taskDescriptionAddendum(offloadAgent: string, mcp: string): string {
  return `\n\nOFFLOAD ROUTE: read-only legs (recon, sweep, digest, extract, inventory, audit over LOCAL files) belong on subagent_type "${offloadAgent}" — a free local seat with the ${mcp}_* harness tools; two or more such legs are better as ONE ${mcp}_agent_delegate route:"spread" call. Keep judgment/web/write legs on the default agent.`;
}

// Tier-1 mode: the primary sees only the four mechanical-text tools, so it must never be told
// to call a tool it cannot see. Everything else is reached by handing a task leg to the offload
// subagent, which holds the whole harness. Shorter than the full text: it is paid every turn.
function tier1ProtocolText(mcp: string, offloadAgent: string): string {
  const t = (name: string) => `${mcp}_${name}`;
  return [
    `# local-offload harness — dispatch (house protocol, always on)`,
    `Free local lanes. UNDER-use is the measured failure mode; use them by default.`,
    `Direct (single-shot mechanical text): ${t("offload_summarize")} · ${t("offload_classify")} · ${t("offload_extract")} · ${t("offload_triage")}.`,
    `Everything else — multi-file recon, digests, audits, extraction over local files, vision, media, web research over given URLs, roster/health — goes to the local offload seat: issue a task call with subagent_type "${offloadAgent}" and hand it NAMED FILES and one bounded question. It holds the whole harness (agent_delegate, agent_run, vision, media, research).`,
    `Several independent read-only legs: issue them as separate "${offloadAgent}" task calls in the SAME message so they run concurrently.`,
    `Keep here: judgment, design, review, writes, commands. A local seat's output is not load-bearing until you spot-check the consequential parts.`,
  ].join("\n");
}
