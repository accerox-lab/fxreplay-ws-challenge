CLUSTER     ?= fxreplay
IMAGE_REPO  ?= ghcr.io/accerox-lab/fxreplay-ws-challenge
IMAGE_TAG   ?= dev
IMAGE       := $(IMAGE_REPO):$(IMAGE_TAG)
NS          := ws-demo

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: cluster
cluster: ## Create the kind cluster (control plane + 2 workers, ports 80/443 mapped)
	kind create cluster --config .kind/cluster.yaml

.PHONY: cluster-down
cluster-down: ## Delete the kind cluster
	kind delete cluster --name $(CLUSTER)

.PHONY: ingress
ingress: ## Install ingress-nginx (kind variant)
	kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.11.2/deploy/static/provider/kind/deploy.yaml
	kubectl wait --namespace ingress-nginx --for=condition=ready pod --selector=app.kubernetes.io/component=controller --timeout=120s

.PHONY: observability
observability: ## Install Prometheus Operator + Prometheus + Grafana
	kubectl apply --server-side -f https://github.com/prometheus-operator/prometheus-operator/releases/download/v0.76.1/bundle.yaml
	kubectl wait --for=condition=available --timeout=60s deployment/prometheus-operator -n default
	kubectl apply -k observability/

.PHONY: gitops
gitops: ## Install ArgoCD + apply the Application that watches this repo
	kubectl apply -f gitops/00-argocd-namespace.yaml
	kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/v2.13.1/manifests/install.yaml
	kubectl wait --for=condition=available --timeout=120s deployment/argocd-server -n argocd
	kubectl apply -f gitops/15-argocd-config.yaml
	kubectl apply -f gitops/20-argocd-ingress.yaml
	kubectl -n argocd rollout restart deployment/argocd-server statefulset/argocd-application-controller
	kubectl -n argocd rollout status  deployment/argocd-server --timeout=60s
	kubectl -n argocd rollout status  statefulset/argocd-application-controller --timeout=60s
	kubectl apply -f gitops/30-application.yaml
	@echo ""
	@echo "ArgoCD UI: http://argocd.local.test  (add to /etc/hosts: 127.0.0.1 argocd.local.test)"
	@echo "Admin password:"
	@kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
	@echo ""

.PHONY: build
build: ## Build the docker image
	docker build -t $(IMAGE) .

.PHONY: load
load: ## Push the local image into the kind cluster
	kind load docker-image $(IMAGE) --name $(CLUSTER)

.PHONY: deploy
deploy: ## Apply the app manifests with kustomize
	kubectl apply -k deploy/

.PHONY: up
up: cluster ingress observability build load deploy ## Full bootstrap from zero (no GitOps)
	@echo "Add to /etc/hosts: 127.0.0.1 ws.local.test grafana.local.test"
	@echo "Then visit: http://grafana.local.test  (admin / admin)"

.PHONY: up-gitops
up-gitops: cluster ingress observability build load gitops ## Full bootstrap WITH ArgoCD managing the app
	@echo "Add to /etc/hosts: 127.0.0.1 ws.local.test grafana.local.test argocd.local.test"
	@echo "ArgoCD will auto-sync the app from git — no make deploy needed."

.PHONY: redeploy
redeploy: build load ## Rebuild + reload + roll the deployment
	kubectl -n $(NS) rollout restart deployment/ws-server
	kubectl -n $(NS) rollout status deployment/ws-server

.PHONY: logs
logs: ## Tail logs from all ws-server pods
	kubectl -n $(NS) logs -l app.kubernetes.io/name=ws-server -f --max-log-requests=10 --prefix

.PHONY: test
test: ## Run the cross-pod broadcast smoke test
	node hack/broadcast-test.js

.PHONY: hosts-hint
hosts-hint: ## Print the /etc/hosts entries needed for the ingress hostnames
	@echo "Add this to /etc/hosts:"
	@echo "127.0.0.1 ws.local.test grafana.local.test"
