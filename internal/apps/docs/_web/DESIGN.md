---
name: Flats Docs
description: Quiet Markdown editing and reading with visible collaboration state.
colors:
  paper: "#fff"
  canvas: "#f5f5f2"
  ink: "#242924"
  muted: "#626a62"
  line: "#dfe3dc"
  accent: "#356649"
  active: "#f3f6f1"
  selection: "#c8dfd0"
  error: "#8a3b24"
  paper-dark: "#1c211e"
  canvas-dark: "#171b18"
  ink-dark: "#e3e9e2"
  muted-dark: "#a2ada2"
  line-dark: "#363e36"
  accent-dark: "#95c6a5"
  active-dark: "#263028"
  selection-dark: "#385b43"
  error-dark: "#ffb498"
typography:
  body:
    fontFamily: "-apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "16px"
    lineHeight: 1.8
  title:
    fontFamily: "-apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "17px"
    fontWeight: 600
  headline:
    fontFamily: "-apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "2rem"
    lineHeight: 1.25
    letterSpacing: "-0.025em"
  label:
    fontFamily: "-apple-system, BlinkMacSystemFont, \"Segoe UI\", sans-serif"
    fontSize: "12px"
  source:
    fontFamily: "ui-monospace, SFMono-Regular, Consolas, monospace"
    fontSize: "14px"
    lineHeight: 1.75
rounded:
  control: "5px"
  avatar: "50%"
spacing:
  compact: "8px"
  mobile-gutter: "16px"
  pane: "24px"
  desktop-gutter: "28px"
  reading: "32px"
components:
  button:
    backgroundColor: "transparent"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "7px 13px"
  mode-selected:
    backgroundColor: "{colors.active}"
    textColor: "{colors.accent}"
    typography: "{typography.label}"
    rounded: "{rounded.control}"
    padding: "6px 11px"
  document-current:
    backgroundColor: "{colors.paper}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "10px 12px"
---

# Design System: Flats Docs

## Overview

**Creative North Star: "Operate / Read"**

Operate / Read is a quiet document workspace: compact controls frame a spacious reading surface. System typography, restrained green, and thin dividers keep Markdown and collaboration state legible. Editing and reading share one document surface: private links open in Edit, where editors edit the rendered document in place, and can switch to View to read it rendered; the public read-only view shows only that rendered surface, without an editor or mode controls.

**Key Characteristics:**

- System typography and a restrained green accent.
- Flat surfaces separated by borders and tonal contrast.
- Persistent collaboration and save state above the document.
- One centered document surface with a 16px mobile gutter.

## Colors

### Primary

Restrained green (`accent`) marks links, saved state, the selected mode and keyboard focus. Warm earth (`error`) marks interruption notices.

### Neutral

`paper` holds the main workspace; `canvas` distinguishes document navigation, and code blocks. `ink` carries content, `muted` carries supporting labels, and `line` separates regions. `active` and `selection` distinguish interaction states. The paired `-dark` tokens replace the corresponding light values under `prefers-color-scheme: dark`; CSS custom properties remain the implementation authority.

**The Restrained Accent Rule.** Use green for links, saved state, the selected mode and focus; keep document text neutral.

## Typography

Use the system sans-serif stack for the shell and for prose, both rendered and while editing, and the system monospace stack for code and for table source while it is being edited. The header title is compact and semibold; supporting state labels are smaller. Prose has a generous body line height, a tightened first-level heading, second-level headings at 1.5rem with 1.35 line height, and third-level headings at 1.15rem. Rendered paragraphs and lists stop at 72ch.

## Layout

The shell fills 100dvh. Desktop has a 76px header and a 205px document sidebar when multiple documents exist. Below it, one page scrolls a single document surface centered within a 900px maximum width, with 32px padding at the top, clamp(24px, 4vw, 64px) horizontally, and 72px at the bottom. There is no toolbar; on private links the header carries the Edit | View control.

At widths up to 899px, the header wraps, collaboration names collapse to initial avatars, navigation becomes a horizontally scrolling row, and outer gutters become 16px. Surface padding becomes 24px 16px 60px.

**The Single Surface Rule.** Keep the header stable while one document surface consumes the remaining viewport height. Edit is the rendered document edited in place (live preview); View and the public page show the same surface rendered. Never show source and rendered output in separate panes.

## Elevation & Depth

The workspace has no decorative shadows. Borders and tonal surfaces establish regions.

## Shapes

Controls, navigation items, and code blocks share gently curved corners. Collaboration initials sit in circular 25px avatars with explicit 12px type. Dividers and borders are 1px.

## Components

- **Header:** ellipsized document title (no brand, no file path), the Edit | View control on private links, live save/connection state, and collaborator presence. Saved state combines text with accent color; presence retains accessible names and verification descriptions.
- **Live-preview editor:** CodeMirror over the document's Markdown. Headings, emphasis, strikethrough, inline code, links, lists, quotes, code blocks, rules, images and tables appear rendered. Markdown syntax is hidden except on the lines a cursor or selection touches while the editor has focus, where it shows for editing; a table shows its source while the selection is inside it, and clicking a rendered table moves the cursor into it. A plain click on a link places the cursor; Cmd/Ctrl-click opens it, and Cmd/Ctrl-Enter opens the link under the cursor for keyboard users. Away from the cursor, ordered items show their rendered numbers, escapes and entities show the characters they stand for, accepted reference definitions collapse, and reference links resolve against single-line definitions anywhere in the document; anything the renderer leaves as text stays visible as written.
- **Edit | View:** a two-button group with `aria-pressed` selection, private links only; public links never render it. Private links always open in Edit. View shows the rendered document a Public visitor sees, kept live; the connection stays writable, so returning to Edit needs no reload. Selected mode uses the active surface and green text.
- **Document navigation:** muted links on canvas; the current document uses a paper surface, ink text, semibold weight, and `aria-current`.
- **Buttons:** restrained borders; hover uses the active surface. Keyboard focus is a 2px accent outline offset by 3px. Disabled buttons have 0.55 opacity.
- **Collaborator name:** no join prompt. Each browser gets a random two-word name (for example "Quiet Otter"), kept in local storage; a verified tailnet identity replaces it.
- **Footer:** "Powered by Flats" with a GitHub link, placed after the document inside the scrolling page, so it appears only at the end of the document, both while editing and when reading. It is not fixed to the viewport.
- **Interruption notice:** earth-colored text on canvas, with Download local text and Reload actions. Keep state copy concrete.
- **Reading surface:** responsive images, horizontally scrollable code and tables, a muted blockquote rule, and thin horizontal dividers.

## Do's and Don'ts

### Do:

- Do retain system fonts and automatic light/dark color-scheme support.
- Do keep mobile content gutters at 16px and avatar initials legible.
- Do use named controls, visible keyboard focus, and explicit selected states.
- Do let the document surface fill the remaining viewport height and scroll as one page.

### Don't:

- Don't add decorative shadows or a display font to the document workspace.
- Don't communicate saved state or the selected mode through color alone.
- Don't reintroduce split or source-only panes, or more modes than Edit and View.
- Don't expose mode controls in the public read-only view.
