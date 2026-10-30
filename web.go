package main

// indexHTML is the control panel: self-contained, it polls /status and posts
// to the API.
const indexHTML = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>libre3sim</title>
<style>
:root { color-scheme: light dark; --fg:#1d1d1f; --bg:#f5f5f7; --card:#fff; --muted:#6e6e73; --accent:#0a66c2; }
@media (prefers-color-scheme: dark) { :root { --fg:#f5f5f7; --bg:#1c1c1e; --card:#2c2c2e; --muted:#a1a1a6; --accent:#4c9fff; } }
body { font: 15px -apple-system, system-ui, sans-serif; color: var(--fg); background: var(--bg); margin: 0; padding: 16px; }
main { max-width: 720px; margin: 0 auto; }
h1 { font-size: 20px; margin: 0 0 4px; } .note { color: var(--muted); font-size: 13px; margin: 0 0 16px; }
section { background: var(--card); border-radius: 10px; padding: 14px 16px; margin-bottom: 12px; }
h2 { font-size: 14px; margin: 0 0 10px; color: var(--muted); font-weight: 600; }
.big { font-size: 32px; font-weight: 600; } dl { display: grid; grid-template-columns: max-content 1fr; gap: 4px 12px; margin: 0; }
dt { color: var(--muted); } dd { margin: 0; font-family: ui-monospace, monospace; font-size: 13px; overflow-wrap: anywhere; }
.row { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-bottom: 8px; }
input, select { font: inherit; padding: 4px 6px; width: 7em; } button { font: inherit; padding: 5px 12px; border-radius: 6px; border: 1px solid var(--accent); background: none; color: var(--accent); cursor: pointer; }
</style></head><body><main>
<h1>libre3sim</h1>
<p class="note">A Libre 3 sensor emulator for development only. It is not an Abbott product and its data is simulated: never use it for therapy.</p>
<section><h2>Sensor</h2><div class="big" id="glucose">—</div><dl id="status"></dl></section>
<section><h2>Glucose</h2>
<div class="row"><select id="kind"><option>sine</option><option>fixed</option><option>walk</option></select>
<label>mg/dL <input id="value" type="number" value="120"></label>
<label>amplitude <input id="amp" type="number" value="40"></label>
<label>period (min) <input id="period" type="number" value="240"></label>
<button onclick="setGlucose()">Set</button></div></section>
<section><h2>Link</h2>
<div class="row"><button onclick="post('/disconnect')">Drop the link</button>
<label>minutes <input id="outMinutes" type="number" value="20"></label>
<button onclick="post('/out-of-range', {minutes: +val('outMinutes')})">Go out of range</button></div></section>
<section><h2>Sensor life</h2>
<div class="row"><select id="errorData"><option value="6">6 terminated</option><option value="8">8 replace sensor</option><option value="3">3 insertion failure</option></select>
<button onclick="post('/terminate', {errorData: +val('errorData')})">End the sensor</button></div>
<div class="row"><label>wear days <input id="wearDays" type="number" value="14"></label>
<label>warmup (min) <input id="warmup" type="number" value="60"></label>
<label>age (min) <input id="age" type="number" value="0"></label>
<button onclick="post('/new', {wearDays: +val('wearDays'), warmupMinutes: +val('warmup'), ageMinutes: +val('age')})">Start a new sensor</button></div>
<p class="note">Age 0 is a sensor still in its applicator: scan it in the app to activate it.</p></section>
</main><script>
function val(id) { return document.getElementById(id).value; }
async function post(path, body) {
  const r = await fetch(path, {method: 'POST', body: JSON.stringify(body || {})});
  if (!r.ok) alert(await r.text()); refresh();
}
function setGlucose() { post('/glucose', {kind: val('kind'), value: +val('value'), amplitude: +val('amp'), periodMinutes: +val('period')}); }
async function refresh() {
  try {
    const s = await (await fetch('/status')).json();
    document.getElementById('glucose').textContent = s.glucose ? s.glucose + ' mg/dL' : '—';
    const dl = document.getElementById('status'); dl.replaceChildren();
    for (const [k, v] of Object.entries(s)) {
      if (k === 'glucose') continue;
      const dt = document.createElement('dt'); dt.textContent = k;
      const dd = document.createElement('dd'); dd.textContent = typeof v === 'object' ? JSON.stringify(v) : v;
      dl.append(dt, dd);
    }
  } catch (e) {}
}
refresh(); setInterval(refresh, 2000);
</script></body></html>`
