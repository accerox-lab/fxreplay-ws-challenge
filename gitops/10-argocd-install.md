# ArgoCD bootstrap

This directory holds the GitOps glue. ArgoCD is the bridge that promotes
images from the registry into a running cluster — what the challenge calls
*"how changes move from source code or configuration are promoted into a
running Kubernetes deployment"*.

## Install ArgoCD on the cluster

ArgoCD is not committed as flat manifests here because the canonical install
file is ~3 MB. Bootstrap from upstream once per cluster:

```bash
kubectl create namespace argocd
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/v2.13.1/manifests/install.yaml
kubectl wait --for=condition=available --timeout=120s deployment/argocd-server -n argocd
```

Then apply the ingress + Application from this directory:

```bash
kubectl apply -f gitops/20-argocd-ingress.yaml
kubectl apply -f gitops/30-application.yaml
```

The Makefile target `make gitops` does all of the above in one shot.

## The Application

`30-application.yaml` tells ArgoCD: *"watch this git repo's `deploy/` directory,
sync it to the cluster, auto-prune, auto-heal."* Once applied, ArgoCD owns
the `ws-demo` namespace — any drift between git and the cluster is corrected
automatically (within ~3 minutes by default).

## How a code change becomes a running pod

1. Dev edits `app/main.go` and pushes to `main`.
2. GitHub Actions builds the image, tags it `sha-<short>`, pushes to GHCR.
3. The same CI run executes `kustomize edit set image ws-server=ghcr.io/...:sha-<short>`
   on `deploy/kustomization.yaml`, then commits + pushes the change back.
   That commit carries `[skip ci]` so it does not re-trigger CI.
4. ArgoCD polls git (or receives a webhook), sees the new commit, and applies
   the manifest. The Deployment's image reference changes, which triggers a
   rolling update with the graceful shutdown choreography defined in the
   manifest.
5. Pods rotate; in-flight WebSocket clients receive a close-code-1012 and
   reconnect to the new pods.

End-to-end: dev pushes code, dev sees new pods running. No manual `kubectl`
in the loop.

## ArgoCD UI access

After applying the ingress:

```bash
# initial admin password (auto-generated on first install)
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
echo
```

Then open `http://argocd.local.test` (after the /etc/hosts entry), user `admin`.
