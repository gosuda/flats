// Deploy this file as a server flat. Configure API_BASE and the injected secret API_TOKEN on the host.
// API_BASE is an operator-chosen HTTPS origin, never a visitor-supplied URL.
// API_TOKEN is bound to that origin and to the Authorization header: the host adds the header to each
// fetch to the origin, and this code never holds the token, because it is not in env.
// The response exposes only the upstream `message` field; headers
// and upstream error detail never reach the visitor.
'use strict';

export default {
  async fetch(request, env) {
    const path = new URL(request.url).pathname;
    if (path === "/healthz") return new Response("ok");
    if (path !== "/api/message") return new Response("Not found", { status: 404 });
    if (!env.API_BASE) {
      return new Response("API is not configured", { status: 503 });
    }
    try {
      const upstream = await fetch(env.API_BASE + "/message");
      if (!upstream.ok) return new Response("Upstream unavailable", { status: 502 });
      const data = await upstream.json();
      if (typeof data.message !== "string") throw new Error("Invalid response");
      return Response.json({ message: data.message });
    } catch (_) {
      return new Response("Upstream unavailable", { status: 502 });
    }
  },
};
