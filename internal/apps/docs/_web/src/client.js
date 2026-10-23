import * as Y from "yjs";
import { EditorState, Compartment } from "@codemirror/state";
import { EditorView, keymap, drawSelection } from "@codemirror/view";
import { defaultKeymap, indentWithTab } from "@codemirror/commands";
import { yCollab, yUndoManagerKeymap } from "y-codemirror.next";
import { Provider } from "./provider.js";
import { remoteCursors } from "./remote-cursors.js";
import {
  JoinState,
  browserStore,
  endedNotice,
  refusal,
  viewersOf,
} from "./guest.js";
import {
  fullDialog,
  me,
  nameDialog,
  people,
  readersDialog,
} from "./guest-ui.js";
import { livePreview } from "./live-view.js";
import { createMarkdown, linkRules } from "./markdown.js";
import { editorMarkdown } from "./markdown-inline.js";
import { replaceRendered } from "./diagram-preview.js";
import { coalescedRefresh } from "./render-budget.js";
import { initHistory } from "./history.js";
import { randomUUID } from "./uuid.js";
import {
  onThemeChange,
  redrawDiagrams,
  renderDiagrams,
  destroyDiagram,
  drawOnRequest,
} from "./diagrams.js";
import "./client.css";
const $ = (s) => document.querySelector(s),
  path = document.body.dataset.doc,
  initialReadonly = document.body.dataset.readonly === "true";
const md = createMarkdown(),
  links = linkRules(md);
// The page's replica of the document. A guest page whose editing ended
// starts a new one, without its unsaved changes.
let doc, text, provider;
// Assets and relative links resolve against the Markdown file's own directory.
const resolveURL = (value) => {
  try {
    return new URL(value, new URL("/" + path, location.origin)).href;
  } catch {
    return "";
  }
};
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
// Private links open in Edit, the live-preview editor, and can switch to
// View, the rendered document Public visitors get. Read-only viewers (and
// everyone until the editor exists) get the rendered document only.
let view,
  renderFrame,
  mode = "edit",
  writable = false;
const access = new Compartment();
const refresh = coalescedRefresh(() => {
  replaceRendered($("#content"), md.render(text.toString()), destroyDiagram);
  renderDiagrams($("#content"));
});
function renderNow() {
  cancelAnimationFrame(renderFrame);
  renderFrame = 0;
  refresh.run();
}
function render(_event, transaction) {
  if ($("#content").hidden) return;
  // Remote bursts apply to Yjs immediately, but render only the latest state.
  // A pending local frame already includes those remote edits.
  if (transaction && !transaction.local) {
    if (!renderFrame) refresh.request();
    return;
  }
  refresh.cancel();
  cancelAnimationFrame(renderFrame);
  renderFrame = requestAnimationFrame(renderNow);
}
// Diagrams are drawn in the color scheme's theme, in both views.
onThemeChange(() =>
  redrawDiagrams(document).then(() => view?.requestMeasure()),
);
// The rendered document is not kept current while hidden, so it is filled
// once, before it is shown, rather than a frame later.
function showRendered(rendered) {
  const stale = rendered && $("#content").hidden;
  $("#editor").hidden = rendered;
  $("#content").hidden = !rendered;
  if (!rendered) {
    refresh.cancel();
    cancelAnimationFrame(renderFrame);
    renderFrame = 0;
  } else if (stale) renderNow();
  else render();
}
// Private HTML includes the Edit | View group; a page first served
// read-only gets one when a later connection is writable.
function modesGroup() {
  let group = $("#modes");
  if (!group) {
    group = document.createElement("div");
    group.id = "modes";
    group.setAttribute("role", "group");
    group.setAttribute("aria-label", "Document mode");
    for (const [value, label] of [
      ["edit", "Edit"],
      ["view", "View"],
    ]) {
      const b = document.createElement("button");
      b.type = "button";
      b.dataset.mode = value;
      b.textContent = label;
      group.append(b);
    }
    $("header h1").after(group);
  }
  return group;
}
let viewingRevision = false;
function setMode(next) {
  mode = next;
  for (const b of document.querySelectorAll("#modes [data-mode]"))
    b.setAttribute("aria-pressed", String(b.dataset.mode === mode));
  if (viewingRevision) return;
  showRendered(!view || !writable || mode === "view");
  if (view && writable && mode === "edit") view.requestMeasure();
}
document.addEventListener("click", (event) => {
  const b = event.target.closest?.("#modes [data-mode]");
  if (b) setMode(b.dataset.mode);
});
// Collaborators on Private routes get a random name instead of a join
// prompt; it is kept so the same browser shows up under the same name after
// a reload. Guests see a member under this name (hello.nick).
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
let name = pick(adjectives) + " " + pick(animals),
  // A random id of this browser groups its edits into revisions.
  editor = randomUUID().replaceAll("-", "");
