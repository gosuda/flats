// boot.js runs before the app modules. The shell names every app file by
// its content version, and a host serves only the versions it has: a page
// whose shell came from an earlier Flats binary gets a 404 for each file
// that changed since (boot.js itself is served in any version, so it always
// runs). When one of the shell's scripts or stylesheets fails to load, the
// page loads once more, which brings a shell and files of one version. It
// does not reload again within 10 seconds, so a host that keeps failing
// shows the failure instead of reloading forever. This file must stay
// self-contained: no imports, nothing that depends on the other files.
(() => {
  const key = 'flats-board-reload';
  addEventListener('error', (e) => {
    const t = e.target;
    if (!(t instanceof HTMLScriptElement || t instanceof HTMLLinkElement)) return;
    try {
      if (Date.now() - (Number(sessionStorage.getItem(key)) || 0) < 10000) return;
      sessionStorage.setItem(key, String(Date.now()));
    } catch {
      return; // without storage a reload could repeat forever
    }
    location.reload();
  }, true);
})();
