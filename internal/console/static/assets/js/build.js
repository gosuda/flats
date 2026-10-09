// The running build, shown next to the brand: the release (v1.2.3), or the
// commit of a development build (dba5279, dba5279-dirty). It links to the
// release or commit on GitHub when one exists there.

import { h } from './dom.js';
import { extLink } from './ui.js';

const REPO = 'https://github.com/gosuda/flats';

// buildOf returns the build reported by /api/status, under system.build.
export function buildOf(status) {
  return (status && status.system && status.system.build) || null;
}

export function buildLabel(b) {
  if (!b || typeof b.version !== 'string' || !b.version) return null;
  const details = [
    b.commit ? `commit ${b.commit}${b.dirty ? ' with uncommitted changes' : ''}` : null,
    b.go,
    b.os && b.arch ? `${b.os}/${b.arch}` : null,
  ].filter(Boolean).join(' · ');
  // A development version may end in -dirty; narrow screens keep the commit.
  const dirty = !b.release && b.version.endsWith('-dirty');
  const text = dirty
    ? [b.version.slice(0, -'-dirty'.length), h('span', { class: 'build-dirty', text: '-dirty' })]
    : b.version;
  let href = null;
  if (b.release) href = `${REPO}/releases/tag/${encodeURIComponent(b.version)}`;
  else if (b.commit && !b.dirty) href = `${REPO}/commit/${encodeURIComponent(b.commit)}`;
  const cls = 'build' + (b.release ? '' : ' build-dev');
  const el = href ? extLink(href, text, cls) : h('span', { class: cls }, text);
  el.setAttribute('title', details ? `Flats ${b.version} · ${details}` : `Flats ${b.version}`);
  el.setAttribute('aria-label', `Flats version ${b.version}`);
  return el;
}
