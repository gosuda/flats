'use strict';

// Pasting images into the editor (docs/spec/docs-image-paste.md). The rules
// that need no editor or network are plain functions; ImageQueue takes both
// from its caller.

// pastedImages returns the images a paste adds, or an empty list when it is an
// ordinary paste. Text wins, because spreadsheets and word processors put a
// picture of the text next to the text. A file manager puts the file's name
// next to the file, so that name alone does not count as text.
export function pastedImages(clipboard) {
  const images = Array.from(clipboard?.files ?? []).filter((f) =>
    f.type.startsWith("image/"),
  );
  if (!images.length) return images;
  const plain = (clipboard.getData("text/plain") ?? "").trim();
  if (!plain) return images;
  return images.length === 1 && plain === images[0].name ? images : [];
}

// imageMarkdown is the text to insert for an image. An image alone on its line
// gets a line break, so the caret leaves the line and the picture shows.
export function imageMarkdown(reference, before, after) {
  const image = `![](${reference})`;
  return before.trim() === "" && after.trim() === "" ? image + "\n" : image;
}

// imageURL is where to send an image for a document.
export function imageURL(doc, name) {
  const q = new URLSearchParams({ doc, name });
  return "/_docs/api/images?" + q;
}

// A refused image gets the host's reason; anything else a general one.
export async function uploadImage(file, doc, name, fetcher = fetch) {
  let response;
  try {
    response = await fetcher(imageURL(doc, name), {
      method: "POST",
      headers: {
        "X-Flats-Docs": "1",
        "Content-Type": "application/octet-stream",
      },
      body: file,
    });
  } catch {
    throw new Error(
      "The image could not be sent. Check the connection and paste it again.",
    );
  }
  const body = await response.json().catch(() => null);
  if (response.ok && typeof body?.reference === "string") return body.reference;
  throw new Error(
    typeof body?.message === "string"
      ? body.message
      : "The image could not be added.",
  );
}

// ImageQueue adds images one at a time, in the order they were pasted. Each
// place stands for where its image goes and is captured at the paste, so the
// text can keep changing meanwhile.
export class ImageQueue {
  constructor({ upload, insert, onActive, onError }) {
    this.upload = upload;
    this.insert = insert;
    this.onActive = onActive;
    this.onError = onError;
    this.waiting = [];
    this.running = false;
    this.closed = false;
  }
  get active() {
    return this.running || this.waiting.length > 0;
  }
  add(files, place) {
    if (this.closed) return;
    for (const file of files) this.waiting.push({ file, place });
    if (!this.running) this.run();
  }
  // close ends the page's part when it goes away: what is waiting is dropped,
  // and an image already on its way inserts nothing.
  close() {
    this.closed = true;
    this.waiting.length = 0;
  }
  async run() {
    this.running = true;
    this.onActive(true);
    for (let next; (next = this.waiting.shift()); ) {
      try {
        const reference = await this.upload(next.file);
        if (!this.closed) this.insert(reference, next.place);
      } catch (err) {
        if (!this.closed) this.onError(err.message);
      }
    }
    this.running = false;
    this.onActive(false);
  }
}
