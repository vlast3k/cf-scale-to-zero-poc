package main

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Scale-to-Zero Demo — CF Runtime</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body {
    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
    background: #0f1419;
    color: #e7e9ea;
    min-height: 100vh;
    padding: 24px;
}
h1 {
    font-size: 1.6rem;
    margin-bottom: 8px;
    color: #fff;
}
.subtitle {
    color: #71767b;
    font-size: 0.9rem;
    margin-bottom: 24px;
}
.controls {
    display: flex;
    gap: 12px;
    margin-bottom: 24px;
    align-items: center;
}
.btn {
    border: none;
    border-radius: 8px;
    padding: 10px 20px;
    font-size: 0.9rem;
    font-weight: 600;
    cursor: pointer;
    transition: all 0.15s;
}
.btn:hover { transform: translateY(-1px); }
.btn:active { transform: translateY(0); }
.btn-primary { background: #1d9bf0; color: #fff; }
.btn-primary:hover { background: #1a8cd8; }
.btn-primary.active { background: #00ba7c; }
.btn-danger { background: #f4212e; color: #fff; }
.btn-danger:hover { background: #dc1d29; }
.btn-invoke { background: #7856ff; color: #fff; }
.btn-invoke:hover { background: #6644e0; }
.btn-stop { background: #ff7a00; color: #fff; }
.btn-stop:hover { background: #e06c00; }
.btn:disabled { opacity: 0.5; cursor: not-allowed; transform: none; }

.connection-status {
    margin-left: auto;
    font-size: 0.8rem;
    padding: 4px 12px;
    border-radius: 12px;
    background: #2f3336;
}
.connection-status.connected { color: #00ba7c; }
.connection-status.disconnected { color: #f4212e; }

.endpoints-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(380px, 1fr));
    gap: 16px;
    margin-bottom: 24px;
}
.endpoint-card {
    background: #16202a;
    border: 1px solid #2f3336;
    border-radius: 12px;
    padding: 20px;
    transition: border-color 0.3s;
}
.endpoint-card.invoking { border-color: #ffd700; }
.endpoint-card.reachable { border-color: #00ba7c; }
.endpoint-card.started { border-color: #1d9bf0; }
.endpoint-card.stopped { border-color: #f4212e; }
.endpoint-card.stopping { border-color: #ff7a00; }

.endpoint-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 12px;
}
.endpoint-name {
    font-size: 1.1rem;
    font-weight: 600;
}
.state-badge {
    font-size: 0.75rem;
    font-weight: 700;
    padding: 4px 10px;
    border-radius: 6px;
    text-transform: uppercase;
    letter-spacing: 0.5px;
}
.state-badge.stopped { background: #f4212e22; color: #f4212e; }
.state-badge.started { background: #1d9bf022; color: #1d9bf0; }
.state-badge.reachable { background: #00ba7c22; color: #00ba7c; }
.state-badge.invoking { background: #ffd70022; color: #ffd700; }
.state-badge.stopping { background: #ff7a0022; color: #ff7a00; }
.state-badge.unknown { background: #71767b22; color: #71767b; }

.endpoint-actions {
    display: flex;
    gap: 8px;
    margin-bottom: 16px;
}
.endpoint-actions .btn { font-size: 0.8rem; padding: 6px 14px; }

.timer-display {
    font-family: 'SF Mono', 'Fira Code', monospace;
    font-size: 2rem;
    font-weight: 700;
    color: #71767b;
    margin: 12px 0;
    min-height: 2.5rem;
}
.timer-display.running { color: #ffd700; }
.timer-display.done { color: #00ba7c; }
.timer-display.error { color: #f4212e; }

.result-panel {
    background: #1e2a35;
    border-radius: 8px;
    padding: 12px;
    font-family: 'SF Mono', 'Fira Code', monospace;
    font-size: 0.8rem;
    min-height: 60px;
    max-height: 120px;
    overflow-y: auto;
    color: #8b98a5;
    white-space: pre-wrap;
    word-break: break-all;
}
.result-panel .status-code { color: #00ba7c; font-weight: 700; }
.result-panel .status-code.error { color: #f4212e; }
.result-panel .duration { color: #1d9bf0; }

.log-panel {
    background: #16202a;
    border: 1px solid #2f3336;
    border-radius: 12px;
    padding: 16px;
    max-height: 200px;
    overflow-y: auto;
    font-family: 'SF Mono', 'Fira Code', monospace;
    font-size: 0.75rem;
    color: #71767b;
}
.log-panel .log-entry { margin-bottom: 4px; }
.log-panel .log-entry .ts { color: #536471; }
.log-panel .log-entry .msg { color: #8b98a5; }

/* Billing Panel */
.billing-section { margin-bottom: 24px; }
.billing-section h2 { font-size: 1.1rem; margin-bottom: 4px; color: #e7e9ea; }
.billing-section .billing-subtitle { font-size: 0.8rem; color: #71767b; margin-bottom: 12px; }
.billing-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
    gap: 12px;
    margin-bottom: 12px;
}
.billing-card {
    background: #16202a;
    border: 1px solid #2f3336;
    border-radius: 10px;
    padding: 14px;
}
.billing-card.active { border-color: #00ba7c; }
.billing-card .app-name { font-size: 0.85rem; font-weight: 600; margin-bottom: 6px; }
.billing-card .gb-counter {
    font-family: 'SF Mono', 'Fira Code', monospace;
    font-size: 1.3rem;
    font-weight: 700;
    color: #71767b;
}
.billing-card.active .gb-counter { color: #00ba7c; }
.billing-card .gb-label { font-size: 0.7rem; color: #536471; text-transform: uppercase; letter-spacing: 1px; }
.billing-card .billing-meta { font-size: 0.75rem; color: #71767b; margin-top: 4px; }
.billing-card .billing-meta .running-badge { color: #00ba7c; font-weight: 600; }
.billing-card .billing-meta .stopped-badge { color: #f4212e; font-weight: 600; }

.billing-events-log {
    background: #16202a;
    border: 1px solid #2f3336;
    border-radius: 10px;
    padding: 12px;
    max-height: 130px;
    overflow-y: auto;
    font-family: 'SF Mono', 'Fira Code', monospace;
    font-size: 0.72rem;
    color: #71767b;
}
.billing-events-log .be { margin-bottom: 3px; }
.billing-events-log .ev-start { color: #00ba7c; font-weight: 600; }
.billing-events-log .ev-stop { color: #f4212e; font-weight: 600; }
.billing-events-log .ev-scale { color: #ffd700; font-weight: 600; }
.billing-events-log .ev-gb { color: #1d9bf0; }
</style>
</head>
<body>

<h1>⚡ Scale-to-Zero Demo Coordinator</h1>
<p class="subtitle">Real-time CF app cold-start performance testing via WebSocket</p>

<div class="controls">
    <button class="btn btn-primary" id="btn-monitor" onclick="toggleMonitoring()">▶ Start Demo</button>
    <button class="btn btn-danger" id="btn-stop-all" onclick="stopAll()">⏹ Stop All</button>
    <button class="btn btn-stop" id="btn-reset" onclick="resetDemo()">🔄 Reset Demo</button>
    <span class="connection-status disconnected" id="conn-status">● Disconnected</span>
</div>

<div class="endpoints-grid" id="endpoints-grid"></div>

<div class="billing-section">
    <h2>📊 CF Usage Events — GB-Seconds Billing</h2>
    <p class="billing-subtitle">Real-time metering from /v3/app_usage_events (polled every 5s). Billing starts on STARTED, stops on STOPPED.</p>
    <div class="billing-grid" id="billing-grid"></div>
    <div class="billing-events-log" id="billing-events-log">
        <div class="be"><span style="color:#536471">Waiting for usage events...</span></div>
    </div>
</div>

<div class="log-panel" id="log-panel">
    <div class="log-entry"><span class="ts">[--:--:--]</span> <span class="msg">Waiting for WebSocket connection...</span></div>
</div>

<script>
let ws = null;
let endpointState = {};
let timers = {};
let monitoringActive = false;

function connect() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(proto + '//' + location.host + '/ws');

    ws.onopen = () => {
        document.getElementById('conn-status').className = 'connection-status connected';
        document.getElementById('conn-status').textContent = '● Connected';
        addLog('WebSocket connected');
    };

    ws.onclose = () => {
        document.getElementById('conn-status').className = 'connection-status disconnected';
        document.getElementById('conn-status').textContent = '● Disconnected';
        addLog('WebSocket disconnected — reconnecting in 2s...');
        setTimeout(connect, 2000);
    };

    ws.onerror = (err) => {
        addLog('WebSocket error');
    };

    ws.onmessage = (event) => {
        const msg = JSON.parse(event.data);
        handleMessage(msg);
    };
}

function handleMessage(msg) {
    switch (msg.type) {
        case 'initial_state':
            initEndpoints(msg.endpoints);
            if (msg.billing) initBilling(msg.billing);
            if (msg.billing_events) initBillingEvents(msg.billing_events);
            break;
        case 'state_update':
            updateEndpoints(msg.endpoints);
            break;
        case 'state_change':
            updateSingleState(msg.endpoint, msg.state);
            break;
        case 'invoke_start':
            startTimer(msg.endpoint);
            break;
        case 'invoke_result':
            showResult(msg.result);
            break;
        case 'monitoring':
            monitoringActive = msg.monitoring;
            updateMonitorBtn();
            break;
        case 'billing_event':
            addBillingEvent(msg.event);
            break;
        case 'billing_update':
            updateBillingCounters(msg.statuses);
            break;
        case 'billing_reset':
            initBilling(msg.statuses);
            document.getElementById('billing-events-log').innerHTML = '<div class="be"><span style="color:#536471">Counters reset — ready for demo</span></div>';
            addLog('🔄 Demo reset: billing counters cleared');
            break;
        case 'error':
            addLog('ERROR [' + msg.endpoint + ']: ' + msg.error);
            break;
    }
}

function initEndpoints(eps) {
    const grid = document.getElementById('endpoints-grid');
    grid.innerHTML = '';
    eps.forEach(ep => {
        endpointState[ep.name] = ep.state;
        grid.innerHTML += renderCard(ep);
    });
}

function renderCard(ep) {
    const stateClass = ep.state ? ep.state.toLowerCase() : 'unknown';
    return '<div class="endpoint-card ' + stateClass + '" id="card-' + ep.name + '">' +
        '<div class="endpoint-header">' +
            '<span class="endpoint-name">' + ep.name + '</span>' +
            '<span class="state-badge ' + stateClass + '" id="badge-' + ep.name + '">' + (ep.state || 'UNKNOWN') + '</span>' +
        '</div>' +
        '<div class="endpoint-actions">' +
            '<button class="btn btn-invoke" onclick="invoke(\'' + ep.name + '\')">⚡ Invoke</button>' +
            '<button class="btn btn-stop" onclick="stopEndpoint(\'' + ep.name + '\')">⏹ Stop</button>' +
        '</div>' +
        '<div class="timer-display" id="timer-' + ep.name + '">—</div>' +
        '<div class="result-panel" id="result-' + ep.name + '">No invocations yet</div>' +
    '</div>';
}

function updateEndpoints(eps) {
    eps.forEach(ep => {
        updateSingleState(ep.name, ep.state);
    });
}

function updateSingleState(name, state) {
    const prev = endpointState[name];
    endpointState[name] = state;
    const card = document.getElementById('card-' + name);
    const badge = document.getElementById('badge-' + name);
    if (!card || !badge) return;

    const stateClass = state ? state.toLowerCase() : 'unknown';
    card.className = 'endpoint-card ' + stateClass;
    badge.className = 'state-badge ' + stateClass;
    badge.textContent = state || 'UNKNOWN';

    // Log confirmed state transitions (not redundant same-state updates from polling)
    if (prev !== state) {
        addLog(name + ' → ' + state);
    }
}

function startTimer(name) {
    // Stop any existing timer
    if (timers[name]) {
        clearInterval(timers[name].interval);
    }

    const timerEl = document.getElementById('timer-' + name);
    const resultEl = document.getElementById('result-' + name);
    if (!timerEl) return;

    const startTime = performance.now();
    timerEl.className = 'timer-display running';
    resultEl.innerHTML = 'Invoking...';

    timers[name] = {
        startTime: startTime,
        interval: setInterval(() => {
            const elapsed = (performance.now() - startTime) / 1000;
            timerEl.textContent = elapsed.toFixed(1) + 's';
        }, 100)
    };

    addLog('Invoke started: ' + name);
}

function showResult(result) {
    const name = result.endpoint;
    const timerEl = document.getElementById('timer-' + name);
    const resultEl = document.getElementById('result-' + name);

    // Stop timer
    if (timers[name]) {
        clearInterval(timers[name].interval);
    }

    if (!timerEl || !resultEl) return;

    const durationSec = (result.duration_ms / 1000).toFixed(1);
    timerEl.textContent = durationSec + 's';

    if (result.error) {
        timerEl.className = 'timer-display error';
        resultEl.innerHTML = '<span class="status-code error">ERROR</span> ' + result.error +
            '\n<span class="duration">' + durationSec + 's</span>';
        addLog('ERROR [' + name + ']: ' + result.error + ' (' + durationSec + 's)');
    } else {
        timerEl.className = 'timer-display done';
        const statusClass = result.status_code < 400 ? 'status-code' : 'status-code error';
        // Truncate response body for display
        let body = result.response_body || '';
        if (body.length > 200) body = body.substring(0, 200) + '...';
        // Strip HTML tags for readability
        body = body.replace(/<[^>]*>/g, '').trim();
        if (!body) body = '(empty response)';

        resultEl.innerHTML = '<span class="' + statusClass + '">' + result.status_code + '</span> ' +
            body + '\n<span class="duration">⏱ ' + durationSec + 's</span>';
        addLog('OK [' + name + ']: HTTP ' + result.status_code + ' in ' + durationSec + 's');
    }
}

function invoke(name) {
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ action: 'invoke', endpoint: name }));
    }
}

function stopEndpoint(name) {
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ action: 'stop', endpoint: name }));
    }
}

function toggleMonitoring() {
    if (ws && ws.readyState === WebSocket.OPEN) {
        const action = monitoringActive ? 'stop_monitoring' : 'start_monitoring';
        ws.send(JSON.stringify({ action: action }));
    }
}

function stopAll() {
    Object.keys(endpointState).forEach(name => {
        if (endpointState[name] === 'STARTED' || endpointState[name] === 'REACHABLE' || endpointState[name] === 'INVOKING') {
            stopEndpoint(name);
        }
    });
}

function resetDemo() {
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ action: 'reset_demo' }));
        addLog('🔄 Resetting demo (stopping all apps, clearing counters)...');
    }
}

function updateMonitorBtn() {
    const btn = document.getElementById('btn-monitor');
    if (monitoringActive) {
        btn.textContent = '⏸ Stop Monitoring';
        btn.className = 'btn btn-primary active';
    } else {
        btn.textContent = '▶ Start Demo';
        btn.className = 'btn btn-primary';
    }
}

function addLog(msg) {
    const panel = document.getElementById('log-panel');
    const now = new Date().toLocaleTimeString();
    const entry = document.createElement('div');
    entry.className = 'log-entry';
    entry.innerHTML = '<span class="ts">[' + now + ']</span> <span class="msg">' + msg + '</span>';
    panel.appendChild(entry);
    panel.scrollTop = panel.scrollHeight;

    // Keep max 100 entries
    while (panel.children.length > 100) {
        panel.removeChild(panel.firstChild);
    }
}

// =============================================================================
// Billing / Usage Events
// =============================================================================

function initBilling(statuses) {
    const grid = document.getElementById('billing-grid');
    grid.innerHTML = '';
    statuses.forEach(s => {
        grid.innerHTML += renderBillingCard(s);
    });
}

function initBillingEvents(events) {
    if (!events || events.length === 0) return;
    const panel = document.getElementById('billing-events-log');
    panel.innerHTML = '';
    events.forEach(ev => {
        appendBillingEventHTML(panel, ev);
    });
}

function renderBillingCard(s) {
    const activeClass = s.running ? ' active' : '';
    const statusHTML = s.running
        ? '<span class="running-badge">● METERING</span> ' + s.memory_mb + 'MB × ' + s.instances + 'i — ' + s.running_for
        : '<span class="stopped-badge">○ idle</span>';
    return '<div class="billing-card' + activeClass + '" id="bill-' + s.app_name + '">' +
        '<div class="app-name">' + s.app_name + '</div>' +
        '<div class="gb-counter" id="gb-' + s.app_name + '">' + formatGBSec(s.gb_seconds) + '</div>' +
        '<div class="gb-label">GB-seconds</div>' +
        '<div class="billing-meta" id="billmeta-' + s.app_name + '">' + statusHTML + '</div>' +
    '</div>';
}

function updateBillingCounters(statuses) {
    statuses.forEach(s => {
        const card = document.getElementById('bill-' + s.app_name);
        const counter = document.getElementById('gb-' + s.app_name);
        const meta = document.getElementById('billmeta-' + s.app_name);
        if (!card || !counter) return;

        counter.textContent = formatGBSec(s.gb_seconds);
        card.className = s.running ? 'billing-card active' : 'billing-card';

        if (meta) {
            meta.innerHTML = s.running
                ? '<span class="running-badge">● METERING</span> ' + s.memory_mb + 'MB × ' + s.instances + 'i — ' + s.running_for
                : '<span class="stopped-badge">○ idle</span>';
        }
    });
}

function addBillingEvent(ev) {
    const panel = document.getElementById('billing-events-log');
    // Clear placeholder
    if (panel.children.length === 1 && panel.children[0].textContent.includes('Waiting')) {
        panel.innerHTML = '';
    }
    appendBillingEventHTML(panel, ev);
    addLog('📊 ' + ev.app_name + ' ' + ev.event_type + ' (' + ev.memory_mb + 'MB×' + ev.instances + 'i) — ' + formatGBSec(ev.gb_seconds) + ' GB-s total');
}

function appendBillingEventHTML(panel, ev) {
    const div = document.createElement('div');
    div.className = 'be';
    const evClass = ev.event_type === 'STARTED' ? 'ev-start' : ev.event_type === 'STOPPED' ? 'ev-stop' : 'ev-scale';
    const ts = ev.timestamp ? ev.timestamp.substring(11, 19) : '--:--:--';
    div.innerHTML = '<span style="color:#536471">[' + ts + ']</span> ' +
        '<span class="' + evClass + '">' + ev.event_type + '</span> ' +
        ev.app_name + ' ' + ev.memory_mb + 'MB×' + ev.instances + 'i ' +
        '<span class="ev-gb">' + formatGBSec(ev.gb_seconds) + ' GB-s</span>';
    panel.appendChild(div);
    panel.scrollTop = panel.scrollHeight;
    while (panel.children.length > 30) {
        panel.removeChild(panel.firstChild);
    }
}

function formatGBSec(val) {
    if (val < 0.001) return '0.000';
    if (val < 10) return val.toFixed(3);
    if (val < 100) return val.toFixed(2);
    return val.toFixed(1);
}

// Connect on page load
connect();
</script>
</body>
</html>`
