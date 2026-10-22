// Anonymous public editing (docs-public-edit.md rules 8 to 13, 22 and 29;
// TECH 4): guest names, the tab's choice to join or only read, the hello
// fields they send and the texts of guest refusals. Kept free of the DOM so
// node tests cover every popup decision; guest-ui.js draws them.

export const GUEST_NAME_MAX = 40;
export const VIEWERS_LISTED = 20;

// Browser storage. The join and View only choices hold the access epoch
// they were made at, so a later edit period (a newer epoch) asks again.
export const KEYS = Object.freeze({
  guestToken: "flats-docs-guest-token",
  viewerToken: "flats-docs-viewer-token",
  viewOnly: "flats-docs-view-only",
  // The epoch this tab joined at. Rejoin sets it before reloading; it also
  // keeps a guest a guest across reloads and moves between documents.
  joined: "flats-docs-rejoin",
  guestName: "flats-docs-guest-name",
});

// Control (Cc) and format (Cf) characters are removed, then the name is
// trimmed, as the server's cleanGuestName does (TECH 3.3).
const UNWANTED = /[\p{Cc}\p{Cf}]/gu;

export const runeCount = (s) => [...s].length;

export function cleanGuestName(s) {
  return String(s ?? "")
    .replace(UNWANTED, "")
    .trim();
}

// guestNameError is "" for a name the server accepts after cleaning, or
// the reason it would refuse it.
export function guestNameError(s) {
  const n = runeCount(cleanGuestName(s));
  if (!n) return "Type a name to join.";
  if (n > GUEST_NAME_MAX) return `Use at most ${GUEST_NAME_MAX} characters.`;
  return "";
}

// browserStore reads and writes the keys above. Storage can be blocked
// (private windows, sandboxed frames); values then last for this page only.
export function browserStore(
  session = () => globalThis.sessionStorage,
  local = () => globalThis.localStorage,
) {
  const memory = new Map();
  const area = (key) => (key === KEYS.guestName ? local() : session());
  return {
    get(key) {
      try {
        const value = area(key).getItem(key);
        if (value !== null) return value;
      } catch {
        // Storage blocked: fall back to this page's memory.
      }
      return memory.get(key) || "";
    },
    set(key, value) {
      const v = value === null || value === undefined ? "" : String(value);
      if (v) memory.set(key, v);
      else memory.delete(key);
      try {
        if (v) area(key).setItem(key, v);
        else area(key).removeItem(key);
      } catch {
        // Storage blocked: the memory copy above serves this page.
      }
    },
  };
}

const epochOf = (m) =>
  Number.isSafeInteger(m.access_epoch) ? String(m.access_epoch) : "";

// JoinState decides when the name popup opens and what each connection
// asks for. kind is what the last welcome made this page: "member" (a
// Private route), "guest" or "viewer"; "" before the first welcome.
// prompt is the popup that should be open: null, or {mode, reason} with
// mode "join" (reason "arrival", "opened", "join" or "invalid") or "change".
// Methods return what the page does next: "none", "prompt", "close"
// (close the popup), "reconnect", "full" or "reload".
export class JoinState {
  constructor(store) {
    this.store = store;
    this.kind = "";
    this.access = "";
    this.epoch = "";
    this.label = "";
    this.prompt = null;
    // Whether the next hello asks to join as a guest.
    this.join = !!(store.get(KEYS.joined) && this.name());
    // Read was chosen before any welcome said which epoch it belongs to.
    this.readNext = false;
  }

  name() {
    return cleanGuestName(this.store.get(KEYS.guestName));
  }

  // hello holds the guest and viewer fields of the next hello: guest only
  // when this tab chose to join, the tokens whenever it holds them, and
  // nick, the page's collaborator name, always.
  hello(nick) {
    const out = {},
      name = this.name(),
      token = this.store.get(KEYS.guestToken),
      viewer = this.store.get(KEYS.viewerToken);
    if (this.join && name) {
      out.guest = { name };
      if (token) out.guest.token = token;
    }
    if (viewer) out.viewer_token = viewer;
    if (nick) out.nick = nick;
    return out;
  }

  welcome(m) {
    if (typeof m.guest_token === "string" && m.guest_token)
      this.store.set(KEYS.guestToken, m.guest_token);
    if (typeof m.viewer_token === "string" && m.viewer_token)
      this.store.set(KEYS.viewerToken, m.viewer_token);
    this.access = m.access === "view" || m.access === "edit" ? m.access : "";
    this.epoch = epochOf(m);
    const kind = m.me?.kind;
    if (kind !== "guest" && kind !== "viewer") {
      this.kind = "member";
      this.prompt = null;
      return "none";
    }
    this.label = kind === "viewer" ? String(m.me.label || "Viewer") : "";
    if (this.readNext && this.epoch) {
      this.readNext = false;
      this.store.set(KEYS.viewOnly, this.epoch);
    }
    if (kind === "guest") {
      const joined = this.store.get(KEYS.joined);
      // A join chosen before the epoch was known belongs to this one.
      if (this.join && (!joined || joined === this.epoch)) {
        this.kind = "guest";
        this.store.set(KEYS.joined, this.epoch);
        const name = cleanGuestName(m.me.name);
        if (name) this.store.set(KEYS.guestName, name);
        if (this.prompt?.mode === "join") this.prompt = null;
        return "none";
      }
      // Joined in an earlier edit period that this tab missed: nobody
      // edits without choosing to in this one, so read and ask again.
      this.leave();
      this.kind = "viewer";
      return "reconnect";
    }
    this.kind = "viewer";
    // Admitted as a reader although this tab asked to edit: the edit
    // period it joined in is over.
    if (this.join) this.leave();
    return this.offer("arrival");
  }

