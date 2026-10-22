import { test } from "node:test";
import assert from "node:assert/strict";
import {
  JoinState,
  KEYS,
  browserStore,
  cleanGuestName,
  endedNotice,
  guestNameError,
  refusal,
  viewersOf,
} from "./guest.js";

// A tab: sessionStorage is per tab and survives reloads and moves between
// documents; localStorage is shared by the browser.
function tab(local = fakeStorage()) {
  const session = fakeStorage();
  return {
    session,
    local,
    store: browserStore(
      () => session,
      () => local,
    ),
  };
}
function fakeStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
  };
}
const viewer = (access, epoch, n = 3) => ({
  access,
  access_epoch: epoch,
  me: { kind: "viewer", label: `Viewer ${n}` },
  viewer_token: "vt",
});
const guest = (epoch, name = "Mina") => ({
  access: "edit",
  access_epoch: epoch,
  me: { kind: "guest", name },
  guest_token: "gt",
});

test("guest names are cleaned and checked as the server does", () => {
  assert.equal(cleanGuestName("  ​Mi\u0007na‮ \n"), "Mina");
  assert.equal(guestNameError(" ​ "), "Type a name to join.");
  assert.equal(guestNameError("x".repeat(40)), "");
  assert.match(guestNameError("x".repeat(41)), /at most 40/);
  // Code points, not UTF-16 units.
  assert.equal(guestNameError("😀".repeat(40)), "");
});

test("arrival at edit opens the popup; a remembered name only fills it", () => {
  const t = tab();
  t.store.set(KEYS.guestName, "Mina");
  const s = new JoinState(t.store);
  assert.deepEqual(s.hello("Quiet Otter"), { nick: "Quiet Otter" });
  assert.equal(s.welcome(viewer("edit", 7)), "prompt");
  assert.deepEqual(s.prompt, { mode: "join", reason: "arrival" });
  assert.equal(s.name(), "Mina");
  assert.equal(s.label, "Viewer 3");
  assert.equal(t.session.getItem(KEYS.viewerToken), "vt");
  // A reconnect with the same epoch while the popup is open changes nothing.
  assert.equal(s.welcome(viewer("edit", 7)), "none");
  // At view nobody is asked.
  const v = new JoinState(tab().store);
  assert.equal(v.welcome(viewer("view", 7)), "none");
  assert.equal(v.prompt, null);
  assert.equal(v.canJoin(), false);
});

test("View only keeps reading across reconnects and documents until edit starts again", () => {
  const t = tab(),
    s = new JoinState(t.store);
  s.welcome(viewer("edit", 7));
  assert.equal(s.dismiss(), "none");
  assert.equal(s.prompt, null);
  assert.equal(t.session.getItem(KEYS.viewOnly), "7");
  assert.equal(s.canJoin(), true);
  // Reconnect with the same epoch.
  assert.equal(s.welcome(viewer("edit", 7)), "none");
  // Another document of the flat in the same tab.
  const next = new JoinState(t.store);
  assert.equal(next.welcome(viewer("edit", 7)), "none");
  assert.deepEqual(next.hello(), { viewer_token: "vt" });
  // A level frame of the same edit period changes nothing.
  assert.equal(next.level({ access: "edit", access_epoch: 7 }), "none");
  // Back to view, then edit again: a newer epoch asks again.
  assert.equal(next.level({ access: "view", access_epoch: 8 }), "none");
  assert.equal(next.canJoin(), false);
  assert.equal(next.level({ access: "edit", access_epoch: 9 }), "prompt");
  assert.deepEqual(next.prompt, { mode: "join", reason: "opened" });
});

test("a missed view, edit, view, edit cycle asks again", () => {
  // Chose View only at 7; the tab reloads after the level went to view (8)
  // and back to edit (9) without this page seeing it.
  const t = tab(),
    s = new JoinState(t.store);
  s.welcome(viewer("edit", 7));
  s.dismiss();
  const later = new JoinState(t.store);
  assert.equal(later.welcome(viewer("edit", 9)), "prompt");
  // A guest that joined at 7 and comes back at 9 is admitted as a guest by
  // the server, but did not choose to edit in this period: it reads and is
  // asked again.
  const g = tab(),
    joined = new JoinState(g.store);
  joined.welcome(viewer("edit", 7));
  assert.equal(joined.submit("Mina"), "reconnect");
  assert.equal(joined.welcome(guest(7)), "none");
  const back = new JoinState(g.store);
  assert.equal(back.hello().guest.name, "Mina");
  assert.equal(back.welcome(guest(9)), "reconnect");
  assert.equal(back.hello().guest, undefined);
  assert.equal(back.welcome(viewer("edit", 9)), "prompt");
});

