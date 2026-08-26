// One shared AudioContext for the page.
//
// The meter needs an AudioContext, and browsers refuse to start one (or to play
// audio at all) until the user has interacted with the page. A listener in an
// interpreted call may never click anything — they just talk — so the context is
// created lazily and the page offers a one-click unlock when it is suspended.
window.AudioCtx = (function () {
  let actx = null;

  function ctx() {
    if (actx) return actx;
    const C = window.AudioContext || window.webkitAudioContext;
    if (!C) return null;
    try { actx = new C(); } catch (e) { return null; }
    return actx;
  }

  // suspended reports whether the browser is holding audio back pending a
  // gesture. The page shows its unlock banner on this.
  function suspended() {
    return !!actx && actx.state === 'suspended';
  }

  function resume() {
    const a = ctx();
    if (a && a.state === 'suspended') a.resume().catch(() => {});
  }

  return { ctx, suspended, resume };
})();
