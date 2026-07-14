// Roger (courtesy) tones — the beep a radio sends when the operator releases
// PTT, telling everyone else the transmission has ended.
//
// The specs below are the real ones, not invented:
//   - "quindar" is NASA's Apollo outro tone: a 2475 Hz sine for 250 ms. (The
//     matching 2525 Hz intro tone signalled key-down.)
//   - "nasa" is the 2450 Hz / 200 ms "over" beep.
//   - "cuckoo" is the two-tone beep Baofeng handhelds send: 1123 Hz for 136 ms
//     then 865 Hz for 202 ms.
//   - the rest are classic repeater courtesy tones (Bumble Bee, Piano Chord,
//     Shooting Star, Stardust, Tumbleweed, Doorbell, Beep, Boop, Ascending,
//     Descending) as published in the repeater-builder courtesy-tone tables.
//   - "nextel" is the familiar 1760 Hz triple chirp (30 ms on / 30 ms off).
//
// A step is either {f:[hz,…], ms:n} (one or more frequencies sounded together)
// or {gap:n} (silence). Steps play back to back.
window.Roger = (function () {
  const TONES = {
    off: null,

    // Single beeps.
    classic: [{ f: [880], ms: 100 }],                     // repeater "Beep"
    boop: [{ f: [440], ms: 100 }],                        // repeater "Boop"
    honk: [{ f: [500, 700], ms: 100 }],                   // repeater "Honk"

    // Spacecraft.
    quindar: [{ f: [2475], ms: 250 }],                    // Apollo outro tone
    nasa: [{ f: [2450], ms: 200 }],                       // NASA "over" beep

    // Handheld / commercial.
    cuckoo: [{ f: [1123], ms: 136 }, { f: [865], ms: 202 }], // Baofeng
    nextel: [
      { f: [1760], ms: 30 }, { gap: 30 },
      { f: [1760], ms: 30 }, { gap: 30 },
      { f: [1760], ms: 30 },
    ],

    // Repeater courtesy tones.
    bumblebee: [{ f: [330], ms: 100 }, { f: [495], ms: 100 }, { f: [660], ms: 100 }],
    piano: [{ f: [660, 880], ms: 100 }],                  // "Piano Chord"
    shootingstar: [{ f: [800], ms: 100 }, { f: [800], ms: 100 }, { f: [540], ms: 100 }],
    stardust: [{ f: [750], ms: 125 }, { f: [880], ms: 80 }, { f: [880, 1200], ms: 80 }],
    wasp: [{ f: [660], ms: 100 }, { f: [500], ms: 100 }, { f: [385], ms: 100 }],
    tumbleweed: [{ f: [1000], ms: 20 }, { f: [800], ms: 20 }, { f: [600], ms: 20 }],
    doorbell: [{ f: [800], ms: 75 }, { f: [400], ms: 50 }],
    chirp: [{ f: [500], ms: 50 }, { f: [750], ms: 50 }, { f: [1000], ms: 50 }],   // "Ascending"
    descending: [{ f: [1000], ms: 50 }, { f: [750], ms: 50 }, { f: [500], ms: 50 }],
  };

  // Display order + labels for the pickers.
  const ORDER = [
    ['off', 'Off'],
    ['classic', 'Classic beep (880 Hz)'],
    ['boop', 'Boop (440 Hz)'],
    ['honk', 'Honk (500+700 Hz)'],
    ['quindar', 'Quindar — Apollo (2475 Hz)'],
    ['nasa', 'NASA “over” (2450 Hz)'],
    ['cuckoo', 'Cuckoo — Baofeng'],
    ['nextel', 'Nextel chirp'],
    ['bumblebee', 'Bumble Bee'],
    ['piano', 'Piano chord'],
    ['shootingstar', 'Shooting Star'],
    ['stardust', 'Stardust'],
    ['wasp', 'Wasp'],
    ['tumbleweed', 'Tumbleweed'],
    ['doorbell', 'Doorbell'],
    ['chirp', 'Ascending chirp'],
    ['descending', 'Descending'],
  ];

  // The channel alert ("ring") — a radio's call-alert / page tone. Deliberately
  // longer, louder and more insistent than any roger beep: it must pull someone
  // back to the tab. Two urgent double-chirps, a beat apart.
  const ALERT = [
    { f: [1046, 1318], ms: 150 }, { gap: 80 }, { f: [1046, 1318], ms: 150 },
    { gap: 260 },
    { f: [1046, 1318], ms: 150 }, { gap: 80 }, { f: [1046, 1318], ms: 250 },
  ];

  let actx = null;

  // unlock creates/resumes the AudioContext. Browsers start it suspended until
  // a user gesture, so this MUST be called from a real click/keypress —
  // otherwise a listener who never touched the page hears no roger tone.
  function unlock() {
    try {
      actx = actx || new (window.AudioContext || window.webkitAudioContext)();
      if (actx.state === 'suspended') return actx.resume();
    } catch (e) { /* audio unavailable */ }
    return Promise.resolve();
  }

  // suspended reports whether audio is still gated by the autoplay policy.
  function suspended() {
    return !actx || actx.state === 'suspended';
  }

  // ctx exposes the one AudioContext, so the signal meter analyses audio on the
  // same context the unlock path resumes — a second context would stay gated.
  function ctx() {
    try {
      actx = actx || new (window.AudioContext || window.webkitAudioContext)();
    } catch (e) { return null; }
    return actx;
  }

  // playSpec renders a step list through WebAudio. gain scales the whole thing
  // (the channel alert is louder than a courtesy beep).
  function playSpec(spec, gain) {
    if (!spec) return;
    try {
      actx = actx || new (window.AudioContext || window.webkitAudioContext)();
      if (actx.state === 'suspended') actx.resume();
      let t = actx.currentTime + 0.01;
      for (const step of spec) {
        if (step.gap) { t += step.gap / 1000; continue; }
        const dur = step.ms / 1000;
        // Short attack/release so each element re-attacks (two identical
        // consecutive tones still read as two beeps) without clicking.
        const edge = Math.min(0.006, dur / 3);
        // Split headroom across a chord's partials so it doesn't clip.
        const peak = (gain || 0.22) / Math.sqrt(step.f.length);
        for (const hz of step.f) {
          const o = actx.createOscillator(), g = actx.createGain();
          o.frequency.value = hz;
          o.connect(g); g.connect(actx.destination);
          g.gain.setValueAtTime(0.0001, t);
          g.gain.exponentialRampToValueAtTime(peak, t + edge);
          g.gain.setValueAtTime(peak, t + dur - edge);
          g.gain.exponentialRampToValueAtTime(0.0001, t + dur);
          o.start(t); o.stop(t + dur + 0.01);
        }
        t += dur;
      }
    } catch (e) { /* audio unavailable — stay silent */ }
  }

  // play synthesizes the named roger tone. Unknown names and "off" are silent.
  function play(style) { playSpec(TONES[style]); }

  // ring plays the channel alert — louder than a roger beep, on purpose.
  function ring() { playSpec(ALERT, 0.34); }

  // fill populates a <select> with every tone and selects `current`.
  function fill(sel, current) {
    if (!sel) return;
    sel.innerHTML = '';
    for (const [value, label] of ORDER) {
      const o = document.createElement('option');
      o.value = value; o.textContent = label;
      sel.appendChild(o);
    }
    sel.value = TONES[current] !== undefined ? current : 'classic';
  }

  return { play, ring, fill, unlock, suspended, ctx, TONES, ORDER, ALERT };
})();