test("Join to edit and Join and edit start a guest session that moves between documents", () => {
  const t = tab(),
    s = new JoinState(t.store);
  s.welcome(viewer("edit", 7));
  s.dismiss();
  assert.equal(s.openJoin(), "prompt");
  assert.deepEqual(s.prompt, { mode: "join", reason: "join" });
  assert.equal(s.submit("  "), "none");
  assert.equal(s.submit(" Mina "), "reconnect");
  assert.equal(t.session.getItem(KEYS.viewOnly), null);
  assert.equal(t.session.getItem(KEYS.joined), "7");
  assert.equal(t.local.getItem(KEYS.guestName), "Mina");
  assert.deepEqual(s.hello("Quiet Otter"), {
    guest: { name: "Mina" },
    viewer_token: "vt",
    nick: "Quiet Otter",
  });
  assert.equal(s.welcome(guest(7, "Mina")), "none");
  assert.equal(s.kind, "guest");
  assert.equal(t.session.getItem(KEYS.guestToken), "gt");
  // The next document joins with the token and no popup.
  const next = new JoinState(t.store);
  assert.deepEqual(next.hello().guest, { name: "Mina", token: "gt" });
  assert.equal(next.welcome(guest(7)), "none");
  assert.equal(next.prompt, null);
  // Change name keeps the session.
  assert.equal(next.openChange(), "prompt");
  assert.equal(next.submit("Mina Kim"), "reconnect");
  assert.deepEqual(next.hello().guest, { name: "Mina Kim", token: "gt" });
  // Members are never asked and guests get no level popup.
  assert.equal(next.level({ access: "edit", access_epoch: 7 }), "none");
  const member = new JoinState(tab().store);
  assert.equal(member.welcome({ access: "edit", access_epoch: 7 }), "none");
  assert.equal(member.kind, "member");
  assert.equal(member.level({ access: "edit", access_epoch: 8 }), "none");
  assert.equal(member.openJoin(), "none");
});

test("a new browser tab asks again even with a remembered name", () => {
  const first = tab(),
    s = new JoinState(first.store);
  s.welcome(viewer("edit", 7));
  s.submit("Mina");
  s.welcome(guest(7));
  const other = tab(first.local),
    o = new JoinState(other.store);
  assert.equal(o.hello().guest, undefined);
  assert.equal(o.welcome(viewer("edit", 7)), "prompt");
  assert.equal(o.name(), "Mina");
});

test("editing ended reads on and asks only when editing opens again", () => {
  const t = tab(),
    s = new JoinState(t.store);
  s.welcome(viewer("edit", 7));
  s.submit("Mina");
  s.welcome(guest(7));
  assert.equal(s.ended(), "reconnect");
  assert.equal(s.hello().guest, undefined);
  assert.equal(s.welcome(viewer("view", 8)), "none");
  assert.equal(new JoinState(t.store).hello().guest, undefined);
  assert.equal(s.level({ access: "edit", access_epoch: 9 }), "prompt");
  // Reconnecting after the owner stopped editing, without editing_ended,
  // also ends the session.
  const u = tab(),
    r = new JoinState(u.store);
  r.welcome(viewer("edit", 7));
  r.submit("Mina");
  r.welcome(guest(7));
  assert.equal(r.welcome(viewer("view", 8)), "none");
  assert.equal(r.kind, "viewer");
  assert.equal(r.hello().guest, undefined);
});

