# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# The packages controller-gen reads, named rather than ./...: controller-gen walks into nested
# Go modules below the repository root (a git worktree under .claude/, say) and would generate
# CRDs from whatever code they hold.
CONTROLLER_GEN_PATHS ?= {./api/...,./cmd/...,./internal/...,./test/...}

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd:allowDangerousTypes=true webhook paths="$(CONTROLLER_GEN_PATHS)" output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="$(CONTROLLER_GEN_PATHS)"

# The API reference is generated from the Go types, so a field cannot be added without appearing
# in it: CI regenerates it and fails on a diff.
.PHONY: api-docs
api-docs: crd-ref-docs ## Generate the API reference, docs/reference/api.md.
	@mkdir -p docs/reference
	@tmp=$$(mktemp -d); \
	"$(CRD_REF_DOCS)" --source-path=./api/v1alpha1 --config=hack/api-docs/config.yaml --renderer=markdown \
		--output-path="$$tmp/api.md" --log-level=warn && \
	{ head -n 1 "$$tmp/api.md"; echo; cat hack/api-docs/header.md; tail -n +2 "$$tmp/api.md"; } > docs/reference/api.md; \
	rc=$$?; rm -rf "$$tmp"; exit $$rc

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest prometheus helm ## Run tests.
	HELM="$(HELM)" PROMETHEUS="$(PROMETHEUS)" PROMTOOL="$(PROMTOOL)" KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# e2e tests run the operator in a Kind cluster next to a small Prometheus and Alertmanager
# (test/e2e). Kind and kubectl versions are pinned below.
KIND_CLUSTER ?= alert-operator-test-e2e
# Kubernetes version of the Kind node, as a kindest/node image pinned by digest. Empty means
# Kind's own default.
KIND_NODE_IMAGE ?=

# The integration test runs the whole operator in-process against a real kube-apiserver
# (envtest), a real Prometheus evaluating real rules on a tiny exporter, and a real
# Alertmanager: every v0.1 finding but NeverFired, which needs an hour. No Docker; about three
# minutes. The Kind e2e (test-e2e) adds prometheus-operator, the image and the chart.
.PHONY: test-integration
test-integration: manifests generate setup-envtest prometheus alertmanager ## Run the integration test (real Prometheus and Alertmanager, ~3 minutes).
	PROMETHEUS="$(PROMETHEUS)" ALERTMANAGER="$(ALERTMANAGER)" \
		KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -tags=integration ./test/integration/ -v -count=1 -timeout 10m

.PHONY: setup-test-e2e
setup-test-e2e: kind kubectl ## Set up a Kind cluster for e2e tests if it does not exist
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) $(if $(KIND_NODE_IMAGE),--image $(KIND_NODE_IMAGE)) --wait 4m ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet helm ## Run the e2e tests in Kind, then tear everything down.
	@status=0; \
	PATH="$(LOCALBIN):$$PATH" KIND="$(KIND)" KIND_CLUSTER="$(KIND_CLUSTER)" HELM="$(HELM)" \
		go test -tags=e2e ./test/e2e/ -v -count=1 -timeout 40m || status=$$?; \
	$(MAKE) cleanup-test-e2e; \
	exit $$status

.PHONY: cleanup-test-e2e
cleanup-test-e2e: kind ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER) || true
	@docker rm -f "$(KIND_CLUSTER)-control-plane" >/dev/null 2>&1 || true
	@# The suite tags the manager image per cluster; untagged, these pile up on a shared runner.
	@docker rmi "example.com/alert-operator:$(KIND_CLUSTER)" >/dev/null 2>&1 || true

CHART_DIR ?= charts/alert-operator

