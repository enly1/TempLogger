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
const sensorAlias = deviceId => sensors.find(sensor => sensor.deviceId === deviceId)?.alias?.trim() || deviceId;
const clock = new Intl.DateTimeFormat(undefined, {hour: '2-digit', minute: '2-digit', second: '2-digit'});
const dateTime = new Intl.DateTimeFormat(undefined, {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'});
const date = new Intl.DateTimeFormat(undefined, {month: 'short', day: 'numeric'});
let span = 86400000, range = span, pinnedEnd = null;
let points = [], sensorPoints = [], sensors = [], displayed, cursor = null, hoveredCanvas = null, request = 0;
let latestSensorReadings = [];
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
    const currentSensors = mockMode
      ? (points.length ? [{...points[points.length - 1], deviceId: 'Mock data', format: 'Preview'}] : [])
      : (next.latestSensors?.length ? next.latestSensors : next.latest ? [{...next.latest, deviceId: 'Sensor', format: 'ATC'}] : []);
    renderSensorMetrics(currentSensors);
    get('status').textContent = mockMode ? 'Mock data · local preview' : next.state;
    if (!mockMode && next.latest && next.state === 'Collecting' && next.now - next.latest.at > Math.max(90000, next.interval * 3)) {
      get('status').textContent = 'Waiting for a new reading';
    }
    get('window').textContent = dateTime.format(displayed.from) + ' – ' + dateTime.format(displayed.to);
    draw();
  } catch (error) {
    if (version === request) get('status').textContent = error.message;
  }
}

function renderSensorMetrics(currentSensors) {
  latestSensorReadings = currentSensors;
  const container = get('sensor-metrics');
  container.replaceChildren();
  for (const sensor of currentSensors) {
    const card = document.createElement('article');
    card.className = 'sensor-card';
    card.style.setProperty('--sensor-color', sensorColor(sensor.deviceId));

    const heading = document.createElement('div');
    heading.className = 'sensor-card-heading';
    const canEditAlias = sensor.deviceId && sensor.deviceId !== 'Mock data';
    const identity = document.createElement(canEditAlias ? 'button' : 'div');
    identity.className = canEditAlias ? 'sensor-card-identity sensor-name-button' : 'sensor-card-identity';
    if (canEditAlias) {
      identity.type = 'button';
      identity.setAttribute('aria-label', `${sensor.alias ? 'Edit' : 'Set'} alias for sensor ${sensor.deviceId}`);
      identity.addEventListener('click', () => { void editSensorAlias(sensor.deviceId); });
    }
    const id = document.createElement('strong');
    id.textContent = sensorAlias(sensor.deviceId);
    identity.append(id);
    heading.append(identity);
    card.append(heading);

    const primary = document.createElement('div');
    primary.className = 'sensor-primary';
    const primaryLabel = document.createElement('span');
    primaryLabel.textContent = 'Temperature';
    const temperature = document.createElement('strong');
    temperature.textContent = sensor.temperature == null ? '—' : sensor.temperature.toFixed(1) + ' °C';
    primary.append(primaryLabel, temperature);
    card.append(primary);

    const stats = document.createElement('dl');
    stats.className = 'sensor-stats';
    const addStat = (label, value) => {
      const wrapper = document.createElement('div');
      const term = document.createElement('dt');
      term.textContent = label;
      const description = document.createElement('dd');
      description.textContent = value;
      wrapper.append(term, description);
      stats.append(wrapper);
    };
    addStat('Humidity', sensor.humidity == null ? '—' : sensor.humidity.toFixed(1) + '%');
    addStat('Battery', sensor.battery == null ? '—' : sensor.battery.toFixed(0) + '%');
    addStat('Voltage', sensor.millivolts == null ? '—' : (sensor.millivolts / 1000).toFixed(3) + ' V');
    addStat('Signal', sensor.rssi == null ? '—' : sensor.rssi + ' dBm');
    card.append(stats);

    const updated = document.createElement('small');
    updated.className = 'sensor-updated';
    updated.textContent = sensor.at ? 'Updated ' + dateTime.format(sensor.at) : 'Waiting for a reading';
    card.append(updated);
    container.append(card);
  }
}

