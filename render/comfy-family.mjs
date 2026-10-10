// comfy-family.mjs — a kept instance that last ran another family is freed before its first job.
//
// THE INCIDENT (2026-10-09, the reference 3-card Windows box, 127.7 GiB physical). Two ComfyUI media
// lanes streaming bf16 weights ran at once and committed memory reached 162.9 GiB; one keyed (per-card)
// instance held 57 GiB private. The PROBABLE cause is that it still cached a qwen-image model from an
// earlier lease next to the krea2 model of the current one: reconstructed from the code and the surviving
// logs, not observed (the chain cannot be re-read, below). The session that handled the incident reported
// that POSTing ComfyUI's own /free {"unload_models":true, "free_memory":true} released 52 GiB of it between
// prompts without killing a job; no record of that reading survives in this repository.
//
// WHAT THIS DOES NOT EXPLAIN. A ComfyUI process launched fresh on 2026-10-10, about seven minutes before it
// was read (a session's reading on the reference box, not recorded in this repository), held the same 57.7 GiB private as the
// earlier one. So a single lane may be that large without any
// long cache history, this fix may account for only part of the 57 GiB, and the footprint of ONE lane is
// unmeasured: the S1 acceptance runs record it (docs/systems/gpu-lease.md, "Known limits").
//
// HOW AN INSTANCE COMES TO HOLD TWO FAMILIES (read from the code and the instances' own logs; the
// chain of the 57 GiB instance itself cannot be re-read, only four rotated console logs per card
// survive and none of them serves two families). ComfyUI holds the models it loaded in host memory
// between prompts ("Using RAM pressure cache", "Model Krea2 prepared for dynamic VRAM loading.
// 24449MB Staged"): they leave only when told to, or under pressure of ComfyUI's own measuring. The
// harness told it at the end of every runner (gpu-lock.mjs cleanup), and that is not enough, in the
// ways the code allows:
//   1. A runner killed before its `finally` (the pipeline's timeout kills the whole process tree, a
//      lost lease kills the wrapped command, a crash) never sends it, and gpugen's belt-and-braces
//      /free waits one second, which a ComfyUI in the middle of a prompt does not always answer in.
//      The instance is KEPT (detached, comfy-lifecycle.mjs), so it outlives the runner and keeps what
//      it loaded.
//   2. The proof the lease holder needs before it stops a kept instance at release is an HTTP round
//      trip to that very instance (internal/comfyinst) with a 3 second timeout, and the qwen-image
//      prompts in those logs run 550 to 585 seconds. An instance that answers late reads "did not
//      answer ... left running": it outlives its lease with its models.
//   3. The next lease on that card REUSES the surviving instance (the keyed launch marker proves it is
//      ours; the epoch is re-stamped) and loads ITS family next to the one already cached.
//   4. run-graph ran an arbitrary graph on an instance that was already up and then "left it entirely
//      alone", whatever the graph had loaded.
//
// THE FIX, here: the launch marker remembers whose weights the instance may still hold (`lastFamily`);
// a runner that finds a different family there frees the instance BEFORE its first job, and frees it
// again at the end, recording that it holds nothing. internal/comfyinst proves slow instances with
// retries so closing the lease usually stops them too; this is the guard for the ones that survive.
//
// Dependency-free; the HTTP free and the marker reads are injected for tests.
import { readLaunchOwner, stampLaunchFamily, harnessLaunched, pidAlive as defaultPidAlive } from "./comfy-ownership.mjs";

/** How long to let ComfyUI's worker process a /free before the first prompt follows it. */
export const FAMILY_FREE_SETTLE_MS = 1500;

/**
 * familySignature: the weights a runner is about to load, as one string: the graph family plus the main
 * weights file (its base name, any directory dropped), "krea2:krea2_turbo_bf16.safetensors". The family
 * alone is not enough: a Q5 GGUF and a bf16 safetensors of the same family are two models, and holding
 * both is the same 40 GiB mistake.
 */
export function familySignature(family, weights = "") {
  const f = String(family ?? "").trim().toLowerCase() || "default";
  const w = String(weights ?? "").trim().replace(/\\/g, "/").split("/").pop();
  return w ? `${f}:${w}` : f;
}

