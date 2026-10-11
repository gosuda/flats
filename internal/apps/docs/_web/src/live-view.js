// Turns the descriptors from live.js into CodeMirror decorations.
import {
  Decoration,
  EditorView,
  keymap,
  ViewPlugin,
  WidgetType,
} from "@codemirror/view";
import {
  EditorSelection,
  Prec,
  StateEffect,
  StateField,
} from "@codemirror/state";
import { syntaxTree } from "@codemirror/language";
import {
  activeLines,
  diagramBlocks,
  linkAt,
  liveDecorations,
  referenceDefinitions,
  tableBlocks,
} from "./live.js";
import {
  diagramKeys,
  diagramClosures,
  closedDiagramCursor,
  setDiagramEditing,
  sourceAction,
  updatePreview,
  trackFocusExit,
} from "./diagram-preview.js";
import {
  renderDiagrams,
  updateDiagram,
  destroyDiagram,
  invalidateDiagram,
} from "./diagrams.js";

class Bullet extends WidgetType {
  eq() {
    return true;
  }
  toDOM() {
    const span = document.createElement("span");
    span.className = "cm-bullet";
    span.textContent = "•";
    return span;
  }
}

// Shows a label in place of syntax: a list number or a decoded entity.
class Label extends WidgetType {
  constructor(text) {
    super();
    this.text = text;
  }
  eq(other) {
    return other.text === this.text;
  }
  toDOM() {
    const span = document.createElement("span");
    span.textContent = this.text;
    return span;
  }
}

class Rule extends WidgetType {
  eq() {
    return true;
  }
  toDOM() {
    const span = document.createElement("span");
    span.className = "cm-rule";
    span.setAttribute("aria-hidden", "true");
    return span;
  }
}

class ImageWidget extends WidgetType {
  constructor(src, alt) {
    super();
    this.src = src;
    this.alt = alt;
  }
  eq(other) {
    return other.src === this.src && other.alt === this.alt;
  }
  toDOM() {
    // An unsafe or unresolvable source keeps its alt text instead.
    if (!this.src) {
      const span = document.createElement("span");
      span.className = "cm-image-missing";
      span.textContent = this.alt || "image";
      return span;
    }
    const img = document.createElement("img");
    img.className = "cm-image";
    img.src = this.src;
    img.alt = this.alt;
    img.loading = "lazy";
    return img;
  }
}

// A rendered table. Clicking it moves the cursor into the table, which then
// shows its Markdown source for editing.
class TableWidget extends WidgetType {
  constructor(source, context, from, render) {
    super();
    this.source = source;
    this.context = context;
    this.from = from;
    this.render = render;
  }
  eq(other) {
    return (
      other.source === this.source &&
      other.context === this.context &&
      other.from === this.from
    );
  }
  toDOM(view) {
    const div = document.createElement("div");
    div.className = "cm-table-widget";
    // render is markdown-it with raw HTML disabled.
    // Rendering with the document's reference definitions resolves its
    // reference links; definitions themselves render nothing.
    div.innerHTML = this.render(this.source + "\n\n" + this.context);
    // Links follow the editor's policy: Cmd/Ctrl-click opens one (its href
    // was already resolved and validated by the renderer), and any other
    // click edits the table.
    div.addEventListener("click", (event) => {
      const link = event.target.closest?.("a[href]");
      if (!link) return;
      event.preventDefault();
      if (event.metaKey || event.ctrlKey)
        window.open(link.href, "_blank", "noopener,noreferrer");
    });
    div.addEventListener("mousedown", (event) => {
      event.preventDefault();
      if ((event.metaKey || event.ctrlKey) && event.target.closest?.("a[href]"))
        return;
      view.focus();
      view.dispatch({ selection: EditorSelection.cursor(this.from) });
    });
    return div;
  }
  ignoreEvent() {
    return true;
  }
}

