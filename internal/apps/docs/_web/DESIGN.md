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

Operate / Read is a quiet document workspace: compact controls frame a spacious reading surface. System typography, restrained green, and thin dividers keep Markdown and collaboration state legible. The same visual language serves editing and the public rendered read-only view.

**Key Characteristics:**

- System typography and a restrained green accent.
- Flat surfaces separated by borders and tonal contrast.
- Persistent collaboration and save state above the document.
- Responsive panes with a 16px mobile gutter.

## Colors

### Primary

Restrained green (`accent`) marks links, saved state, selected modes and keyboard focus. Warm earth (`error`) marks interruption notices.

### Neutral

`paper` holds the main workspace; `canvas` distinguishes document navigation, and code blocks. `ink` carries content, `muted` carries supporting labels, and `line` separates regions. `active` and `selection` distinguish interaction states. The paired `-dark` tokens replace the corresponding light values under `prefers-color-scheme: dark`; CSS custom properties remain the implementation authority.

**The Restrained Accent Rule.** Use green for links, saved state, selected modes and focus; keep document text neutral.

## Typography

Use the system sans-serif stack for the shell and rendered prose, and the system monospace stack for source and code. The header title is compact and semibold; supporting state and path labels are smaller. Rendered content has a generous body line height, a tightened first-level heading, second-level headings at 1.5rem with 1.35 line height, and third-level headings at 1.15rem. Paragraphs and lists stop at 72ch. Source uses its own denser monospace rhythm.

## Layout

The shell fills 100dvh. Desktop has a 76px header, a 205px document sidebar when multiple documents exist, and a 53px toolbar. Split mode places editor and preview in equal columns. Edit and Preview use a single pane; Preview centers within a 900px maximum width. Preview padding is 32px vertically at the top, clamp(24px, 4vw, 64px) horizontally, and 72px at the bottom.

At widths up to 899px, the header wraps, collaboration names collapse to initial avatars, navigation becomes a horizontally scrolling row, and outer gutters become 16px. Mobile defaults to Edit; Split stacks two equal rows. Preview padding becomes 24px 16px 60px. The public read-only view uses Preview and hides the mode controls.

**The Remaining Height Rule.** Keep the header and toolbar stable while the document panes consume the remaining viewport height.

## Elevation & Depth

The workspace has no decorative shadows. Borders and tonal surfaces establish regions.

## Shapes

Controls, navigation items, and code blocks share gently curved corners. Collaboration initials sit in circular 25px avatars with explicit 12px type. Dividers and borders are 1px.

## Components

- **Header:** ellipsized document title (no brand, no file path), live save/connection state, and collaborator presence. Saved state combines text with accent color; presence retains accessible names and verification descriptions.
- **Modes:** Edit, Split, and Preview are a compact button group with `aria-pressed` selection. Selected mode uses the active surface and green text.
- **Document navigation:** muted links on canvas; the current document uses a paper surface, ink text, semibold weight, and `aria-current`.
- **Buttons:** restrained borders; hover uses the active surface. Keyboard focus is a 2px accent outline offset by 3px. Disabled buttons have 0.55 opacity.
- **Collaborator name:** no join prompt. Each browser gets a random two-word name (for example "Quiet Otter"), kept in local storage; a verified tailnet identity replaces it.
- **Footer:** "Powered by Flats" with a GitHub link, placed after the document inside the scrolling preview, so it appears only at the end of the document. It is not fixed to the viewport.
- **Interruption notice:** earth-colored text on canvas, with Download local text and Reload actions. Keep state copy concrete.
- **Reading surface:** responsive images, horizontally scrollable code and tables, a muted blockquote rule, and thin horizontal dividers.

## Do's and Don'ts

### Do:

- Do retain system fonts and automatic light/dark color-scheme support.
- Do keep mobile content gutters at 16px and avatar initials legible.
- Do use named controls, visible keyboard focus, and explicit selected states.
- Do let editor and preview fill the remaining viewport height and scroll within their panes.

### Don't:

- Don't add decorative shadows or a display font to the document workspace.
- Don't communicate saved state or selected mode through color alone.
- Don't expose editing modes in the public read-only view.
