# DEMO — script for the live walk-through

Run through this top-to-bottom. Times are approximate; the cluster takes the
longest part of the demo (~2 minutes for the cold path) and the rest is fast.
The audience should never see a blank screen — every step produces visible
output.

## Pre-flight (do BEFORE the call starts)

```bash
make cluster-down 2>/dev/null
# add hosts entries once:
grep -q ws.local.test /etc/hosts || echo "127.0.0.1 ws.local.test grafana.local.test" | sudo tee -a /etc/hosts
```

Tabs to have open before the call:

- Terminal 1 — running commands
- Terminal 2 — `make logs` (will start at step 3)
- Browser tab 1 — http://ws.local.test (will open at step 4)
- Browser tab 2 — http://ws.local.test (second instance, also step 4)
- Browser tab 3 — http://grafana.local.test (step 5)

---

## 1. Cluster from zero (90 seconds)

> *"The cluster is local kind — 1 control plane, 2 workers, with host ports 80
> and 443 mapped so the ingress is reachable on `localhost`. Same manifests
> would deploy unmodified to EKS or GKE."*

```bash
make cluster
```

Wait for "Ready after Xs". Run `kubectl get nodes` to show the 3-node topology.

---

## 2. Stack up (60 seconds)

```bash
make ingress
make observability
make build && make load
make deploy
```

Walk through what each step did. Highlight:

- `make ingress` installs nginx-ingress with the kind variant (host port mapping ready).
- `make observability` installs the Prometheus Operator + a Prometheus instance + Grafana with a pre-loaded dashboard (mounted as a ConfigMap so no manual import).
- `make build && make load` builds the multi-stage distroless image (13 MB) and side-loads it into all 3 kind nodes — no registry required for the local path.
- `make deploy` applies the kustomize overlay: namespace, Redis, 3-replica Deployment with anti-affinity + PDB, Service, Ingress with WS annotations, ServiceMonitor.

```bash
kubectl -n ws-demo get pods,svc,ingress
```

---

## 3. Logs streaming (background, leave running)

```bash
# Terminal 2
make logs
```

This tails all 3 pods, prefixed. You'll refer back to it during the rolling
update demo.

---

## 4. WebSocket end-to-end (90 seconds)

Open both browser tabs to `http://ws.local.test`. In each:

1. Click **connect** — the dot turns green.
2. Type a message in one tab, press Enter.
3. The same message appears in BOTH tabs.

Point at the message: *"the `sender` field shows the pod that published the
message. If both tabs land on different pods, you'll see two different sender
IDs. That's the Redis pub/sub fanout doing its job — without it, only the tab
on the same pod as the sender would receive."*

To force the point, run the deterministic smoke test:

```bash
make test
```

Output:

```
opened 5 clients
sent "hello-..." from client[0]
  client[0]: GOT broadcast
  client[1]: GOT broadcast
  client[2]: GOT broadcast
  client[3]: GOT broadcast
  client[4]: GOT broadcast
result: 5/5 received  PASS
```

---

## 5. Observability (60 seconds)

Open `http://grafana.local.test` — login `admin` / `admin`, hit **Dashboards →
WebSocket Service**.

Send a few messages from the browser tabs, watch the panels move:

- **Active connections** rises.
- **Messages in/out per pod** ticks up.
- **Broadcast fan-out latency p95** stays low (this is the per-pod overhead of
  delivering a Redis message to local clients; should be sub-millisecond).

Then show Prometheus targets (`port-forward` to skip the ingress):

```bash
kubectl -n observability port-forward svc/prometheus 9090:9090 &
# in another tab: http://localhost:9090/targets
```

All 3 `ws-server` endpoints should be `UP`.

---

## 6. Rolling update with no dropped sessions (90 seconds — the killer demo)

In the browser tabs, leave the WebSocket connections open. Type a few messages,
confirm both still receive.

In the terminal:

```bash
make redeploy
```

Watch:

- `make logs` (Terminal 2) shows new pods coming up and old ones receiving
  SIGTERM. Look for the "shutdown signal received, draining" log lines.
- The browser tabs will momentarily show `close code=1012 reason=shutting down`
  and immediately reconnect on the new pods. **The disconnect is clean** — no
  TCP RST, no scary error.
- Send a message after reconnect: still broadcasts to both tabs. The Redis
  channel state survived.

> *"Without the graceful shutdown choreography, a rolling update would send
> TCP RSTs to every connected client. With 10k connections that's a
> thundering-herd reconnect storm. The combination of readiness flip + drain
> delay + close-frame is what makes WebSockets safe to update in place."*

---

## 7. Tear down

```bash
make cluster-down
```

Done.

## Backup plan if something breaks live

- **The image won't load**: `sg docker -c "docker images | grep fxreplay"` to
  verify it's there; `kind load docker-image ...` to push manually.
- **Ingress 404**: `kubectl -n ingress-nginx logs deploy/ingress-nginx-controller --tail=50` — usually it's a Host header mismatch.
- **Pods CrashLoop**: `kubectl -n ws-demo logs <pod>` — almost always Redis
  connectivity (the readiness probe will keep them out of Service until they
  can reach it).
- **Grafana panel empty**: scrape isn't happening yet; the first 15s after
  pods come up has no data. Wait 30s.
