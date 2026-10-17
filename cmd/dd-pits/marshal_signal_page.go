package main

import (
	"fmt"
	"net/http"
	"net/url"
)

// signalMarshalPage is the trace view of one race. The page loads the traces
// from /marshal/api/race and asks the server for every recalculation, so the
// arithmetic lives in one place (internal/marshal).
func (s *liveServer) signalMarshalPage(w http.ResponseWriter, r *http.Request, race, label string) {
	body := fmt.Sprintf(`<div class="card sig-head"><p class="mut"><a href="/marshal">Races</a> · <a href="/marshal?race=%s&simple=1">Simple view</a></p>
<h2>%s</h2>
<p class="mut" id="sigWhere">Reading the signal traces…</p>
<div class="sig-help"><b>Drag</b> the red enter line and orange exit line. <b>Click</b> a pass to remove or restore it, <b>shift-click</b> the trace to add a missed lap.
<b>Scroll</b> to zoom, drag the background to pan. Every pilot shares the time axis, so a bleed from another channel lines up.</div>
<div class="row sig-top"><button id="sigAutoAll">Auto-detect every pilot</button><button class="sec" id="sigFresh">Reload from the timer</button></div></div>
<div id="sigPilots"></div>
<script>window.SIG_RACE=%q;</script>%s%s`,
		url.QueryEscape(race), esc(label), race, signalCSS, signalJS)
	writeSetup(w, r, "Marshal", body)
}

const signalCSS = `<style>
.sig-help{font-size:.88rem;color:#9aa3ad;margin:.4rem 0 .8rem;line-height:1.5}
.sig-top{gap:.5rem;flex-wrap:wrap}.sig-top button{flex:0 0 auto}
.sigp{padding:14px 16px 12px}
.sigp h3{margin:0;font-size:1.1rem;display:flex;gap:.6rem;align-items:baseline;flex-wrap:wrap}
.sigp h3 small{color:#8e96a3;font-weight:500;font-size:.8rem}
.sig-canvas{width:100%;height:190px;display:block;border-radius:10px;background:#0b0e12;cursor:crosshair;touch-action:none;margin:.6rem 0 .4rem}
.sig-bar{display:flex;gap:.45rem;flex-wrap:wrap;align-items:center;margin:.3rem 0}
.sig-bar button{padding:.35rem .8rem;font-size:.82rem;flex:0 0 auto}
.sig-lines{font-family:var(--mono,ui-monospace,monospace);font-size:.85rem;color:#cfd4dc}
.sig-lines b.en{color:#ff5a4f}.sig-lines b.ex{color:#ff9f3d}
.sig-chip{display:inline-block;padding:.12rem .55rem;border-radius:999px;font-size:.75rem;font-weight:700}
.sig-chip.high{background:rgba(198,255,61,.16);color:#c6ff3d}.sig-chip.medium{background:rgba(255,190,60,.16);color:#ffc04a}.sig-chip.low{background:rgba(255,90,80,.16);color:#ff7a70}
.sig-why{font-size:.83rem;color:#9aa3ad;margin:.25rem 0}
.sig-cols{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1.3fr);gap:14px;margin-top:.5rem}
@media(max-width:820px){.sig-cols{grid-template-columns:1fr}}
.sig-laps{font-size:.84rem;width:100%;border-collapse:collapse}
.sig-laps td,.sig-laps th{padding:.22rem .4rem;border-bottom:1px solid rgba(255,255,255,.06);text-align:left}
.sig-laps tr.del td{color:#6e737b;text-decoration:line-through}
.sig-laps tr.flag td:first-child{box-shadow:inset 3px 0 #ffc04a}
.sig-issues{list-style:none;margin:0;padding:0;display:grid;gap:.4rem}
.sig-issues li{background:rgba(255,192,74,.07);border:1px solid rgba(255,192,74,.25);border-radius:10px;padding:.45rem .6rem;font-size:.84rem}
.sig-issues li.ok{background:rgba(198,255,61,.06);border-color:rgba(198,255,61,.22)}
.sig-issues li b{color:#ffc04a}.sig-issues li button{margin-left:.4rem;padding:.15rem .6rem;font-size:.76rem}
.sig-dirty{color:#ffc04a;font-size:.82rem;font-weight:700}
.sig-msg{font-size:.84rem;margin-left:.3rem}
</style>`

