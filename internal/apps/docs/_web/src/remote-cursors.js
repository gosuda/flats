// Other editors' cursors and selections, with the guest badge next to a
// guest's name (docs-public-edit.md rule 13). Adapted from
// y-codemirror.next 0.3.6 src/y-remote-selections.js (MIT License,
// Copyright (c) 2022 Kevin Jahns), whose caret label holds only the name.
import { Decoration, ViewPlugin, WidgetType } from "@codemirror/view";
import { Annotation, RangeSet } from "@codemirror/state";
import { yRemoteSelectionsTheme } from "y-codemirror.next";
import * as Y from "yjs";

const remoteChange = Annotation.define();

// caretLabel is what a remote caret shows: the name as text, and whether a
// guest badge follows it. Only a #rrggbb color reaches a style attribute.
export function caretLabel(user = {}) {
  return {
    name: typeof user.name === "string" ? user.name : "Anonymous",
    guest: user.guest === true,
    color: /^#[0-9a-f]{6}$/i.test(user.color) ? user.color : "#30bced",
  };
}

class Caret extends WidgetType {
  constructor(label) {
    super();
    this.label = label;
  }
  toDOM() {
    const { color, name, guest } = this.label;
    const caret = document.createElement("span");
    caret.className = "cm-ySelectionCaret";
    caret.style.backgroundColor = color;
    caret.style.borderColor = color;
    const dot = document.createElement("div");
    dot.className = "cm-ySelectionCaretDot";
    const info = document.createElement("div");
    info.className = "cm-ySelectionInfo";
    info.append(document.createTextNode(name));
    if (guest) {
      const badge = document.createElement("span");
      badge.className = "guest-badge";
      badge.textContent = "guest";
      info.append(" ", badge);
    }
    caret.append("⁠", dot, "⁠", info, "⁠");
    return caret;
  }
  eq(other) {
    const a = this.label,
      b = other.label;
    return a.color === b.color && a.name === b.name && a.guest === b.guest;
  }
  get estimatedHeight() {
    return -1;
  }
  ignoreEvent() {
    return true;
  }
}

// mark adds a selection range; CodeMirror does not allow empty marks.
function mark(ranges, from, to, colorLight) {
  if (to > from)
    ranges.push({
      from,
      to,
      value: Decoration.mark({
        attributes: { style: `background-color: ${colorLight}` },
        class: "cm-ySelection",
      }),
    });
}

// remoteCursors publishes this page's cursor in awareness and draws the
// others'. It replaces yCollab's remote selections, which the page leaves
// out by passing yCollab no awareness.
export function remoteCursors(ytext, awareness) {
  const plugin = ViewPlugin.fromClass(
    class {
      constructor(view) {
        this.listener = ({ added, updated, removed }) => {
          const clients = added.concat(updated, removed);
          if (clients.some((id) => id !== awareness.doc.clientID))
            view.dispatch({ annotations: [remoteChange.of([])] });
        };
        awareness.on("change", this.listener);
        this.decorations = RangeSet.of([]);
      }
      destroy() {
        awareness.off("change", this.listener);
      }
      update(update) {
        const doc = ytext.doc,
          local = awareness.getLocalState();
        if (local != null) {
          const focused =
            update.view.hasFocus && update.view.dom.ownerDocument.hasFocus();
          const sel = focused ? update.state.selection.main : null;
          if (sel != null) {
            const anchor = Y.createRelativePositionFromTypeIndex(
                ytext,
                sel.anchor,
              ),
              head = Y.createRelativePositionFromTypeIndex(ytext, sel.head);
            const current = local.cursor;
            if (
              current == null ||
              !Y.compareRelativePositions(
                Y.createRelativePositionFromJSON(current.anchor),
                anchor,
              ) ||
              !Y.compareRelativePositions(
                Y.createRelativePositionFromJSON(current.head),
                head,
              )
            )
              awareness.setLocalStateField("cursor", { anchor, head });
          } else if (local.cursor != null && focused)
            awareness.setLocalStateField("cursor", null);
        }
        const ranges = [];
        awareness.getStates().forEach((state, client) => {
          if (client === awareness.doc.clientID) return;
          const cursor = state.cursor;
          if (cursor?.anchor == null || cursor.head == null) return;
          const anchor = Y.createAbsolutePositionFromRelativePosition(
              cursor.anchor,
              doc,
            ),
            head = Y.createAbsolutePositionFromRelativePosition(
              cursor.head,
              doc,
            );
          if (
            anchor == null ||
            head == null ||
            anchor.type !== ytext ||
            head.type !== ytext
          )
            return;
          const label = caretLabel(state.user),
            light = label.color + "33",
            start = Math.min(anchor.index, head.index),
            end = Math.max(anchor.index, head.index),
            first = update.view.state.doc.lineAt(start),
            last = update.view.state.doc.lineAt(end);
          if (first.number === last.number) mark(ranges, start, end, light);
          else {
            mark(ranges, start, first.from + first.length, light);
            mark(ranges, last.from, end, light);
            for (let i = first.number + 1; i < last.number; i++) {
              const from = update.view.state.doc.line(i).from;
              ranges.push({
                from,
                to: from,
                value: Decoration.line({
                  attributes: {
                    style: `background-color: ${light}`,
                    class: "cm-yLineSelection",
                  },
                }),
              });
            }
          }
          ranges.push({
            from: head.index,
            to: head.index,
            value: Decoration.widget({
              side: head.index - anchor.index > 0 ? -1 : 1,
              block: false,
              widget: new Caret(label),
            }),
          });
        });
        this.decorations = Decoration.set(ranges, true);
      }
    },
    { decorations: (v) => v.decorations },
  );
  return [yRemoteSelectionsTheme, plugin];
}
