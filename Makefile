# The tag is the version: ldflags, image build args and hack/bump-version.sh read it here.
IMG ?= somaz940/plainbucket:v0.1.0
APP_NAME := plainbucket
MODULE := github.com/somaz94/plainbucket

GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -ldflags "\
	-X $(MODULE)/internal/version.Version=$(shell echo ${IMG} | cut -d: -f2) \
	-X $(MODULE)/internal/version.GitCommit=$(GIT_COMMIT) \
	-X $(MODULE)/internal/version.BuildDate=$(BUILD_DATE) \
	-s -w"

# Container tool (docker or podman)
CONTAINER_TOOL ?= docker

DOCKER_BUILD_ARGS = \
	--build-arg VERSION=$(shell echo ${IMG} | cut -d: -f2) \
	--build-arg GIT_COMMIT=$(GIT_COMMIT) \
	--build-arg BUILD_DATE=$(BUILD_DATE)

PLATFORMS ?= linux/amd64,linux/arm64

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

GOLANGCI_LINT_VERSION ?= v2.15.0
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

# Minimum total statement coverage enforced by cover-check (and CI).
COVERAGE_MIN ?= 90

# Local run
DEPLOY_NAME ?= $(APP_NAME)
DEPLOY_PORT ?= 9000
DEPLOY_ROOT ?= $(CURDIR)/testdata
DEPLOY_ACCESS_KEY ?= plainbucket
DEPLOY_SECRET_KEY ?= plainbucket-secret

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: build
build: ## Build binary to bin/
	go build $(LDFLAGS) -o bin/$(APP_NAME) ./cmd/

.PHONY: run
run: build ## Build and run the server
	./bin/$(APP_NAME)

.PHONY: fmt
fmt: ## Run go fmt against code
	go fmt ./...
	cd compat && go fmt ./...

.PHONY: vet
vet: ## Run go vet against code
	go vet ./...
	cd compat && go vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

##@ Testing

.PHONY: test
test: ## Run all tests with race detection and a cross-package coverage profile
	go test ./... -race -coverpkg=./... -coverprofile=coverage.out

.PHONY: test-unit
test-unit: ## Run unit tests only (internal packages)
	go test ./internal/... -race -cover

.PHONY: test-compat
test-compat: ## Run the AWS SDK for Go v2 compatibility suite (separate module)
	cd compat && go test ./... -race -count=1

.PHONY: test-awscli
test-awscli: build ## Run the aws cli end-to-end script against bin/plainbucket
	@bash hack/test-awscli.sh

.PHONY: test-rclone
test-rclone: build ## Run the rclone end-to-end script against bin/plainbucket
	@bash hack/test-rclone.sh

.PHONY: test-runner
test-runner: build ## Run the GitLab Runner cache end-to-end script (downloads a pinned gitlab-runner)
	@bash hack/test-gitlab-runner.sh

.PHONY: test-s3tests
test-s3tests: build ## Run the pinned ceph/s3-tests subset and compare with hack/s3tests-pass.txt
	@bash hack/test-s3tests.sh

.PHONY: s3tests-update
s3tests-update: build ## Rewrite hack/s3tests-pass.txt from a ceph/s3-tests run
	@S3TESTS_UPDATE=1 bash hack/test-s3tests.sh

.PHONY: test-helm
test-helm: ## Lint and template-test the Helm chart
	@bash hack/test-helm.sh

.PHONY: test-all
test-all: test cover-check test-compat test-awscli test-rclone test-runner test-s3tests test-helm ## Run every test suite

.PHONY: cover
cover: test ## Generate HTML coverage report
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

.PHONY: cover-check
cover-check: ## Fail if total coverage in coverage.out is below COVERAGE_MIN
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {sub("%", "", $$3); print $$3}'); \
	echo "total coverage: $${total}% (minimum $(COVERAGE_MIN)%)"; \
	awk -v t="$${total}" -v m="$(COVERAGE_MIN)" 'BEGIN { exit (t + 0 < m + 0) }'

##@ Build

.PHONY: install
install: build ## Install binary to /usr/local/bin
	cp bin/$(APP_NAME) /usr/local/bin/$(APP_NAME)

.PHONY: uninstall
uninstall: ## Remove binary from /usr/local/bin
	rm -f /usr/local/bin/$(APP_NAME)

.PHONY: cross-build
cross-build: ## Build for multiple OS/arch (output: dist/)
	@mkdir -p dist
	@for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os=$${platform%%/*}; \
		arch=$${platform##*/}; \
		echo "Building $${os}/$${arch}..."; \
		GOOS=$${os} GOARCH=$${arch} go build $(LDFLAGS) -o dist/$(APP_NAME)-$${os}-$${arch} ./cmd/; \
	done
	@echo "Cross-build complete. Binaries in dist/"

##@ Docker

.PHONY: docker-build
docker-build: ## Build docker image
	$(CONTAINER_TOOL) build $(DOCKER_BUILD_ARGS) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: docker-buildx
