import { PlaceTool } from './place.js';

// Frame tool (F). Frames go to the back of the stacking order.
export class FrameTool extends PlaceTool {
  constructor(app) { super(app, 'frame', [600, 400]); }

  extra() {
    const n = [...this.app.state.all()].filter((el) => el.type === 'frame').length + 1;
    return { text: `Frame ${n}` };
  }
}
