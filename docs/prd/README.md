# Product principles (PRD)

These pages hold the product principles Flats is built on: what a feature must
do and why, independent of any implementation. Development follows them; a
change that needs to break one updates the page first.

How these pages differ from the rest of `docs/`:

- `docs/prd/` (here): agreed principles and requirements. They may describe a
  target that is not implemented yet, and say so in their status line.
- `docs/spec/`: the design of one feature, often with a `-TECH` page for its
  implementation.
- Contracts and references (`design.md`, `lifecycle-*.md`, generated
  references, `agent/`): how the implemented system behaves now.

Each page starts with a status line, keeps a "Current state" section while the
implementation differs, and lists undecided points under "Open questions".
When a question is decided, move the answer into the principles and remove it
from that list.

| Page | Covers |
| --- | --- |
| [Content data models](content-models.md) | The Version model (websites, pages) and the Revision model (documents, design) |
