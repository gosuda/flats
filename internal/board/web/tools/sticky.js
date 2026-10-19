import { PlaceTool } from './place.js';

// Sticky tool (S).
export class StickyTool extends PlaceTool {
  constructor(app) { super(app, 'sticky', [200, 200]); }

  extra() { return { text: '' }; }
}
