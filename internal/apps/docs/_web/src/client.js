import * as Y from "yjs";
import { EditorState, Compartment } from "@codemirror/state";
import { EditorView, keymap, drawSelection } from "@codemirror/view";
import { defaultKeymap, indentWithTab } from "@codemirror/commands";
import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import { yCollab, yUndoManagerKeymap } from "y-codemirror.next";
import MarkdownIt from "markdown-it";
import { Provider } from "./provider.js";
import { livePreview } from "./live-view.js";
import "./client.css";
const $ = (s) => document.querySelector(s),
  path = document.body.dataset.doc,
  initialReadonly = document.body.dataset.readonly === "true";
const doc = new Y.Doc(),
  text = doc.getText("markdown"),
  md = new MarkdownIt({ html: false, linkify: true, typographer: false });
const defaultValidate = md.validateLink,
  unsafeScheme = /^\s*(?:javascript|vbscript|file|data):/i;
md.validateLink = (link) => defaultValidate(link) && !unsafeScheme.test(link);
// Assets and relative links resolve against the Markdown file's own directory.
const resolveURL = (value) => {
  try {
    return new URL(value, new URL("/" + path, location.origin)).href;
  } catch {
    return "";
  }
};
// The editor applies the same link rules as the rendered view.
const safeURL = (value) =>
  value && md.validateLink(md.normalizeLink(value)) ? resolveURL(value) : "";
const defaultLink =
  md.renderer.rules.link_open ||
  ((tokens, i, options, env, self) => self.renderToken(tokens, i, options));
md.renderer.rules.link_open = (tokens, i, options, env, self) => {
  const href = tokens[i].attrGet("href");
  if (href) tokens[i].attrSet("href", resolveURL(href));
  tokens[i].attrSet("rel", "noopener noreferrer");
  return defaultLink(tokens, i, options, env, self);
};
const defaultImage = md.renderer.rules.image;
md.renderer.rules.image = (tokens, i, options, env, self) => {
  const value = tokens[i].attrGet("src");
  tokens[i].attrSet("src", resolveURL(value));
  tokens[i].attrSet("loading", "lazy");
  return defaultImage(tokens, i, options, env, self);
};
// One view: editors get the live-preview editor, read-only viewers (and
// everyone until the editor exists) get the rendered document.
let view, renderFrame;
const access = new Compartment();
function render() {
  if ($("#content").hidden) return;
  cancelAnimationFrame(renderFrame);
  renderFrame = requestAnimationFrame(() => {
    $("#content").innerHTML = md.render(text.toString());
  });
}
text.observe(render);
function showRendered(rendered) {
  $("#editor").hidden = rendered;
  $("#content").hidden = !rendered;
  render();
}
// Collaborators get a random name instead of a join prompt; it is kept so
// the same browser shows up under the same name after a reload.
const adjectives = [
    "Amber",
    "Brave",
    "Calm",
    "Clever",
    "Gentle",
    "Happy",
    "Kind",
    "Lucky",
    "Merry",
    "Quiet",
    "Swift",
    "Witty",
  ],
  animals = [
    "Badger",
    "Crane",
    "Dolphin",
    "Falcon",
    "Fox",
    "Heron",
    "Koala",
    "Lynx",
    "Otter",
    "Owl",
    "Panda",
    "Robin",
  ],
  pick = (list) => list[Math.floor(Math.random() * list.length)];