try {
  const saved = localStorage.getItem("flats-docs-name");
  if (saved) name = saved;
  else localStorage.setItem("flats-docs-name", name);
  const id = localStorage.getItem("flats-docs-editor");
  if (id && /^[A-Za-z0-9_-]{8,64}$/.test(id)) editor = id;
  else localStorage.setItem("flats-docs-editor", editor);
} catch {}
// download saves local text, by default the live replica's.
function download(markdown = text.toString()) {
  const a = document.createElement("a"),
    url = URL.createObjectURL(
      new Blob([markdown], { type: "text/markdown;charset=utf-8" }),
    );
  a.href = url;
  a.download = path.split("/").pop();
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
// notice shows an interruption above the document: a bold reason when
// there is one, the details and its actions.
function notice({ reason = "", detail }, actions) {
  const n = $("#notice");
  n.hidden = false;
  n.replaceChildren();
  if (reason) {
    const strong = document.createElement("strong");
    strong.textContent = reason;
    n.append(strong, " ");
  }
  n.append(detail + " ");
  for (const [label, action] of actions) {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = label;
    b.addEventListener("click", action);
    n.append(b);
  }
}
function readOnlyEditor() {
  view?.dispatch({
    effects: access.reconfigure(EditorState.readOnly.of(true)),
  });
}
function stopped(message) {
  notice({ detail: message }, [
    ["Download local text", () => download()],
    ["Reload", () => location.reload()],
  ]);
  readOnlyEditor();
}
// The history panel views a revision in place of the live surface; the
// editor stays connected meanwhile.
const history = initHistory({
  path,
  name: () => name,
  liveText: () => text.toString(),
  onView() {
    viewingRevision = true;
    $("#editor").hidden = true;
    $("#content").hidden = true;
  },
  onLive() {
    viewingRevision = false;
    $("#content").hidden = true;
    setMode(mode);
  },
  status(message) {
    $("#status").textContent = message;
    $("#status").dataset.state = "Saved";
    provider.catchUp();
  },
});

// Public pages (docs-public-edit.md): whether this page reads, edits as a
// guest or is a member decides each hello and the popup (guest.js).
const join = new JoinState(browserStore());
let viewers = { count: 0, labels: [] },
  flat = "",
  popup = null,
  // Local text when this page stopped editing as a guest, with its unsaved
  // changes, for Download local text.
  endedText = null,
  endedUnsaved = 0,
  endedShown = false,
  readers = false;
const flatName = () => flat || "these documents";

function createEditor() {
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
        editorMarkdown(),
        livePreview(links, resolveURL, (source) => md.render(source)),
        access.of(EditorState.readOnly.of(false)),
        keymap.of([...yUndoManagerKeymap, ...defaultKeymap, indentWithTab]),
        // Without awareness yCollab leaves cursors to remoteCursors, which
        // draws the guest badge.
        yCollab(text, null, { undoManager }),
        remoteCursors(text, provider.awareness),
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

function startReplica() {
  doc = new Y.Doc();
  text = doc.getText("markdown");
  text.observe(render);
  provider = new Provider(doc, path, {
    readonly: initialReadonly,
    name,
    editor,
    hello: () => join.hello(name),
    onState: (label) => {
      $("#status").textContent = label;
      $("#status").dataset.state = label;
    },
    onStop: stopped,
    onAccess,
    onWelcome,
    onLevel(m) {
      if (m.access === "edit") drawOnRequest();
      join.level(m);
      header();
      sync();
    },
    onViewers(m) {
      viewers = viewersOf(m);
      showPresence();
    },
    onError,
    // Only Public pages tell a visitor the readers are too many; members
    // reconnect as before.
    onCapacity() {
      if (!initialReadonly) return false;
      tooManyReaders();
      return true;
    },
    onUnreachable,
  });
  provider.awareness.on("change", showPresence);
}

// onAccess sees each welcome before its text: the diagram policy must hold
// before any of that text renders (TECH 3.10).
function onAccess(m) {
  const wasGuest = join.kind === "guest";
  if (m.guest_text === true || m.access === "edit") drawOnRequest();
  viewers = { count: 0, labels: [] };
  const next = join.welcome(m);
  // A guest page that may not edit on: the owner turned off guest editing
  // while it was away.
  if (wasGuest && join.kind !== "guest") {
    leaveGuestEditing();
    return false;
  }
  if (next === "reconnect") {
    provider.reconnect();
    return false;
  }
  provider.paced = join.kind === "guest";
  return true;
}

function onWelcome(m) {
  // Each welcome carries this connection's access. A read-only one hides
  // the controls and shows the rendered document; a later writable one
  // restores them, the chosen mode and editing. Guests edit without
  // History (rule 13a).
  writable = !m.readonly && (join.kind === "member" || join.kind === "guest");
  if (!view && writable) createEditor();
  const group = writable ? modesGroup() : $("#modes");
  if (group) group.hidden = !writable;
  history.enable(writable && join.kind === "member");
  view?.dispatch({
    effects: access.reconfigure(
      EditorState.readOnly.of(!writable || provider.model.stopped),
    ),
  });
  setMode(mode);
  header();
  sync();
  showPresence();
  if (join.kind === "guest" && endedShown) {
    // Joined again: the "turned off" notice is out of date, but text that
    // was not saved then stays downloadable.
    endedShown = false;
    if (endedUnsaved)
      notice(
        {
          reason: "",
          detail: "Changes from before guest editing ended weren't saved.",
        },
        [["Download local text", () => download(endedText)]],
      );
    else $("#notice").hidden = true;
  }
}

function onError(m) {
  // A page that was editing as a guest leaves its session for these.
  const session = join.kind === "guest";
  if (m.code === "editing_ended") {
    join.ended();
    leaveGuestEditing();
    return true;
  }
  if (m.code === "full") {
    provider.pause();
    join.full();
    fullDialog({
      flat: flatName(),
      read() {
        join.read();
        if (session)
          leaveGuestEditing("This document is full, so editing stopped.");
        else provider.reconnect();
        header();
      },
      retry: () => provider.reconnect(),
    });
    return true;
  }
  if (m.code === "name_invalid") {
    join.invalidName();
    if (session)
      leaveGuestEditing("This name can't be used, so editing stopped.");
    else provider.reconnect();
    header();
    sync();
    return true;
  }
  if (!session) return false;
  // A refused guest update stops editing on this page (rule 22): later
  // changes build on it, so none of them can be saved either.
  provider.halt();
  readOnlyEditor();
  header();
  notice(refusal(m.code), [
    ["Download local text", () => download()],
    [
      "Rejoin",
      () => {
        join.rejoin();
        location.reload();
      },
    ],
  ]);
  return true;
}

// leaveGuestEditing: this page no longer edits as a guest, by default
// because the owner turned off guest editing (rule 29). Unsaved changes stay
// available to download; the page reads on, live, from a new replica
// without them.
function leaveGuestEditing(reason) {
  const unsaved = provider.model.items.size;
  endedText = text.toString();
  endedUnsaved += unsaved;
  provider.destroy();
  text.unobserve(render);
  view?.destroy();
  view = null;
  $("#editor").replaceChildren();
  doc.destroy();
  writable = false;
  if ($("#modes")) $("#modes").hidden = true;
  startReplica();
  setMode(mode);
  header();
  sync();
  showPresence();
  endedShown = true;
  notice(
    endedNotice(unsaved, reason),
    endedUnsaved ? [["Download local text", () => download(endedText)]] : [],
  );
}

// A refused handshake looks like a network failure. When the page's own
// server still answers, the Public side is full: say so instead of
// retrying for ever. When it answers with an error, the documents are no
// longer public (rule 30): stop, and keep a guest's unsaved text to
// download. A network error proves neither and keeps retrying.
function onUnreachable(failures) {
  if (!initialReadonly || failures < 3 || readers || withdrawn) return false;
  const p = provider;
  fetch("/_docs/api/documents", { cache: "no-store" }).then(
    (r) => {
      if (p !== provider || p.model.connected || p.paused) return;
      p.pause();
      if (r.ok) tooManyReaders();
      else noLongerPublic();
    },
    () => {},
  );
  return false;
}

function noLongerPublic() {
  withdrawn = true;
  readOnlyEditor();
  const unsaved = join.kind === "guest" && provider.model.unsynced();
  notice(
    {
      reason: "These documents are no longer public.",
      detail: unsaved
        ? "Your latest changes weren't saved. Download them to keep your text."
        : "The page is not updating.",
    },
    unsaved ? [["Download local text", () => download()]] : [],
  );
}

let withdrawn = false;

function tooManyReaders() {
  if (readers) return;
  readers = true;
  readersDialog({
    flat: flatName(),
    retry() {
      readers = false;
      provider.reconnect();
    },
  });
}

// sync opens or closes the name popup to match the join state.
function sync() {
  const prompt = join.prompt;
  if (!prompt) {
    const open = popup;
    popup = null;
    open?.close("closed");
    return;
  }
  if (popup) return;
  const d = (popup = nameDialog({
    prompt,
    flat: flatName(),
    name: join.name(),
    submit(value) {
      if (join.submit(value) === "reconnect") provider.reconnect();
      header();
    },
    dismiss() {
      join.dismiss();
      header();
    },
  }));
  d.addEventListener("close", () => {
    if (popup === d) popup = null;
  });
}

function header() {
  let node = $("#me");
  if (!node) {
    node = document.createElement("span");
    node.id = "me";
    $("#presence").before(node);
  }
  me(node, {
    kind: join.kind,
    label: join.label,
    name: join.name(),
    canJoin: join.canJoin() && !provider.model.stopped,
    join() {
      if (join.openJoin() === "prompt") sync();
    },
    change() {
      if (join.openChange() === "prompt") sync();
    },
  });
}

// A reader takes no part in presence: it shows nobody, not even itself.
function showPresence() {
  people(
    $("#presence"),
    writable ? provider.awareness.getStates() : new Map(),
    writable ? viewers : null,
  );
}

startReplica();
fetch("/_docs/api/documents")
  .then((r) => r.json())
  .then((data) => {
    flat =
      data.title ||
      data.documents.find((d) => d.path === data.entry)?.title ||
      "";
    if (popup && join.prompt?.mode === "join")
      popup.querySelector("h2").textContent = `Join ${flatName()}`;
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
// Warn before a reload or close loses local text, also after sync stopped
// (for example over the disk quota), which keeps that text for download.
window.addEventListener("beforeunload", (event) => {
  if (provider.model.unsynced() || endedUnsaved) {
    event.preventDefault();
    event.returnValue = "";
  }
});
window.addEventListener("pagehide", () => {
  refresh.cancel();
  provider.destroy();
});
