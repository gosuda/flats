import { build } from "esbuild";
import { readFile, writeFile, readdir, mkdir } from "node:fs/promises";
import { createHash } from "node:crypto";
import path from "node:path";
import { gzipSync } from "node:zlib";
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
// Mermaid is served as its own module from a content-hashed path, so it is
// fetched only for documents with diagrams and cached by browsers. It is
// wrapped as a string module the worker imports when the path is requested.
const mermaid = await build({
  ...options,
  stdin: { contents: 'export { default } from "mermaid";', resolveDir: "." },
  write: false,
  platform: "browser",
});
const mermaidJS = mermaid.outputFiles[0].text,
  mermaidURL = `/_docs/assets/mermaid-${createHash("sha256").update(mermaidJS).digest("hex").slice(0, 16)}.js`,
  mermaidDefine = { FLATS_MERMAID_URL: JSON.stringify(mermaidURL) };
const mermaidGzip = gzipSync(mermaidJS, { level: 9 });
// The deployed QuickJS host codec accepts at most 1.5 MiB of decoded bytes.
if (mermaidGzip.length > 1536 * 1024)
  throw new Error("Compressed Mermaid exceeds the host codec limit");
await writeFile(
  out + "/mermaid-asset.js",
  `export default ${JSON.stringify(mermaidJS)};\nexport const gzip = ${JSON.stringify(mermaidGzip.toString("base64"))};\n`,
);
const client = await build({
  ...options,
  define: mermaidDefine,
  entryPoints: ["src/client.js"],
  outfile: "client.js",
  write: false,
  platform: "browser",
});
const js = client.outputFiles.find((f) => f.path.endsWith(".js")).text,
  css = client.outputFiles.find((f) => f.path.endsWith(".css")).text;
const assets = `export const clientJS=${JSON.stringify(js)};\nexport const clientCSS=${JSON.stringify(css)};\n`;
const docsUTF8 = {
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
};
const server = await build({
  ...options,
  entryPoints: ["src/server.js"],
  outfile: out + "/server.js",
  platform: "neutral",
  conditions: ["browser"],
  define: mermaidDefine,
  external: ["./content.js", "./mermaid-asset.js"],
  plugins: [
    docsUTF8,
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
const spike = await build({
  ...options,
  entryPoints: ["src/spike.js"],
  outfile: "../testdata/spike.js",
  platform: "neutral",
  conditions: ["browser"],
});
const clientEdits = await build({
  ...options,
  entryPoints: ["src/client-edits.js"],
  outfile: "../testdata/client-edits.js",
  plugins: [docsUTF8],
  platform: "neutral",
  conditions: ["browser"],
});
// The diagram viewer imports the design app's wheel.js. Files outside src/ and
// node_modules/ that a bundle includes must be hashed build inputs, or editing
// one without a rebuild would pass TestBuildInputs. The stdin input and the
// embedded: namespace of the assets plugin are not files.
const sharedInputs = ["../../../design/web/wheel.js"];
for (const [name, result] of Object.entries({
  mermaid,
  client,
  server,
  spike,
  clientEdits,
})) {
  for (const input of Object.keys(result.metafile.inputs)) {
    if (/^(src|node_modules)\//.test(input)) continue;
    if (input === "<stdin>" || input.startsWith("embedded:")) continue;
    if (!sharedInputs.includes(input))
      throw new Error(
        `The ${name} bundle includes ${input}, which is not a hashed build input. Add it to sharedInputs here and to TestBuildInputs.`,
      );
  }
}
const packages = new Set();
for (const input of [
  ...Object.keys(mermaid.metafile.inputs),
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
  // A few packages (fastdom) carry their license only as a README section.
  let readmeLicense = "";
  if (!files.length) {
    const readme = await readFile(dir + "/README.md", "utf8").catch(() => ""),
      m = readme.match(/^##+ *Licen[cs]e *\n([\s\S]*?)(?=^#|(?![\s\S]))/m);
    if (!m || !/permission is hereby granted/i.test(m[1]))
      throw new Error("Missing license text for " + name);
    readmeLicense = m[1].trim() + "\n";
  }
  licenses += `===== ${name} ${pkg.version} (${pkg.license}) =====\n`;
  for (const f of files.sort())
    licenses += (await readFile(dir + "/" + f, "utf8")) + "\n";
  if (readmeLicense) licenses += readmeLicense + "\n";
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
    "../testdata/spike.js",
    "../testdata/client-edits.js",
    ...sharedInputs,
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
  `Client JS ${Buffer.byteLength(js)} bytes; Mermaid ${Buffer.byteLength(mermaidJS)} bytes (${mermaidGzip.length} gzip); CSS ${Buffer.byteLength(css)} bytes; server (includes client assets) ${(await readFile(out + "/server.js")).length} bytes; ${packages.size} bundled packages.`,
);
