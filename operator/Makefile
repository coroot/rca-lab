IMAGE ?= ghcr.io/coroot/rca-lab/operator:1.0.0
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@latest

.PHONY: all build test vet generate manifests docker-build

all: generate manifests build test

build:
	go build -o bin/rca-lab ./cmd

test:
	go test ./...

vet:
	go vet ./...

## generate: update zz_generated.deepcopy.go
generate:
	$(CONTROLLER_GEN) object paths=./api/...

## manifests: regenerate the committed CRD in deploy/rca-operator/
manifests:
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=.crd-tmp
	mv .crd-tmp/rcalab.dev_failurescenarios.yaml ../deploy/rca-operator/crd.yaml
	rmdir .crd-tmp

docker-build:
	docker build -t $(IMAGE) .
