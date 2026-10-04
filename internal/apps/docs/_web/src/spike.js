import * as Y from "yjs";
export default {
  fetch() {
    const a = new Y.Doc(),
      b = new Y.Doc();
    a.getText("markdown").insert(0, "한국어 🌍\n");
    const first = Y.encodeStateAsUpdate(a);
    Y.applyUpdate(b, first);
    const sv = Y.encodeStateVector(b);
    a.getText("markdown").insert(a.getText("markdown").length, "second");
    const diff = Y.encodeStateAsUpdate(a, sv);
    Y.applyUpdate(b, diff);
    return Response.json({
      text: b.getText("markdown").toString(),
      first: Array.from(first),
      sv: Array.from(sv),
      diff: Array.from(diff),
      encoder: typeof TextEncoder,
      decoder: typeof TextDecoder,
    });
  },
};
