// Test-only Yjs client: applies the live welcome state and generates real edits.
// Bundled into testdata so Go tests need neither Node nor node_modules.
import * as Y from "yjs";
export default {
  async fetch(request) {
    const { u, count } = await request.json();
    const codec = globalThis.__flats_docsCodec;
    const doc = new Y.Doc();
    doc.clientID = 900;
    try {
      Y.applyUpdate(doc, codec.decode(u));
      const result = [];
      for (let i = 0; i < count; i++) {
        const sv = Y.encodeStateVector(doc);
        doc.getText("markdown").insert(0, "x");
        result.push({
          id: "soak-" + i,
          u: codec.encode(Y.encodeStateAsUpdate(doc, sv)),
        });
      }
      return Response.json(result);
    } finally {
      doc.destroy();
    }
  },
};
