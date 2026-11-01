'use strict';

import test from 'node:test';
import assert from 'node:assert/strict';
import { installDOM } from './dom.mjs';
import { cellDisplay } from '../dbschema.js';
import { renderRuns } from '../richtext.js';

for (const value of ['https://example.com/a?b=c', 'HTTP://example.com/', 'mailto:person@example.com', 'javascript:alert(1)', 'JaVaScRiPt:alert(1)', 'data:text/html,<script>alert(1)</script>', '//example.com/', ' https://example.com/', '/relative']) {
  test(`URL cells and rich-text links apply the same scheme check to ${value}`, () => {
    installDOM();
    const rich = renderRuns([{ text: value, marks: [], link: { url: value } }])[0];
    const cell = cellDisplay({ options: {} }, { id: 'url', type: 'url' }, value);
    if (rich.nodeName === 'A') {
      assert.equal(cell.nodeName, 'A');
      assert.equal(cell.getAttribute('href'), rich.getAttribute('href'));
      assert.equal(cell.getAttribute('target'), '_blank');
      assert.equal(cell.getAttribute('rel'), 'noopener noreferrer');
    } else {
      assert.equal(cell, value, 'an unsafe URL is text, not an anchor');
    }
  });
}
