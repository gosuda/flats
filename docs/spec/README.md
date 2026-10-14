# Product specs

These pages hold what Flats must do and why. Development follows them; a
change that needs to break one updates the page first.

- `<name>.md` is the product spec (PRD): principles and requirements,
  independent of any implementation. It may describe a target that is not
  implemented yet.
- `<name>-TECH.md`, when present, is the technical design of that spec.
  Principles that span several features, such as the content data models,
  have no TECH page.
- Contracts and references elsewhere in `docs/` (`design.md`,
  `lifecycle-*.md`, the generated references, `agent/`) describe how the
  implemented system behaves now.

A spec whose target is not fully implemented starts with a status line, keeps
a "Current state" section listing the differences, and lists undecided points
under "Open questions". When a question is decided, its answer moves into the
spec and leaves that list.

| Spec | Covers |
| --- | --- |
| [Content data models](content-models.md) | The Version model (websites, pages) and the Revision model (documents, design) |
| [Design content type](design-type.md) ([technical design](design-type-TECH.md)) | `type: "design"`: a canvas of HTML artboards people and agents edit live, with revisions |
| [Page content type](page-type.md) ([technical design](page-type-TECH.md)) | `type: "page"`: one static HTML document with assets, on the website lifecycle |
| [Agent self-update](agent-self-update.md) ([technical design](agent-self-update-TECH.md)) | Client compatibility checks and explicit updates of the CLI, connection skill and guidance |