  // level handles a level frame: the flat's access changed.
  level(m) {
    this.access = m.access === "view" || m.access === "edit" ? m.access : "";
    const epoch = epochOf(m);
    if (epoch) this.epoch = epoch;
    if (this.kind !== "viewer") return "none";
    return this.offer("opened");
  }

  // offer opens the popup for a reader at level edit unless one is open or
  // this tab chose View only in the current edit period, and closes a join
  // popup when the level is no longer edit.
  offer(reason) {
    if (this.access !== "edit") {
      if (this.prompt?.mode !== "join") return "none";
      this.prompt = null;
      return "close";
    }
    if (this.prompt) return "none";
    if (this.epoch && this.store.get(KEYS.viewOnly) === this.epoch)
      return "none";
    this.prompt = { mode: "join", reason };
    return "prompt";
  }

  // canJoin is true while the header offers Join to edit.
  canJoin() {
    return this.kind === "viewer" && this.access === "edit";
  }

  // openJoin is the header's Join to edit.
  openJoin() {
    if (!this.canJoin() || this.prompt) return "none";
    this.prompt = { mode: "join", reason: "join" };
    return "prompt";
  }

  // openChange is the header's Change name.
  openChange() {
    if (this.kind !== "guest" || this.prompt) return "none";
    this.prompt = { mode: "change" };
    return "prompt";
  }

  // submit takes the popup's name: Join and edit, or Save name.
  submit(raw) {
    if (guestNameError(raw)) return "none";
    this.store.set(KEYS.guestName, cleanGuestName(raw));
    const change = this.prompt?.mode === "change";
    this.prompt = null;
    if (!change) {
      this.store.set(KEYS.viewOnly, "");
      this.store.set(KEYS.joined, this.epoch);
      this.join = true;
    }
    return "reconnect";
  }

  // dismiss is View only, or closing the popup: the tab keeps reading
  // until the level becomes edit again.
  dismiss() {
    const mode = this.prompt?.mode;
    this.prompt = null;
    if (mode === "join") this.readOnly();
    return "none";
  }

  readOnly() {
    this.leave();
    if (this.epoch) this.store.set(KEYS.viewOnly, this.epoch);
    else this.readNext = true;
  }

  leave() {
    this.join = false;
    this.store.set(KEYS.joined, "");
  }

  // ended: the owner turned off guest editing (rule 29). The page
  // reconnects as a viewer.
  ended() {
    this.leave();
    this.kind = "viewer";
    this.prompt = null;
    return "reconnect";
  }

  // full: the guest pool refused this join. Read keeps reading, Try again
  // asks to join again.
  full() {
    this.prompt = null;
    return "full";
  }

  read() {
    this.readOnly();
    if (this.kind === "guest") this.kind = "viewer";
    return "reconnect";
  }

  // invalidName: the server refused the name. The page reads meanwhile and
  // asks for another name; a refused name is not offered again.
  invalidName() {
    this.store.set(KEYS.guestName, "");
    this.leave();
    if (this.kind === "guest") this.kind = "viewer";
    this.prompt = { mode: "join", reason: "invalid" };
    return "reconnect";
  }

  // rejoin after a refusal: the reloaded page joins with the remembered
  // name without the popup.
  rejoin() {
    this.store.set(KEYS.joined, this.epoch || this.store.get(KEYS.joined));
    return "reload";
  }
}

// viewersOf reads a viewers frame: the numbers of the viewers, or only a
// count above 20.
export function viewersOf(m) {
  if (Array.isArray(m.n)) {
    const numbers = m.n
      .filter((n) => Number.isSafeInteger(n) && n > 0)
      .sort((a, b) => a - b);
    return numbers.length > VIEWERS_LISTED
      ? { count: numbers.length, labels: [] }
      : { count: numbers.length, labels: numbers.map((n) => `Viewer ${n}`) };
  }
  const count = Number.isSafeInteger(m.count) && m.count > 0 ? m.count : 0;
  return { count, labels: [] };
}

// Refusals that stop a guest's editing (rule 22): the start of the notice,
// before ", so editing stopped.", and what to do about it.
const REFUSALS = {
  guest_size: ["This change is too large for a guest"],
  guest_room: [
    "The document has no room left for guest changes",
    "Ask the owner to make room.",
  ],
  capacity: ["The document is too large for more guest text"],
  disk_quota: ["The disk quota is full", "Ask the owner to raise the quota."],
  guest_rate: ["You sent changes faster than guests may"],
  guest_client: ["The document refused changes from this page"],
};

// refusal describes a guest refusal for the stop notice.
export function refusal(code) {
  const [reason, hint = ""] = REFUSALS[code] || [
    "The document refused a change",
  ];
  return {
    reason: reason + ", so editing stopped.",
    detail:
      (hint ? hint + " " : "") +
      (code === "guest_size"
        ? "It and what you typed after it aren't saved."
        : "Your last change and what you typed after it aren't saved.") +
      " Download them to keep your text, then rejoin to keep editing.",
  };
}

// endedNotice is the text after this page stopped editing as a guest, by
// default because the owner turned off guest editing, with the number of
// its changes that were not saved.
export function endedNotice(
  unsaved,
  reason = "The owner turned off guest editing.",
) {
  const lost =
    unsaved === 1
      ? "1 of your latest changes wasn't saved. "
      : unsaved > 1
        ? `${unsaved} of your latest changes weren't saved. `
        : "";
  return { reason, detail: lost + "You can keep reading live." };
}

export const NAME_INVALID = `This name can't be used. Use 1 to ${GUEST_NAME_MAX} characters, without control or invisible characters.`;
