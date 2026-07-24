IMG ?= ghcr.io/defilantech/llmkube-kueue:dev
KUEUE_VERSION ?= v0.19.0
CERT_MANAGER_VERSION ?= v1.16.3
ENVTEST_K8S_VERSION ?= 1.36.2
KIND_CLUSTER ?= llmkube-kueue-e2e
LOCALBIN := $(shell pwd)/bin

.PHONY: build test lint docker-build kind-load deploy undeploy kueue cert-manager envtest-bins kind-create llmkube-crds e2e-setup test-e2e

build:
	go build -o bin/llmkube-kueue ./cmd

$(LOCALBIN)/setup-envtest:
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.24

envtest-bins: $(LOCALBIN)/setup-envtest
	$(LOCALBIN)/setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path

test: envtest-bins
	KUBEBUILDER_ASSETS="$$($(LOCALBIN)/setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./... -coverprofile cover.out

lint: $(LOCALBIN)/golangci-lint
	$(LOCALBIN)/golangci-lint run ./...

$(LOCALBIN)/golangci-lint:
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

docker-build:
	docker build -t $(IMG) .

kind-load: docker-build
	kind load docker-image $(IMG) --name $(KIND_CLUSTER)

deploy:
	kubectl apply -k config/default

undeploy:
	kubectl delete -k config/default --ignore-not-found

kueue:
	kubectl apply --server-side -f https://github.com/kubernetes-sigs/kueue/releases/download/$(KUEUE_VERSION)/manifests.yaml
	kubectl -n kueue-system create configmap kueue-manager-config --from-file=controller_manager_config.yaml=hack/kueue-config.yaml -o yaml --dry-run=client | kubectl apply -f -
	kubectl -n kueue-system rollout restart deployment/kueue-controller-manager

cert-manager:
	kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml

kind-create:
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --name $(KIND_CLUSTER)

llmkube-crds:
	kubectl apply --server-side -f "$$(go list -m -f '{{.Dir}}' github.com/defilantech/llmkube)/config/crd/bases"

e2e-setup: kind-create cert-manager llmkube-crds
	kubectl -n cert-manager rollout status deployment/cert-manager --timeout=180s
	kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=180s
	kubectl -n cert-manager rollout status deployment/cert-manager-cainjector --timeout=180s
	$(MAKE) kueue
	kubectl -n kueue-system rollout status deployment/kueue-controller-manager --timeout=180s
	$(MAKE) kind-load KIND_CLUSTER=$(KIND_CLUSTER)
	$(MAKE) deploy
	kubectl -n llmkube-kueue-system rollout status deployment/llmkube-kueue-controller --timeout=180s
	kubectl -n llmkube-kueue-system rollout status deployment/llmkube-kueue-webhook --timeout=180s
	kubectl -n llmkube-kueue-system wait certificate/llmkube-kueue-webhook-cert --for=condition=Ready --timeout=180s
	kubectl -n llmkube-kueue-system get endpoints llmkube-kueue-webhook -o jsonpath='{.subsets[*].addresses[*].ip}' | grep -q . || (echo "webhook endpoints empty" && exit 1)

test-e2e: e2e-setup
	go test -tags e2e -count=1 -timeout 30m ./test/e2e/... -v
