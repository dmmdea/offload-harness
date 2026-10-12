// test-isolation.mjs - the environment a render test hands a runner it spawns. Test support only; never
// imported by a runner.
//
// A runner that reaches withGpuSlot (gpu-lock.mjs) frees the machine's llama-swap before it starts: it
// lists the models, drains every seat that is not in the memory stack and sends a real unload. Two things
// of the PARENT's environment reach it unless the test closes them:
//
//   LLAMA_SWAP_API   names the llama-swap to drain (default: the machine's own, on loopback). A test that
//                    forgets it drains and unloads the seat that is serving someone, and a seat that is
//                    still starting holds the drain's probe for 3 x 5 s, so the test also runs long.
//   GPU_LEASE_*      a test that runs under `gpu reserve` (the suite does, on the gate hosts) inherits that
//                    lease: the runner would fence against it, claim its once-per-lease unload marker and
//                    be handed its unload list, all in the lease of the run that started the suite.
//
// isolatedRunnerEnv gives the child neither: LLAMA_SWAP_* is replaced by a loopback port nothing listens
// on (a refused connection is an answer the drain handles at once), and every GPU_LEASE_* variable is
// dropped. A test that WANTS a lease or a llama-swap passes it in `extra`, where it is explicit.
// test-isolation.test.mjs fails any render test that spawns a runner without this (or the same two
// closings written out).
export const DEAD_LLAMA_SWAP = "http://127.0.0.1:9";

export function isolatedRunnerEnv(extra = {}, base = process.env) {
  const env = {};
  for (const [k, v] of Object.entries(base)) {
    if (/^(GPU_LEASE_|LLAMA_SWAP)/i.test(k)) continue;
    env[k] = v;
  }
  env.LLAMA_SWAP_API = DEAD_LLAMA_SWAP;
  return { ...env, ...extra };
}
