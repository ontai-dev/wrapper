.PHONY: build test e2e lint lint-docs install-hooks generate generate-deepcopy generate-crd clean

CONTROLLER_GEN ?= $(shell which controller-gen 2>/dev/null || echo $(HOME)/go/bin/controller-gen)

build:
	go build ./...

test:
	go test ./test/unit/...

e2e:
	MGMT_KUBECONFIG=$(MGMT_KUBECONFIG) TENANT_KUBECONFIG=$(TENANT_KUBECONFIG) \
	REGISTRY_ADDR=$(REGISTRY_ADDR) MGMT_CLUSTER_NAME=$(MGMT_CLUSTER_NAME) \
	TENANT_CLUSTER_NAME=$(TENANT_CLUSTER_NAME) \
	go test ./test/e2e/... -v -timeout 30m

lint: lint-docs install-hooks
	golangci-lint run ./...

lint-docs:
	@echo ">>> lint-docs: verifying no unintended tracked .md files"
	@bad=$$(git ls-files '*.md' | grep -v '^README\.md$$' | grep -v -- '-schema\.md$$'); \
	if [ -n "$$bad" ]; then \
		echo "FAIL: tracked .md files violating policy:"; \
		echo "$$bad"; \
		exit 1; \
	fi
	@echo "PASS: no unintended tracked .md files"
	@echo ">>> lint-docs: scanning session/1-governor-init for Co-Authored-By trailers"
	@if git log session/1-governor-init --format='%B' 2>/dev/null | grep -qE '^Co-Authored-By:|^Co-authored-by:'; then \
		echo "FAIL: Co-Authored-By trailer found in commit history"; \
		exit 1; \
	fi
	@echo "PASS: no Co-Authored-By trailers in commit history"

install-hooks:
	@echo ">>> install-hooks: installing commit-msg hook"
	@cp scripts/commit-msg .git/hooks/commit-msg
	@chmod +x .git/hooks/commit-msg
	@echo "PASS: commit-msg hook installed at .git/hooks/commit-msg"

generate: generate-deepcopy generate-crd

generate-deepcopy:
	$(CONTROLLER_GEN) object paths=./api/...

generate-crd:
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=config/crd

clean:
	rm -rf bin/
