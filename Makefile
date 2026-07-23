IMG ?= ghcr.io/defilantech/llmkube-kueue:dev
KUEUE_VERSION ?= v0.19.0
CERT_MANAGER_VERSION ?= v1.16.3
LOCALBIN := $(shell pwd)/bin

.PHONY: build test lint docker-build kind-load deploy undeploy kueue cert-manager

build:
	go build -o bin/llmkube-kueue ./cmd

test:
	go test ./... -coverprofile cover.out

lint: $(LOCALBIN)/golangci-lint
	$(LOCALBIN)/golangci-lint run ./...

$(LOCALBIN)/golangci-lint:
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

docker-build:
	docker build -t $(IMG) .

kind-load: docker-build
	kind load docker-image $(IMG)

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
