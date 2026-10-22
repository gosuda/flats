// The guest and viewer parts of a document page (docs-public-edit.md rules 8
// to 13 and 23): the name popup, "You are …" in the header, the people
// list with the guest badge and the viewer entries, and the dialogs for a
// full document. guest.js decides; this module only draws. Names are only
// ever drawn as text.
import {
  GUEST_NAME_MAX,
  NAME_INVALID,
  cleanGuestName,
  guestNameError,
  runeCount,
} from "./guest.js";

const el = (tag, props = {}, ...children) => {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === "class") node.className = v;
    else if (k === "text") node.textContent = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false)
      node.setAttribute(k, v === true ? "" : v);
  }
  node.append(...children.filter((c) => c !== null && c !== undefined));
  return node;
};

// badge is the mark next to a guest's name, outside the name text, so no
// name can imitate it.
export const badge = () => el("span", { class: "guest-badge", text: "guest" });

let dialogs = 0;

// dialog shows a modal over the live document, which keeps updating behind
// it. done runs once with how it closed: the value given to close, or ""
// for Escape unless cancel is false.
function dialog({ title, describe, body, alert = false, cancel = true, done }) {
  const id = "guest-dialog-" + ++dialogs;
  const d = el(
    "dialog",
    {
      class: "guest-dialog",
      role: alert ? "alertdialog" : null,
      "aria-labelledby": id + "-title",
      "aria-describedby": id + "-describe",
    },
    el("h2", { id: id + "-title", text: title }),
    el("p", { id: id + "-describe", class: "guest-describe", text: describe }),
    body,
  );
  d.addEventListener("cancel", (event) => {
    if (!cancel) event.preventDefault();
  });
  d.addEventListener(
    "close",
    () => {
      d.remove();
      done(d.returnValue);
    },
    { once: true },
  );
  document.body.append(d);
  d.showModal();
  return d;
}

const actions = (...buttons) =>
  el("div", { class: "guest-actions" }, ...buttons);

const POPUP_NOTE =
  "You can add, change and delete text in every document. You can't see history or restore earlier text.";

// nameDialog is the name popup: "Join <flat>" with View only and Join and
// edit, or "Change your name" for a guest. prompt is JoinState.prompt.
// submit gets the typed name; dismiss runs for View only or Escape. The
// returned dialog's close("closed") closes it without either.
export function nameDialog({ prompt, flat, name, submit, dismiss }) {
  const change = prompt.mode === "change",
    input = el("input", {
      id: "guest-name",
      type: "text",
      autocomplete: "nickname",
      placeholder: "Pick a name",
      "aria-describedby": "guest-name-count guest-name-preview",
    }),
    count = el("span", { id: "guest-name-count", class: "guest-count" }),
    preview = el("p", { id: "guest-name-preview", class: "guest-preview" }),
    error = el("p", {
      class: "guest-error",
      role: "alert",
      text: prompt.reason === "invalid" ? NAME_INVALID : "",
    }),
    ok = el("button", {
      type: "submit",
      class: "primary",
      text: change ? "Save name" : "Join and edit",
    });
  input.value = name;
  const update = () => {
    const clean = cleanGuestName(input.value),
      n = runeCount(clean);
    count.textContent = `${n}/${GUEST_NAME_MAX}`;
    count.classList.toggle("over", n > GUEST_NAME_MAX);
    preview.replaceChildren(
      "Shown as ",
      el("strong", { text: clean || "your name" }),
      " ",
      badge(),
      " to other editors. Nobody checks it.",
    );
    ok.disabled = !!guestNameError(input.value);
  };
  input.addEventListener("input", () => {
    error.textContent = "";
    update();
  });
  const form = el(
    "form",
    {
      method: "dialog",
      onsubmit: (event) => {
        event.preventDefault();
        const problem = guestNameError(input.value);
        if (problem) {
          error.textContent = problem;
          input.focus();
          return;
        }
        d.close("submit");
      },
    },
    el(
      "div",
      { class: "guest-field" },
      el(
        "div",
        { class: "guest-label" },
        el("label", { for: "guest-name", text: "Your name" }),
        count,
      ),
      input,
      preview,
    ),
    error,
    el("p", { class: "guest-note", text: POPUP_NOTE }),
    actions(
      change
        ? null
        : el("button", {
            type: "button",
            text: "View only",
            onclick: () => d.close("view"),
          }),
      ok,
    ),
  );
  const d = dialog({
    title: change ? "Change your name" : `Join ${flat}`,
    describe: change
      ? "Others see this name next to your cursor."
      : prompt.reason === "opened"
        ? "The owner just opened these documents for editing. Pick the name others will see next to your cursor, or keep reading."
        : "Anyone with this link can edit these documents. Pick the name others will see next to your cursor, or just read.",
    body: form,
    done: (value) => {
      if (value === "submit") submit(input.value);
      else if (value !== "closed") dismiss();
    },
  });
  update();
  input.focus();
  return d;
}

