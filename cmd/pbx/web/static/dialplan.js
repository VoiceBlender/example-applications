// Inbound dial-plan visual editor. Loaded only by the Dial plan page. Uses the
// shared PBX helpers and the one snapshot socket (for the live extension/trunk
// option sources and the initial graph load).
(() => {
  'use strict';
  const $ = PBX.$, el = PBX.el;

  let dp = { nodes: [], edges: [] }, dpLoaded = false, dpArm = null, dpExts = [], dpTrunks = [], dpEditId = null;
  let dpSaved = ''; // JSON of the graph as last loaded/saved (unsaved-changes check before a test call)
  const DP_META = {
    start:  { label: 'Start',     outs: ['out'],             hasIn: false },
    match:  { label: 'Match',     outs: ['match', 'nomatch'], hasIn: true },
    answer: { label: 'Answer',    outs: ['next'],            hasIn: true },
    wait:   { label: 'Wait',      outs: ['next'],            hasIn: true },
    gather: { label: 'Gather',    outs: [],                  hasIn: true },
    ext:    { label: 'Extension', outs: ['noanswer'],        hasIn: true },
    ivr:    { label: 'IVR',       outs: [],                  hasIn: true },
    forward:{ label: 'Forward',   outs: ['noanswer'],        hasIn: true },
    play:   { label: 'Play',      outs: ['next'],            hasIn: true },
    tts:    { label: 'TTS',       outs: ['next'],            hasIn: true },
    reject: { label: 'Reject',    outs: [],                  hasIn: true },
  };
  function normDp(g) {
    const nodes = (g.nodes || []).map(n => ({ id: n.id, type: n.type, x: n.x || 0, y: n.y || 0, params: Object.assign({}, n.params || {}) }));
    const edges = (g.edges || []).map(e => ({ from: e.from, port: e.port, to: e.to }));
    return { nodes, edges };
  }
  function dpNode(id) { return dp.nodes.find(n => n.id === id); }
  function gatherOpts(n) { return ((n.params && n.params.options) || '').split(',').map(s => s.trim()).filter(Boolean); }
  function nodeOuts(n) {
    if (n.type === 'gather') { return gatherOpts(n).concat(['default']); }
    return (DP_META[n.type] || { outs: [] }).outs;
  }
  function dpNewId() { return 'n' + Math.random().toString(36).slice(2, 8); }
  function trunkName(id) { const t = dpTrunks.find(t => t.id === id); return t ? t.name : (id ? id : 'any'); }

  function dpSummary(n) {
    const p = n.params || {};
    switch (n.type) {
      case 'start':  return 'inbound entry';
      case 'answer': return 'answer the call';
      case 'wait':   return 'wait ' + (p.seconds || '1') + 's';
      case 'gather': { const im = (n.params && n.params.input) || 'dtmf'; const lbl = im === 'speech' ? 'speech' : im === 'both' ? 'DTMF/speech' : 'DTMF'; return 'gather ' + lbl + ' [' + (gatherOpts(n).join(',') || '…') + ']'; }
      case 'match':  return trunkName(p.trunk) + ' · DID ' + (p.did_mode && p.did_mode !== 'any' ? (p.did_mode + ' ' + (p.did || '?')) : 'any');
      case 'ext':    { const ns = (p.number || '?'); return '→ ring ' + ns + (p.ring_time ? (' · ' + p.ring_time + 's') : ''); }
      case 'ivr':    return '→ dial-by-extension IVR';
      case 'forward':return '→ ' + (p.number || '?') + ' via ' + trunkName(p.trunk) + (p.ring_time ? (' · ' + p.ring_time + 's') : '');
      case 'play':   return (p.url || '(no url)');
      case 'tts':    return '“' + (p.text || '') + '”';
      case 'reject': return 'hang up (' + (p.reason || 'declined') + ')';
    }
    return '';
  }

  function renderDialplan() {
    const cv = $('dp-canvas');
    if (!cv) return;
    cv.querySelectorAll('.dp-node').forEach(n => n.remove());
    dp.nodes.forEach(n => {
      const meta = DP_META[n.type] || { outs: [], hasIn: true };
      const box = el('div', 'dp-node dp-' + n.type); box.style.left = n.x + 'px'; box.style.top = n.y + 'px'; box.dataset.node = n.id;
      const head = el('div', 'dp-node-head');
      head.appendChild(el('span', null, (meta.label || n.type)));
      if (n.type !== 'start') { const d = el('button', 'dp-del', '✕'); d.onclick = (e) => { e.stopPropagation(); dpDeleteNode(n.id); }; head.appendChild(d); }
      box.appendChild(head);
      const body = el('div', 'dp-node-body', dpSummary(n)); body.onclick = (e) => { e.stopPropagation(); openNode(n.id); }; box.appendChild(body);
      if (meta.hasIn) { const inh = el('div', 'dp-handle dp-in'); inh.dataset.node = n.id; inh.dataset.in = '1'; inh.onclick = (e) => { e.stopPropagation(); dpConnectTo(n.id); }; box.appendChild(inh); }
      const outs = nodeOuts(n);
      const OUT_TOP = 38, OUT_GAP = 22;
      outs.forEach((port, i) => {
        const oh = el('div', 'dp-handle dp-out'); oh.dataset.node = n.id; oh.dataset.port = port; oh.style.top = (OUT_TOP + i * OUT_GAP) + 'px';
        oh.onclick = (e) => { e.stopPropagation(); dpArmOut(n.id, port, oh); }; box.appendChild(oh);
        if (outs.length > 1) { const lb = el('div', 'dp-outlabel', port); lb.style.top = (OUT_TOP - 3 + i * OUT_GAP) + 'px'; box.appendChild(lb); }
      });
      if (outs.length > 0) { box.style.minHeight = (OUT_TOP + outs.length * OUT_GAP) + 'px'; }
      head.addEventListener('pointerdown', (e) => dpStartDrag(e, n, box));
      cv.appendChild(box);
    });
    dpDrawWires();
    testMarkNodes();
  }

  function handleCenter(elm) { const node = elm.closest('.dp-node'); return { x: node.offsetLeft + elm.offsetLeft + elm.offsetWidth / 2, y: node.offsetTop + elm.offsetTop + elm.offsetHeight / 2 }; }
  function dpDrawWires() {
    const svg = $('dp-wires'); if (!svg) return; svg.innerHTML = '';
    dp.edges.forEach(edge => {
      const src = $('dp-canvas').querySelector('.dp-out[data-node="' + edge.from + '"][data-port="' + edge.port + '"]');
      const dst = $('dp-canvas').querySelector('.dp-in[data-node="' + edge.to + '"]');
      if (!src || !dst) return;
      const a = handleCenter(src), b = handleCenter(dst);
      const dx = Math.max(40, Math.abs(b.x - a.x) / 2);
      const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
      path.setAttribute('d', `M ${a.x} ${a.y} C ${a.x + dx} ${a.y}, ${b.x - dx} ${b.y}, ${b.x} ${b.y}`);
      path.setAttribute('class', 'dp-wire');
      path.addEventListener('click', () => { dp.edges = dp.edges.filter(e => e !== edge); dpDrawWires(); });
      svg.appendChild(path);
    });
  }

  function dpStartDrag(e, n, box) {
    e.preventDefault();
    const sx = e.clientX, sy = e.clientY, ox = n.x, oy = n.y;
    const move = (ev) => { n.x = Math.round(Math.max(0, ox + (ev.clientX - sx))); n.y = Math.round(Math.max(0, oy + (ev.clientY - sy))); box.style.left = n.x + 'px'; box.style.top = n.y + 'px'; dpDrawWires(); };
    const up = () => { document.removeEventListener('pointermove', move); document.removeEventListener('pointerup', up); };
    document.addEventListener('pointermove', move); document.addEventListener('pointerup', up);
  }

  function dpArmOut(nodeId, port, elm) {
    if (dpArm) { $('dp-canvas').querySelectorAll('.dp-out.armed').forEach(h => h.classList.remove('armed')); }
    if (dpArm && dpArm.from === nodeId && dpArm.port === port) { dpArm = null; return; }
    dpArm = { from: nodeId, port: port }; elm.classList.add('armed');
  }
  function dpConnectTo(nodeId) {
    if (!dpArm) return;
    if (dpArm.from === nodeId) { dpArm = null; renderDialplan(); return; }
    dp.edges = dp.edges.filter(e => !(e.from === dpArm.from && e.port === dpArm.port));
    dp.edges.push({ from: dpArm.from, port: dpArm.port, to: nodeId });
    dpArm = null; renderDialplan();
  }
  function dpDeleteNode(id) {
    dp.nodes = dp.nodes.filter(n => n.id !== id);
    dp.edges = dp.edges.filter(e => e.from !== id && e.to !== id);
    renderDialplan();
  }
  function dpAdd(type) {
    const cv = $('dp-canvas');
    const x = Math.round(cv.scrollLeft) + 40 + Math.floor(Math.random() * 260);
    const y = Math.round(cv.scrollTop) + 30 + Math.floor(Math.random() * 320);
    const params = type === 'gather' ? { options: '1,2' } : {};
    const n = { id: dpNewId(), type, x, y, params };
    dp.nodes.push(n); renderDialplan();
  }

  function fillSelect(sel, items) { sel.innerHTML = ''; items.forEach(it => { const o = document.createElement('option'); o.value = it.v; o.textContent = it.t; sel.appendChild(o); }); }
  function openNode(id) {
    const n = dpNode(id); if (!n) return; dpEditId = id; const p = n.params || {};
    $('dpn-title').textContent = 'Edit ' + (DP_META[n.type] ? DP_META[n.type].label : n.type);
    document.querySelectorAll('.dpn-f').forEach(f => f.classList.add('hidden'));
    document.querySelectorAll('.dpn-' + n.type).forEach(f => f.classList.remove('hidden'));
    if (['start', 'ivr', 'answer'].includes(n.type)) $('dpn-none').classList.remove('hidden');
    const trunkOpts = [{ v: '', t: 'Any / auto' }].concat(dpTrunks.map(t => ({ v: t.id, t: t.name })));
    fillSelect($('dpn-trunk'), trunkOpts); $('dpn-trunk').value = p.trunk || '';
    fillSelect($('dpn-fwdtrunk'), trunkOpts); $('dpn-fwdtrunk').value = p.trunk || '';
    { const dl = document.getElementById('dpn-ext-list'); dl.innerHTML = ''; dpExts.forEach(e => { const o = document.createElement('option'); o.value = e.number; o.textContent = e.name || ''; dl.appendChild(o); }); }
    $('dpn-ext').value = p.number || ''; $('dpn-extring').value = p.ring_time || '';
    $('dpn-didmode').value = p.did_mode || 'any'; $('dpn-did').value = p.did || '';
    $('dpn-fwdnum').value = p.number || ''; $('dpn-fwdring').value = p.ring_time || ''; $('dpn-url').value = p.url || '';
    $('dpn-text').value = p.text || ''; $('dpn-voice').value = p.voice || ''; $('dpn-reason').value = p.reason || 'declined';
    $('dpn-gtext').value = p.text || ''; $('dpn-gvoice').value = p.voice || ''; $('dpn-gurl').value = p.url || ''; $('dpn-goptions').value = p.options || '';
    $('dpn-gnum').value = p.num_digits || ''; $('dpn-gtimeout').value = p.timeout || '';
    $('dpn-ginput').value = p.input || 'dtmf'; $('dpn-glang').value = p.language || '';
    $('dpn-wsecs').value = p.seconds || '';
    $('dpn-modal').hidden = false;
  }
  function closeNode() { $('dpn-modal').hidden = true; dpEditId = null; }
  function saveNode() {
    const n = dpNode(dpEditId); if (!n) { closeNode(); return; }
    const p = {};
    switch (n.type) {
      case 'match':   p.trunk = $('dpn-trunk').value; p.did_mode = $('dpn-didmode').value; p.did = $('dpn-did').value.trim(); break;
      case 'ext':     p.number = $('dpn-ext').value.split(',').map(s => s.trim()).filter(Boolean).join(','); p.ring_time = $('dpn-extring').value.trim(); break;
      case 'forward': p.number = $('dpn-fwdnum').value.trim(); p.trunk = $('dpn-fwdtrunk').value; p.ring_time = $('dpn-fwdring').value.trim(); break;
      case 'play':    p.url = $('dpn-url').value.trim(); break;
      case 'tts':     p.text = $('dpn-text').value.trim(); p.voice = $('dpn-voice').value.trim(); break;
      case 'gather':  p.text = $('dpn-gtext').value.trim(); p.voice = $('dpn-gvoice').value.trim(); p.url = $('dpn-gurl').value.trim();
                      p.options = $('dpn-goptions').value.split(',').map(s => s.trim()).filter(Boolean).join(',');
                      p.num_digits = $('dpn-gnum').value.trim(); p.timeout = $('dpn-gtimeout').value.trim();
                      p.input = $('dpn-ginput').value; p.language = $('dpn-glang').value.trim(); break;
      case 'wait':    p.seconds = $('dpn-wsecs').value.trim(); break;
      case 'reject':  p.reason = $('dpn-reason').value; break;
    }
    n.params = p; closeNode(); renderDialplan();
  }

  async function saveDialplan() {
    const s = $('dp-status');
    const body = JSON.stringify(dp);
    const r = await fetch('/api/dialplan', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body });
    if (r.ok) { s.style.color = 'var(--green)'; s.textContent = 'Saved'; dpSaved = body; }
    else { s.style.color = 'var(--red)'; s.textContent = 'Save failed (' + r.status + ')'; }
    setTimeout(() => { s.textContent = ''; }, 2500);
    return r.ok;
  }
  async function reloadDialplan() {
    const r = await fetch('/api/dialplan'); if (!r.ok) return;
    dp = normDp(await r.json()); dpSaved = JSON.stringify(dp); dpArm = null; renderDialplan();
  }

  // ── Test call ─────────────────────────────────────────────────────────────
  // A browser WebRTC leg runs the saved dial plan as a simulated inbound call
  // (/api/dialplan/test). The keypad sends DTMF over that socket; the server
  // traces each step back so the active node is highlighted on the canvas.
  const T = { ws: null, pc: null, mic: null, started: false, active: null, visited: new Set(), pendingCands: [], entered: '', audioCtx: null };
  const KEYS = ['1', '2', '3', '4', '5', '6', '7', '8', '9', '*', '0', '#'];
  const DTMF_HZ = { '1': [697, 1209], '2': [697, 1336], '3': [697, 1477], '4': [770, 1209], '5': [770, 1336], '6': [770, 1477],
    '7': [852, 1209], '8': [852, 1336], '9': [852, 1477], '*': [941, 1209], '0': [941, 1336], '#': [941, 1477] };

  function tSend(m) { if (T.ws && T.ws.readyState === 1) T.ws.send(JSON.stringify(m)); }
  function tState(cls, label) { const s = $('dpt-state'); s.className = 'state ' + cls; $('dpt-state-label').textContent = label; }
  function tLog(text, cls, nodeId) {
    const li = el('li');
    li.appendChild(el('span', 't', new Date().toLocaleTimeString([], { hour12: false })));
    const body = el('span', cls || null, text);
    if (nodeId) { body.onclick = () => { const b = $('dp-canvas').querySelector('.dp-node[data-node="' + nodeId + '"]'); if (b) b.scrollIntoView({ block: 'nearest', inline: 'nearest', behavior: 'smooth' }); }; }
    li.appendChild(body);
    const log = $('dpt-log'); log.appendChild(li); log.scrollTop = log.scrollHeight;
  }
  function testMarkNodes() {
    const cv = $('dp-canvas'); if (!cv) return;
    cv.querySelectorAll('.dp-node').forEach(b => {
      b.classList.toggle('dp-active', b.dataset.node === T.active);
      b.classList.toggle('dp-visited', T.visited.has(b.dataset.node) && b.dataset.node !== T.active);
    });
  }
  function tSetButtons(inCall) {
    $('dpt-call').disabled = inCall; $('dpt-hangup').disabled = !inCall;
    $('dpt-keys').classList.toggle('off', !T.started);
  }
  function tTone(d) {
    const hz = DTMF_HZ[d]; if (!hz) return;
    try {
      T.audioCtx = T.audioCtx || new (window.AudioContext || window.webkitAudioContext)();
      const ctx = T.audioCtx, g = ctx.createGain(); g.gain.value = 0.08; g.connect(ctx.destination);
      hz.forEach(f => { const o = ctx.createOscillator(); o.frequency.value = f; o.connect(g); o.start(); o.stop(ctx.currentTime + 0.12); });
    } catch (e) {}
  }
  function tPress(d) {
    if (!T.started) return;
    tSend({ type: 'dtmf', digit: d }); tTone(d);
    T.entered = (T.entered + d).slice(-24); $('dpt-entered').textContent = T.entered;
    const k = $('dpt-keys').querySelector('[data-key="' + CSS.escape(d) + '"]');
    if (k) { k.classList.add('hit'); setTimeout(() => k.classList.remove('hit'), 140); }
  }

  function openTest() {
    const sel = $('dpt-trunk'), cur = sel.value;
    fillSelect(sel, [{ v: '', t: 'None / unidentified' }].concat(dpTrunks.map(t => ({ v: t.id, t: t.name }))));
    sel.value = dpTrunks.some(t => t.id === cur) ? cur : '';
    $('dp-test').hidden = false; tSetButtons(!!T.ws);
    $('dp-test').scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  }
  function closeTest() { testTeardown(); $('dp-test').hidden = true; T.active = null; T.visited.clear(); testMarkNodes(); }

  async function testCall() {
    if (T.ws) return;
    if (JSON.stringify(dp) !== dpSaved) {
      if (confirm('The dial plan has unsaved changes. Save them before testing?\n(Cancel tests the previously saved version.)')) {
        if (!(await saveDialplan())) { tLog('save failed — test not started', 'err'); return; }
      }
    }
    $('dpt-log').innerHTML = ''; T.entered = ''; $('dpt-entered').innerHTML = '&nbsp;';
    T.active = null; T.visited.clear(); T.started = false; T.pendingCands = []; testMarkNodes();
    tState('warn', 'Connecting'); tSetButtons(true);

    try { T.mic = await navigator.mediaDevices.getUserMedia({ audio: true }); }
    catch (e) { T.mic = null; tLog('no microphone — listen-only (speech input unavailable)', 'err'); }

    const proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    const ws = new WebSocket(proto + location.host + '/api/dialplan/test');
    T.ws = ws;
    ws.onopen = async () => {
      const pc = new RTCPeerConnection({ iceServers: [{ urls: 'stun:stun.l.google.com:19302' }] });
      T.pc = pc;
      if (T.mic) { T.mic.getAudioTracks().forEach(t => pc.addTrack(t, T.mic)); }
      else { pc.addTransceiver('audio', { direction: 'sendrecv' }); }
      pc.ontrack = (ev) => { const s = $('dpt-sink'); s.srcObject = ev.streams[0] || new MediaStream([ev.track]); const p = s.play(); if (p && p.catch) p.catch(() => {}); };
      pc.onicecandidate = (ev) => { if (ev.candidate) tSend({ type: 'webrtc.candidate', candidate: ev.candidate.toJSON() }); };
      pc.onconnectionstatechange = () => {
        if (T.pc !== pc) return;
        if (pc.connectionState === 'connected' && !T.started) {
          tSend({ type: 'start', from: $('dpt-from').value, did: $('dpt-did').value, trunk: $('dpt-trunk').value });
        } else if (pc.connectionState === 'failed') {
          tLog('audio connection failed', 'err'); testTeardown();
        }
      };
      try {
        await pc.setLocalDescription(await pc.createOffer());
        tSend({ type: 'webrtc.offer', sdp: pc.localDescription.sdp });
      } catch (e) { tLog('offer error: ' + e, 'err'); testTeardown(); }
    };
    ws.onmessage = async (ev) => {
      let m; try { m = JSON.parse(ev.data); } catch (e) { return; }
      switch (m.type) {
        case 'webrtc.answer':
          try {
            await T.pc.setRemoteDescription({ type: 'answer', sdp: m.sdp });
            T.pendingCands.splice(0).forEach(c => T.pc.addIceCandidate(c).catch(() => {}));
          } catch (e) { tLog('answer error: ' + e, 'err'); testTeardown(); }
          break;
        case 'webrtc.candidate':
          if (!T.pc) break;
          if (T.pc.remoteDescription) T.pc.addIceCandidate(m.candidate).catch(() => {}); else T.pendingCands.push(m.candidate);
          break;
        case 'started':
          T.started = true; tState('ok', 'In call'); tSetButtons(true); tLog('call started');
          break;
        case 'trace':
          if (m.node) {
            if (T.active) T.visited.add(T.active);
            T.active = m.node; T.visited.add(m.node); testMarkNodes();
            const n = dpNode(m.node), label = n ? ((DP_META[n.type] || {}).label || n.type) : m.node;
            tLog(m.text && n && m.text !== n.type ? label + ' · ' + m.text : label + (n ? ' · ' + dpSummary(n) : ''), 'node', m.node);
          } else {
            tLog(m.text, /^DTMF/.test(m.text) ? 'dtmf' : null);
          }
          break;
        case 'ended':
          tLog('call ended' + (m.reason ? ' (' + m.reason + ')' : '')); testTeardown();
          break;
        case 'error':
          tLog(m.message || 'error', 'err'); testTeardown();
          break;
      }
    };
    ws.onclose = () => { if (T.ws === ws) { testTeardown(); } };
  }

  function hangupTest() { if (T.started) tSend({ type: 'hangup' }); else testTeardown(); }

  function testTeardown() {
    const ws = T.ws, pc = T.pc;
    T.ws = null; T.pc = null; T.started = false;
    if (pc) { try { pc.close(); } catch (e) {} }
    if (ws) { try { ws.close(); } catch (e) {} }
    if (T.mic) { T.mic.getTracks().forEach(t => t.stop()); T.mic = null; }
    const s = $('dpt-sink'); if (s) s.srcObject = null;
    if (T.active) { T.visited.add(T.active); T.active = null; }
    testMarkNodes();
    if (ws) tState('', 'Ended');
    tSetButtons(false);
  }

  PBX.onSnapshot(snap => {
    dpExts = snap.extensions || [];
    dpTrunks = snap.trunks || [];
    if (!dpLoaded && snap.dialplan) { dp = normDp(snap.dialplan); dpSaved = JSON.stringify(dp); dpLoaded = true; renderDialplan(); }
  });

  document.addEventListener('DOMContentLoaded', () => {
    document.querySelectorAll('[data-dp-add]').forEach(b => b.onclick = () => dpAdd(b.getAttribute('data-dp-add')));
    $('dp-save').onclick = saveDialplan;
    $('dp-reload').onclick = reloadDialplan;
    $('dpn-close').onclick = closeNode;
    $('dpn-save').onclick = saveNode;
    $('dp-canvas').addEventListener('click', () => { if (dpArm) { dpArm = null; renderDialplan(); } });
    document.addEventListener('keydown', e => { if (e.key === 'Escape') { closeNode(); dpArm = null; } });

    $('dp-test-open').onclick = openTest;
    $('dp-test-close').onclick = closeTest;
    $('dpt-call').onclick = testCall;
    $('dpt-hangup').onclick = hangupTest;
    KEYS.forEach(k => { const b = el('button', 'dpt-key', k); b.dataset.key = k; b.onclick = () => tPress(k); $('dpt-keys').appendChild(b); });
    tSetButtons(false);
    // Physical keyboard → DTMF while a test call is running (not while typing in a field).
    document.addEventListener('keydown', e => {
      if (!T.started || e.ctrlKey || e.metaKey || e.altKey) return;
      if (e.target.closest && e.target.closest('input,select,textarea')) return;
      if (KEYS.includes(e.key)) { e.preventDefault(); tPress(e.key); }
    });
    window.addEventListener('beforeunload', () => testTeardown());
    $('dpn-modal').addEventListener('click', e => { if (e.target === $('dpn-modal')) { $('dpn-modal').hidden = true; } });
  });
})();
