# Scale-to-Zero for Cloud Foundry

**Zero platform changes. ~1,350 lines of Go. Cold wake in 3 seconds.**

A proof of concept that enables scale-to-zero on Cloud Foundry using three user-space CF apps. Stopped apps wake automatically on the first inbound request. Validated end-to-end on a live CF landscape.

## The Problem

CF apps cannot scale to zero. The CF Application Autoscaler enforces `instance_min_count >= 1` at the JSON schema level. There is no code path in the autoscaler for zero instances.

The deeper constraint: stopping a CF app removes its routes. Gorouter returns 503 at the Lookup middleware (step 13 of 24) — before the RouteService middleware (step 18) ever fires. There is nothing to intercept. Route services cannot help. The request is dead before any user code runs.

Every idle app burns memory, CPU quota, and billing (GB-seconds) 24/7 whether it serves traffic or not.

## The Solution

Three small Go apps that implement wake-on-request without touching gorouter, Diego, or CAPI source:

```
broker/        — OSB service broker, holds CC admin token, starts apps on demand
wake-proxy/    — reverse proxy, holds requests during cold start, forwards when ready
target-app/    — example app being managed (your app goes here)
```

Total footprint: 96M memory (32M broker + 64M proxy). The proxy exits the request path after the first wake — subsequent requests go direct to the app at zero overhead.

## How It Works

### The Dual-Route Design

Each managed app gets two CF routes:

| Route | Points to | Purpose |
|-------|-----------|---------|
| `app.example.com` (primary) | wake-proxy when sleeping, real app when awake | What users hit |
| `app-backend.example.com` (secondary) | always the real app | Health polling, request forwarding |

The primary route is the only thing that gets swapped. The secondary route never changes.

### Wake Flow

```
    User request
        │
        ▼
  ① Gorouter routes to wake-proxy
     (primary route points there while app sleeps)
        │
        ▼
  ② Wake-proxy calls broker
     POST /wake/{app_guid}
        │
        ▼
  ③ Broker starts app via CC API
     POST /v3/apps/{guid}/actions/start
        │
        ▼
  ④ Wake-proxy polls secondary route every 300ms
     GET https://app-backend.example.com/health
        │
        ▼
  ⑤ App starts. Diego schedules container.
     Route-emitter registers secondary route.
     Health poll returns 200.
        │
        ▼
  ⑥ Wake-proxy forwards held request via secondary route.
     Response delivered to user.
     Total: ~3 seconds.
        │
        ▼
  ⑦ ASYNC: PATCH /v3/routes/{primary}/destinations
     Swap primary route back to the real app.
        │
        ▼
  ⑧ Wake-proxy receives no more traffic.
     All subsequent requests go direct. 0ms overhead.
```

### Sleep Flow

```
cf stop myapp
    → PATCH /v3/routes/{primary}/destinations → point to wake-proxy
```

## Measured Performance

Validated on a live CF landscape (Diego cells, HAProxy, gorouter — production topology).

| Scenario | Latency | HTTP Status |
|----------|---------|-------------|
| Cold wake (app stopped) | **3.24s** | 200 |
| Hot path (app already running) | **0.43s** | 200 |
| Direct (no proxy in path) | **0.12s** | 200 |

The 3s cold start breaks down as:
- ~0.5s: broker call + CC API start
- ~1.5s: Diego schedules container, downloads droplet
- ~0.5s: process starts, binds port
- ~0.5s: route-emitter registers route, gorouter picks it up
- ~0.2s: forward with retry (multi-gorouter convergence)

The proxy adds 0.3s on the first request. After route swap, it adds 0s.

## Architecture Details

### Broker (`broker/`)

