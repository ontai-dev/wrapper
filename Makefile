.PHONY: build test lint generate generate-deepcopy generate-crd clean

CONTROLLER_GEN ?= $(shell which controller-gen 2>/dev/null || echo $(HOME)/go/bin/controller-gen)

build:
	go build ./...

test:
	go test ./...

lint:
	golangci-lint run ./...

generate: generate-deepcopy generate-crd

generate-deepcopy:
	$(CONTROLLER_GEN) object paths=./api/...

generate-crd:
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=config/crd

clean:
	rm -rf bin/
