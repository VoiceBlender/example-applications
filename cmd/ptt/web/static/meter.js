// Signal meter — the panel's one piece of live instrumentation.
//
// It is driven by the ACTUAL audio, not a canned animation: an AnalyserNode taps
// the WebRTC stream and the segments light to the real speech level. While you
// transmit it shows your microphone; while you receive it shows the incoming
// voice. That is what a signal meter on a radio does, and it is the difference
// between an instrument and a picture of one.
//
// The segments are built once and always rendered — unlit ones stay faintly
// visible (--lcd-dim), so the meter reads as a physical scale that audio fills.
window.Meter = (function () {
  const SEGMENTS = 16;

  let segs = [];        // the segment elements
  let raf = null;       // animation frame handle
  let source = null;    // MediaStreamAudioSourceNode (disconnected on stop)
  let tapped = null;    // externally-owned node we tapped (attachNode) — undone on stop
  let analyser = null;
  let buf = null;

  // build populates the meter element with its segments.
  function build(el) {
    if (!el || segs.length) return;
    for (let i = 0; i < SEGMENTS; i++) {
      const s = document.createElement('span');
      s.className = 'seg';
      el.appendChild(s);
      segs.push(s);
    }
  }

  function light(n) {
    for (let i = 0; i < segs.length; i++) {
      segs[i].classList.toggle('on', i < n);
    }
  }

  // attach starts metering a MediaStream. Safe to call repeatedly; the previous
  // source is released first.
  function attach(stream) {
    stop();
    if (!stream) return;
    const actx = window.Roger && Roger.ctx();
    if (!actx) return;
    try {
      source = actx.createMediaStreamSource(stream);
      newAnalyser(actx);
      source.connect(analyser); // analyser is a sink here — not routed to output,
                                // so metering never double-plays the audio.
    } catch (e) { return; }
    runTick();
  }

  // attachNode meters an existing AudioNode (e.g. the tail of the radio-filter
  // chain) instead of making a fresh MediaStreamAudioSource. Reusing the caller's
  // source avoids creating a second MediaStreamAudioSource from the same WebRTC
  // stream, which some browsers leave silent. The node stays owned by the caller;
  // stop() only undoes our tap.
  function attachNode(node) {
    stop();
    if (!node) return;
    const actx = window.Roger && Roger.ctx();
    if (!actx) return;
    try {
      newAnalyser(actx);
      node.connect(analyser);
      tapped = node;
    } catch (e) { return; }
    runTick();
  }

  function newAnalyser(actx) {
    analyser = actx.createAnalyser();
    analyser.fftSize = 256;
    analyser.smoothingTimeConstant = 0.6;
    buf = new Uint8Array(analyser.fftSize);
  }

  function runTick() {
    const tick = () => {
      if (!analyser) return;
      analyser.getByteTimeDomainData(buf);
      // RMS of the waveform around the 128 midpoint.
      let sum = 0;
      for (let i = 0; i < buf.length; i++) {
        const v = (buf[i] - 128) / 128;
        sum += v * v;
      }
      const rms = Math.sqrt(sum / buf.length);
      // Speech sits low in a linear scale; curve it so normal talking uses most
      // of the meter and only shouting reaches the red segments.
      const level = Math.min(1, Math.pow(rms * 3.2, 0.7));
      light(Math.round(level * SEGMENTS));
      raf = requestAnimationFrame(tick);
    };
    tick();
  }

  // stop releases the graph and drops every segment back to unlit.
  function stop() {
    if (raf) { cancelAnimationFrame(raf); raf = null; }
    if (source) { try { source.disconnect(); } catch (e) {} source = null; }
    if (tapped && analyser) { try { tapped.disconnect(analyser); } catch (e) {} }
    tapped = null;
    analyser = null; buf = null;
    light(0);
  }

  return { build, attach, attachNode, stop, SEGMENTS };
})();
