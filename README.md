# fxreplay-ws-challenge

A real-time WebSocket service running on Kubernetes — built from scratch for the
Senior DevOps / Platform Engineer challenge. The repo is everything needed to
build, deploy, observe, and CI/CD the workload, plus a one-page client to
demo it end to end.

## What this is

A small Go server that accepts WebSocket connections and broadcasts every
message to every connected client, across pods, via Redis pub/sub. The
interesting part is not the server itself — there are plenty of those — but the
deployment around it: how it survives a rolling update without dropping
sessions, how it scales horizontally without losing the "every client gets every
message" property, and how the cluster sees what it's doing.

## What's in the box

```
.
├── app/                     Go source for the WebSocket server
│   ├── main.go              Hub, Redis pub/sub fanout, metrics, graceful shutdown
│   ├── client/index.html    Single-page browser client (embedded into the image)
│   └── go.mod
├── Dockerfile               Multi-stage, distroless, nonroot, static binary (~13 MB)
├── deploy/                  Application manifests, applied with kustomize
│   ├── 00-namespace.yaml
│   ├── 10-redis.yaml        Single Redis instance — the pub/sub spine
│   ├── 20-deployment.yaml   3 replicas, anti-affinity, PDB, preStop drain
│   ├── 30-ingress.yaml      nginx-ingress with WebSocket-friendly timeouts
│   ├── 40-servicemonitor.yaml   Picked up by Prometheus automatically
│   └── kustomization.yaml   Image tag overridden by CI
├── observability/           Prometheus + Grafana with a pre-loaded dashboard
│   ├── 00-rbac.yaml
│   ├── 10-prometheus.yaml
│   ├── 20-grafana.yaml      ConfigMap-mounted dashboard, no Helm
│   └── kustomization.yaml
├── .github/workflows/ci.yml GitHub Actions: lint -> build+push -> e2e in kind
├── .kind/cluster.yaml       Local cluster definition (1 cp + 2 workers, host port 80/443)
├── hack/broadcast-test.js   Smoke test used by CI and humans alike
└── Makefile                 `make up` brings everything online from zero
```

No Helm charts, no operators beyond Prometheus, no managed services — just raw
manifests applied with `kubectl` / `kustomize`. That was a constraint from the
challenge.

## The interesting decisions

### Why Redis pub/sub for broadcast

WebSocket connections are sticky to one pod. When client A (on pod 1) sends a
message intended for client B (on pod 2), the message has to cross the pod
boundary somehow. The naive "loop over local connections" only reaches clients
attached to the same pod, and with 3 pods you'd see 1/3 of broadcasts on
average.

The fix: every pod publishes outgoing messages to a Redis channel and
subscribes to it. Each pod fans out incoming Redis messages to its local
connections. Redis is doing nothing fancy here — it's a coordination point,
not the message store. The `hack/broadcast-test.js` smoke test asserts this
works by opening N clients (which land on different pods round-robin) and
verifying every one of them receives a message sent by client 0.

For production you'd swap Redis for NATS, Kafka, or whatever your platform
already runs. The pattern is the same.

### Why a 60-second graceful shutdown

WebSocket connections are long-lived. A vanilla `kubectl rollout restart` would
SIGTERM the pods, the process would exit, and every connected client would get
a TCP RST and reconnect in panic. With 1,000 active connections that's a
thundering-herd reconnect storm and a bad time for everyone.

The shutdown path:

1. SIGTERM arrives. App flips `/readyz` to 503.
2. `terminationGracePeriodSeconds: 60` keeps the container alive while the
   readiness change propagates through kube-proxy / iptables. New connections
   stop landing on this pod within ~3-5s of the readiness flip.
3. After `PRESTOP_DRAIN` seconds (10s by default), the app sends a WebSocket
   close frame with code `1012 (Service Restart)` to every connected client.
   Browsers treat that as "reconnect soon" instead of "panic".
4. HTTP server `Shutdown()` finishes any in-flight requests, then exits.

The `lifecycle.preStop` `sleep 5` is belt-and-suspenders: it delays SIGTERM
arrival so the readiness change has even more time to propagate before the app
starts its drain.

### Why nginx-ingress with explicit timeouts

By default nginx-ingress times out idle connections at 60s. A WebSocket that
doesn't send a frame for a minute would be reset by the proxy. The
`proxy-read-timeout` / `proxy-send-timeout` annotations on the Ingress bump that
to an hour. The app also sends a WS ping every 30s as a keepalive on top of
that.

`proxy-buffering: off` because buffering and WebSockets do not get along.

### Why no Helm

Two reasons. First, the challenge asked for it explicitly. Second, for a
4-resource app it's not worth the indirection — kustomize gives you image-tag
substitution and namespace prefixing without learning a templating language.

## Observability — what's actually measured

Metrics exposed by the app (`/metrics` on port 8080, Prometheus format):

