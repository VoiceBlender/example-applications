// Inbound dial-plan visual editor. Loaded only by the Dial plan page. Uses the
// shared PBX helpers and the one snapshot socket (for the live extension/trunk
// option sources and the initial graph load).
(() => {
  'use strict';
  const $ = PBX.$, el = PBX.el;

  let dp = { nodes: [], edges: [] }, dpLoaded = false, dpArm = null, dpExts = [], dpTrunks = [], dpEditId = null;
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
    const r = await fetch('/api/dialplan', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(dp) });
    if (r.ok) { s.style.color = 'var(--green)'; s.textContent = 'Saved'; }
    else { s.style.color = 'var(--red)'; s.textContent = 'Save failed (' + r.status + ')'; }
    setTimeout(() => { s.textContent = ''; }, 2500);
  }
  async function reloadDialplan() {
    const r = await fetch('/api/dialplan'); if (!r.ok) return;
    dp = normDp(await r.json()); dpArm = null; renderDialplan();
  }

  PBX.onSnapshot(snap => {
    dpExts = snap.extensions || [];
    dpTrunks = snap.trunks || [];
    if (!dpLoaded && snap.dialplan) { dp = normDp(snap.dialplan); dpLoaded = true; renderDialplan(); }
  });

  document.addEventListener('DOMContentLoaded', () => {
    document.querySelectorAll('[data-dp-add]').forEach(b => b.onclick = () => dpAdd(b.getAttribute('data-dp-add')));
    $('dp-save').onclick = saveDialplan;
    $('dp-reload').onclick = reloadDialplan;
    $('dpn-close').onclick = closeNode;
    $('dpn-save').onclick = saveNode;
    $('dp-canvas').addEventListener('click', () => { if (dpArm) { dpArm = null; renderDialplan(); } });
    document.addEventListener('keydown', e => { if (e.key === 'Escape') { closeNode(); dpArm = null; } });
    $('dpn-modal').addEventListener('click', e => { if (e.target === $('dpn-modal')) { $('dpn-modal').hidden = true; } });
  });
})();
