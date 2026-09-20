# Image ref used by docker-build; e2e overrides this (IMG=... make docker-build).
IMG ?= ghcr.io/isvaldi-consulting/tsidp-operator:dev

## Development

.PHONY: generate
generate: ## Generate deepcopy code from API types.
	go tool controller-gen object paths=./api/...

.PHONY: manifests
manifests: ## Generate the CRD manifest. RBAC has a single source of truth: charts/tsidp/templates/rbac.yaml (the +kubebuilder:rbac markers in the controller document the need).
	go tool controller-gen crd paths=./... output:crd:artifacts:config=config/crd/bases

.PHONY: sync-chart-crds
sync-chart-crds: manifests ## Copy generated CRDs into the Helm chart.
	cp config/crd/bases/*.yaml charts/tsidp/crds/

.PHONY: build
build: generate
	go build ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./...

.PHONY: lint
lint: vet
	gofmt -l . | (! grep .)

## Packaging

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: helm-lint
helm-lint:
	helm lint charts/tsidp
	helm template charts/tsidp >/dev/null
	helm template charts/tsidp --set 'operator.watchNamespaces={a-ns,b-ns}' >/dev/null

## Version pinning (see docs/COMPATIBILITY.md)

.PHONY: sync-tsidp-version
sync-tsidp-version: ## Propagate the hack/tsidp.Dockerfile pin into the chart and versions.yaml.
	hack/sync-tsidp-version.sh

## End-to-end (kind; requires TS_AUTHKEY)

.PHONY: e2e
e2e:
	hack/e2e.sh
