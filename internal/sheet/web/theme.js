// Theme and layout (rule 20). Colors live in app.css as custom properties
// for light and dark; this module only tracks the scheme and the phone
// layout so components can react without measuring. On a phone it also
// follows the visual viewport: the on-screen keyboard covers the bottom of
// the page without resizing it, and the app then takes the part that stays
// visible, so the grid and the tab bar end above the keyboard.
'use strict';

export const PHONE = '(max-width: 599px)';

export class Theme {
  constructor(onChange = () => {}) {
    this.onChange = onChange;
    const mm = globalThis.matchMedia;
    this.dark = mm ? mm('(prefers-color-scheme: dark)') : { matches: false };
    this.phoneQuery = mm ? mm(PHONE) : { matches: false };
    for (const q of [this.dark, this.phoneQuery]) if (q.addEventListener) q.addEventListener('change', () => this.apply());
    this.viewport = globalThis.visualViewport || null;
    if (this.viewport) for (const type of ['resize', 'scroll']) this.viewport.addEventListener(type, () => this.apply());
    this.apply();
  }

  get scheme() { return this.dark.matches ? 'dark' : 'light'; }
  get phone() { return this.phoneQuery.matches; }

  apply() {
    const root = document.documentElement;
    if (root?.dataset) {
      root.dataset.scheme = this.scheme;
      root.dataset.layout = this.phone ? 'phone' : 'wide';
      // A pinch zoom shrinks the visual viewport too; only an unzoomed page follows it.
      const v = this.phone && this.viewport?.scale === 1 ? this.viewport : null;
      root.style.setProperty('--view-height', v ? `${Math.round(v.height)}px` : '100%');
      root.style.setProperty('--view-top', v ? `${Math.round(v.offsetTop)}px` : '0px');
    }
    this.onChange(this);
  }
}
