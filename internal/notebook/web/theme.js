// Theme and layout (rules 16 and 17). Colors live in app.css as custom
// properties for light and dark; this module only tracks the scheme and the
// phone layout so components can react without measuring.
'use strict';


export const PHONE = '(max-width: 720px)';

export class Theme {
  constructor(onChange = () => {}) {
    this.onChange = onChange;
    const mm = globalThis.matchMedia;
    this.dark = mm ? mm('(prefers-color-scheme: dark)') : { matches: false };
    this.phoneQuery = mm ? mm(PHONE) : { matches: false };
    for (const q of [this.dark, this.phoneQuery]) if (q.addEventListener) q.addEventListener('change', () => this.apply());
    this.apply();
  }

  get scheme() { return this.dark.matches ? 'dark' : 'light'; }
  get phone() { return this.phoneQuery.matches; }

  apply() {
    const root = document.documentElement;
    if (root && root.dataset) {
      root.dataset.scheme = this.scheme;
      root.dataset.layout = this.phone ? 'phone' : 'wide';
    }
    this.onChange(this);
  }
}
