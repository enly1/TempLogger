'use strict';

const get = id => document.getElementById(id);
const charts = [
  {canvas: get('temp-chart'), field: 'temperature', color: '#72d7a9'},
  {canvas: get('humidity-chart'), field: 'humidity', color: '#8dbcf0'},
  {canvas: get('battery-chart'), field: 'battery', color: '#e6bc72'},
  {canvas: get('voltage-chart'), field: 'millivolts', color: '#bb9bed'},
  {canvas: get('rssi-chart'), field: 'rssi', color: '#e98da8'}
];
const sensorPalette = ['#72d7a9', '#8dbcf0', '#e6bc72', '#e98da8', '#bb9bed', '#8ed6d1', '#e5a46f', '#c1d979'];
const sensorColors = new Map();
const sensorColor = deviceId => {
  if (!sensorColors.has(deviceId)) sensorColors.set(deviceId, sensorPalette[sensorColors.size % sensorPalette.length]);
  return sensorColors.get(deviceId);
};
const clock = new Intl.DateTimeFormat(undefined, {hour: '2-digit', minute: '2-digit', second: '2-digit'});
const dateTime = new Intl.DateTimeFormat(undefined, {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'});
const date = new Intl.DateTimeFormat(undefined, {month: 'short', day: 'numeric'});
let span = 86400000, range = span, pinnedEnd = null;
let points = [], sensorPoints = [], sensors = [], displayed, cursor = null, request = 0;
const selectedSensors = new Set();
const knownSensors = new Set();
let axisLimits = {};
let chartMode = 'smooth';
let mockMode = false, drag = null;
const BUCKETS_PER_PIXEL = 2;
const MAX_BUCKETS = 5000;

function mockReadings(from, to, buckets) {
  const duration = Math.max(1000, to - from);
  const count = Math.max(2, Math.min(MAX_BUCKETS, buckets));
  const result = [];
  for (let i = 0; i <= count; i++) {
    const fraction = i / count;
    const at = Math.round(from + duration * fraction);
    const seconds = (at - from) / 1000;
    const gap = fraction > 0.64 && fraction < 0.68;
    if (gap) continue;
    result.push({
      at,
      temperature: 20.5 + 2.2 * Math.sin(seconds * Math.PI / 43200) + 0.45 * Math.sin(seconds * 2 * Math.PI / 90),
      humidity: Math.round(54 + 10 * Math.cos(seconds * Math.PI / 36000) + 3 * Math.sin(seconds * 2 * Math.PI / 120)),
      battery: Math.round(85 + 6 * Math.cos(seconds * 2 * Math.PI / 21600)), millivolts: 2900, rssi: -63,
      segment: fraction >= 0.68 ? 1 : 0
    });
  }
  // Add one-second samples only when the selected window is five minutes or
  // shorter. Longer views stay evenly sampled across their full timeline.
  if (duration > 5 * 60000) return result;
  const byTime = new Map(result.map(point => [point.at, point]));
  const denseFrom = Math.max(from, to - 5 * 60000);
  const denseCount = Math.ceil((to - denseFrom) / 1000);
  for (let i = 0; i <= denseCount; i++) {
    const at = Math.min(to, denseFrom + i * 1000);
    const fraction = (at - from) / duration;
    const seconds = (at - from) / 1000;
    byTime.set(at, {
      at,
      temperature: 20.5 + 2.2 * Math.sin(seconds * Math.PI / 43200) + 0.45 * Math.sin(seconds * 2 * Math.PI / 90),
      humidity: Math.round(54 + 10 * Math.cos(seconds * Math.PI / 36000) + 3 * Math.sin(seconds * 2 * Math.PI / 120)),
      battery: Math.round(85 + 6 * Math.cos(seconds * 2 * Math.PI / 21600)), millivolts: 2900, rssi: -63,
      segment: fraction >= 0.68 ? 1 : 0
    });
  }
  return [...byTime.values()].sort((a, b) => a.at - b.at);
}

function sampleSensorRows(rows, from, to, buckets) {
  const width = Math.max(1, (to - from) / Math.max(1, buckets));
  const grouped = new Map();
  for (const row of rows) {
    if (!grouped.has(row.deviceId)) grouped.set(row.deviceId, []);
    grouped.get(row.deviceId).push(row);
  }
  const result = [];
  for (const series of grouped.values()) {
    const keep = new Set([0, series.length - 1]);
    const byBucket = new Map();
    for (let i = 0; i < series.length; i++) {
      if (i && series[i].segment !== series[i - 1].segment) { keep.add(i - 1); keep.add(i); }
      const bucket = Math.floor((series[i].at - from) / width);
      if (!byBucket.has(bucket)) byBucket.set(bucket, []);
      byBucket.get(bucket).push(i);
    }
    for (const indexes of byBucket.values()) {
      keep.add(indexes[0]); keep.add(indexes[indexes.length - 1]);
      for (const field of ['temperature', 'humidity', 'battery', 'millivolts']) {
        const valid = indexes.filter(i => series[i][field] != null);
        if (!valid.length) continue;
        keep.add(valid.reduce((best, i) => series[i][field] < series[best][field] ? i : best));
        keep.add(valid.reduce((best, i) => series[i][field] > series[best][field] ? i : best));
      }
    }
    for (const i of [...keep].sort((a, b) => a - b)) result.push(series[i]);
  }
  return result.sort((a, b) => a.at - b.at || a.deviceId.localeCompare(b.deviceId));
}

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
    const to = pinnedEnd ?? (range === 0 && !mockMode ? next.latest?.at ?? next.now : next.now);
    if (pinnedEnd === null && range === 0) {
      span = mockMode && !next.first
        ? 7 * 86400000
        : Math.max(300000, to - (next.first || to));
    }
    const from = Math.max(0, to - span);
    const plotWidth = Math.max(1, charts[0].canvas.clientWidth - 62);
    const buckets = Math.min(MAX_BUCKETS, Math.max(1, Math.ceil(plotWidth * BUCKETS_PER_PIXEL)));
    const key = [range, span, pinnedEnd, next.latest?.at, next.first, buckets,
      pinnedEnd === null && range !== 0 ? Math.floor(to / Math.max(1000, span / 600)) : null].join('/');
    if (displayed?.key !== key) {
      if (mockMode) {
        points = mockReadings(from, to, buckets);
        sensorPoints = [];
      } else {
        const [rows, allSensors, sensorRows] = await Promise.all([
          readJSON('/api/readings?from=' + from + '&to=' + to + '&buckets=' + buckets),
          readJSON('/api/sensors'),
          readJSON('/api/sensor-readings?from=' + from + '&to=' + to + '&buckets=' + buckets)
        ]);
        if (version !== request) return;
        points = rows;
        sensorPoints = sampleSensorRows(sensorRows, from, to, buckets);
        sensors = allSensors;
        for (const sensor of sensors) {
          if (!knownSensors.has(sensor.deviceId)) selectedSensors.add(sensor.deviceId);
          knownSensors.add(sensor.deviceId);
        }
        for (const id of [...knownSensors]) {
          if (!sensors.some(sensor => sensor.deviceId === id)) {
            knownSensors.delete(id);
            selectedSensors.delete(id);
          }
        }
        renderSensorToggles();
      }
      displayed = {from, to, key};
    }
    const latest = mockMode ? points[points.length - 1] : sensorPoints[sensorPoints.length - 1] ?? next.latest;
    get('status').textContent = mockMode ? 'Mock data · local preview' : next.state;
    get('temperature').textContent = latest?.temperature != null ? latest.temperature.toFixed(1) + ' °C' : '—';
    get('humidity').textContent = latest?.humidity != null ? latest.humidity + '%' : '—';
    get('battery').textContent = latest?.battery != null ? latest.battery + '%' : '—';
    get('battery-voltage').textContent = latest?.millivolts > 0
      ? (latest.millivolts / 1000).toFixed(3) + ' V · sensor estimate'
      : 'Sensor estimate';
    get('seen').textContent = latest ? dateTime.format(latest.at) : '—';
    if (!mockMode && latest && next.state === 'Collecting' && next.now - latest.at > Math.max(90000, next.interval * 3)) {
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
    ? [clock.format(cursor.at), cursor.deviceId || '', cursor.temperature == null ? '' : cursor.temperature.toFixed(1) + ' °C', cursor.humidity == null ? '' : cursor.humidity + '% RH', cursor.battery == null ? '' : cursor.battery + '% battery', cursor.millivolts == null ? '' : cursor.millivolts + ' mV', cursor.rssi == null ? '' : cursor.rssi + ' dBm'].filter(Boolean).join(' · ')
    : '';
}

function renderSensorToggles() {
  const list = get('sensor-list');
  list.hidden = mockMode || sensors.length === 0;
  list.querySelectorAll('label').forEach(label => label.remove());
  for (const sensor of sensors) {
    const label = document.createElement('label');
    label.style.color = sensorColor(sensor.deviceId);
    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.checked = selectedSensors.has(sensor.deviceId);
    checkbox.addEventListener('change', () => {
      if (checkbox.checked) selectedSensors.add(sensor.deviceId);
      else selectedSensors.delete(sensor.deviceId);
      axisLimits = {};
      draw();
    });
    label.append(checkbox, document.createTextNode(`${sensor.deviceId} · ${sensor.format}`));
    list.append(label);
  }
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
  const source = sensorPoints.length ? sensorPoints.filter(p => selectedSensors.has(p.deviceId)) : points;
  const visible = source.filter(p => p[field] != null && p.at >= displayed.from && p.at <= displayed.to);
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
  const groups = source === points ? new Map([['legacy', visible]]) : new Map(sensors.filter(s => selectedSensors.has(s.deviceId)).map(s => [s.deviceId, visible.filter(p => p.deviceId === s.deviceId)]));
  for (const [deviceId, series] of groups) {
    const seriesColor = source === points ? color : sensorColor(deviceId);
    if (!series.length) continue;
    ctx.strokeStyle = seriesColor; ctx.fillStyle = seriesColor; ctx.lineWidth = 1.6; ctx.beginPath();
    const segments = [];
    for (const point of series) {
      const segment = segments[segments.length - 1];
      if (!segment || point.segment !== segment[0].segment) segments.push([point]);
      else segment.push(point);
    }
    for (const segment of segments) {
    const coords = segment.map(point => [x(point.at), y(point[field])]);
    ctx.moveTo(coords[0][0], coords[0][1]);
    if (chartMode === 'raw' || coords.length < 3) {
      for (let i = 1; i < coords.length; i++) ctx.lineTo(coords[i][0], coords[i][1]);
      continue;
    }
    // Catmull-Rom control points make a smooth curve that passes through every
    // sampled reading. Draw each gap-delimited segment independently.
    for (let i = 0; i < coords.length - 1; i++) {
      const p0 = coords[Math.max(0, i - 1)], p1 = coords[i];
      const p2 = coords[i + 1], p3 = coords[Math.min(coords.length - 1, i + 2)];
      ctx.bezierCurveTo(
        p1[0] + (p2[0] - p0[0]) / 6, p1[1] + (p2[1] - p0[1]) / 6,
        p2[0] - (p3[0] - p1[0]) / 6, p2[1] - (p3[1] - p1[1]) / 6,
        p2[0], p2[1]
      );
    }
    }
    ctx.stroke();
  }
  if (chartMode === 'raw' && visible.length < 250) {
    for (const point of visible) {
      ctx.beginPath(); ctx.arc(x(point.at), y(point[field]), 2, 0, Math.PI * 2); ctx.fill();
    }
  }
  if (cursor && cursor.at >= displayed.from && cursor.at <= displayed.to) {
    ctx.strokeStyle = '#72877d';
    ctx.beginPath(); ctx.moveTo(x(cursor.at), top); ctx.lineTo(x(cursor.at), bottom); ctx.stroke();
    ctx.beginPath(); ctx.arc(x(cursor.at), y(cursor[field]), 4, 0, Math.PI * 2); ctx.fill();
  }
  if (drag && drag.canvas === canvas) {
    const startX = Math.max(left, Math.min(right, drag.startX));
    const endX = Math.max(left, Math.min(right, drag.currentX));
    const color = drag.operation === 'delete' ? '#f08b82' : '#8dbcf0';
    ctx.fillStyle = drag.operation === 'delete' ? 'rgba(240, 139, 130, 0.2)' : 'rgba(141, 188, 240, 0.18)';
    ctx.fillRect(Math.min(startX, endX), top, Math.abs(endX - startX), bottom - top);
    ctx.strokeStyle = color;
    ctx.lineWidth = 1;
    ctx.strokeRect(Math.min(startX, endX), top, Math.abs(endX - startX), bottom - top);
  }
}

function setChartMode(mode) {
  chartMode = mode;
  get('smooth-mode').setAttribute('aria-pressed', mode === 'smooth');
  get('raw-mode').setAttribute('aria-pressed', mode === 'raw');
  draw();
}

async function confirmDeleteReadings(from, to) {
  const formatter = new Intl.DateTimeFormat(undefined, {dateStyle: 'medium', timeStyle: 'medium'});
  if (!window.confirm(`Permanently delete recorded readings from ${formatter.format(from)} through ${formatter.format(to)}? This cannot be undone.`)) return;
  try {
    const response = await fetch('/api/readings?from=' + from + '&to=' + to, {method: 'DELETE'});
    if (!response.ok) throw new Error('Could not delete readings');
    const result = await response.json();
    displayed = null;
    cursor = null;
    axisLimits = {};
    await refresh();
    get('detail').textContent = `Deleted ${result.deleted} reading${result.deleted === 1 ? '' : 's'}.`;
  } catch (error) {
    get('status').textContent = error.message;
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
get('smooth-mode').onclick = () => setChartMode('smooth');
get('raw-mode').onclick = () => setChartMode('raw');
for (const {canvas} of charts) {
  const timeAt = clientX => {
    const rect = canvas.getBoundingClientRect();
    const plotWidth = canvas.clientWidth - 62;
    const offsetX = clientX - rect.left;
    const fraction = Math.max(0, Math.min(1, (offsetX - 48) / plotWidth));
    return displayed.from + fraction * (displayed.to - displayed.from);
  };
  canvas.addEventListener('pointerdown', event => {
    if (!displayed || (event.button !== 0 && event.button !== 2)) return;
    const operation = event.button === 2 ? 'delete' : 'zoom';
    if (operation === 'delete' && mockMode) {
      get('status').textContent = 'Turn off Mock data before deleting recorded readings';
      event.preventDefault();
      return;
    }
    const rect = canvas.getBoundingClientRect();
    const x = event.clientX - rect.left;
    if (x < 48 || x > canvas.clientWidth - 14) return;
    if (operation === 'delete') event.preventDefault();
    drag = {canvas, operation, startX: x, currentX: x, startAt: timeAt(event.clientX), currentAt: timeAt(event.clientX)};
    cursor = null;
    canvas.setPointerCapture(event.pointerId);
    draw();
  });
  canvas.addEventListener('pointermove', event => {
    if (!displayed) return;
    if (drag?.canvas === canvas) {
      const rect = canvas.getBoundingClientRect();
      drag.currentX = event.clientX - rect.left;
      drag.currentAt = timeAt(event.clientX);
      draw();
      return;
    }
    const at = timeAt(event.clientX);
    const hoverPoints = sensorPoints.length ? sensorPoints.filter(p => selectedSensors.has(p.deviceId)) : points;
    cursor = hoverPoints.reduce((best, p) =>
      !best || Math.abs(p.at - at) < Math.abs(best.at - at) ? p : best, null);
    if (cursor && Math.abs(cursor.at - at) > (displayed.to - displayed.from) * 0.08) cursor = null;
    draw();
  });
  canvas.addEventListener('pointerup', event => {
    if (drag?.canvas !== canvas) return;
    const selection = drag;
    drag = null;
    if (Math.abs(selection.currentX - selection.startX) >= 8) {
      const from = Math.max(displayed.from, Math.floor(Math.min(selection.startAt, selection.currentAt)));
      const to = Math.min(displayed.to, Math.ceil(Math.max(selection.startAt, selection.currentAt)));
      if (to - from >= 1000) {
        if (selection.operation === 'delete') {
          void confirmDeleteReadings(from, to);
          return;
        }
        span = to - from;
        range = -1;
        pinnedEnd = to;
        navigate();
        return;
      }
    }
    cursor = null;
    draw();
  });
  canvas.addEventListener('pointercancel', () => { drag = null; draw(); });
  canvas.addEventListener('contextmenu', event => event.preventDefault());
  canvas.addEventListener('pointerleave', () => { if (!drag) { cursor = null; draw(); } });
  new ResizeObserver(refresh).observe(canvas);
}
get('mock-mode').onclick = () => {
  mockMode = !mockMode;
  get('mock-mode').setAttribute('aria-pressed', mockMode);
  renderSensorToggles();
  displayed = null;
  axisLimits = {};
  refresh();
};
refresh();
setInterval(refresh, 5000);