const signalJS = `<script>
(function(){
const race = window.SIG_RACE;
const host = document.getElementById('sigPilots');
let data = null, view = {t0: -1, t1: 60}, maxT = 60;
const pilots = []; // per-pilot state

function api(path, body){
  return fetch('/marshal/api/' + path, body ? {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body), credentials:'same-origin'} : {credentials:'same-origin'})
    .then(r => r.json().then(j => { if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status)); return j; }));
}
const fmtT = ms => (ms/1000).toFixed(2);
const clock = s => { s = Math.max(0, s); const m = Math.floor(s/60); return m + ':' + (s - m*60).toFixed(1).padStart(4,'0'); };
const esc = t => String(t).replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));

function load(fresh){
  document.getElementById('sigWhere').textContent = 'Reading the signal traces…';
  api('race?race=' + encodeURIComponent(race) + (fresh ? '&fresh=1' : '')).then(d => {
    data = d; host.innerHTML = ''; pilots.length = 0;
    maxT = 10;
    d.pilots.forEach(p => { if (p.t && p.t.length) maxT = Math.max(maxT, p.t[p.t.length-1]); });
    view = {t0: -1, t1: maxT + 1};
    document.getElementById('sigWhere').textContent = 'From ' + d.where + (d.target ? ' · ' + d.target + ' laps' : '') + (d.holeshot ? ' · first pass is the holeshot' : '') + (d.minLapMs ? ' · minimum lap ' + (d.minLapMs/1000) + 's' : '');
    if (!d.pilots.length) host.innerHTML = '<div class="card"><p class="mut">The timer kept no signal for this race.</p></div>';
    d.pilots.forEach(p => addPilot(p));
  }).catch(e => { document.getElementById('sigWhere').innerHTML = '<span class="warn">' + esc(e.message) + '</span> · <a href="/marshal?race=' + encodeURIComponent(race) + '&simple=1">Use the simple view</a>'; });
}

function addPilot(p){
  const el = document.createElement('div');
  el.className = 'card sigp';
  el.innerHTML = '<h3>' + esc(p.name || p.pilot) + (p.node >= 0 ? ' <small>node ' + (p.node+1) + (p.freq ? ' · ' + p.freq + ' MHz' : '') + '</small>' : '') + ' <span class="sig-dirty" hidden>unsaved changes</span></h3>' +
    '<canvas class="sig-canvas"></canvas>' +
    '<div class="sig-bar"><span class="sig-lines"></span>' +
    '<button class="sec b-auto">Auto-detect</button><button class="sec b-reset">Reset</button>' +
    '<button class="b-apply">Save laps</button><button class="sec b-cal" title="Set this node\'s lines for the races to come">Use these lines from now on</button><span class="sig-msg"></span></div>' +
    '<div class="sig-why"></div>' +
    '<div class="sig-cols"><table class="sig-laps"></table><ul class="sig-issues"></ul></div>';
  host.appendChild(el);
  const st = {p, el, canvas: el.querySelector('canvas'), enter: p.savedEnter, exit: p.savedExit, removed: [], added: [], res: null, sug: p.suggest, dirty: false};
  pilots.push(st);
  wire(st);
  // Open on exactly what the timer has saved: a saved lap the lines don't
  // produce was added by hand, a produced lap that isn't saved was removed.
  api('analyse', {race, pilot: p.pilot, enter: Math.round(st.enter), exit: Math.round(st.exit)}).then(res => {
    const saved = p.saved || [], kept = res.crossings.filter(c => !c.x).map(c => c.t);
    st.added = saved.filter(s => !kept.some(k => Math.abs(k - s) < 250)).map(Math.round);
    st.removed = kept.filter(k => !saved.some(s => Math.abs(k - s) < 250)).map(Math.round);
    if (st.added.length || st.removed.length) analyse(st); else { st.res = res; render(st); }
  }).catch(e => msg(st, e.message, true));
}

let pending = new Map();
function analyse(st){
  clearTimeout(pending.get(st));
  pending.set(st, setTimeout(() => {
    api('analyse', {race, pilot: st.p.pilot, enter: Math.round(st.enter), exit: Math.round(st.exit), removed: st.removed, added: st.added})
      .then(res => { st.res = res; render(st); }).catch(e => msg(st, e.message, true));
  }, 90));
}

function msg(st, text, bad){ const m = st.el.querySelector('.sig-msg'); m.textContent = text; m.className = 'sig-msg ' + (bad ? 'warn' : 'ok'); }
function dirty(st, on){ st.dirty = on; st.el.querySelector('.sig-dirty').hidden = !on; }

function render(st){
  draw(st);
  const r = st.res; if (!r) return;
  st.el.querySelector('.sig-lines').innerHTML = 'Enter <b class="en">' + Math.round(st.enter) + '</b> · Exit <b class="ex">' + Math.round(st.exit) + '</b>' +
    (Math.round(st.enter) !== st.p.savedEnter || Math.round(st.exit) !== st.p.savedExit ? ' <span class="mut">(saved ' + st.p.savedEnter + ' / ' + st.p.savedExit + ')</span>' : '');
  const why = st.el.querySelector('.sig-why');
  why.innerHTML = st.sug ? '<span class="sig-chip ' + st.sug.Confidence + '">auto-detect: ' + st.sug.Confidence + ' confidence</span> ' + esc(st.sug.Reason) : '';
  // Laps
  const flagged = new Set((r.issues || []).map(i => Math.round(i.StampMs)));
  let rows = '<tr><th>Lap</th><th>Time</th><th>At</th><th>Peak</th></tr>';
  let n = data.holeshot ? -1 : 0, prev = 0;
  r.crossings.forEach(c => {
    const del = !!c.x; let lap = '', time = '';
    if (!del){ n++; lap = n === 0 ? 'HS' : n; time = n === 0 ? fmtT(c.t) : fmtT(c.t - prev); prev = c.t; }
    rows += '<tr class="' + (del ? 'del' : '') + (flagged.has(Math.round(c.t)) ? ' flag' : '') + '" title="' + esc(c.why || '') + '"><td>' + (del ? '–' : lap) + '</td><td>' + time + '</td><td>' + clock(c.t/1000) + '</td><td>' + Math.round(c.p || 0) + '</td></tr>';
  });
  st.el.querySelector('.sig-laps').innerHTML = rows;
  // Issues
  const ul = st.el.querySelector('.sig-issues');
  if (!r.issues || !r.issues.length){ ul.innerHTML = '<li class="ok">Nothing looks wrong: lap times are consistent and every pass stands clear of the background.</li>'; }
  else {
    ul.innerHTML = r.issues.map((is, i) => '<li><b>' + clock(is.StampMs/1000) + '</b> ' + esc(is.Text) +
      (is.SuggestS ? '<button class="sec" data-add="' + Math.round(is.SuggestS*1000) + '">Add lap here</button>' : '') +
      ((is.Kind === 'short' || is.Kind === 'crosstalk' || is.Kind === 'weak') ? '<button class="sec" data-del="' + Math.round(is.StampMs) + '">Remove it</button>' : '') +
      '<button class="sec" data-go="' + is.StampMs + '">Show</button></li>').join('');
  }
}

function wire(st){
  const c = st.canvas;
  st.el.querySelector('.b-auto').onclick = () => api('auto', {race, pilot: st.p.pilot}).then(res => {
    st.sug = res.suggest; st.enter = res.enter; st.exit = res.exit; st.removed = []; st.added = []; st.res = res; dirty(st, true); render(st);
    msg(st, 'Lines found: check the laps, then save.');
  }).catch(e => msg(st, e.message, true));
  st.el.querySelector('.b-reset').onclick = () => { st.enter = st.p.savedEnter; st.exit = st.p.savedExit; st.removed = []; st.added = []; dirty(st, false); msg(st, ''); analyse(st); };
  st.el.querySelector('.b-apply').onclick = () => api('apply', {race, pilot: st.p.pilot, enter: Math.round(st.enter), exit: Math.round(st.exit), removed: st.removed, added: st.added})
    .then(r => { st.p.savedEnter = Math.round(st.enter); st.p.savedExit = Math.round(st.exit); dirty(st, false); msg(st, 'Saved: ' + r.laps + ' laps. Results and brackets update now.'); render(st); })
    .catch(e => msg(st, 'Not saved: ' + e.message, true));
  st.el.querySelector('.b-cal').onclick = () => api('calibrate', {race, pilot: st.p.pilot, enter: Math.round(st.enter), exit: Math.round(st.exit)})
    .then(r => msg(st, 'Node ' + r.node + ' now uses enter ' + Math.round(st.enter) + ', exit ' + Math.round(st.exit) + ' for the races to come.'))
    .catch(e => msg(st, e.message, true));
  st.el.querySelector('.sig-issues').onclick = ev => {
    const b = ev.target.closest('button'); if (!b) return;
    if (b.dataset.add){ st.added.push(+b.dataset.add); dirty(st, true); analyse(st); }
    if (b.dataset.del){ st.removed.push(+b.dataset.del); dirty(st, true); analyse(st); }
    if (b.dataset.go){ const t = +b.dataset.go/1000, w = Math.max(6, (view.t1-view.t0)/4); view = {t0: t - w/2, t1: t + w/2}; drawAll(); }
  };

  // Pointer: drag a line, pan, click a pass, shift-click to add.
  let drag = null;
  const geo = () => { const r = c.getBoundingClientRect(); return {r, w: r.width, h: r.height}; };
  const yOf = (v, g) => g.h - 18 - (v - st.lo) / (st.hi - st.lo) * (g.h - 30);
  const vOf = (y, g) => st.lo + (g.h - 18 - y) / (g.h - 30) * (st.hi - st.lo);
  const tOf = (x, g) => view.t0 + x / g.w * (view.t1 - view.t0);
  c.addEventListener('pointerdown', ev => {
    const g = geo(), x = ev.clientX - g.r.left, y = ev.clientY - g.r.top;
    c.setPointerCapture(ev.pointerId);
    if (Math.abs(y - yOf(st.enter, g)) < 8) drag = {kind: 'enter'};
    else if (Math.abs(y - yOf(st.exit, g)) < 8) drag = {kind: 'exit'};
    else drag = {kind: 'pan', x, t0: view.t0, t1: view.t1, moved: false, shift: ev.shiftKey};
  });
  c.addEventListener('pointermove', ev => {
    const g = geo(), x = ev.clientX - g.r.left, y = ev.clientY - g.r.top;
    if (!drag){ c.style.cursor = (Math.abs(y - yOf(st.enter, g)) < 8 || Math.abs(y - yOf(st.exit, g)) < 8) ? 'ns-resize' : 'crosshair'; return; }
    if (drag.kind === 'enter'){ st.enter = Math.max(st.exit + 1, Math.round(vOf(y, g))); dirty(st, true); draw(st); analyse(st); }
    else if (drag.kind === 'exit'){ st.exit = Math.min(st.enter - 1, Math.max(1, Math.round(vOf(y, g)))); dirty(st, true); draw(st); analyse(st); }
    else { const dt = (x - drag.x) / g.w * (drag.t1 - drag.t0); if (Math.abs(x - drag.x) > 3) drag.moved = true; view = {t0: drag.t0 - dt, t1: drag.t1 - dt}; drawAll(); }
  });
  c.addEventListener('pointerup', ev => {
    const g = geo(), x = ev.clientX - g.r.left, d = drag; drag = null;
    if (!d || d.kind !== 'pan' || d.moved || !st.res) return;
    const tm = tOf(x, g) * 1000;
    if (d.shift){ st.added.push(Math.round(tm)); dirty(st, true); analyse(st); return; }
    // Toggle the nearest crossing (within a third of a second).
    let best = null;
    st.res.crossings.forEach(cr => { const dd = Math.abs(cr.t - tm); if (dd < 350 && (!best || dd < Math.abs(best.t - tm))) best = cr; });
    if (!best) return;
    const r = Math.round(best.t);
    const ai = st.added.findIndex(a => Math.abs(a - r) < 60);
    if (ai >= 0) st.added.splice(ai, 1);
    else { const ri = st.removed.findIndex(a => Math.abs(a - r) < 60); if (ri >= 0) st.removed.splice(ri, 1); else st.removed.push(r); }
    dirty(st, true); analyse(st);
  });
  c.addEventListener('wheel', ev => {
    ev.preventDefault();
    const g = geo(), x = ev.clientX - g.r.left, t = tOf(x, g), k = Math.exp(ev.deltaY * 0.0015);
    let t0 = t - (t - view.t0) * k, t1 = t + (view.t1 - t) * k;
    if (t1 - t0 < 1.5) return;
    if (t1 - t0 > maxT + 4){ t0 = -1; t1 = maxT + 1; }
    view = {t0, t1}; drawAll();
  }, {passive: false});
  new ResizeObserver(() => draw(st)).observe(c);
}

function drawAll(){ pilots.forEach(draw); }

function draw(st){
  const c = st.canvas, dpr = window.devicePixelRatio || 1, r = c.getBoundingClientRect();
  if (!r.width) return;
  c.width = r.width * dpr; c.height = r.height * dpr;
  const g = c.getContext('2d'); g.setTransform(dpr, 0, 0, dpr, 0, 0);
  const w = r.width, h = r.height, p = st.p;
  let lo = Infinity, hi = -Infinity;
  p.v.forEach(v => { if (v < lo) lo = v; if (v > hi) hi = v; });
  lo = Math.min(lo, st.exit) - 4; hi = Math.max(hi, st.enter) + 6; st.lo = lo; st.hi = hi;
  const X = t => (t - view.t0) / (view.t1 - view.t0) * w;
  const Y = v => h - 18 - (v - lo) / (hi - lo) * (h - 30);
  g.clearRect(0, 0, w, h);
  // time grid
  g.font = '11px ui-monospace, monospace'; g.fillStyle = '#5c636c'; g.strokeStyle = 'rgba(255,255,255,.05)';
  const span = view.t1 - view.t0, step = span > 120 ? 20 : span > 60 ? 10 : span > 25 ? 5 : span > 10 ? 2 : 1;
  for (let t = Math.ceil(view.t0/step)*step; t < view.t1; t += step){ const x = X(t); g.beginPath(); g.moveTo(x, 0); g.lineTo(x, h - 16); g.stroke(); g.fillText(clock(t), x + 3, h - 4); }
  // crossings
  if (st.res) st.res.crossings.forEach(cr => {
    const x0 = X(cr.s), x1 = Math.max(X(cr.e), x0 + 3);
    g.fillStyle = cr.x ? 'rgba(140,140,150,.16)' : 'rgba(198,255,61,.16)'; g.fillRect(x0, 0, x1 - x0, h - 16);
  });
  // trace (min/max per pixel)
  g.strokeStyle = '#3de0ff'; g.lineWidth = 1.2; g.beginPath();
  let started = false;
  for (let i = 0; i < p.t.length; i++){
    const t = p.t[i]; if (t < view.t0 - 1 || t > view.t1 + 1) continue;
    const x = X(t), y = Y(p.v[i]);
    if (!started){ g.moveTo(x, y); started = true; } else g.lineTo(x, y);
  }
  g.stroke();
  // lines
  const line = (v, col, label) => { const y = Y(v); g.strokeStyle = col; g.lineWidth = 2; g.setLineDash([6, 4]); g.beginPath(); g.moveTo(0, y); g.lineTo(w, y); g.stroke(); g.setLineDash([]); g.fillStyle = col; g.fillText(label + ' ' + Math.round(v), 6, y - 4); };
  line(st.exit, '#ff9f3d', 'exit'); line(st.enter, '#ff5a4f', 'enter');
  // laps
  if (st.res){
    let n = data.holeshot ? -1 : 0, prev = 0, lastX = -1e9, row = 0;
    st.res.crossings.forEach(cr => {
      if (cr.x) return; n++;
      const x = X(cr.t/1000); g.strokeStyle = '#c6ff3d'; g.lineWidth = 1.5; g.beginPath(); g.moveTo(x, 12); g.lineTo(x, h - 16); g.stroke();
      g.fillStyle = '#c6ff3d'; g.font = 'bold 11px ui-monospace, monospace';
      row = (x - lastX < 78) ? (row + 1) % 2 : 0; // labels that would collide take turns on two rows
      g.fillText(n === 0 ? 'HS' : 'L' + n + ' ' + fmtT(cr.t - prev), x + 3, 11 + row * 13); prev = cr.t; lastX = x; g.font = '11px ui-monospace, monospace';
    });
    (st.res.issues || []).forEach(is => {
      const x = X(is.StampMs/1000); g.fillStyle = '#ffc04a'; g.beginPath(); g.moveTo(x, 20); g.lineTo(x - 6, 31); g.lineTo(x + 6, 31); g.closePath(); g.fill();
      if (is.SuggestS){ const sx = X(is.SuggestS); g.strokeStyle = '#ffc04a'; g.setLineDash([3, 3]); g.beginPath(); g.moveTo(sx, 14); g.lineTo(sx, h - 16); g.stroke(); g.setLineDash([]); }
    });
  }
}

document.getElementById('sigFresh').onclick = () => load(true);
document.getElementById('sigAutoAll').onclick = () => pilots.forEach(st => st.el.querySelector('.b-auto').click());
window.addEventListener('beforeunload', e => { if (pilots.some(p => p.dirty)) { e.preventDefault(); e.returnValue = ''; } });
load(false);
})();
</script>`