// The preview stays immediately below an active fence. Updating its existing
// DOM preserves the last successful SVG while a debounced render is pending.
class DiagramWidget extends WidgetType {
  constructor(code, active, key) {
    super();
    this.code = code;
    this.active = active;
    this.key = key;
  }
  eq(other) {
    return (
      other.key === this.key &&
      other.code === this.code &&
      other.active === this.active
    );
  }
  toDOM(view) {
    const div = document.createElement("div");
    div.className = "cm-diagram-widget";
    div.dataset.active = String(this.active);
    div.diagramKey = this.key;
    div.addEventListener("diagram-rendered", () => view.requestMeasure());
    // Pointer actions keep the caret/source stable. Keyboard focus remains
    // native and is included by the focus-change handler below.
    div.addEventListener(
      "mousedown",
      (event) => {
        if (event.target.closest(".mermaid-tools")) event.preventDefault();
      },
      true,
    );
    const diagram = document.createElement("div");
    diagram.className = "mermaid-diagram";
    diagram.dataset.editable = "true";
    const pre = document.createElement("pre");
    pre.className = "mermaid-source";
    const code = document.createElement("code");
    code.textContent = this.code;
    pre.append(code);
    diagram.append(pre);
    div.append(diagram);
    const edit = document.createElement("button");
    edit.type = "button";
    edit.className = "mermaid-edit";
    sourceAction(edit, this.active);
    edit.addEventListener("click", (event) => {
      event.preventDefault();
      const key = div.diagramKey;
      if (div.dataset.active === "true") {
        const pos = view.posAtDOM(div);
        const block = diagramBlocks(view.state, false).find(
          (b) => b.from <= pos && pos <= b.to,
        );
        const outside =
          block && closedDiagramCursor(block, view.state.doc.length);
        view.dispatch({
          effects: setDiagramEditing.of({ key, open: false }),
          ...(outside != null
            ? { selection: EditorSelection.cursor(outside) }
            : {}),
        });
        // CM may replace the tile when source collapses. Find the retained
        // block by identity and focus its Edit source control after layout.
        view.requestMeasure({
          read: () =>
            [...view.dom.querySelectorAll(".cm-diagram-widget")]
              .find((widget) => widget.diagramKey === key)
              ?.querySelector(".mermaid-edit"),
          write: (button) => button?.focus({ preventScroll: true }),
        });
        return;
      }
      const pos = view.posAtDOM(div);
      const block = diagramBlocks(view.state, false).find(
        (b) => b.from <= pos && pos <= b.to,
      );
      if (!block) return;
      view.dispatch({
        effects: [setDiagramEditing.of({ key, open: true }), setFocus.of(true)],
        selection: EditorSelection.cursor(block.edit),
        scrollIntoView: true,
      });
      view.focus();
    });
    div.append(edit);
    div.dataset.code = this.code;
    renderDiagrams(div).then(() => view.requestMeasure());
    diagram.querySelector(".mermaid-tools")?.append(edit);
    return div;
  }
  updateDOM(div, view, previous) {
    return updatePreview(
      div,
      this,
      previous,
      invalidateDiagram,
      (diagram, code) =>
        updateDiagram(diagram, code).then(() => view.requestMeasure()),
    );
  }
  destroy(div) {
    clearTimeout(div.diagramTimer);
    destroyDiagram(div.querySelector(".mermaid-diagram"));
  }
  ignoreEvent() {
    return true;
  }
}

// Tables and diagrams span lines, so they are replaced from a state field: a
// view plugin may not replace line breaks. The field tracks focus through an
// effect, so a block renders again when the editor loses focus.
const setFocus = StateEffect.define();
// The document's reference definitions, rescanned when the text changes.
function definitions(links) {
  return StateField.define({
    create: (state) => referenceDefinitions(state.doc, links),
    update: (value, tr) =>
      tr.docChanged ? referenceDefinitions(tr.state.doc, links) : value,
  });
}

function blocks(render, refs) {
  const build = (state, focused, keys, closed) =>
    Decoration.set(
      [
        ...tableBlocks(state, focused, state.field(refs))
          .filter((t) => !t.active)
          .map((t) =>
            Decoration.replace({
              widget: new TableWidget(t.source, t.context, t.from, render),
              block: true,
            }).range(t.from, t.to),
          ),
        ...diagramBlocks(state, focused).map((d) => {
          const key = keys.get(d.from);
          const active = d.active && !closed.has(key);
          const widget = new DiagramWidget(d.code, active, key);
          return active
            ? Decoration.widget({ widget, block: true, side: 1 }).range(d.to)
            : Decoration.replace({ widget, block: true }).range(d.from, d.to);
        }),
      ],
      true,
    );
  const field = StateField.define({
    create: (state) => {
      const keys = diagramKeys(diagramBlocks(state, false));
      const closed = new Set();
      return {
        focused: false,
        keys,
        closed,
        decorations: build(state, false, keys, closed),
      };
    },
    update(value, tr) {
      let focused = value.focused;
      for (const e of tr.effects) if (e.is(setFocus)) focused = e.value;
      if (
        tr.effects.some((effect) => effect.is(setDiagramEditing)) ||
        focused !== value.focused ||
        tr.docChanged ||
        tr.selection ||
        syntaxTree(tr.startState) !== syntaxTree(tr.state)
      ) {
        const keys = diagramKeys(
          diagramBlocks(tr.state, focused),
          value.keys,
          tr.changes,
        );
        const closed = diagramClosures(
          diagramBlocks(tr.state, focused),
          keys,
          value.closed,
          tr,
        );
        return {
          focused,
          keys,
          closed,
          decorations: build(tr.state, focused, keys, closed),
        };
      }
      return value;
    },
    provide: (f) => EditorView.decorations.from(f, (v) => v.decorations),
  });
  return [
    field,
    ViewPlugin.fromClass(
      class {
        constructor(view) {
          this.cleanup = trackFocusExit(view.dom, () => {
            if (view.state.field(field).focused)
              view.dispatch({ effects: setFocus.of(false) });
          });
        }
        destroy() {
          this.cleanup();
        }
      },
    ),
    EditorView.focusChangeEffect.of((state, focusing) =>
      setFocus.of(
        focusing ||
          (!!document.activeElement?.closest(".cm-diagram-widget") &&
            EditorView.findFromDOM(document.activeElement)?.state === state),
      ),
    ),
  ];
}

