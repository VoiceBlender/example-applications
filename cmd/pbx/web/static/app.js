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


  // ---- audio filters -------------------------------------------------------
  // Filters are stored structurally ([{type, params}]) but edited as the same
  // compact string AUDIO_FILTERS uses: "bandpass:low_hz=300, denoise".
  const FILTER_PRESETS = [
    ['denoise', 'denoise'],
    ['denoise_gtcrn', 'denoise_gtcrn'],
    ['bandpass', 'bandpass:low_hz=300:high_hz=3400'],
    ['gain', 'gain:volume=2'],
    ['pitch', 'pitch:semitones=-5'],
    ['robotic', 'robotic'],
    ['vocoder', 'vocoder'],
  ];

  // denoise (RNNoise) and denoise_gtcrn (GTCRN) are alternatives and a chain
  // may hold only one, so the chip replaces rather than appends. It also leads:
  // it is corrective, and an enhancer placed after an effect fights what it is
  // handed.
  const DENOISERS = ['denoise', 'denoise_gtcrn'];
  const isDenoise = t => DENOISERS.includes(t);

  function addFilter(cur, value) {
    let items = (cur || '').split(',').map(s => s.trim()).filter(Boolean);
    if (isDenoise(value.split(':')[0])) {
      return [value].concat(items.filter(s => !isDenoise(s.split(':')[0]))).join(', ');
    }
    items.push(value);
    return items.join(', ');
  }

  // denoiseKind reports which denoiser a chain runs, or '' for none.
  function denoiseKind(specs) {
    const f = (specs || []).find(f => isDenoise(f.type));
    return f ? f.type : '';
  }

  // withDenoise returns the chain with the given denoiser at the front, or with
  // none when kind is '', leaving every other filter as it was.
  function withDenoise(specs, kind) {
    const rest = (specs || []).filter(f => !isDenoise(f.type));
    return kind ? [{ type: kind }].concat(rest) : rest;
  }

  function parseFilters(s) {
    s = (s || '').trim();
    if (!s) return null; // blank = inherit the server default
    return s.split(',').map(x => x.trim()).filter(Boolean).map(item => {
      const parts = item.split(':');
      const spec = { type: parts[0].trim().toLowerCase() }, params = {};
      parts.slice(1).forEach(kv => {
        const i = kv.indexOf('=');
        if (i > 0) {
          const v = parseFloat(kv.slice(i + 1));
          if (!isNaN(v)) params[kv.slice(0, i).trim()] = v;
        }
      });
      if (Object.keys(params).length) spec.params = params;
      return spec;
    });
  }

  function formatFilters(list) {
    if (!list || !list.length) return '';
    return list.map(f => {
      const p = f.params || {};
      return Object.keys(p).sort().reduce((s, k) => s + ':' + k + '=' + p[k], f.type);
    }).join(', ');
  }

  // filterChips renders the built-in filters as clickable chips. A datalist
  // alone is easy to miss: browsers only reveal it once you type, so the field
  // reads as a plain text box and the filters stay invisible.
  function filterChips(host, input) {
    host.textContent = '';
    FILTER_PRESETS.forEach(([label, value]) => {
      const b = el('button', 'filter-chip', '+ ' + label);
      b.type = 'button'; b.title = value;
      b.onclick = () => {
        input.value = addFilter(input.value, value);
        // Dispatch so any listener (the live-calls draft tracker) sees it; a
        // programmatic value change fires no event on its own.
        input.dispatchEvent(new Event('input', { bubbles: true }));
      };
      host.appendChild(b);
    });
    const clear = el('button', 'filter-chip clear', 'clear');
    clear.type = 'button';
    clear.onclick = () => {
      input.value = '';
      input.dispatchEvent(new Event('input', { bubbles: true }));
    };
    host.appendChild(clear);
  }

  return { $, el, cell2, parseFilters, formatFilters, filterChips, denoiseKind, withDenoise, extStateChip, trunkStateChip, callStateChip, fmtDur, onSnapshot, onTick };
})();
