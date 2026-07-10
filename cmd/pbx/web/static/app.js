// Shared console plumbing: the snapshot WebSocket, DOM helpers, chip/format
// builders, the stats bar, and nav highlighting. Every page links this file and
// registers a page renderer via PBX.onSnapshot(fn); the socket calls it with
// each snapshot. One socket, shared across all pages.
window.PBX = (function () {
  'use strict';
  const $ = (ref, root = document) => root.querySelector(`[data-ref="${ref}"]`);
  const el = (tag, cls, txt) => { const e = document.createElement(tag); if (cls) e.className = cls; if (txt != null) e.textContent = txt; return e; };

  // cell2 builds a two-line cell: a primary line + a muted mono sub-line.
  function cell2(primary, secondary) {
    const d = el('div');
    d.appendChild(el('div', 'cell-line', primary || '—'));
    if (secondary) { d.appendChild(el('div', 'cell-sub', secondary)); }
    return d;
  }
  function extStateChip(e) {
    const chip = el('span', 'state ' + (e.registered ? 'ok' : 'bad'));
    chip.appendChild(el('span', 'state-dot'));
    chip.appendChild(document.createTextNode(e.registered ? 'Registered' : 'Offline'));
    return chip;
  }
  function trunkStateChip(t) {
    let cls = 'warn', label = t.state || 'pending';
    if (t.state === 'registered' || t.state === 'active') cls = 'ok';
    else if (t.state === 'failed' || t.state === 'expired') cls = 'bad';
    const chip = el('span', 'state ' + cls);
    chip.appendChild(el('span', 'state-dot'));
    chip.appendChild(document.createTextNode(label));
    if (t.last_error) { chip.title = t.last_error; }
    return chip;
  }
  function callStateChip(c) {
    let cls = 'info', label = c.state || '';
    if (c.state === 'connected') { cls = 'ok'; label = 'Connected'; }
    else if (c.state === 'ringing') { cls = 'warn'; label = 'Ringing'; }
    else { cls = 'info'; label = (c.state || '').replace(/\b\w/g, ch => ch.toUpperCase()); }
    const chip = el('span', 'state ' + cls);
    chip.appendChild(el('span', 'state-dot'));
    chip.appendChild(document.createTextNode(label));
    return chip;
  }
  function fmtDur(sinceISO) {
    const t = Date.parse(sinceISO);
    if (isNaN(t)) return '0:00';
    let s = Math.max(0, Math.floor((Date.now() - t) / 1000));
    const h = Math.floor(s / 3600); s %= 3600;
    const m = Math.floor(s / 60); s %= 60;
    const mm = (h > 0 ? String(m).padStart(2, '0') : String(m));
    const pre = h > 0 ? (h + ':') : '';
    return pre + mm + ':' + String(s).padStart(2, '0');
  }

  function updateStats(snap) {
    const st = snap.stats || {};
    const se = $('stat-ext'); if (se) { se.textContent = (st.ext_registered || 0) + ' / ' + (st.ext_total || 0); se.dataset.zero = (st.ext_registered || 0) === 0; }
    const stk = $('stat-trunks'); if (stk) { stk.textContent = (st.trunks_up || 0) + ' / ' + (st.trunks_total || 0); stk.dataset.zero = (st.trunks_up || 0) === 0; }
    const sc = $('stat-calls'); if (sc) { sc.textContent = String(st.active_calls || 0); sc.dataset.zero = (st.active_calls || 0) === 0; }
  }

  let pageRender = null, tickFn = null;
  function onSnapshot(fn) { pageRender = fn; }
  function onTick(fn) { tickFn = fn; }

  let ws = null, reconnectDelay = 700;
  function setStream(state, label) { const s = $('stream'); if (!s) return; s.dataset.state = state; const l = $('stream-label'); if (l) l.textContent = label; }
  function connect() {
    const proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    ws = new WebSocket(proto + location.host + '/api/stream');
    ws.onopen = () => { setStream('live', 'Live'); reconnectDelay = 700; };
    ws.onmessage = (ev) => { try { const snap = JSON.parse(ev.data); updateStats(snap); if (pageRender) pageRender(snap); } catch (e) {} };
    ws.onclose = () => { setStream('lost', 'Reconnecting'); setTimeout(connect, reconnectDelay); reconnectDelay = Math.min(reconnectDelay * 1.6, 8000); };
    ws.onerror = () => { try { ws.close(); } catch (e) {} };
  }

  function markNav() {
    const path = location.pathname;
    document.querySelectorAll('.nav a').forEach(a => {
      const href = a.getAttribute('href');
      if (href === path || (href !== '/' && path.indexOf(href) === 0)) a.classList.add('active');
    });
  }

  document.addEventListener('DOMContentLoaded', () => {
    markNav();
    connect();
    // Advance call-duration timers once a second between snapshots.
    setInterval(() => { if (tickFn) tickFn(); }, 1000);
  });

  return { $, el, cell2, extStateChip, trunkStateChip, callStateChip, fmtDur, onSnapshot, onTick };
})();
