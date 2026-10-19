// Hand tool: drag to pan (rule 8). The app pans for every tool on Space,
// the middle button and one-finger touch.

export class HandTool {
  constructor(app) { this.app = app; }

  down(e, p, s) { this.from = { s, cam: { ...this.app.cam } }; this.app.renderer.dyn.classList.add('panning'); }

  move(e, p, s) {
    if (!this.from) return;
    const { cam } = this.from;
    this.app.setCam(cam.x - (s[0] - this.from.s[0]) / cam.z, cam.y - (s[1] - this.from.s[1]) / cam.z, cam.z);
  }

  up() { this.cancel(); }

  cancel() { this.from = null; this.app.renderer.dyn.classList.remove('panning'); }
}