| Metric | What it tells you |
| --- | --- |
| `ws_active_connections` | Per-pod gauge of currently open WS connections |
| `ws_messages_received_total` | Counter, increments on every frame from a client |
| `ws_messages_sent_total` | Counter, increments on every frame written to a client |
| `ws_redis_publish_errors_total` | Counter, non-zero means Redis is in trouble |
| `ws_broadcast_latency_seconds` | Histogram, time from "Redis delivered the message" to "local fan-out started" |

The 4 golden signals end up well covered: latency = the histogram; traffic =
the in/out counters; errors = publish errors + the absence of expected message
counts; saturation = connections gauge plus the standard kubelet pod-level
CPU/memory.

`ServiceMonitor` is the discovery mechanism — the Prometheus instance picks up
any namespace with a matching label. The Grafana dashboard is mounted as a
ConfigMap so it loads at startup with no manual import.

### How you'd extend this in production

- **Alerts**: `ws_redis_publish_errors_total` going non-zero, `absent(ws_active_connections)` per pod, p95 broadcast latency above 100ms sustained.
- **SLOs**: e.g. "99% of pods scrape successfully", "99% of broadcasts deliver under 50ms".
- **Logs**: this repo logs JSON to stdout (slog). Production would pair it with Loki + Promtail (or whatever the company already runs) for query/retention. Adding a Loki manifest set would be ~3 more files; intentionally out of scope here.
- **Tracing**: WebSocket doesn't trivially map to OpenTelemetry spans, but every incoming frame could be wrapped in a span and propagated via Redis.
- **Dashboards per environment**: pin dashboards in git, use Grafana's file provider, version them with the app. Same approach as here, scaled up.

## CI/CD — what GitHub Actions does

`.github/workflows/ci.yml` runs three stages:

1. **lint** — `go vet`, `go build`, and `kustomize build` against both overlay
   directories. Fails fast on a broken manifest. ~15s on a cold runner.
2. **build** — BuildKit build, push to `ghcr.io/<owner>/<repo>` with a tag
   strategy that covers immutable (`sha-<short>`), human-readable (`<branch>`,
   semver `v*`) and convenience (`latest` on main). Layer cache via GHA cache.
3. **e2e** — bring up a kind cluster *inside the runner*, install the image we
   just built, run `hack/broadcast-test.js`. This is the "basic validation
   step" the challenge asks for; it catches at least the class of bugs where
   the image is fine in isolation but the manifests have drifted.

Image pushes use the default `GITHUB_TOKEN` for GHCR — no extra secret to
manage. PRs build but don't push, which keeps the registry clean of throwaway
tags.

## Running it locally

You need Docker, `kubectl`, `kind`, `kustomize`, and node + npm (for the smoke
test only). On Ubuntu 24:

```bash
sudo apt install -y docker.io
curl -fsSL -o /usr/local/bin/kind https://kind.sigs.k8s.io/dl/v0.24.0/kind-linux-amd64
chmod +x /usr/local/bin/kind
```

Then:

```bash
make up                              # cluster + ingress + observability + image + deploy
echo "127.0.0.1 ws.local.test grafana.local.test" | sudo tee -a /etc/hosts
xdg-open http://ws.local.test        # the demo client, open in 2 browser tabs
xdg-open http://grafana.local.test   # admin / admin -> "WebSocket Service" dashboard
```

To watch what's happening:

```bash
make logs                            # tails all 3 ws-server pods
make test                            # runs the cross-pod broadcast smoke test
```

To prove rolling updates don't drop connections:

```bash
# in one terminal:
make logs
# in a browser tab, open http://ws.local.test, hit "connect", type a few messages.
# in a second terminal:
make redeploy                        # rebuilds image, reloads to kind, restarts deployment
# the browser tab should briefly see "close code=1012 reason=shutting down"
# and the page reconnects automatically. No RST, no lost session.
```

Tear it all down with `make cluster-down`.

## Demo plan (for the live screen-share)

See `DEMO.md` — a 10-minute scripted walk-through with the exact commands to
type so the audience never sees a blank screen.

## What I deliberately did not do

- **Helm charts** — the challenge banned them.
- **Multi-arch images** — kind is amd64-only; arm64 adds CI time. One line change when needed.
- **A persistent volume for Prometheus/Grafana** — demo cluster, restart wipes
  metrics. Production would attach PVCs.
- **TLS on the ingress** — the cluster has no real DNS or ACME issuer. Adding
  cert-manager + Let's Encrypt is a one-manifest change for a real cluster.
- **External secrets** — Grafana admin pwd is in a plain Secret. Production
  pulls from Vault / ExternalSecretsOperator / SealedSecrets.
- **HPA** — connection count is a non-CPU signal. Plumbing it as an external
  metric (KEDA scaler on `ws_active_connections`) is the right way; out of
  scope for the time budget.

These are flagged here so the reviewer doesn't have to look for them.