/**
 * settleInstanceFamily: called once per runner, after ComfyUI is up and before its first job. If the
 * instance's marker says it may still hold ANOTHER family's weights, free it (awaited) and let the
 * worker process that; then record this family. An instance with no marker is not one this harness
 * launched and is not touched here (the runner's own end-of-run free still reaches it). A free that did
 * not succeed leaves the old family recorded, so the next runner tries again rather than believing the
 * instance clean.
 *
 * `free(api)` resolves to true when ComfyUI acknowledged the /free (gpu-lock.mjs freeComfy).
 */
export async function settleInstanceFamily({
  comfyDir, key = "", api, family, free,
  log = (m) => console.error(m),
  readLaunch = readLaunchOwner, stamp = stampLaunchFamily,
  settleMs = FAMILY_FREE_SETTLE_MS, sleep = (ms) => new Promise((r) => setTimeout(r, ms)),
}) {
  if (!family || !comfyDir) return { freed: false, previous: "" };
  let marker = null;
  try { marker = readLaunch(comfyDir, key); } catch {}
  if (!marker) return { freed: false, previous: "", why: "no launch marker: not an instance this harness launched" };
  const previous = typeof marker.lastFamily === "string" ? marker.lastFamily : "";
  let freed = false;
  if (previous && previous !== family) {
    log(`COMFY-FAMILY-FREE: the instance${key ? ` '${key}'` : ""} on ${api} last ran ${previous}; this job is ${family}. Freeing its models before the first job of a different family`);
    freed = (await free(api)) !== false;
    if (!freed) {
      log(`COMFY-FAMILY-WARN: the free before the first job did not succeed, so the instance may still hold ${previous}'s weights next to ${family}'s; it stays recorded as ${previous} and the next runner will try again`);
      return { freed: false, previous };
    }
    if (settleMs > 0) await sleep(settleMs);
  }
  try { stamp(comfyDir, key, family); } catch (e) {
    log(`COMFY-FAMILY-WARN: could not record ${family} on the launch marker (${e && e.message}); the next runner cannot tell what this instance holds`);
  }
  return { freed, previous };
}

/** releaseInstanceFamily: the instance was just freed, so it holds nothing: clear what it may hold. */
export function releaseInstanceFamily({ comfyDir, key = "", stamp = stampLaunchFamily, log = (m) => console.error(m) }) {
  if (!comfyDir) return;
  try { stamp(comfyDir, key, ""); } catch (e) {
    log(`COMFY-FAMILY-WARN: could not clear the family on the launch marker (${e && e.message})`);
  }
}

/**
 * freeHarnessInstance: free the models of an instance a graph just ran on, but only an instance this
 * harness launched (a live pid whose argv is exactly the marker's, the proof comfy-lifecycle.mjs applies
 * before it reuses or replaces one). run-graph runs an arbitrary graph, so what it loaded is unknown;
 * leaving it behind was the fourth way an instance came to hold two families. A ComfyUI the harness did
 * not start is not ours to unload, and is left alone.
 */
export async function freeHarnessInstance({
  comfyDir, key = "", api, free, systemArgv,
  log = (m) => console.error(m),
  readLaunch = readLaunchOwner, alive = defaultPidAlive, release = releaseInstanceFamily,
}) {
  if (!comfyDir) return { freed: false, why: "no ComfyUI directory" };
  let marker = null;
  try { marker = readLaunch(comfyDir, key); } catch {}
  if (!marker) return { freed: false, why: "no launch marker: not an instance this harness launched" };
  const argv = await systemArgv(api);
  if (!harnessLaunched(marker, argv, alive)) return { freed: false, why: "not shown to be this harness's instance (no live pid with the marker's argv): left alone" };
  const ok = (await free(api)) !== false;
  if (ok) release({ comfyDir, key, log });
  else log(`COMFY-FAMILY-WARN: could not free the models the graph left on the instance${key ? ` '${key}'` : ""} at ${api}`);
  return { freed: ok };
}