.PHONY: helm-crds
helm-crds: manifests ## Copy the generated CRDs and the Grafana dashboard into the Helm chart.
	# The chart ships exactly the generated CRDs: one that is no longer generated goes too.
	rm -f "$(CHART_DIR)"/crds/*.yaml
	cp config/crd/bases/*.yaml "$(CHART_DIR)/crds/"
	cp deploy/grafana/alert-quality.json "$(CHART_DIR)/dashboards/alert-quality.json"

.PHONY: helm-lint
helm-lint: helm helm-crds ## Lint and render the Helm chart in its main variants.
	"$(HELM)" lint "$(CHART_DIR)"
	"$(HELM)" lint "$(CHART_DIR)" --set scope.create=true --set networkPolicy.enabled=true \
		--set 'networkPolicy.apiServer.cidrs[0]=10.96.0.1/32' --set metrics.serviceMonitor.enabled=true \
		--set prometheusRule.enabled=true --set grafanaDashboard.enabled=true
	"$(HELM)" template release "$(CHART_DIR)" --set metrics.secure=false --set replicaCount=1 \
		--set alertRuleProtection.enabled=false --set rbac.aggregateToView=false >/dev/null
	# The webhook with each kind of certificate.
	"$(HELM)" lint "$(CHART_DIR)" --set webhook.enabled=true --set webhook.certificate.certManager=false
	"$(HELM)" template release "$(CHART_DIR)" --set webhook.enabled=true --set webhook.certificate.certManager=true >/dev/null
	"$(HELM)" template release "$(CHART_DIR)" --set webhook.enabled=true --set webhook.certificate.existingSecret=mine \
		--set webhook.certificate.caBundle=PEM --set networkPolicy.enabled=true >/dev/null

# Artifact Hub reads the chart's artifacthub.io/* annotations; its own linter checks them the way
# Artifact Hub will when it indexes a release.
.PHONY: artifacthub-lint
artifacthub-lint: ah ## Check the chart's Artifact Hub metadata with Artifact Hub's linter.
	"$(AH)" lint --kind helm --path charts

CHARTMUSEUM_URL ?= https://charts.hypersurgery.dev

.PHONY: helm-publish
helm-publish: helm helm-lint ## Package the chart and push it to ChartMuseum (needs CHARTMUSEUM_USER/PASSWORD).
	@[ -n "$(CHARTMUSEUM_USER)" ] && [ -n "$(CHARTMUSEUM_PASSWORD)" ] || { \
		echo "Set CHARTMUSEUM_USER and CHARTMUSEUM_PASSWORD"; exit 1; }
	@set -e; \
	out=$$(mktemp -d); \
	"$(HELM)" package "$(CHART_DIR)" -d "$$out" >/dev/null; \
	pkg=$$(ls "$$out"/*.tgz); \
	echo "Uploading $$(basename $$pkg) to $(CHARTMUSEUM_URL)"; \
	code=$$(curl -sS -u "$(CHARTMUSEUM_USER):$(CHARTMUSEUM_PASSWORD)" -o "$$out/resp" -w "%{http_code}" \
		--data-binary "@$$pkg" "$(CHARTMUSEUM_URL)/api/charts"); \
	cat "$$out/resp"; echo; \
	rm -rf "$$out"; \
	[ "$$code" = "201" ] || { echo "upload failed with HTTP $$code"; exit 1; }

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

# The Semgrep rule packs the sibling project runs. With Docker, in the pinned image; without it,
# a semgrep on PATH (pip install semgrep==$(SEMGREP_VERSION)) runs the same packs.
SEMGREP_VERSION ?= 1.178.0
SEMGREP_IMAGE ?= semgrep/semgrep:$(SEMGREP_VERSION)@sha256:32e459968daabe7ab86968184a29109b9564aa00392401156f9788452b42786b
SEMGREP_ARGS = scan --config=p/golang --config=p/security-audit --config=p/secrets --error --metrics=off

.PHONY: semgrep
semgrep: ## Run the Semgrep rules (Docker, or a semgrep on PATH).
	@if command -v docker >/dev/null 2>&1; then \
		docker run --rm --user "$$(id -u):$$(id -g)" -e HOME=/tmp -v "$(CURDIR):/src" -w /src \
			"$(SEMGREP_IMAGE)" semgrep $(SEMGREP_ARGS); \
	else \
		semgrep $(SEMGREP_ARGS); \
	fi

# govulncheck reports known vulnerabilities. The tool is pinned; the database it asks
# (vuln.go.dev) is live, so a run can fail without a change here: the fix is the dependency
# bump its output names.
GOVULNCHECK_VERSION ?= v1.8.0
GOVULNCHECK = go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

# The binary, not the sources: this is what a release ships, it takes seconds and little memory.
# The source scan builds the whole program's call graph and needs about 9 GB, more than the CI
# runner has, so it is a separate target that CI does not run.
.PHONY: govulncheck
govulncheck: ## Report known vulnerabilities in the manager and hsalert binaries (needs the network).
	@set -e; out=$$(mktemp -d); trap 'rm -rf "$$out"' EXIT; \
	go build -o "$$out/manager" ./cmd; \
	go build -o "$$out/hsalert" ./cmd/hsalert; \
	$(GOVULNCHECK) -mode=binary "$$out/manager"; \
	$(GOVULNCHECK) -mode=binary "$$out/hsalert"

.PHONY: govulncheck-source
govulncheck-source: ## The same from the sources, tests included (needs about 9 GB of memory).
	$(GOVULNCHECK) ./...

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the manager and the hsalert binaries.
	go build -o bin/manager ./cmd
	go build -o bin/hsalert ./cmd/hsalert

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.27
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name alert-operator-builder
	$(CONTAINER_TOOL) buildx use alert-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm alert-operator-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

# hsalert, the lint CLI, for every platform a CI runner or a laptop runs: dist/hsalert-<os>-<arch>
# with the version stamped. The release workflow builds the same files and attaches them, with
# signed checksums, to the GitHub release.
CLI_PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
CLI_VERSION ?= $(shell sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\}$$/\1/p' $(CHART_DIR)/Chart.yaml)

.PHONY: build-cli
build-cli: ## Build hsalert for linux and darwin, amd64 and arm64, into dist/.
	@mkdir -p dist
	@set -e; for p in $(CLI_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "dist/hsalert-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags "-s -w -X hypersurgery.dev/alert-operator/internal/version.Version=$(CLI_VERSION)" \
			-o "dist/hsalert-$$os-$$arch" ./cmd/hsalert; \
	done
	@cd dist && { sha256sum hsalert-* 2>/dev/null || shasum -a 256 hsalert-*; } > SHA256SUMS && cat SHA256SUMS

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize kubectl ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize kubectl ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize kubectl ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize kubectl ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= $(LOCALBIN)/kubectl
HELM ?= $(LOCALBIN)/helm
KIND ?= $(LOCALBIN)/kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0
CRD_REF_DOCS_VERSION ?= v0.3.0
CRD_REF_DOCS_SUM ?= h1:9bGSUkBR56Z7TuDGQAu3KGbBkagwwZ6RkZmS+qvDuDM=

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.15.0
KIND_VERSION ?= v0.33.0
HELM_VERSION ?= v4.3.0
KUBECTL_VERSION ?= v1.37.0

# The BSD and GNU checksum tools disagree about reading a checksum list, so compare digests.
define sha256of
$$( { sha256sum "$(1)" 2>/dev/null || shasum -a 256 "$(1)"; } | cut -d' ' -f1 )
endef

PROMETHEUS ?= $(LOCALBIN)/prometheus
PROMTOOL ?= $(LOCALBIN)/promtool
PROMETHEUS_VERSION ?= 3.14.0

# Prometheus and promtool from the same release tarball, checksum verified. The quality
# analyzer's tests start this Prometheus on a TSDB that promtool backfills with synthetic alert
# history (internal/analyze/prometheus_test.go).
.PHONY: prometheus
prometheus: $(PROMETHEUS) ## Download prometheus and promtool locally if necessary (checksum verified).
$(PROMETHEUS): $(LOCALBIN)
	@[ -f "$(PROMETHEUS)-$(PROMETHEUS_VERSION)" ] && [ -f "$(PROMTOOL)-$(PROMETHEUS_VERSION)" ] || { \
		set -e; \
		os=$$(go env GOOS); arch=$$(go env GOARCH); \
		name=prometheus-$(PROMETHEUS_VERSION).$$os-$$arch; \
		base=https://github.com/prometheus/prometheus/releases/download/v$(PROMETHEUS_VERSION); \
		echo "Downloading $$base/$$name.tar.gz"; \
		tmp=$$(mktemp -d); \
		curl -fsSL --retry 5 --retry-all-errors -o "$$tmp/$$name.tar.gz" "$$base/$$name.tar.gz"; \
		want=$$(curl -fsSL --retry 5 --retry-all-errors "$$base/sha256sums.txt" | grep " $$name.tar.gz$$" | cut -d' ' -f1); \
		got=$(call sha256of,$$tmp/$$name.tar.gz); \
		[ -n "$$want" ] && [ "$$want" = "$$got" ] || { echo "checksum mismatch for $$name.tar.gz"; exit 1; }; \
		tar -xzf "$$tmp/$$name.tar.gz" -C "$$tmp"; \
		mv "$$tmp/$$name/prometheus" "$(PROMETHEUS)-$(PROMETHEUS_VERSION)"; \
		mv "$$tmp/$$name/promtool" "$(PROMTOOL)-$(PROMETHEUS_VERSION)"; \
		rm -rf "$$tmp"; \
	}
	@ln -sf "$(PROMETHEUS)-$(PROMETHEUS_VERSION)" "$(PROMETHEUS)"
	@ln -sf "$(PROMTOOL)-$(PROMETHEUS_VERSION)" "$(PROMTOOL)"

ALERTMANAGER ?= $(LOCALBIN)/alertmanager
ALERTMANAGER_VERSION ?= 0.35.0

# Alertmanager from its release tarball, checksum verified, for the integration test.
.PHONY: alertmanager
alertmanager: $(ALERTMANAGER) ## Download alertmanager locally if necessary (checksum verified).
$(ALERTMANAGER): $(LOCALBIN)
	@[ -f "$(ALERTMANAGER)-$(ALERTMANAGER_VERSION)" ] || { \
		set -e; \
		os=$$(go env GOOS); arch=$$(go env GOARCH); \
		name=alertmanager-$(ALERTMANAGER_VERSION).$$os-$$arch; \
		base=https://github.com/prometheus/alertmanager/releases/download/v$(ALERTMANAGER_VERSION); \
		echo "Downloading $$base/$$name.tar.gz"; \
		tmp=$$(mktemp -d); \
		curl -fsSL --retry 5 --retry-all-errors -o "$$tmp/$$name.tar.gz" "$$base/$$name.tar.gz"; \
		want=$$(curl -fsSL --retry 5 --retry-all-errors "$$base/sha256sums.txt" | grep " $$name.tar.gz$$" | cut -d' ' -f1); \
		got=$(call sha256of,$$tmp/$$name.tar.gz); \
		[ -n "$$want" ] && [ "$$want" = "$$got" ] || { echo "checksum mismatch for $$name.tar.gz"; exit 1; }; \
		tar -xzf "$$tmp/$$name.tar.gz" -C "$$tmp"; \
		mv "$$tmp/$$name/alertmanager" "$(ALERTMANAGER)-$(ALERTMANAGER_VERSION)"; \
		rm -rf "$$tmp"; \
	}
	@ln -sf "$(ALERTMANAGER)-$(ALERTMANAGER_VERSION)" "$(ALERTMANAGER)"

AH ?= $(LOCALBIN)/ah
AH_VERSION ?= 1.23.0

.PHONY: ah
ah: $(AH) ## Download the Artifact Hub CLI locally if necessary (checksum verified).
$(AH): $(LOCALBIN)
	@[ -f "$(AH)-$(AH_VERSION)" ] || { \
		set -e; \
		os=$$(go env GOOS | sed 's/^darwin$$/macos/'); arch=$$(go env GOARCH); \
		name=ah_$(AH_VERSION)_$${os}_$$arch.tar.gz; \
		base=https://github.com/artifacthub/hub/releases/download/v$(AH_VERSION); \
		echo "Downloading $$base/$$name"; \
		tmp=$$(mktemp -d); \
		curl -fsSL --retry 5 --retry-all-errors -o "$$tmp/$$name" "$$base/$$name"; \
		want=$$(curl -fsSL --retry 5 --retry-all-errors "$$base/ah_$(AH_VERSION)_checksums.txt" | grep " $$name$$" | cut -d' ' -f1); \
		got=$(call sha256of,$$tmp/$$name); \
		[ -n "$$want" ] && [ "$$want" = "$$got" ] || { echo "checksum mismatch for $$name"; exit 1; }; \
		tar -xzf "$$tmp/$$name" -C "$$tmp" ah; \
		mv "$$tmp/ah" "$(AH)-$(AH_VERSION)"; \
		rm -rf "$$tmp"; \
	}
	@ln -sf "$(AH)-$(AH_VERSION)" "$(AH)"

ORAS ?= $(LOCALBIN)/oras
ORAS_VERSION ?= 1.3.4

.PHONY: oras
oras: $(ORAS) ## Download oras locally if necessary (checksum verified).
$(ORAS): $(LOCALBIN)
	@[ -f "$(ORAS)-$(ORAS_VERSION)" ] || { \
		set -e; \
		os=$$(go env GOOS); arch=$$(go env GOARCH); \
		name=oras_$(ORAS_VERSION)_$${os}_$$arch.tar.gz; \
		base=https://github.com/oras-project/oras/releases/download/v$(ORAS_VERSION); \
		echo "Downloading $$base/$$name"; \
		tmp=$$(mktemp -d); \
		curl -fsSL --retry 5 --retry-all-errors -o "$$tmp/$$name" "$$base/$$name"; \
		want=$$(curl -fsSL --retry 5 --retry-all-errors "$$base/oras_$(ORAS_VERSION)_checksums.txt" | grep " $$name$$" | cut -d' ' -f1); \
		got=$(call sha256of,$$tmp/$$name); \
		[ -n "$$want" ] && [ "$$want" = "$$got" ] || { echo "checksum mismatch for $$name"; exit 1; }; \
		tar -xzf "$$tmp/$$name" -C "$$tmp" oras; \
		mv "$$tmp/oras" "$(ORAS)-$(ORAS_VERSION)"; \
		rm -rf "$$tmp"; \
	}
	@ln -sf "$(ORAS)-$(ORAS_VERSION)" "$(ORAS)"

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): $(LOCALBIN)
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind,$(KIND_VERSION))

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary (checksum verified).
$(HELM): $(LOCALBIN)
	@[ -f "$(HELM)-$(HELM_VERSION)" ] || { \
		set -e; \
		os=$$(go env GOOS); arch=$$(go env GOARCH); \
		tgz=helm-$(HELM_VERSION)-$$os-$$arch.tar.gz; \
		echo "Downloading https://get.helm.sh/$$tgz"; \
		tmp=$$(mktemp -d); \
		curl -fsSL --retry 5 --retry-all-errors -o "$$tmp/$$tgz" "https://get.helm.sh/$$tgz"; \
		want=$$(curl -fsSL --retry 5 --retry-all-errors "https://get.helm.sh/$$tgz.sha256sum" | cut -d' ' -f1); \
		got=$(call sha256of,$$tmp/$$tgz); \
		[ "$$want" = "$$got" ] || { echo "checksum mismatch for $$tgz"; exit 1; }; \
		tar -xzf "$$tmp/$$tgz" -C "$$tmp"; \
		mv "$$tmp/$$os-$$arch/helm" "$(HELM)-$(HELM_VERSION)"; \
		rm -rf "$$tmp"; \
	}
	@ln -sf "$(HELM)-$(HELM_VERSION)" "$(HELM)"

.PHONY: kubectl
kubectl: $(KUBECTL) ## Download kubectl locally if necessary (checksum verified).
$(KUBECTL): $(LOCALBIN)
	@[ -f "$(KUBECTL)-$(KUBECTL_VERSION)" ] || { \
		set -e; \
		url=https://dl.k8s.io/release/$(KUBECTL_VERSION)/bin/$$(go env GOOS)/$$(go env GOARCH)/kubectl; \
		echo "Downloading $$url"; \
		curl -fsSL --retry 5 --retry-all-errors -o "$(KUBECTL)-$(KUBECTL_VERSION)" "$$url"; \
		want=$$(curl -fsSL --retry 5 --retry-all-errors "$$url.sha256"); \
		got=$(call sha256of,$(KUBECTL)-$(KUBECTL_VERSION)); \
		[ "$$want" = "$$got" ] || { echo "checksum mismatch for kubectl"; exit 1; }; \
		chmod +x "$(KUBECTL)-$(KUBECTL_VERSION)"; \
	}
	@ln -sf "$(KUBECTL)-$(KUBECTL_VERSION)" "$(KUBECTL)"

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS) ## Download crd-ref-docs locally if necessary (checksum verified).
$(CRD_REF_DOCS): $(LOCALBIN)
	@sum=$$(GOFLAGS=-mod=mod go mod download -json github.com/elastic/crd-ref-docs@$(CRD_REF_DOCS_VERSION) | sed -n 's/^[[:space:]]*"Sum": "\(.*\)",$$/\1/p'); \
	[ "$$sum" = "$(CRD_REF_DOCS_SUM)" ] || { echo "crd-ref-docs $(CRD_REF_DOCS_VERSION): module hash $$sum, want $(CRD_REF_DOCS_SUM)" >&2; exit 1; }
	$(call go-install-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs,$(CRD_REF_DOCS_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint golangci-lint-go-matches
golangci-lint: golangci-lint-go-matches $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
# golangci-lint reads the standard library's export data, so a binary built by an older Go cannot type-check
# code built by a newer one ("export data version 5 is greater than maximum supported version 4"). After a
# toolchain bump the cached binary is thrown away and built again, the custom build (plugins) with the same Go.
golangci-lint-go-matches:
	@f="$(GOLANGCI_LINT)"; \
	if [ -f "$$f" ] && [ "$$(go version "$$f" | awk '{print $$2}')" != "$$(go env GOVERSION)" ]; then \
	  echo "golangci-lint was built with another Go than $$(go env GOVERSION); building it again"; \
	  rm -f "$$f" "$$f-$(GOLANGCI_LINT_VERSION)"; \
	fi
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		GOTOOLCHAIN="$$(go env GOVERSION)" $(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
