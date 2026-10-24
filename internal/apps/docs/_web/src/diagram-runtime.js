// Size caps for a diagram drawn on request (docs-public-edit-TECH 3.10):
// what one click on a diagram anyone may have written can cost.
export const DIAGRAM_CAPS = Object.freeze({
  maxTextSize: 20000,
  maxEdges: 500,
});

// tooLarge is the reason a capped diagram is not drawn, or "".
export function tooLarge(source, error = "") {
  if (source.length > DIAGRAM_CAPS.maxTextSize)
    return `Too large to draw: over ${DIAGRAM_CAPS.maxTextSize.toLocaleString("en-US")} characters.`;
  if (/^Edge limit exceeded/.test(error))
    return `Too large to draw: over ${DIAGRAM_CAPS.maxEdges} connections.`;
  return "";
}

// Serialize Mermaid's global configuration, share in-flight work, and bound
// retained strings by bytes as well as entry count. A capped request runs
// with DIAGRAM_CAPS.
export function createDiagramRuntime(
  load,
  freshID,
  { maxEntries = 64, maxBytes = 4 * 1024 * 1024 } = {},
) {
  const cache = new Map(),
    pending = new Map();
  let queue = Promise.resolve(),
    initialized = "",
    bytes = 0;
  return {
    request(source, theme, current = () => true, capped = false) {
      const config = theme + (capped ? " capped" : ""),
        key = config + "\n" + source;
      if (cache.has(key)) {
        const entry = cache.get(key);
        cache.delete(key);
        cache.set(key, entry);
        return Promise.resolve(entry.result);
      }
      if (pending.has(key)) {
        const entry = pending.get(key);
        entry.consumers.push(current);
        return entry.promise;
      }
      const entry = { consumers: [current] };
      const active = () => entry.consumers.some((consumer) => consumer());
      entry.promise = queue
        .then(async () => {
          if (!active()) return null;
          const mermaid = await load();
          if (!active()) return null;
          if (initialized !== config) {
            mermaid.initialize({
              startOnLoad: false,
              securityLevel: "strict",
              suppressErrorRendering: true,
              theme,
              ...(capped ? DIAGRAM_CAPS : {}),
            });
            initialized = config;
          }
          const id = freshID();
          let result;
          try {
            const { svg } = await mermaid.render(id, source);
            result = { svg, id };
          } catch (error) {
            result = {
              error: String(error?.message || error || "Invalid diagram")
                .split("\n")
                .slice(0, 4)
                .join("\n")
                .slice(0, 500),
            };
          }
          const size = 2 * (key.length + (result.svg || result.error).length);
          if (size <= maxBytes) {
            cache.set(key, { result, size });
            bytes += size;
            while (cache.size > maxEntries || bytes > maxBytes) {
              const oldest = cache.keys().next().value;
              bytes -= cache.get(oldest).size;
              cache.delete(oldest);
            }
          }
          return result;
        })
        .finally(() => pending.delete(key));
      pending.set(key, entry);
      queue = entry.promise.catch(() => {});
      return entry.promise;
    },
  };
}
