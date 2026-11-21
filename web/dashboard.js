'use strict';

const get = id => document.getElementById(id);
const charts = [
  {canvas: get('temp-chart'), field: 'temperature', color: '#72d7a9'},
  {canvas: get('humidity-chart'), field: 'humidity', color: '#8dbcf0'}
];
const clock = new Intl.DateTimeFormat(undefined, {hour: '2-digit', minute: '2-digit', second: '2-digit'});
const dateTime = new Intl.DateTimeFormat(undefined, {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'});
const date = new Intl.DateTimeFormat(undefined, {month: 'short', day: 'numeric'});
let span = 86400000, range = span, pinnedEnd = null;
let points = [], displayed, cursor = null, request = 0;
let axisLimits = {};

async function readJSON(url) {
  const response = await fetch(url);
  if (!response.ok) throw new Error('Dashboard data unavailable');
  return response.json();
}

async function refresh() {
  const version = ++request;
  try {
    const next = await readJSON('/api/status');
    if (version !== request) return;
    const to = pinnedEnd ?? (range === 0 ? next.latest?.at ?? next.now : next.now);
    if (pinnedEnd === null && range === 0) span = Math.max(300000, to - (next.first || to));
    const from = Math.max(0, to - span);
    const key = [range, span, pinnedEnd, next.latest?.at, next.first,
      pinnedEnd === null && range !== 0 ? Math.floor(to / Math.max(1000, span / 600)) : null].join('/');
    if (displayed?.key !== key) {
      const rows = await readJSON('/api/readings?from=' + from + '&to=' + to);
      if (version !== request) return;
      points = rows;
      displayed = {from, to, key};
    }
    const latest = next.latest;
    get('status').textContent = next.state;
    get('temperature').textContent = latest ? latest.temperature.toFixed(1) + ' °C' : '—';
    get('humidity').textContent = latest ? latest.humidity + '%' : '—';
    get('battery').textContent = latest ? latest.battery + '%' : '—';
    get('battery-voltage').textContent = latest?.millivolts > 0
      ? (latest.millivolts / 1000).toFixed(3) + ' V · sensor estimate'
      : 'Sensor estimate';
    get('seen').textContent = latest ? dateTime.format(latest.at) : '—';
    if (latest && next.state === 'Collecting' && next.now - latest.at > Math.max(90000, next.interval * 3)) {
      get('status').textContent = 'Waiting for a new reading';
    }
    get('window').textContent = dateTime.format(displayed.from) + ' – ' + dateTime.format(displayed.to);
    draw();
  } catch (error) {
    if (version === request) get('status').textContent = error.message;
  }
}

function draw() {
  for (const chart of charts) render(chart);
  get('detail').textContent = cursor
    ? clock.format(cursor.at) + ' · ' + cursor.temperature.toFixed(1) + ' °C · ' + cursor.humidity + '% RH'
    : '';
}

function render({canvas, field, color}) {
  const width = canvas.clientWidth, height = canvas.clientHeight;
  if (width < 100 || !displayed) return;
  const ratio = window.devicePixelRatio || 1;
  canvas.width = Math.round(width * ratio);
  canvas.height = Math.round(height * ratio);
  const ctx = canvas.getContext('2d');
  ctx.scale(ratio, ratio);
  const left = 48, right = width - 14, top = 12, bottom = height - 24;
  const duration = displayed.to - displayed.from;
  const x = at => left + (at - displayed.from) * (right - left) / duration;
  ctx.font = '11px system-ui';
  const visible = points.filter(p => p.at >= displayed.from && p.at <= displayed.to);
  if (!visible.length) {
    ctx.fillStyle = '#a1b4ac';
    ctx.textAlign = 'center';
    ctx.fillText('No readings in this window', width / 2, height / 2);
    return;
  }
  let [min, max] = axisLimits[field] ?? [Infinity, -Infinity];
  for (const point of visible) {
    if (point[field] < min) min = Math.floor(point[field]) - 1;
    if (point[field] > max) max = Math.ceil(point[field]) + 1;
  }
  axisLimits[field] = [min, max];
  const y = value => bottom - (value - min) * (bottom - top) / (max - min);
  for (let i = 0; i <= 4; i++) {
    const value = min + (max - min) * i / 4;
    ctx.strokeStyle = '#304039';
    ctx.beginPath();
    ctx.moveTo(left, y(value)); ctx.lineTo(right, y(value)); ctx.stroke();
    ctx.fillStyle = '#a1b4ac';
    ctx.textAlign = 'right';
    ctx.fillText(value.toFixed(1), left - 8, y(value) + 4);
  }
  const ticks = width < 480 ? 3 : 5;
  for (let i = 0; i < ticks; i++) {
    const at = displayed.from + duration * i / (ticks - 1);
    ctx.textAlign = i === 0 ? 'left' : i === ticks - 1 ? 'right' : 'center';
    ctx.fillText((duration > 172800000 ? date : clock).format(at), x(at), height - 6);
  }
  ctx.strokeStyle = color;
  ctx.fillStyle = color;
  ctx.lineWidth = 1.6;
  ctx.beginPath();
  let previous;
  for (const point of visible) {
    const px = x(point.at), py = y(point[field]);
    if (!previous || point.segment !== previous.segment) ctx.moveTo(px, py);
    else ctx.lineTo(px, py);
    previous = point;
  }
  ctx.stroke();
  if (visible.length < 250) {
    for (const point of visible) {
      ctx.beginPath(); ctx.arc(x(point.at), y(point[field]), 2, 0, Math.PI * 2); ctx.fill();
    }
  }
  if (cursor && cursor.at >= displayed.from && cursor.at <= displayed.to) {
    ctx.strokeStyle = '#72877d';
    ctx.beginPath(); ctx.moveTo(x(cursor.at), top); ctx.lineTo(x(cursor.at), bottom); ctx.stroke();
    ctx.beginPath(); ctx.arc(x(cursor.at), y(cursor[field]), 4, 0, Math.PI * 2); ctx.fill();
  }
}

function navigate() {
  axisLimits = {};
  cursor = null;
  get('live').setAttribute('aria-pressed', pinnedEnd === null);
  for (const button of document.querySelectorAll('[data-range]')) {
    button.setAttribute('aria-pressed', Number(button.dataset.range) === range);
  }
  refresh();
}

get('ranges').addEventListener('click', event => {
  const selected = event.target.closest('[data-range]');
  if (!selected) return;
  range = Number(selected.dataset.range);
  span = range || 300000;
  pinnedEnd = null;
  navigate();
});
get('earlier').onclick = () => {
  if (!displayed) return;
  pinnedEnd = Math.max(span, Math.floor(displayed.to - span / 2));
  navigate();
};
get('later').onclick = () => {
  if (!displayed) return;
  const end = Math.floor(displayed.to + span / 2);
  pinnedEnd = end < Date.now() ? end : null;
  navigate();
};
get('zoom').onclick = () => {
  if (!displayed) return;
  range = -1;
  span = Math.max(30000, Math.floor(span / 2));
  navigate();
};
get('live').onclick = () => {
  pinnedEnd = null;
  navigate();
};
for (const {canvas} of charts) {
  canvas.addEventListener('pointermove', event => {
    if (!displayed) return;
    const fraction = Math.max(0, Math.min(1, (event.offsetX - 48) / (canvas.clientWidth - 62)));
    const at = displayed.from + fraction * (displayed.to - displayed.from);
    cursor = points.reduce((best, p) =>
      !best || Math.abs(p.at - at) < Math.abs(best.at - at) ? p : best, null);
    if (cursor && Math.abs(cursor.at - at) > (displayed.to - displayed.from) * 0.08) cursor = null;
    draw();
  });
  canvas.addEventListener('pointerleave', () => { cursor = null; draw(); });
  new ResizeObserver(draw).observe(canvas);
}
refresh();
setInterval(refresh, 5000);