const hidden = Decoration.replace({}),
  bullet = Decoration.replace({ widget: new Bullet() }),
  rule = Decoration.replace({ widget: new Rule() });

// livePreview renders Markdown in place. links holds the renderer's link
// rules (markdown.js), resolve makes an accepted URL absolute against the
// document, and render turns a Markdown table or diagram into safe HTML.
export function livePreview(links, resolve, render) {
  const refs = definitions(links);
  const build = (view) => {
    const active = activeLines(view.state, view.hasFocus),
      ranges = [];
    for (const d of liveDecorations(
      view.state,
      view.visibleRanges,
      active,
      links,
      view.state.field(refs),
    )) {
      switch (d.kind) {
        case "line":
          ranges.push(Decoration.line({ class: d.cls }).range(d.from));
          break;
        case "mark":
          ranges.push(
            Decoration.mark(
              d.href === undefined
                ? { class: d.cls }
                : {
                    class: d.cls,
                    attributes: {
                      "data-href": d.href,
                      title: "Open: Ctrl/⌘-click, or Ctrl/⌘-Enter",
                    },
                  },
            ).range(d.from, d.to),
          );
          break;
        case "hide":
          ranges.push(hidden.range(d.from, d.to));
          break;
        case "bullet":
          ranges.push(bullet.range(d.from, d.to));
          break;
        case "number":
        case "text":
          ranges.push(
            Decoration.replace({ widget: new Label(d.label) }).range(
              d.from,
              d.to,
            ),
          );
          break;
        case "rule":
          ranges.push(rule.range(d.from, d.to));
          break;
        case "image":
          ranges.push(
            Decoration.replace({
              widget: new ImageWidget(resolve(d.src), d.alt),
            }).range(d.from, d.to),
          );
          break;
      }
    }
    return Decoration.set(ranges, true);
  };
  return [
    refs,
    blocks(render, refs),
    ViewPlugin.fromClass(
      class {
        constructor(view) {
          this.decorations = build(view);
        }
        update(u) {
          if (
            u.docChanged ||
            u.viewportChanged ||
            u.selectionSet ||
            u.focusChanged ||
            syntaxTree(u.startState) !== syntaxTree(u.state)
          )
            this.decorations = build(u.view);
        }
      },
      { decorations: (p) => p.decorations },
    ),
    // Ctrl/Cmd-Enter opens the link under the cursor, for keyboard users;
    // elsewhere it keeps its usual meaning.
    Prec.high(
      keymap.of([
        {
          key: "Mod-Enter",
          run(view) {
            const sel = view.state.selection.main;
            if (!sel.empty) return false;
            const href = resolve(
              linkAt(view.state, sel.head, links, view.state.field(refs)),
            );
            if (!href) return false;
            window.open(href, "_blank", "noopener,noreferrer");
            return true;
          },
        },
      ]),
    ),
    // Cmd/Ctrl-click opens a link; a plain click places the cursor to edit it.
    EditorView.domEventHandlers({
      click(event) {
        if (!(event.metaKey || event.ctrlKey)) return false;
        const link = event.target.closest?.("[data-href]");
        if (!link) return false;
        const href = resolve(link.dataset.href);
        if (!href) return false;
        event.preventDefault();
        window.open(href, "_blank", "noopener,noreferrer");
        return true;
      },
    }),
  ];
}
