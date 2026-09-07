# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

GO ?= go
GOFMT ?= gofmt
HELM ?= helm
ENVTEST_K8S_VERSION ?= 1.31.0
# Immutable pin of the release-0.19 tree (commit 5fe7bb3e). The upstream
# `release-0.19` branch is mutable, so it cannot be trusted as a build input.
SETUP_ENVTEST_VERSION ?= v0.0.0-20250308055145-5fe7bb3edc86
SETUP_ENVTEST ?= $(shell $(GO) env GOPATH)/bin/setup-envtest

.PHONY: all fmt-check vet build test test-envtest ci operator-build admission-build operator-image admission-image helm-lint helm-template helm-digest-check helm-scheduling-check helm-admission-check helm-check print-setup-envtest-version clean

all: build

fmt-check:
	@unformatted="$$($(GOFMT) -l $$($(GO) list -f '{{.Dir}}' ./...))"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

test:
	$(GO) test -race ./...

$(SETUP_ENVTEST):
	$(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

print-setup-envtest-version:
	@echo $(SETUP_ENVTEST_VERSION)

test-envtest: $(SETUP_ENVTEST)
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" $(GO) test -race ./...

ci: fmt-check vet build test-envtest helm-check

operator-build:
	$(GO) build -o bin/operator ./cmd/manager

admission-build:
	$(GO) build -o bin/admission ./cmd/admission

operator-image:
	docker build -t ghcr.io/microsoft/brewlet-operator:dev -f Dockerfile ..

admission-image:
	docker build -t ghcr.io/microsoft/brewlet-admission:dev -f Dockerfile .. --build-arg CMD=admission

# The chart deliberately ships NO default JDK sources: Brewlet has no built-in
# runtime catalog, and a chart-pinned digest could never receive a CVE patch
# (§5.2/§5.3). Every render therefore has to name one, exactly as an operator
# would. Kept in one variable so the rendering targets cannot drift.
HELM_MIN_PROFILE := --set provisioner.pools="{general}" \
	--set provisioner.jdks[0].distribution=temurin \
	--set provisioner.jdks[0].feature=21 \
	--set provisioner.jdks[0].source.image=docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b \
	--set provisioner.jdks[0].source.javaHome=/opt/java/openjdk

helm-lint:
	$(HELM) lint charts/brewlet $(HELM_MIN_PROFILE)

helm-template:
	$(HELM) template brewlet charts/brewlet $(HELM_MIN_PROFILE) >/dev/null

# Released charts record image digests; verify both resolution modes render and
# that a malformed digest fails closed rather than silently producing a tag.
helm-digest-check:
	@digest="sha256:$$(printf 'a%.0s' $$(seq 1 64))"; \
	rendered="$$($(HELM) template brewlet charts/brewlet \
		$(HELM_MIN_PROFILE) \
		--set images.digests.operator="$$digest" \
		--set images.digests.provisioner="$$digest" \
		--set images.digests.admission="$$digest")"; \
	for component in operator node-provisioner admission; do \
		printf '%s\n' "$$rendered" \
			| grep -Fq "ghcr.io/microsoft/brewlet-$$component@$$digest" || { \
				echo "chart did not render a digest reference for $$component"; \
				exit 1; \
			}; \
	done; \
	if $(HELM) template brewlet charts/brewlet \
		$(HELM_MIN_PROFILE) \
		--set images.digests.operator=sha256:deadbeef >/dev/null 2>&1; then \
		echo "chart accepted a malformed image digest"; \
		exit 1; \
	fi

# Remediation test for SECURITY-REVIEW.md finding 10: the privileged provisioner
# must never be rendered as an every-node, every-taint, cluster-wide-RBAC
# install.
helm-scheduling-check:
	@if $(HELM) template brewlet charts/brewlet >/dev/null 2>&1; then \
		echo "default values rendered a profile without provisioner.pools"; \
		exit 1; \
	fi
	@if $(HELM) template brewlet charts/brewlet --set provisioner.pools="{general}" >/dev/null 2>&1; then \
		echo "default values rendered a profile without explicit provisioner.jdks"; \
		exit 1; \
	fi
	@if $(HELM) template brewlet charts/brewlet --set provisioner.pools="{general}" 2>&1 \
		| grep -q 'provisioner.jdks is required'; then :; else \
		echo "empty provisioner.jdks did not fail with the documented message"; \
		exit 1; \
	fi
	@if $(HELM) template brewlet charts/brewlet $(HELM_MIN_PROFILE) 2>/dev/null \
		| grep -Eq 'image: .*@sha256:85f00967'; then :; else \
		echo "explicit provisioner.jdks did not reach the rendered NodeProfile"; \
		exit 1; \
	fi
	@rendered="$$($(HELM) template brewlet charts/brewlet $(HELM_MIN_PROFILE))"; \
	if printf '%s\n' "$$rendered" | grep -q 'includeControlPlane'; then \
		echo "default render opted into control-plane nodes"; exit 1; \
	fi; \
	if printf '%s\n' "$$rendered" | grep -Eq 'operator:[[:space:]]*"?Exists'; then \
		echo "chart rendered a blanket Exists toleration"; exit 1; \
	fi; \
	if ! printf '%s\n' "$$rendered" | grep -q '^      - "general"$$'; then \
		echo "default profile did not render explicit pool names"; exit 1; \
	fi
	@ops="$$($(HELM) template brewlet charts/brewlet $(HELM_MIN_PROFILE) -s templates/operator.yaml)"; \
	if printf '%s\n' "$$ops" | sed -n '/^kind: ClusterRole$$/,/^---$$/p' | grep -q daemonsets; then \
		echo "operator ClusterRole still grants cluster-wide DaemonSet access"; exit 1; \
	fi; \
	if ! printf '%s\n' "$$ops" | sed -n '/^kind: Role$$/,/^---$$/p' | grep -q daemonsets; then \
		echo "operator Role does not grant namespaced DaemonSet access"; exit 1; \
	fi; \
	if ! printf '%s\n' "$$ops" | grep -q '^kind: RoleBinding$$'; then \
		echo "operator RoleBinding is missing"; exit 1; \
	fi
	@if ! diff -q charts/brewlet/crds/nodeprofile-crd.yaml deploy/nodeprofile-crd.yaml >/dev/null; then \
		echo "chart and deploy NodeProfile CRDs have diverged"; exit 1; \
	fi

# The pod webhook is cluster-wide on CREATE, so it must stay off kube-system by
# default, and the self-signed serving certificate must record when it is due for
# rotation (nothing renews it on its own).
helm-admission-check:
	@rendered="$$($(HELM) template brewlet charts/brewlet $(HELM_MIN_PROFILE))"; \
	if ! printf '%s\n' "$$rendered" | grep -q 'namespaceSelector'; then \
		echo "pod webhook rendered without a namespaceSelector"; exit 1; \
	fi; \
	if ! printf '%s\n' "$$rendered" | grep -q 'kube-system'; then \
		echo "pod webhook namespaceSelector does not exclude kube-system"; exit 1; \
	fi; \
	if ! printf '%s\n' "$$rendered" | grep -q 'brewlet.sh/cert-renew-after'; then \
		echo "self-signed certificate Secret records no renewal deadline"; exit 1; \
	fi
	@if $(HELM) template brewlet charts/brewlet $(HELM_MIN_PROFILE) \
		--set admission.selfSigned.renewBeforeDays=90 >/dev/null 2>&1; then \
		echo "chart accepted renewBeforeDays >= validityDays"; exit 1; \
	fi
	@if $(HELM) template brewlet charts/brewlet $(HELM_MIN_PROFILE) \
		--set admission.namespaceSelector=null >/dev/null 2>&1; then :; else \
		echo "chart could not render with an empty namespaceSelector"; exit 1; \
	fi

helm-check: helm-lint helm-template helm-digest-check helm-scheduling-check helm-admission-check

clean:
	rm -rf bin
