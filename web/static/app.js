"use strict";

// TraceForge live dashboard: connects to the server's /ws WebSocket and renders
// the metric stream and (for admin credentials) the stats counters + a chart.

const $ = (id) => document.getElementById(id);
const MAX_ROWS = 200;
const MAX_POINTS = 120;

const state = {
  ws: null,
  backoff: 500,
  chart: [],           // recent "stored" counter values
  alerts: new Map(),   // fingerprint -> latest alert event
};

function setStatus(on) {
  const el = $("status");
  el.textContent = on ? "online" : "offline";
  el.className = "status " + (on ? "on" : "off");
}

function wsURL() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const cred = $("cred").value.trim();
  let url = `${proto}//${location.host}/ws`;
  if (cred) url += `?token=${encodeURIComponent(cred)}&api_key=${encodeURIComponent(cred)}`;
  return url;
}

function connect() {
  if (state.ws) {
    state.ws.onclose = null;
    state.ws.close();
  }
  localStorage.setItem("cred", $("cred").value);
  const ws = new WebSocket(wsURL());
  state.ws = ws;

  ws.onopen = () => { setStatus(true); state.backoff = 500; };
  ws.onmessage = (ev) => {
    let msg;
    try { msg = JSON.parse(ev.data); } catch { return; }
    if (msg.type === "metrics") addMetrics(msg.metrics || []);
    else if (msg.type === "stats") updateStats(msg.stats);
    else if (msg.type === "alert") updateAlert(msg.alert);
  };
  ws.onclose = () => {
    setStatus(false);
    state.backoff = Math.min(state.backoff * 2, 10000);
    setTimeout(connect, state.backoff);
  };
  ws.onerror = () => ws.close();
}

function fmt(v) {
  if (typeof v !== "number") return v;
  return Math.abs(v) >= 1000 ? v.toLocaleString() : v.toFixed(2).replace(/\.00$/, "");
}

function addMetrics(metrics) {
  const tbody = $("feed").querySelector("tbody");
  const frag = document.createDocumentFragment();
  for (const m of metrics) {
    const tr = document.createElement("tr");
    const labels = m.labels || {};
    const t = new Date(m.timestamp).toLocaleTimeString();
    tr.innerHTML =
      `<td>${t}</td><td>${escapeHTML(m.name)}</td>` +
      `<td class="num">${fmt(m.value)}</td>` +
      `<td>${escapeHTML(labels.tenant || "")}</td>` +
      `<td>${escapeHTML(labels.agent_id || "")}</td>`;
    frag.appendChild(tr);
  }
  tbody.insertBefore(frag, tbody.firstChild);
  while (tbody.children.length > MAX_ROWS) tbody.removeChild(tbody.lastChild);
}

// Alerts are keyed by fingerprint, so a re-sent firing alert updates its row in
// place instead of piling up. A resolved alert lingers briefly so the operator
// sees that it cleared, then disappears.
const RESOLVED_LINGER_MS = 30000;

function updateAlert(a) {
  if (!a || !a.fingerprint) return;
  if (a.status === "resolved") {
    a.removeAt = Date.now() + RESOLVED_LINGER_MS;
  }
  state.alerts.set(a.fingerprint, a);
  renderAlerts();
}

function renderAlerts() {
  const now = Date.now();
  for (const [fp, a] of state.alerts) {
    if (a.removeAt && a.removeAt <= now) state.alerts.delete(fp);
  }

  const rows = [...state.alerts.values()].sort((x, y) =>
    (x.status === y.status) ? x.rule.localeCompare(y.rule) : (x.status === "firing" ? -1 : 1));

  const tbody = $("alerts").querySelector("tbody");
  tbody.textContent = "";
  for (const a of rows) {
    const tr = document.createElement("tr");
    tr.className = "alert-" + a.status;
    const labels = Object.entries(a.labels || {})
      .filter(([k]) => k !== "alertname" && k !== "severity")
      .map(([k, v]) => `${k}=${v}`).join(", ");
    tr.innerHTML =
      `<td><span class="badge ${escapeHTML(a.status)}">${escapeHTML(a.status)}</span></td>` +
      `<td>${escapeHTML(a.severity || "")}</td>` +
      `<td>${escapeHTML(a.rule || "")}</td>` +
      `<td class="num">${fmt(a.value)}</td>` +
      `<td class="labels">${escapeHTML(labels)}</td>` +
      `<td>${new Date(a.starts_at).toLocaleTimeString()}</td>`;
    tr.title = Object.entries(a.annotations || {}).map(([k, v]) => `${k}: ${v}`).join("\n");
    tbody.appendChild(tr);
  }

  const firing = rows.filter((a) => a.status === "firing").length;
  $("alert-count").textContent = firing ? `${firing} firing` : "";
  $("alerts-hint").style.display = rows.length ? "none" : "";
}

function updateStats(stats) {
  if (!stats) return;
  const p = stats.pipeline || {};
  const s = stats.storage || {};
  $("c-ingested").textContent = fmt(p.ingested ?? 0);
  $("c-stored").textContent = fmt(p.stored ?? 0);
  $("c-dropped").textContent = fmt(p.dropped ?? 0);
  $("c-invalid").textContent = fmt(p.invalid ?? 0);
  // Distinct from `dropped`: these metrics were accepted and then lost to a
  // storage write error. Any non-zero value here is an incident.
  $("c-failed").textContent = fmt(p.failed ?? 0);
  $("c-series").textContent = fmt(s.series ?? 0);
  $("c-points").textContent = fmt(s.points ?? 0);
  $("chart-hint").style.display = "none";

  state.chart.push(s.points ?? 0);
  if (state.chart.length > MAX_POINTS) state.chart.shift();
  drawChart();
}

function drawChart() {
  const c = $("chart");
  const dpr = window.devicePixelRatio || 1;
  const w = c.clientWidth, h = c.clientHeight;
  c.width = w * dpr; c.height = h * dpr;
  const ctx = c.getContext("2d");
  ctx.scale(dpr, dpr);
  ctx.clearRect(0, 0, w, h);
  const data = state.chart;
  if (data.length < 2) return;

  const min = Math.min(...data), max = Math.max(...data);
  const range = max - min || 1;
  const x = (i) => (i / (data.length - 1)) * w;
  const y = (v) => h - 6 - ((v - min) / range) * (h - 12);

  ctx.beginPath();
  ctx.moveTo(x(0), y(data[0]));
  for (let i = 1; i < data.length; i++) ctx.lineTo(x(i), y(data[i]));
  ctx.strokeStyle = "#4ea1ff";
  ctx.lineWidth = 2;
  ctx.stroke();

  ctx.lineTo(x(data.length - 1), h);
  ctx.lineTo(x(0), h);
  ctx.closePath();
  ctx.fillStyle = "rgba(78,161,255,.12)";
  ctx.fill();
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (ch) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch]));
}

window.addEventListener("load", () => {
  $("cred").value = localStorage.getItem("cred") || "";
  $("connect").addEventListener("click", connect);
  $("cred").addEventListener("keydown", (e) => { if (e.key === "Enter") connect(); });
  // Sweep resolved alerts even when no new event arrives to trigger a render.
  setInterval(renderAlerts, 5000);
  connect();
});
