// Session — one interpreted conversation, from the browser's side.
//
// It owns the signalling WebSocket, the single RTCPeerConnection that carries
// this participant's microphone up and the interpreted voice back down, and the
// state the page renders from.
//
// The media model is deliberately simple: one peer connection, brought up on
// join and kept for the whole session. The interpreter has to be listening
// continuously, so there is nothing to negotiate per utterance.
//
// What you HEAR on this connection is never the other person's actual voice —
// the server silences that path — it is only the translated speech synthesized
// for you. See media.go.
window.Session = function (sessionID, hooks) {
  hooks = hooks || {};
  const id = String(sessionID);

  let me = '', myName = '', mySeat = 0;
  // Seeded from the caller's choices so the very first socket carries them.
  let myLang = hooks.lang || 'en', myGender = hooks.gender || 'unspecified';
  let people = [];
  let conn = 'connecting';   // 'connecting' | 'connected' | 'reconnecting'
  let media = 'idle';        // 'idle' | 'connecting' | 'live' | 'down'
  let interpreting = false;
  let speaking = {};         // participant id → bool
  let ended = '';            // non-empty once the server has ended the session
  let translating = true;    // false when TRANSLATE_PROVIDER=none
  let limits = { idle: 0, max: 0 };

  const audio = document.createElement('audio');
  audio.autoplay = true;
  (hooks.sinkMount || document.body).appendChild(audio);

  function snapshot() {
    return { id, me, myName, mySeat, myLang, myGender, people: people.slice(),
      conn, media, interpreting, ended, limits, translating,
      speaking: Object.assign({}, speaking) };
  }
  function emit() { if (hooks.onState) hooks.onState(snapshot()); }

  // ── signalling WebSocket ──────────────────────────────────────────────────
  let ws = null, backoff = 700, hbTimer = null, closed = false;
  function send(o) { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(o)); }

  function connect() {
    if (closed) return;
    const proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    // Carry the chosen language and voice on the socket URL so the server seats
    // us correctly from the first frame, rather than at the defaults followed by
    // a correction — which would transcribe the opening words in the wrong
    // language. Reconnects reuse whatever is current.
    const url = proto + location.host + '/api/interpreter/stream'
      + '?session=' + encodeURIComponent(id)
      + '&name=' + encodeURIComponent(hooks.name || '')
      + '&lang=' + encodeURIComponent(myLang)
      + '&gender=' + encodeURIComponent(myGender)
      + (hooks.token ? '&t=' + encodeURIComponent(hooks.token) : '');
    ws = new WebSocket(url);
    ws.onopen = () => {
      backoff = 700; conn = 'connected'; emit();
      clearInterval(hbTimer); hbTimer = setInterval(() => send({ type: 'ping' }), 10000);
    };
    ws.onmessage = (m) => { let msg; try { msg = JSON.parse(m.data); } catch (e) { return; } handle(msg); };
    ws.onclose = () => {
      if (closed) return;   // deliberate close, or the session ended
      conn = 'reconnecting'; emit();
      clearInterval(hbTimer);
      stopMedia();
      setTimeout(connect, backoff); backoff = Math.min(backoff * 2, 8000);
    };
    ws.onerror = () => { try { ws.close(); } catch (e) {} };
  }

  function handle(msg) {
    switch (msg.type) {
      case 'hello':
        me = msg.you || ''; myName = msg.name || ''; mySeat = msg.seat || 0;
        myLang = msg.lang || myLang;
        myGender = msg.gender || myGender;
        limits = { idle: msg.idle_timeout_s || 0, max: msg.max_duration_s || 0 };
        if (msg.translating !== undefined) translating = !!msg.translating;
        emit();
        // The server is ready for us; bring the media up.
        start();
        break;
      case 'presence':
        people = msg.people || [];
        emit();
        break;
      case 'peer':
        if (hooks.onNotice && msg.state === 'left') hooks.onNotice((msg.name || 'the other person') + ' left');
        break;
      case 'lang':
        if (msg.who === me && msg.lang) myLang = msg.lang;
        emit();
        break;
      case 'gender':
        if (msg.who === me && msg.gender) myGender = msg.gender;
        emit();
        break;
      case 'state':
        if (msg.media) { media = msg.media; }
        if (msg.interpreting !== undefined) interpreting = !!msg.interpreting;
        emit();
        break;
      case 'speaking':
        speaking[msg.who] = !!msg.on;
        emit();
        break;
      case 'caption':
        if (hooks.onCaption) hooks.onCaption(msg);
        break;
      case 'warning':
        if (hooks.onNotice) hooks.onNotice(msg.message || 'something went wrong');
        break;
      case 'ended':
        // The server ended the session (an idle or duration limit, usually).
        // Tear down locally and stop reconnecting — the link is retired, so a
        // reconnect would only bounce off a 404 forever.
        ended = msg.reason || 'session ended';
        closed = true;
        clearInterval(hbTimer);
        stopMedia();
        emit();
        if (hooks.onEnded) hooks.onEnded(ended);
        break;
      case 'webrtc.answer': onAnswer(msg); break;
      case 'webrtc.candidate': onCandidate(msg.candidate); break;
      case 'webrtc.error':
        if (hooks.onNotice) hooks.onNotice(msg.message || 'audio error');
        media = 'down'; emit();
        break;
      case 'pong': break;
    }
  }

  // ── WebRTC ────────────────────────────────────────────────────────────────
  let pc = null, micStream = null, remoteStream = null;

  function newPC() {
    const p = new RTCPeerConnection({ iceServers: [{ urls: 'stun:stun.l.google.com:19302' }] });
    p.ontrack = (ev) => {
      remoteStream = ev.streams[0];
      audio.srcObject = remoteStream;
      const pr = audio.play();
      if (pr && pr.catch) pr.catch(() => { if (hooks.onAudioBlocked) hooks.onAudioBlocked(); });
      // Meter the interpreted voice — that is the audio that matters here.
      Meter.attach(remoteStream);
    };
    p.onicecandidate = (ev) => { if (ev.candidate) send({ type: 'webrtc.candidate', candidate: ev.candidate.toJSON() }); };
    p.onconnectionstatechange = () => {
      if (!pc) return;
      if (pc.connectionState === 'failed') { media = 'down'; emit(); }
    };
    return p;
  }

  // start brings the microphone up and offers it to VoiceBlender. Failing to get
  // a microphone is fatal to the session — with nothing to transcribe there is
  // nothing to interpret — so it is surfaced rather than retried silently.
  async function start() {
    if (pc) return;
    media = 'connecting'; emit();
    try {
      micStream = await navigator.mediaDevices.getUserMedia({ audio: true });
    } catch (e) {
      media = 'down'; emit();
      if (hooks.onNotice) hooks.onNotice('microphone denied — the interpreter cannot hear you');
      return;
    }
    pc = newPC();
    micStream.getAudioTracks().forEach(t => pc.addTrack(t, micStream));
    try {
      const o = await pc.createOffer();
      await pc.setLocalDescription(o);
      send({ type: 'webrtc.offer', sdp: pc.localDescription.sdp });
    } catch (e) {
      media = 'down'; emit();
      if (hooks.onNotice) hooks.onNotice('could not negotiate audio');
    }
  }

  function onAnswer(msg) {
    if (!pc) return;
    pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp })
      .catch(() => { if (hooks.onNotice) hooks.onNotice('bad answer from server'); });
  }
  function onCandidate(c) { if (pc && c) pc.addIceCandidate(c).catch(() => {}); }

  function stopMedia() {
    media = 'idle';
    Meter.stop();
    remoteStream = null;
    if (pc) { try { pc.close(); } catch (e) {} pc = null; }
    if (micStream) { micStream.getTracks().forEach(t => t.stop()); micStream = null; }
    emit();
  }

  // ── public surface ────────────────────────────────────────────────────────

  // setLanguage tells the server which language this participant is speaking.
  // The server restarts transcription on this leg, so it takes effect on the
  // next utterance, not the one in progress.
  function setLanguage(code) {
    if (!code || code === myLang) return;
    myLang = code; emit();
    send({ type: 'lang.set', lang: code });
  }

  // setGender picks the voice this participant's words are spoken in on the
  // OTHER side. It does not change what you hear, and it does not restart
  // transcription — it takes effect on the next utterance.
  function setGender(code) {
    if (!code || code === myGender) return;
    myGender = code; emit();
    send({ type: 'gender.set', gender: code });
  }

  // unlockAudio re-plays the sink and resumes the audio context after a user
  // gesture. Browsers gate audio until one, and someone who only listens may
  // never otherwise click anything.
  function unlockAudio() {
    AudioCtx.resume();
    if (audio.srcObject) { const pr = audio.play(); if (pr && pr.catch) pr.catch(() => {}); }
  }

  function close() {
    closed = true;
    clearInterval(hbTimer);
    stopMedia();
    try { if (ws) ws.close(); } catch (e) {}
    try { audio.pause(); audio.srcObject = null; audio.remove(); } catch (e) {}
  }

  connect();
  return { id, setLanguage, setGender, unlockAudio, close, state: snapshot };
};