test("a full document: Read keeps reading, Try again joins again", () => {
  // Refused at the first hello, before any welcome named the epoch.
  const t = tab();
  t.session.setItem(KEYS.joined, "7");
  t.local.setItem(KEYS.guestName, "Mina");
  const s = new JoinState(t.store);
  assert.equal(s.full(), "full");
  assert.ok(s.hello().guest, "Try again asks to join again");
  assert.equal(s.read(), "reconnect");
  assert.equal(s.hello().guest, undefined);
  assert.equal(s.welcome(viewer("edit", 7)), "none");
  assert.equal(t.session.getItem(KEYS.viewOnly), "7");
  assert.equal(s.canJoin(), true);
});

test("a guest that leaves for Read or a refused name reads on", () => {
  const t = tab(),
    s = new JoinState(t.store);
  s.welcome(viewer("edit", 7));
  s.submit("Mina");
  s.welcome(guest(7));
  assert.equal(s.full(), "full");
  assert.equal(s.read(), "reconnect");
  assert.equal(s.kind, "viewer");
  assert.equal(s.welcome(viewer("edit", 7)), "none");
  const u = new JoinState(tab().store);
  u.welcome(viewer("edit", 7));
  u.submit("Mina");
  u.welcome(guest(7));
  u.invalidName();
  assert.equal(u.kind, "viewer");
  assert.equal(
    endedNotice(1, "This document is full, so editing stopped.").reason,
    "This document is full, so editing stopped.",
  );
});

test("a refused name is forgotten and asked again", () => {
  const t = tab(),
    s = new JoinState(t.store);
  s.welcome(viewer("edit", 7));
  s.submit("Mina");
  assert.equal(s.invalidName(), "reconnect");
  assert.deepEqual(s.prompt, { mode: "join", reason: "invalid" });
  assert.equal(s.name(), "");
  assert.equal(s.hello().guest, undefined);
  assert.equal(s.welcome(viewer("edit", 7)), "none");
});

test("Rejoin after a refusal reloads into the same guest session", () => {
  const t = tab(),
    s = new JoinState(t.store);
  s.welcome(viewer("edit", 7));
  s.submit("Mina");
  s.welcome(guest(7));
  assert.equal(s.rejoin(), "reload");
  const reloaded = new JoinState(t.store);
  assert.equal(reloaded.hello().guest.name, "Mina");
  assert.equal(reloaded.welcome(guest(7)), "none");
  assert.equal(reloaded.prompt, null);
});

test("the popup closes when the level leaves edit", () => {
  const s = new JoinState(tab().store);
  s.welcome(viewer("edit", 7));
  assert.equal(s.level({ access: "view", access_epoch: 8 }), "close");
  assert.equal(s.prompt, null);
});

test("blocked storage keeps the choices for this page", () => {
  const blocked = () => {
    throw new Error("SecurityError");
  };
  const store = browserStore(blocked, blocked),
    s = new JoinState(store);
  s.welcome(viewer("edit", 7));
  s.submit("Mina");
  assert.deepEqual(s.hello().guest, { name: "Mina" });
  s.welcome(guest(7));
  assert.deepEqual(s.hello().guest, { name: "Mina", token: "gt" });
});

test("viewer lists name up to 20 viewers and count the rest", () => {
  assert.deepEqual(viewersOf({ n: [3, 1, 0, "x"] }), {
    count: 2,
    labels: ["Viewer 1", "Viewer 3"],
  });
  const many = Array.from({ length: 21 }, (_, i) => i + 1);
  assert.deepEqual(viewersOf({ n: many }), { count: 21, labels: [] });
  assert.deepEqual(viewersOf({ count: 45 }), { count: 45, labels: [] });
});

test("refusal and editing-ended notices say what happened", () => {
  assert.equal(
    refusal("guest_size").reason,
    "This change is too large for a guest, so editing stopped.",
  );
  assert.match(refusal("guest_size").detail, /^It and what you typed/);
  assert.match(refusal("guest_room").detail, /^Ask the owner to make room\./);
  assert.match(refusal("disk_quota").reason, /^The disk quota is full,/);
  assert.match(refusal("unknown").reason, /, so editing stopped\.$/);
  assert.deepEqual(endedNotice(2), {
    reason: "The owner turned off guest editing.",
    detail:
      "2 of your latest changes weren't saved. You can keep reading live.",
  });
  assert.match(endedNotice(1).detail, /^1 of your latest changes wasn't/);
  assert.equal(endedNotice(0).detail, "You can keep reading live.");
});
