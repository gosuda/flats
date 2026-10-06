import * as Y from "yjs";
import { writeFile } from "node:fs/promises";
const encode = (u) => Buffer.from(u).toString("base64");
const seed = new Y.Doc();
seed.clientID = 1;
seed.getText("markdown").insert(0, "# Shared\n\nBase line\nAgent line\n");
const a = new Y.Doc(),
  b = new Y.Doc();
a.clientID = 101;
b.clientID = 102;
for (const d of [a, b]) Y.applyUpdate(d, Y.encodeStateAsUpdate(seed));
const updates = [];
a.on("update", (u) => updates.push({ id: "a", u: encode(u) }));
b.on("update", (u) => updates.push({ id: "b", u: encode(u) }));
a.getText("markdown").insert(10, "Person 한국어 🙂\n");
const peer = new Y.Doc();
peer.clientID = 102;
peer.getText("markdown").insert(0, "Peer 🌍\n");
updates[1] = { id: "b", u: encode(Y.encodeStateAsUpdate(peer)) };
const chain = [];
const edits = new Y.Doc();
edits.clientID = 103;
for (let i = 0; i < 70; i++) {
  const sv = Y.encodeStateVector(edits);
  edits
    .getText("markdown")
    .insert(edits.getText("markdown").length, String(i) + ",");
  chain.push({
    id: "compact-" + i,
    u: encode(Y.encodeStateAsUpdate(edits, sv)),
  });
}
// Inserts independent of the server seed work with any server's randomized seed id.
const independent = new Y.Doc();
independent.clientID = 104;
independent.getText("markdown").insert(0, "Person 한국어 🙂\n");
await writeFile(
  new URL("../testdata/updates.json", import.meta.url),
  JSON.stringify(
    {
      seed: encode(Y.encodeStateAsUpdate(seed)),
      updates,
      independent: encode(Y.encodeStateAsUpdate(independent)),
      compact: chain,
    },
    null,
    2,
  ) + "\n",
);
