// Turns the descriptors from live.js into CodeMirror decorations.
import {
  Decoration,
  EditorView,
  ViewPlugin,
  WidgetType,
} from "@codemirror/view";
import { EditorSelection, StateEffect, StateField } from "@codemirror/state";
import { syntaxTree } from "@codemirror/language";
import { activeLines, liveDecorations, tableBlocks } from "./live.js";

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
  constructor(source, from, render) {
    super();
    this.source = source;
    this.from = from;
    this.render = render;
  }
  eq(other) {
    return other.source === this.source && other.from === this.from;
  }
  toDOM(view) {
    const div = document.createElement("div");
    div.className = "cm-table-widget";
    // render is markdown-it with raw HTML disabled.
    div.innerHTML = this.render(this.source);
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

// Tables span lines, so they are replaced from a state field: a view plugin
// may not replace line breaks. The field tracks focus through an effect, so a
// table renders again when the editor loses focus.
const setFocus = StateEffect.define();
function tables(render) {
  const build = (state, focused) =>
    Decoration.set(
      tableBlocks(state, focused)
        .filter((t) => !t.active)
        .map((t) =>
          Decoration.replace({
            widget: new TableWidget(t.source, t.from, render),
            block: true,
          }).range(t.from, t.to),
        ),
    );
  const field = StateField.define({
    create: (state) => ({ focused: false, decorations: build(state, false) }),
    update(value, tr) {
      let focused = value.focused;
      for (const e of tr.effects) if (e.is(setFocus)) focused = e.value;
      if (
        focused !== value.focused ||
        tr.docChanged ||
        tr.selection ||
        syntaxTree(tr.startState) !== syntaxTree(tr.state)
      )
        return { focused, decorations: build(tr.state, focused) };
      return value;
    },
    provide: (f) => EditorView.decorations.from(f, (v) => v.decorations),
  });
  return [
    field,
    EditorView.focusChangeEffect.of((state, focusing) => setFocus.of(focusing)),
  ];
}

const hidden = Decoration.replace({}),
  bullet = Decoration.replace({ widget: new Bullet() }),
  rule = Decoration.replace({ widget: new Rule() });

// livePreview renders Markdown in place. links holds the renderer's link
// rules (markdown.js), resolve makes an accepted URL absolute against the
// document, and render turns a Markdown table into safe HTML.
export function livePreview(links, resolve, render) {
  const build = (view) => {
    const active = activeLines(view.state, view.hasFocus),
      ranges = [];
    for (const d of liveDecorations(
      view.state,
      view.visibleRanges,
      active,
      links,
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
                : { class: d.cls, attributes: { "data-href": d.href } },
            ).range(d.from, d.to),
          );
          break;
        case "hide":
          ranges.push(hidden.range(d.from, d.to));
          break;
        case "bullet":
          ranges.push(bullet.range(d.from, d.to));
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
    tables(render),
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
