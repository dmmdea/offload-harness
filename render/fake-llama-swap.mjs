// fake-llama-swap.mjs - a tiny llama-swap on a loopback port for the render tests. Test support only; never
// imported by a runner.
//
// It answers the routes the render helper's drain and unload use (gpu-lock.mjs freeLlamaSwap /
// quiesceLlamaSwap), with the per-seat behaviour a test asks for, and records every request so a test can
// say what was touched and what was not:
//
//   GET  /v1/models               every seat
//   GET  /running                 the seats that have a state (a seat given `state: null` is unlisted)
//   GET  /upstream/<id>/slots     the seat's `slots`: an array (answered as JSON), "hang" (accepted and never
//                                 answered, the way llama-swap holds a request for a seat that is still
//                                 loading), or a function of the call number returning either
//   GET  /upstream/<id>/metrics   404 (no exposition)
//   POST /api/models/unload/<id>  200 OK, and the seat leaves /running
//   GET  /unload                  200 OK, every seat leaves /running
//   anything else                 404
//
// close() destroys every connection a hung request still holds, so a test never leaves a socket open.
import { createServer } from "node:http";

export async function startFakeLlamaSwap({ seats = [] } = {}) {
  const table = new Map(seats.map((s) => [s.id, { state: "ready", slots: [], ...s, calls: 0 }]));
  const seen = [];
  const server = createServer((req, res) => {
    const url = new URL(req.url, "http://fake");
    seen.push(`${req.method} ${url.pathname}`);
    const json = (body, status = 200) => {
      res.writeHead(status, { "content-type": "application/json" });
      res.end(JSON.stringify(body));
    };
    if (req.method === "GET" && url.pathname === "/v1/models") return json({ data: [...table.keys()].map((id) => ({ id })) });
    if (req.method === "GET" && url.pathname === "/running") {
      return json({ running: [...table.entries()].filter(([, s]) => s.state).map(([id, s]) => ({ model: id, state: s.state })) });
    }
    const slots = /^\/upstream\/([^/]+)\/slots$/.exec(url.pathname);
    if (req.method === "GET" && slots) {
      const seat = table.get(decodeURIComponent(slots[1]));
      if (!seat) { res.writeHead(404); res.end(); return; }
      seat.calls++;
      const answer = typeof seat.slots === "function" ? seat.slots(seat.calls) : seat.slots;
      if (answer === "hang") return; // held open until close()
      return json(answer);
    }
    const unload = /^\/api\/models\/unload\/([^/]+)$/.exec(url.pathname);
    if (req.method === "POST" && unload) {
      const seat = table.get(decodeURIComponent(unload[1]));
      if (seat) seat.state = null;
      res.writeHead(200); res.end("OK");
      return;
    }
    if (req.method === "GET" && url.pathname === "/unload") {
      for (const s of table.values()) s.state = null;
      res.writeHead(200); res.end("OK");
      return;
    }
    res.writeHead(404); res.end();
  });
  await new Promise((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  return {
    url: `http://127.0.0.1:${server.address().port}`,
    seen,
    // how many requests so far began with this "METHOD /path" prefix
    count: (prefix) => seen.filter((s) => s.startsWith(prefix)).length,
    seat: (id) => table.get(id),
    close: () => new Promise((resolve) => {
      server.closeAllConnections();
      server.close(() => resolve());
    }),
  };
}