// fullDialog: the guest pool refused this join (rule 23).
export function fullDialog({ flat, read, retry }) {
  return dialog({
    title: "This document is full",
    describe: `The most people ${flat} allows are editing it right now. You can read the document live while you wait.`,
    alert: true,
    body: el(
      "form",
      { method: "dialog" },
      actions(
        el("button", { value: "read", text: "Read" }),
        el("button", { value: "retry", class: "primary", text: "Try again" }),
      ),
    ),
    done: (value) => (value === "retry" ? retry() : read()),
  });
}

// readersDialog: the reader pool refused the connection. Nothing is shown
// until the visitor tries again, so it cannot be dismissed otherwise.
export function readersDialog({ flat, retry }) {
  return dialog({
    title: "Too many readers right now",
    describe: `${flat} has as many readers as it allows. Try again in a few minutes.`,
    alert: true,
    cancel: false,
    body: el(
      "form",
      { method: "dialog" },
      actions(
        el("button", { value: "retry", class: "primary", text: "Try again" }),
      ),
    ),
    done: retry,
  });
}

// me fills the header's "You are …": a viewer's number with Join to edit
// while the flat is open for editing, or a guest's name with the badge and
// Change name. Members see nothing new.
export function me(node, { kind, label, name, canJoin, join, change }) {
  node.hidden = !(kind === "guest" ? name : kind === "viewer" && label);
  if (node.hidden) return;
  node.replaceChildren(
    "You are ",
    el("strong", { text: kind === "guest" ? name : label }),
    ...(kind === "guest"
      ? [
          " ",
          badge(),
          " ",
          el("button", {
            type: "button",
            class: "link",
            text: "Change name",
            onclick: change,
          }),
        ]
      : canJoin
        ? [
            " ",
            el("button", {
              type: "button",
              class: "link",
              text: "Join to edit",
              onclick: join,
            }),
          ]
        : []),
  );
}

// people draws the people list: everyone with presence, a guest with the
// badge, then the viewers of this document as "Viewer N" entries, or their
// count above 20 (rule 10).
export function people(node, states, viewers) {
  node.replaceChildren();
  for (const [, s] of states) {
    if (!s.user || typeof s.user.name !== "string") continue;
    const guest = s.user.guest === true,
      title =
        s.user.name +
        (guest ? " · guest" : "") +
        (s.user.verified ? " · verified" : " · unverified");
    const person = el(
      "span",
      { class: "person", title, "aria-label": title },
      el("span", {
        class: "avatar",
        "aria-hidden": "true",
        text: s.user.name.slice(0, 1).toUpperCase(),
      }),
      s.user.name,
    );
    person.style.setProperty(
      "--person-color",
      /^#[0-9a-f]{6}$/i.test(s.user.color) ? s.user.color : "#3b65b9",
    );
    if (guest) person.append(" ", badge());
    node.append(person);
  }
  if (!viewers?.count) return;
  const label = `${viewers.count} viewer${viewers.count === 1 ? "" : "s"}`;
  node.append(
    el(
      "span",
      { class: "viewers", "aria-label": label },
      ...(viewers.labels.length
        ? viewers.labels.map((text) => el("span", { class: "viewer", text }))
        : [el("span", { class: "viewer", text: label })]),
    ),
  );
}
