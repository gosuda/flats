import { PlaceTool } from './place.js';

// Shape tool: R rectangle, O ellipse, D diamond (TECH 11.2).
export class ShapeTool extends PlaceTool {
  constructor(app) {
    super(app, 'shape', [160, 100]);
    this.shape = 'rect';
  }

  activate(opts) { if (opts && opts.shape) this.shape = opts.shape; }

  extra() { return { text: '', style: { shape: this.shape } }; }
}