An [Open Service Broker](https://www.openservicebrokerapi.org/) implementation. Registered via `cf create-service-broker`. Implements catalog, provision, deprovision, bind, unbind.

The broker holds CC admin credentials. Users never see them. Each service binding generates a unique bearer token scoped to the caller's CF space. The wake endpoint (`POST /wake/{app_guid}`) validates:

1. Bearer token is valid (issued during bind)
2. App belongs to the caller's space (CC API check)
3. Rate limit: max 1 wake per app per 60 seconds

670 lines of Go. 32M memory. No dependencies beyond stdlib.

### Wake-Proxy (`wake-proxy/`)

Reverse proxy that intercepts requests when the target app is stopped.

Key behaviors:
- **Request coalescing**: one poller per hostname. 50 concurrent requests waiting for the same app share a single wake session — 1 broker call, 1 health poller, 1 route swap.
- **Retry with backoff**: multi-gorouter deployments have independent route tables. After cold start, route propagation takes ~1s via NATS fan-out. The proxy retries 502/503/404 responses with exponential backoff (200ms → 3.2s) to handle HAProxy round-robin hitting an un-updated gorouter.
- **No redirect following**: backend redirects pass through to the original client with Location headers intact.
- **Graceful shutdown**: SIGTERM sets a drain flag. New wake requests get `503 + Retry-After: 1`. In-flight requests complete. 8s drain timeout (Diego gives 10s before SIGKILL).
- **Rate limiting**: 50 concurrent waiters per hostname. At cap, returns `429 + Retry-After: 5`.
- **Self-healing**: if the proxy crashes mid-wake (app started but route not swapped), the next request to a surviving instance creates a new session — health passes immediately, forward works, swap fires.
- **Body buffering**: request bodies are buffered (1MB cap) so POST/PUT can be retried across gorouter convergence.

640 lines of Go. 64M memory. No dependencies beyond stdlib.

### Target App (`target-app/`)

Minimal Go HTTP server for testing. Replace with your app.

40 lines. 32M memory.

## HA / Multi-Instance

The system is stateless per instance. No distributed lock. No shared state. No coordination.

This works because every CF API operation used is idempotent:
- `POST /v3/apps/.../start` returns 422 if already running — harmless
- Health polling the secondary route is read-only
- `PATCH /v3/routes/.../destinations` replaces all destinations atomically — same payload = same result

Multiple wake-proxy instances polling the same secondary route = redundant but harmless. Multiple instances calling the broker = one start succeeds, the rest get 422.

## Why Not Just Use Route Services?

Route services bind at the route level via gorouter middleware. When an app is stopped, gorouter removes the route entirely. The Lookup middleware (step 13) returns 503 before the RouteService middleware (step 18) runs.

The request never reaches any route service. There is nothing to intercept.

The dual-route design sidesteps this: the primary route always points to *something alive* (either the app or the wake-proxy). Gorouter never sees an empty route pool.

## Relation to the CF Application Autoscaler

The autoscaler and this PoC are complementary, not competing.

| Layer | What it does | Who |
|-------|-------------|-----|
| **This PoC** | Makes `cf stop` safe — requests don't 503, they wait and get served | Wake-proxy (infrastructure primitive) |
| **Autoscaler** | Decides *when* to stop/start based on load metrics | Autoscaler (policy engine) |

The autoscaler cannot scale to zero today because there is no safe way to stop an app. This PoC provides that safe stop. With it in place, the autoscaler change is small:
- Allow `instance_min_count: 0` in the policy schema (currently min is 1)
- When computed instances = 0, call stop-with-wake instead of scaling to 0 instances
- Existing 300s cooldown prevents rapid cycling

## Demo Coordinator (`demo-coordinator/`)

A real-time WebSocket dashboard for live demos. Manages multiple CF apps with:

- Per-app invoke/stop buttons with live stopwatch during cold start
- State badges: STOPPED → INVOKING → REACHABLE → STARTED
- **CF billing events**: polls `/v3/app_usage_events`, computes GB-seconds per app in real time, shows cost savings from scale-to-zero
- Reset button: stops all apps, clears billing counters

Deploy 3 demo apps + coordinator with `scripts/deploy-demo.sh`.

## Project Structure

```
broker/              OSB service broker (Go, 32M)
wake-proxy/          Request-holding reverse proxy (Go, 64M)
target-app/          Example target app (Go, 32M)
demo-coordinator/    Live demo dashboard with billing tracker (Go, 64M)
demo-app/            Simple demo target app (Go, 32M)
scripts/
  deploy.sh          Deploy broker + wake-proxy + target
  deploy-demo.sh     Deploy 3 demo apps + coordinator
  s2z-ctl.sh         CLI helpers (invoke, reset, open UI)
```

## Deployment

### Prerequisites

- `cf` CLI logged in with admin access
- Go 1.22+ for cross-compilation
- A UAA client with `cloud_controller.admin` scope (for the broker to start/stop apps)

### Quick Start

```bash
export DOMAIN="cfapps.example.com"
export CF_API="https://api.cf.example.com"
export CF_ADMIN_USER="admin"
export CF_ADMIN_PASS="..."
export BROKER_PASS="pick-a-strong-password"

./scripts/deploy.sh
```

### Test It

```bash
# 1. Confirm app is running
curl https://s2z-target.$DOMAIN
# → {"app":"s2z-target","status":"running","uptime":"2m30s",...}

# 2. Stop the app, swap route to wake-proxy
cf stop s2z-target
# (deploy.sh prints the PATCH command for route swap)

# 3. Hit it again — wake-proxy catches the request, wakes the app, forwards
time curl https://s2z-target.$DOMAIN
# → {"app":"s2z-target","status":"running","uptime":"1s",...}
# real    0m3.24s

# 4. Hit it again — goes direct, wake-proxy is out of the path
time curl https://s2z-target.$DOMAIN
# → {"app":"s2z-target","status":"running","uptime":"5s",...}
# real    0m0.12s
```

## Production Considerations

This is a PoC. For production:

- **Token management**: the broker uses password grant. Production should use `client_credentials` with a dedicated UAA client.
- **Shared module**: `getCCToken()` and HTTP client setup are duplicated between broker and wake-proxy. Extract to an internal Go module.
- **Persistent binding store**: binding tokens are in-memory. A broker restart loses all tokens. Production needs a persistent store (CredHub, database, or reconstruct from CC service bindings on startup).
- **Metrics**: add Prometheus endpoints for wake latency, wake count, error rate, concurrent waiters.
- **TLS**: the PoC uses `InsecureSkipVerify` for self-signed LOD certs. Production should use proper CA bundles.
- **Rate limit storage**: per-app wake cooldown is in-memory. Multiple broker instances don't share it. Acceptable for PoC; production needs shared state or relies on CC idempotency.

## License

Apache 2.0
