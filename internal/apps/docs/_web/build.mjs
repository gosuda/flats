import { build } from "esbuild";
import { readFile, writeFile, readdir, mkdir } from "node:fs/promises";
import { createHash } from "node:crypto";
import path from "node:path";
import { fileURLToPath } from "node:url";
process.chdir(path.dirname(fileURLToPath(import.meta.url)));
const out = "../dist";
await mkdir(out, { recursive: true });
const options = {
  bundle: true,
  format: "esm",
  minify: true,
  target: "es2022",
  legalComments: "none",
  metafile: true,
};
const client = await build({
  ...options,
  entryPoints: ["src/client.js"],
  outfile: "client.js",
  write: false,
  platform: "browser",
});
const js = client.outputFiles.find((f) => f.path.endsWith(".js")).text,
  css = client.outputFiles.find((f) => f.path.endsWith(".css")).text;
const assets = `export const clientJS=${JSON.stringify(js)};\nexport const clientCSS=${JSON.stringify(css)};\n`;
const server = await build({
  ...options,
  entryPoints: ["src/server.js"],
  outfile: out + "/server.js",
  platform: "neutral",
  conditions: ["browser"],
  external: ["./content.js"],
  plugins: [
    {
      // Keep lib0's public module intact while selecting the host's bounded
      // UTF8 codecs in QuickJS. No TextEncoder/Decoder globals are introduced.
      name: "docs-utf8",
      setup(b) {
        b.onLoad(
          { filter: /node_modules[\\/]lib0[\\/]string\.js$/ },
          async ({ path: file }) => {
            let source = await readFile(file, "utf8");
            const encoder =
              "typeof TextEncoder !== 'undefined' ? new TextEncoder() : null";
            const decoder =
              "typeof TextDecoder === 'undefined' ? null : new TextDecoder('utf-8', { fatal: true, ignoreBOM: true })";
            if (!source.includes(encoder) || !source.includes(decoder))
              throw new Error("lib0 UTF8 codec binding changed");
            source = source
              .replace(
                encoder,
                `globalThis.__flats_docsCodec?.textEncoder || (${encoder})`,
              )
              .replace(
                decoder,
                `globalThis.__flats_docsCodec?.textDecoder || (${decoder})`,
              );
            return { contents: source, loader: "js" };
          },
        );
      },
    },
    {
      name: "assets",
      setup(b) {
        b.onResolve({ filter: /^\.\/assets\.js$/ }, () => ({
          path: "assets",
          namespace: "embedded",
        }));
        b.onLoad({ filter: /.*/, namespace: "embedded" }, () => ({
          contents: assets,
          loader: "js",
        }));
      },
    },
  ],
});
await build({
  ...options,
  entryPoints: ["src/spike.js"],
  outfile: "../testdata/spike.js",
  platform: "neutral",
  conditions: ["browser"],
});
const packages = new Set();
for (const input of [
  ...Object.keys(client.metafile.inputs),
  ...Object.keys(server.metafile.inputs),
]) {
  const m = input.match(/^node_modules\/((?:@[^/]+\/)?[^/]+)/);
  if (m) packages.add(m[1]);
}
let licenses = "Third-party software bundled into Flats Docs\n\n";
for (const name of [...packages].sort()) {
  const dir = "node_modules/" + name,
    pkg = JSON.parse(await readFile(dir + "/package.json", "utf8"));
  const files = (await readdir(dir)).filter((f) =>
    /^(license|licence|copying)([.-]|$)/i.test(f),
  );
  if (!files.length) throw new Error("Missing license text for " + name);
  licenses += `===== ${name} ${pkg.version} (${pkg.license}) =====\n`;
  for (const f of files.sort())
    licenses += (await readFile(dir + "/" + f, "utf8")) + "\n";
  licenses += "\n";
}
await writeFile(out + "/THIRD_PARTY_LICENSES.txt", licenses);
async function walk(dir) {
  const files = [];
  for (const e of await readdir(dir, { withFileTypes: true }))
    if (e.isDirectory()) files.push(...(await walk(dir + "/" + e.name)));
    else if (e.isFile()) files.push(dir + "/" + e.name);
  return files;
}
const inputs = [
    ...(await walk("src")),
    "package.json",
    "package-lock.json",
    "build.mjs",
    ...(await walk(out)).filter((p) => !p.endsWith("/BUILD-INPUTS.sha256")),
  ].sort(),
  hash = createHash("sha256");
for (const p of inputs) {
  hash.update(
    p +
      "\0" +
      createHash("sha256")
        .update(await readFile(p))
        .digest("hex") +
      "\n",
  );
}
await writeFile(out + "/BUILD-INPUTS.sha256", hash.digest("hex") + "\n");
console.log(
  `Client JS ${Buffer.byteLength(js)} bytes; CSS ${Buffer.byteLength(css)} bytes; server (includes client assets) ${(await readFile(out + "/server.js")).length} bytes; ${packages.size} bundled packages.`,
);
