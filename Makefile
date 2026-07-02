IMG ?= quay.io/jeffdyoung/wto:latest
AUTHFILE ?= $(HOME)/.docker/config.json
CONTAINER_RUNTIME ?= podman
GO_CONTAINER := docker.io/golang:1.26

.PHONY: build generate manifests docker-build docker-push deploy undeploy test tidy

tidy:
	$(CONTAINER_RUNTIME) run --rm -v $$(pwd):/workspace:Z -w /workspace $(GO_CONTAINER) go mod tidy

build:
	$(CONTAINER_RUNTIME) run --rm -v $$(pwd):/workspace:Z -w /workspace $(GO_CONTAINER) \
		go build -o bin/wto ./cmd/

generate: tidy
	$(CONTAINER_RUNTIME) run --rm -v $$(pwd):/workspace:Z -w /workspace $(GO_CONTAINER) \
		go run sigs.k8s.io/controller-tools/cmd/controller-gen \
		object paths=./api/...
	$(CONTAINER_RUNTIME) run --rm -v $$(pwd):/workspace:Z -w /workspace $(GO_CONTAINER) \
		go run sigs.k8s.io/controller-tools/cmd/controller-gen \
		crd paths=./api/... output:crd:dir=config/crd

test:
	$(CONTAINER_RUNTIME) run --rm -v $$(pwd):/workspace:Z -w /workspace $(GO_CONTAINER) \
		go test ./... -v

docker-build:
	$(CONTAINER_RUNTIME) build -t $(IMG) .

docker-push:
	$(CONTAINER_RUNTIME) push --authfile $(AUTHFILE) $(IMG)

deploy:
	oc apply -f config/crd/
	oc apply -f config/rbac/
	oc apply -f config/manager/
	oc apply -f config/webhook/

undeploy:
	oc delete -f config/webhook/ --ignore-not-found
	oc delete -f config/manager/ --ignore-not-found
	oc delete -f config/rbac/ --ignore-not-found
	oc delete -f config/crd/ --ignore-not-found

samples:
	oc apply -f config/samples/
