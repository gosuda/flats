import { PlaceTool } from './place.js';

// Text tool (T); double-clicking an empty spot also creates text (rule 16).
export class TextTool extends PlaceTool {
  constructor(app) { super(app, 'text', [240, 36]); }

  extra() { return { text: '' }; }

  createAt(p) {
    if (!this.app.canEdit) return;
    this.create({ x: p[0], y: p[1] - 18, w: 240, h: 36 });
  }
}
