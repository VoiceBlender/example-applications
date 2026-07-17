// Channel — one watched PTT channel: its signalling socket, floor state, on-demand
// WebRTC leg and its own <audio> sink. The room page runs one Channel per channel
// the user is watching, so several channels' audio play at once. Only the "active"
// channel is wired to the shared PTT bar and signal meter; every other channel is a
// receive-only monitor.
//
// This is the per-channel half of the old room.html script, extracted verbatim so
// the shell can instantiate it N times. The channel keeps its state internally and
// calls hooks.onState(state) whenever anything structural changes; the shell renders
// from that snapshot (the active channel drives the chrome, the rest render as strips).
window.Channel = function (roomID, hooks) {
  hooks = hooks || {};
  const id = String(roomID);

  let active = false;
  let me = '', name = id, owner = false, invite = '', roger = 'off';
  let users = [], speaker = '';
  let mode = 'idle';          // 'idle' | 'speaking' | 'listening'
  let conn = 'connecting';    // 'connecting' | 'connected' | 'reconnecting'
  let stText = '', stKind = '';
  let pressed = false;
  // Comms-radio band-limit filter (a per-room "Radio bandwidth" option chosen at
  // creation): when radioOn, incoming audio is band-passed to [radioLow, radioHigh]
  // Hz so voices sound "over the air".
  let radioOn = false, radioLow = 500, radioHigh = 2000;

  // A dedicated sink per channel so monitored channels keep playing while another
  // is active. The shell mounts these somewhere hidden.
  const audio = document.createElement('audio');
  audio.autoplay = true;
  audio.setAttribute('data-room', id);
  (hooks.sinkMount || document.body).appendChild(audio);

  function snapshot() {
    return { id, active, me, name, owner, invite, roger,
      radio: radioOn, radioLow, radioHigh,
      users: users.slice(), speaker, mode, conn, status: { text: stText, kind: stKind } };
  }
  function emit() { if (hooks.onState) hooks.onState(snapshot()); }
  function status(text, kind) { stText = text || ''; stKind = kind || ''; emit(); }

  // ── signalling WebSocket ──────────────────────────────────────────────────
  let ws = null, backoff = 700, hbTimer = null, closed = false;
  function send(o) { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(o)); }
  function connect() {
    if (closed) return;
    const proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    ws = new WebSocket(proto + location.host + '/api/ptt/stream?room=' + encodeURIComponent(id));
    ws.onopen = () => {
      backoff = 700; conn = 'connected'; emit();
      clearInterval(hbTimer); hbTimer = setInterval(() => send({ type: 'ping' }), 10000);
    };
    ws.onmessage = (m) => { let msg; try { msg = JSON.parse(m.data); } catch (e) { return; } handle(msg); };
    ws.onclose = () => {
      if (closed) return;
      conn = 'reconnecting'; emit();
      clearInterval(hbTimer); stopMedia();
      setTimeout(connect, backoff); backoff = Math.min(backoff * 2, 8000);
    };
    ws.onerror = () => { try { ws.close(); } catch (e) {} };
  }

  function handle(msg) {
    switch (msg.type) {
      case 'hello':
        me = msg.user || ''; name = msg.name || name; owner = !!msg.owner; invite = msg.invite || '';
        if (msg.roger !== undefined) roger = msg.roger || 'off';
        applyRadioConfig(msg);
        emit(); break;
      case 'config':
        if (msg.roger !== undefined) roger = msg.roger || 'off';
        if (applyRadioConfig(msg)) reevaluatePlayback();   // re-route only if the filter changed
        emit(); break;
      case 'presence':
        users = msg.users || []; setSpeaker(msg.speaker || ''); break;
      case 'speaker':
        setSpeaker(msg.speaker !== undefined ? msg.speaker : (msg.user || '')); break;
      case 'floor.granted': mode = 'speaking'; startSpeaking(); break;
      case 'floor.denied': status((msg.by || 'someone') + ' has the floor', 'other'); break;
      case 'floor.error': status(msg.message || 'channel error', ''); releasePreheat(); break;
      case 'listen.start': mode = 'listening'; startListening(); break;
      case 'stop': stopMedia(); break;
      case 'ring': if (hooks.onRing) hooks.onRing(id, msg.from || ''); break;
      case 'ring.throttled': if (hooks.onRingThrottled) hooks.onRingThrottled(id, msg.wait || 1); break;
      // Activity: 'history' is the recent log on connect; 'event' is one live
      // event. Both flow to the shell's combined feed via onActivity.
      case 'history': if (hooks.onActivity) hooks.onActivity(id, msg.events || []); break;
      case 'event': if (hooks.onActivity && msg.event) hooks.onActivity(id, [msg.event]); break;
      case 'webrtc.answer': onAnswer(msg); break;
      case 'webrtc.candidate': onCandidate(msg.candidate); break;
      case 'webrtc.error': status('audio error', ''); break;
      case 'pong': break;
    }
  }

  function setSpeaker(user) {
    const prev = speaker;
    speaker = user || '';
    // A transmission just ended (someone → nobody): play this channel's roger tone.
    if (prev && !speaker) Roger.play(roger);
    if (!speaker) { if (mode !== 'speaking') { stText = 'channel clear'; stKind = ''; } }
    else if (speaker === me) { /* status set by startSpeaking */ }
    else { stText = speaker + ' is talking'; stKind = 'other'; }
    emit();
  }

  // ── WebRTC (on-demand: one peer connection per talk burst) ─────────────────
  let pc = null, micStream = null, preheating = null, legID = '', remoteStream = null;
  let radioNodes = null, radioOut = null;   // WebAudio band-pass filter graph (radio mode)

  // applyRadioConfig folds the room's radio settings from a hello/config message
  // into local state; returns whether they changed (so callers can re-route audio).
  function applyRadioConfig(msg) {
    const before = radioOn + '|' + radioLow + '|' + radioHigh;
    if (msg.radio !== undefined) radioOn = !!msg.radio;
    if (msg.radio_low) radioLow = msg.radio_low;
    if (msg.radio_high) radioHigh = msg.radio_high;
    return before !== (radioOn + '|' + radioLow + '|' + radioHigh);
  }

  // routePlayback plays the incoming stream. In radio mode it band-passes it
  // through WebAudio (the <audio> element is muted but must still play() so the
  // remote track flows into WebAudio); otherwise it plays the element directly.
  function routePlayback(stream) {
    audio.srcObject = stream;
    if (radioOn) {
      audio.muted = true;
      applyRadio(stream);
      if (!radioOut) audio.muted = false;   // filter unavailable → play the element directly rather than go silent
    } else {
      clearRadio();
      audio.muted = false;
    }
    const pr = audio.play(); if (pr && pr.catch) pr.catch(() => { if (hooks.onAudioBlocked) hooks.onAudioBlocked(); });
  }

  // applyRadio builds the band-pass filter graph (highpass + lowpass → output),
  // playing to the shared Roger AudioContext. radioOut is tapped by the meter.
  function applyRadio(stream) {
    clearRadio();
    const actx = Roger.ctx(); if (!actx) return;
    let src;
    try { src = actx.createMediaStreamSource(stream); } catch (e) { return; }
    const hp = actx.createBiquadFilter(); hp.type = 'highpass'; hp.frequency.value = radioLow; hp.Q.value = 0.7;
    const lp = actx.createBiquadFilter(); lp.type = 'lowpass'; lp.frequency.value = radioHigh; lp.Q.value = 0.7;
    const out = actx.createGain(); out.gain.value = 1.0;
    src.connect(hp); hp.connect(lp); lp.connect(out); out.connect(actx.destination);
    radioNodes = [src, hp, lp, out];
    radioOut = out;
    if (Roger.suspended() && hooks.onAudioBlocked) hooks.onAudioBlocked();
  }
  function clearRadio() {
    if (radioNodes) { for (const n of radioNodes) { try { n.disconnect(); } catch (e) {} } radioNodes = null; }
    radioOut = null;
  }

  // reevaluatePlayback re-routes the currently-playing stream when the room's
  // radio setting toggles live (via a config message).
  function reevaluatePlayback() {
    if (!remoteStream) return;
    if (active) Meter.stop();
    routePlayback(remoteStream);
    updateMeter();
  }

  // updateMeter points the (singleton) signal meter at the active channel's audio:
  // the mic while transmitting, the filter output in radio mode, else the raw
  // incoming stream. Tapping radioOut (not a second stream source) is what keeps
  // the filtered audio audible.
  function updateMeter() {
    if (!active) return;
    if (mode === 'speaking' && micStream) { Meter.attach(micStream); return; }
    if (radioOn && radioOut) { Meter.attachNode(radioOut); return; }
    if (remoteStream) { Meter.attach(remoteStream); return; }
    Meter.stop();
  }

  function newPC() {
    const p = new RTCPeerConnection({ iceServers: [{ urls: 'stun:stun.l.google.com:19302' }] });
    p.ontrack = (ev) => {
      remoteStream = ev.streams[0];
      routePlayback(remoteStream);
      updateMeter();
    };
    p.onicecandidate = (ev) => { if (ev.candidate) send({ type: 'webrtc.candidate', candidate: ev.candidate.toJSON() }); };
    p.onconnectionstatechange = () => {
      if (!pc) return;
      if (pc.connectionState === 'connected' && mode === 'speaking') { status('you are live', 'talk'); beep(); }
      else if (pc.connectionState === 'failed') { status('audio failed', ''); }
    };
    return p;
  }
  async function offer() {
    try {
      const o = await pc.createOffer();
      await pc.setLocalDescription(o);
      send({ type: 'webrtc.offer', sdp: pc.localDescription.sdp });
    } catch (e) { status('offer error', ''); }
  }
  function preheatMic() {
    if (micStream || preheating) return;
    preheating = navigator.mediaDevices.getUserMedia({ audio: true })
      .then(s => { micStream = s; preheating = null; return s; })
      .catch(() => { preheating = null; status('microphone denied', ''); });
  }
  function releasePreheat() {
    if (mode !== 'speaking' && micStream) { micStream.getTracks().forEach(t => t.stop()); micStream = null; }
  }
  async function ensureMic() {
    if (micStream) return micStream;
    if (preheating) return await preheating;
    preheatMic(); return await preheating;
  }
  async function startSpeaking() {
    status('opening channel…', 'talk');
    pc = newPC();
    const s = await ensureMic();
    if (!s) return;
    s.getAudioTracks().forEach(t => pc.addTrack(t, s));
    updateMeter();   // while transmitting, the meter shows your own voice
    await offer();
  }
  async function startListening() {
    pc = newPC();
    pc.addTransceiver('audio', { direction: 'recvonly' });
    await offer();
  }
  function onAnswer(msg) {
    legID = msg.leg_id || '';
    if (!pc) return;
    pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp }).catch(() => status('sdp error', ''));
  }
  function onCandidate(c) { if (pc && c) pc.addIceCandidate(c).catch(() => {}); }
  function stopMedia() {
    mode = 'idle';
    if (active) Meter.stop();
    clearRadio();
    remoteStream = null;
    if (pc) { try { pc.close(); } catch (e) {} pc = null; }
    if (micStream) { micStream.getTracks().forEach(t => t.stop()); micStream = null; }
    legID = '';
    emit();
  }

  // Short "go ahead" tone once the speaker's leg connects. Shares Roger's one
  // AudioContext so it plays on the same context the unlock path resumes.
  function beep() {
    try {
      const actx = Roger.ctx(); if (!actx) return;
      const o = actx.createOscillator(), g = actx.createGain();
      o.frequency.value = 880; o.connect(g); g.connect(actx.destination);
      g.gain.setValueAtTime(0.0001, actx.currentTime);
      g.gain.exponentialRampToValueAtTime(0.2, actx.currentTime + 0.02);
      g.gain.exponentialRampToValueAtTime(0.0001, actx.currentTime + 0.16);
      o.start(); o.stop(actx.currentTime + 0.18);
    } catch (e) {}
  }

  // ── public surface ─────────────────────────────────────────────────────────
  // press/release drive floor control; the shell only ever calls them on the
  // active channel, so a person's one mic transmits on one channel at a time.
  function press() {
    if (pressed) return false;
    if (speaker && speaker !== me) return false;   // can't grab the floor while someone else holds it
    pressed = true;
    preheatMic();                                  // pre-warm the mic so audio starts fast on grant
    send({ type: 'ptt.press' });
    return true;
  }
  function release() {
    if (!pressed) return;
    pressed = false;
    send({ type: 'ptt.release' });
  }
  function ring() { send({ type: 'ring' }); }

  // setActive toggles whether this channel owns the shared meter. It resets the
  // meter to whatever the newly-active channel is doing, so switching never leaves
  // a frozen reading from the previous channel.
  function setActive(v) {
    active = !!v;
    if (active) updateMeter();
    else Meter.stop();
    emit();
  }

  // unlockAudio re-plays the sink after a user gesture (browsers gate audio until
  // one). The shell calls this on every channel on the first gesture.
  function unlockAudio() {
    if (audio.srcObject) { const pr = audio.play(); if (pr && pr.catch) pr.catch(() => {}); }
  }

  function close() {
    closed = true;
    clearInterval(hbTimer);
    release();
    if (active) Meter.stop();
    stopMedia();
    try { if (ws) ws.close(); } catch (e) {}
    try { audio.pause(); audio.srcObject = null; audio.remove(); } catch (e) {}
  }

  connect();
  return { id, press, release, ring, setActive, unlockAudio, close, state: snapshot };
};