let name = pick(adjectives) + " " + pick(animals);
try {
  const saved = localStorage.getItem("flats-docs-name");
  if (saved) name = saved;
  else localStorage.setItem("flats-docs-name", name);
} catch {}
function download() {
  const a = document.createElement("a"),
    url = URL.createObjectURL(
      new Blob([text.toString()], { type: "text/markdown;charset=utf-8" }),
    );
  a.href = url;
  a.download = path.split("/").pop();
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
function stopped(message) {
  $("#notice").hidden = false;
  $("#notice").replaceChildren(document.createTextNode(message + " "));
  const b = document.createElement("button");
  b.textContent = "Download local text";
  b.addEventListener("click", download);
  const r = document.createElement("button");
  r.textContent = "Reload";
  r.addEventListener("click", () => location.reload());
  $("#notice").append(b, r);
  if (view)
    view.dispatch({
      effects: access.reconfigure(EditorState.readOnly.of(true)),
    });
}
function showConflicts(conflicts) {
  if (initialReadonly || !conflicts?.length) return;
  const el = $("#conflict-notice");
  el.hidden = false;
  el.replaceChildren(
    document.createTextNode(
      "Publication replaced some edits. Previous text is preserved. ",
    ),
  );
  const link = document.createElement("a");
  link.textContent = "View / copy previous text";
  link.href = `/_docs/api/conflict?doc=${encodeURIComponent(path)}&generation=${conflicts[0].generation}&view=1`;
  link.target = "_blank";
  link.rel = "noopener";
  el.append(link);
}
const provider = new Provider(doc, path, {
  readonly: initialReadonly,
  name,
  onState: (label) => {
    $("#status").textContent = label;
    $("#status").dataset.state = label;
  },
  onStop: stopped,
  onConflicts: showConflicts,
  onWelcome: (m) => {
    if (!view && !m.readonly) {
      const undoManager = new Y.UndoManager(text, {
        trackedOrigins: new Set(),
        captureTimeout: 350,
      });
      view = new EditorView({
        parent: $("#editor"),
        state: EditorState.create({
          doc: text.toString(),
          extensions: [
            drawSelection(),
            EditorView.lineWrapping,
            markdown({ base: markdownLanguage }),
            livePreview(safeURL, (source) => md.render(source)),
            access.of(EditorState.readOnly.of(false)),
            keymap.of([...yUndoManagerKeymap, ...defaultKeymap, indentWithTab]),
            yCollab(text, provider.awareness, { undoManager }),
            EditorView.contentAttributes.of({
              "aria-label": "Document",
              spellcheck: "true",
            }),
            EditorView.theme({
              // The page scrolls, not the editor, so the footer follows the
              // document.
              ".cm-scroller": {
                overflow: "visible",
                fontFamily: "inherit",
                lineHeight: "inherit",
              },
              ".cm-content": { padding: "0" },
              ".cm-line": { padding: "0" },
              "&.cm-focused": { outline: "none" },
              ".cm-cursor": { borderLeftColor: "var(--ink)" },
              ".cm-selectionBackground, &.cm-focused .cm-selectionBackground": {
                backgroundColor: "var(--selection)",
              },
            }),
          ],
        }),
      });
    }
    if (m.readonly) {
      view?.dispatch({
        effects: access.reconfigure(EditorState.readOnly.of(true)),
      });
    }
    showRendered(!view || !!m.readonly);
  },
});
function showPresence() {
  const el = $("#presence");
  el.replaceChildren();
  const states = [...provider.awareness.getStates().entries()].filter(
    ([, s]) => s.user,
  );
  for (const [id, s] of states) {
    const span = document.createElement("span");
    span.className = "person";
    span.title =
      s.user.name + (s.user.verified ? " · verified" : " · unverified");
    span.setAttribute("aria-label", span.title);
    span.style.setProperty(
      "--person-color",
      /^#[0-9a-f]{6}$/i.test(s.user.color) ? s.user.color : "#3b65b9",
    );
    const avatar = document.createElement("span");
    avatar.className = "avatar";
    avatar.textContent = s.user.name.slice(0, 1).toUpperCase();
    span.append(avatar, document.createTextNode(s.user.name));
    el.append(span);
  }
}
provider.awareness.on("change", showPresence);
fetch("/_docs/api/documents")
  .then((r) => r.json())
  .then((data) => {
    if (data.documents.length > 1) {
      const nav = $("#documents");
      nav.hidden = false;
      for (const d of data.documents) {
        const a = document.createElement("a");
        a.href = "/" + d.path.split("/").map(encodeURIComponent).join("/");
        a.textContent = d.title;
        if (d.path === path) a.setAttribute("aria-current", "page");
        nav.append(a);
      }
    }
  })
  .catch(() => {});
window.addEventListener("beforeunload", (event) => {
  if (provider.model.items.size) {
    event.preventDefault();
    event.returnValue = "";
  }
});
window.addEventListener("pagehide", () => provider.destroy());