async function editSensorAlias(deviceId) {
  const sensor = sensors.find(item => item.deviceId === deviceId);
  const alias = window.prompt(`Alias for ${deviceId}. Leave blank to show the device ID.`, sensor?.alias || '');
  if (alias === null) return;
  try {
    const response = await fetch('/api/sensors/' + encodeURIComponent(deviceId) + '/alias', {
      method: 'PUT',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({alias})
    });
    if (!response.ok) throw new Error('Could not save sensor alias');
    const result = await response.json();
    if (sensor) sensor.alias = result.alias;
    renderSensorToggles();
    renderSensorMetrics(latestSensorReadings);
    draw();
  } catch (error) {
    get('status').textContent = error.message;
  }
}

function draw() {
  for (const chart of charts) render(chart);
  get('detail').textContent = cursor
    ? [clock.format(cursor.at), sensorAlias(cursor.deviceId), cursor.temperature == null ? '' : cursor.temperature.toFixed(1) + ' °C', cursor.humidity == null ? '' : cursor.humidity + '% RH', cursor.battery == null ? '' : cursor.battery + '% battery', cursor.millivolts == null ? '' : cursor.millivolts + ' mV', cursor.rssi == null ? '' : cursor.rssi + ' dBm'].filter(Boolean).join(' · ')
    : '';
}

function chartPoints() {
  return sensorPoints.length ? sensorPoints.filter(p => selectedSensors.has(p.deviceId)) : points;
}

function visibleChartPoints(field) {
  return chartPoints().filter(p => p[field] != null && p.at >= displayed.from && p.at <= displayed.to);
}

function chartBounds(field, visible) {
  let [min, max] = axisLimits[field] ?? [Infinity, -Infinity];
  for (const point of visible) {
    if (point[field] < min) min = Math.floor(point[field]) - 1;
    if (point[field] > max) max = Math.ceil(point[field]) + 1;
  }
  axisLimits[field] = [min, max];
  return [min, max];
}

function nearestPointOnChart(canvas, field, clientX, clientY) {
  if (!displayed) return null;
  const width = canvas.clientWidth, height = canvas.clientHeight;
  const left = 48, right = width - 14, top = 12, bottom = height - 24;
  const rect = canvas.getBoundingClientRect();
  const pointerX = clientX - rect.left, pointerY = clientY - rect.top;
  if (pointerX < left || pointerX > right || pointerY < top || pointerY > bottom) return null;
  const visible = visibleChartPoints(field);
  if (!visible.length) return null;
  const [min, max] = chartBounds(field, visible);
  const duration = displayed.to - displayed.from;
  const x = at => left + (at - displayed.from) * (right - left) / duration;
  const y = value => bottom - (value - min) * (bottom - top) / (max - min);
  let nearest = null, nearestDistance = Infinity;
  for (const point of visible) {
    const dx = x(point.at) - pointerX, dy = y(point[field]) - pointerY;
    const distance = Math.hypot(dx, dy);
    if (distance < nearestDistance) { nearest = point; nearestDistance = distance; }
  }
  return nearestDistance <= 24 ? nearest : null;
}