docker-buildx: ## Build and push multi-arch image with version and latest tags
	- $(CONTAINER_TOOL) buildx create --name $(APP_NAME)-builder
	$(CONTAINER_TOOL) buildx use $(APP_NAME)-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) \
		$(DOCKER_BUILD_ARGS) \
		--tag ${IMG} \
		--tag $(shell echo ${IMG} | cut -f1 -d:):latest \
		-f Dockerfile .
	- $(CONTAINER_TOOL) buildx rm $(APP_NAME)-builder

##@ Version

.PHONY: version
version: ## Show current version across all files
	@./hack/bump-version.sh --current

VERSION ?=
.PHONY: bump-version
bump-version: ## Bump version across all files. Usage: make bump-version VERSION=v0.2.0
	@if [ -z "$(VERSION)" ]; then echo "Usage: make bump-version VERSION=vX.Y.Z"; exit 1; fi
	@./hack/bump-version.sh $(VERSION)

##@ Workflow

.PHONY: check-gh
check-gh: ## Check if gh CLI is installed and authenticated
	@command -v gh >/dev/null 2>&1 || { echo "\033[31m✗ gh CLI not installed. Run: brew install gh\033[0m"; exit 1; }
	@gh auth status >/dev/null 2>&1 || { echo "\033[31m✗ gh CLI not authenticated. Run: gh auth login\033[0m"; exit 1; }
	@echo "\033[32m✓ gh CLI ready\033[0m"

.PHONY: branch
branch: ## Create feature branch (usage: make branch name=list-v1)
	@if [ -z "$(name)" ]; then echo "Usage: make branch name=<feature-name>"; exit 1; fi
	git checkout main
	git pull origin main
	git checkout -b feat/$(name)
	@echo "\033[32m✓ Branch feat/$(name) created\033[0m"

.PHONY: pr
pr: check-gh ## Run tests, push, and create PR (usage: make pr title="Add feature")
	@if [ -z "$(title)" ]; then echo "Usage: make pr title=\"PR title\""; exit 1; fi
	go test ./... -race
	go vet ./...
	git push -u origin $$(git branch --show-current)
	@./scripts/create-pr.sh "$(title)"
	@echo "\033[32m✓ PR created\033[0m"

##@ Deploy

.PHONY: deploy
deploy: build ## Build binary + run local server on DEPLOY_PORT with DEPLOY_ROOT
	-@pkill -f "bin/$(APP_NAME)" 2>/dev/null || true
	@sleep 0.5
	@mkdir -p $(DEPLOY_ROOT)
	@PLAINBUCKET_ROOT=$(DEPLOY_ROOT) PLAINBUCKET_ADDRESS=:$(DEPLOY_PORT) \
		PLAINBUCKET_ACCESS_KEY=$(DEPLOY_ACCESS_KEY) PLAINBUCKET_SECRET_KEY=$(DEPLOY_SECRET_KEY) \
		./bin/$(APP_NAME) &
	@sleep 1
	@echo "plainbucket at http://localhost:$(DEPLOY_PORT) (access key $(DEPLOY_ACCESS_KEY))"

.PHONY: undeploy
undeploy: ## Stop local server
	-@pkill -f "bin/$(APP_NAME)" 2>/dev/null || true

.PHONY: deploy-docker
deploy-docker: ## Run as a Docker container with DEPLOY_ROOT mounted at /data
	@mkdir -p $(DEPLOY_ROOT)
	-@$(CONTAINER_TOOL) rm -f $(DEPLOY_NAME) 2>/dev/null
	$(CONTAINER_TOOL) run -d --name $(DEPLOY_NAME) -p $(DEPLOY_PORT):9000 \
		-e PLAINBUCKET_ACCESS_KEY=$(DEPLOY_ACCESS_KEY) -e PLAINBUCKET_SECRET_KEY=$(DEPLOY_SECRET_KEY) \
		-v $(DEPLOY_ROOT):/data ${IMG}

.PHONY: undeploy-docker
undeploy-docker: ## Stop and remove Docker container
	-$(CONTAINER_TOOL) rm -f $(DEPLOY_NAME) 2>/dev/null

##@ Cleanup

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/ dist/ coverage.out coverage.html

##@ Dependencies

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool <bin> <pkg> <version>: installs only if <bin> is missing, so a version bump needs `make clean` first.
define go-install-tool
@[ -f $(1) ] || { \
set -e ;\
TMP_DIR=$$(mktemp -d) ;\
cd $$TMP_DIR ;\
go mod init tmp ;\
echo "Downloading $(2)@$(3)" ;\
GOBIN=$(LOCALBIN) CGO_ENABLED=0 GOOS=$$(go env GOOS) GOARCH=$$(go env GOARCH) go install $(2)@$(3) ;\
rm -rf $$TMP_DIR ;\
}
endef
