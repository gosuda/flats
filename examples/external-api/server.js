// Deploy this file as a server flat. Configure API_BASE and API_TOKEN on the host.
// API_BASE is an operator-chosen HTTPS origin, never a visitor-supplied URL.
// The response exposes only the upstream `message` field; headers,
// credentials and upstream error detail never reach the visitor.
'use strict';

export default {
  async fetch(request, env) {
    const path = new URL(request.url).pathname;
    if (path === "/healthz") return new Response("ok");
    if (path !== "/api/message") return new Response("Not found", { status: 404 });
    if (!env.API_BASE || !env.API_TOKEN) {
      return new Response("API is not configured", { status: 503 });
    }
    try {
      const upstream = await fetch(env.API_BASE + "/message", {
        headers: { authorization: "Bearer " + env.API_TOKEN },
      });
      if (!upstream.ok) return new Response("Upstream unavailable", { status: 502 });
      const data = await upstream.json();
      if (typeof data.message !== "string") throw new Error("Invalid response");
      return Response.json({ message: data.message });
    } catch (_) {
      return new Response("Upstream unavailable", { status: 502 });
    }
  },
};
