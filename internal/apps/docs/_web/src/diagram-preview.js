import { ySyncAnnotation } from "y-codemirror.next";
import { StateEffect, Transaction } from "@codemirror/state";

export const setDiagramEditing = StateEffect.define();

// A focus-only transition preserves Done. Deliberate cursor movement or a
// local edit releases it before decorations hide a caret receiving input.
export function diagramClosures(blocks, keys, previous, transaction) {
  const live = new Set(keys.values());
  const closed = new Set([...previous].filter((key) => live.has(key)));
  const localEdit =
    transaction.docChanged &&
    !transaction.annotation(ySyncAnnotation) &&
    !transaction.annotation(Transaction.remote);
  if (transaction.selection || localEdit) closed.clear();
  for (const effect of transaction.effects)
    if (effect.is(setDiagramEditing)) {
      const { key, open } = effect.value;
      if (open) closed.delete(key);
      else if (live.has(key)) closed.add(key);
    }
  return closed;
}

export function sourceAction(button, active) {
  button.textContent = active ? "Done" : "Edit source";
}

// Avoid leaving a hidden caret inside the fence where another position exists.
export function closedDiagramCursor(block, documentLength) {
  if (block.to < documentLength) return block.to + 1;
  if (block.from > 0) return block.from - 1;
  return null;
}

// Keep identities through position shifts, but never transfer a deleted
// block's DOM (and last valid SVG) to another diagram at the same position.
export function diagramKeys(blocks, previous = new Map(), changes) {
  const mapped = new Map();
  for (const [pos, key] of previous) {
    let removed = false;
    changes?.iterChangedRanges((from, to) => {
      if (from <= pos && to > pos) removed = true;
    });
    if (!removed) mapped.set(changes ? changes.mapPos(pos, 1) : pos, key);
  }
  return new Map(blocks.map(({ from }) => [from, mapped.get(from) || {}]));
}

export function updatePreview(div, widget, previous, invalidate, draw) {
  if (widget.key !== previous.key) return false;
  div.dataset.active = String(widget.active);
  sourceAction(div.querySelector(".mermaid-edit"), widget.active);
  if (div.dataset.code !== widget.code) {
    div.dataset.code = widget.code;
    clearTimeout(div.diagramTimer);
    const diagram = div.querySelector(".mermaid-diagram");
    // Source/copy and cancellation must reflect the new text immediately.
    invalidate(diagram, widget.code);
    div.diagramTimer = setTimeout(() => draw(diagram, widget.code), 300);
  }
  return true;
}

// Reuse identical diagrams during a rendered-document refresh so Source
// toggles and open-dialog origins survive unrelated collaborative edits.
export function replaceRendered(root, html, dispose) {
  const groups = new Map();
  const focused = root.ownerDocument.activeElement;
  for (const diagram of root.querySelectorAll(".mermaid-diagram")) {
    const source = diagram.querySelector(".mermaid-source").textContent;
    if (!groups.has(source)) groups.set(source, []);
    groups.get(source).push(diagram);
  }
  const next = root.ownerDocument.createElement("div");
  next.innerHTML = html;
  for (const placeholder of next.querySelectorAll(".mermaid-diagram")) {
    const source = placeholder.querySelector(".mermaid-source").textContent;
    const retained = groups.get(source)?.shift();
    if (retained) placeholder.replaceWith(retained);
  }
  for (const diagrams of groups.values())
    for (const diagram of diagrams) dispose(diagram);
  root.replaceChildren(...next.childNodes);
  if (focused && root.contains(focused)) focused.focus({ preventScroll: true });
}

// CodeMirror observes contentDOM focus, but native toolbar buttons can lose
// focus without a contentDOM blur. Settle that transition after DOM events.
export function trackFocusExit(root, onExit) {
  let timer;
  const leave = () => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      if (!root.contains(root.ownerDocument.activeElement)) onExit();
    }, 0);
  };
  root.addEventListener("focusout", leave);
  return () => {
    clearTimeout(timer);
    root.removeEventListener("focusout", leave);
  };
}