function drawHoverTooltip(ctx, canvas, point, px, py) {
  const lines = [];
  const identity = sensorAlias(point.deviceId) || 'Mock sensor';
  lines.push(identity, dateTime.format(point.at));
  const measurements = [];
  if (point.temperature != null) measurements.push(point.temperature.toFixed(2) + ' °C');
  if (point.humidity != null) measurements.push(point.humidity.toFixed(1) + '% RH');
  if (point.battery != null) measurements.push(point.battery.toFixed(0) + '% battery');
  if (point.millivolts != null) measurements.push(point.millivolts.toFixed(0) + ' mV');
  if (point.rssi != null) measurements.push(point.rssi + ' dBm');
  for (let i = 0; i < measurements.length; i += 2) lines.push(measurements.slice(i, i + 2).join(' · '));

  ctx.save();
  ctx.font = '12px system-ui';
  const boxWidth = Math.min(canvas.clientWidth - 16, Math.max(190, ...lines.map(line => ctx.measureText(line).width + 20)));
  const lineHeight = 17, boxHeight = lines.length * lineHeight + 14;
  const x = Math.max(8, Math.min(canvas.clientWidth - boxWidth - 8, px + 12 + boxWidth <= canvas.clientWidth ? px + 12 : px - boxWidth - 12));
  const y = Math.max(8, Math.min(canvas.clientHeight - boxHeight - 8, py - boxHeight - 12));
  ctx.fillStyle = 'rgba(17, 23, 21, 0.96)';
  ctx.strokeStyle = point.deviceId ? sensorColor(point.deviceId) : '#d5e1db';
  ctx.lineWidth = 1;
  ctx.beginPath();
  ctx.roundRect(x, y, boxWidth, boxHeight, 7);
  ctx.fill();
  ctx.stroke();
  ctx.fillStyle = '#f0f5f2';
  ctx.textAlign = 'left';
  for (let i = 0; i < lines.length; i++) {
    ctx.font = i === 0 ? '600 12px system-ui' : '12px system-ui';
    ctx.fillText(lines[i], x + 10, y + 17 + i * lineHeight, boxWidth - 20);
  }
  ctx.restore();
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
    label.append(checkbox, document.createTextNode(sensorAlias(sensor.deviceId)));
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
  const source = chartPoints();
  const visible = visibleChartPoints(field);
  if (!visible.length) {
    ctx.fillStyle = '#a1b4ac';
    ctx.textAlign = 'center';
    ctx.fillText('No readings in this window', width / 2, height / 2);
    return;
  }
  const [min, max] = chartBounds(field, visible);
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
    if (cursor[field] != null) {
      ctx.beginPath(); ctx.arc(x(cursor.at), y(cursor[field]), 4, 0, Math.PI * 2); ctx.fill();
    }
    if (hoveredCanvas === canvas && cursor[field] != null) drawHoverTooltip(ctx, canvas, cursor, x(cursor.at), y(cursor[field]));
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

async function confirmDeleteReadings(from, to, deviceIds) {
  const selected = sensors.filter(sensor => deviceIds.includes(sensor.deviceId));
  const sensorNames = selected.map(sensor => sensorAlias(sensor.deviceId));
  const formatter = new Intl.DateTimeFormat(undefined, {dateStyle: 'medium', timeStyle: 'medium'});
  const scope = sensorNames.length ? sensorNames.join(', ') : deviceIds.join(', ');
  if (!window.confirm(`Permanently delete readings for ${scope} from ${formatter.format(from)} through ${formatter.format(to)}? This cannot be undone.`)) return;
  try {
    const query = new URLSearchParams({from: String(from), to: String(to)});
    for (const deviceId of deviceIds) query.append('sensor', deviceId);
    const response = await fetch('/api/readings?' + query.toString(), {method: 'DELETE'});
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
    if (operation === 'delete' && selectedSensors.size === 0) {
      get('status').textContent = 'Select at least one sensor before selecting readings to delete';
      event.preventDefault();
      return;
    }
    const rect = canvas.getBoundingClientRect();
    const x = event.clientX - rect.left;
    if (x < 48 || x > canvas.clientWidth - 14) return;
    if (operation === 'delete') event.preventDefault();
    drag = {canvas, operation, startX: x, currentX: x, startAt: timeAt(event.clientX), currentAt: timeAt(event.clientX), sensorIds: operation === 'delete' ? [...selectedSensors] : []};
    cursor = null;
    hoveredCanvas = null;
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
    hoveredCanvas = canvas;
    cursor = nearestPointOnChart(canvas, charts.find(chart => chart.canvas === canvas).field, event.clientX, event.clientY);
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
          void confirmDeleteReadings(from, to, selection.sensorIds);
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
    hoveredCanvas = null;
    draw();
  });
  canvas.addEventListener('pointercancel', () => { drag = null; draw(); });
  canvas.addEventListener('contextmenu', event => event.preventDefault());
  canvas.addEventListener('pointerleave', () => { if (!drag) { cursor = null; hoveredCanvas = null; draw(); } });
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
