PYTHON ?= python3
TOOLS = $(PYTHON) tools/bootstrap.py
GO = $(TOOLS) exec go --
BAZEL = $(TOOLS) exec bazelisk --

.PHONY: all build build-api-generator build-analysis-tools format check-format compile generate check-generated test test-unit test-tools test-native test-setup test-docs vet staticcheck govulncheck boundaries bazel-build bazel-test gazelle benchmark check-fast check

all: build

build: build-api-generator build-analysis-tools
	mkdir -p .cache/bin
	CGO_ENABLED=1 $(GO) build -o .cache/bin/nativeprobe ./build/nativeprobe
	CGO_ENABLED=0 $(GO) -C tools build -o ../.cache/bin/pm-dependency-check ./cmd/pm-dependency-check
	CGO_ENABLED=0 $(GO) -C tools build -o ../.cache/bin/oapi-codegen github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen

build-api-generator:
	mkdir -p .cache/bin
	CGO_ENABLED=0 $(GO) -C tools build -o ../.cache/bin/pm-api-generate ./cmd/pm-api-generate

build-analysis-tools:
	mkdir -p .cache/bin
	CGO_ENABLED=0 $(GO) -C tools build -o ../.cache/bin/staticcheck honnef.co/go/tools/cmd/staticcheck
	CGO_ENABLED=0 $(GO) -C tools build -o ../.cache/bin/govulncheck golang.org/x/vuln/cmd/govulncheck

format:
	CGO_ENABLED=1 $(GO) fmt ./...
	CGO_ENABLED=0 $(GO) -C tools fmt ./...

check-format:
	@set -eu; \
	goroot="$$($(GO) env GOROOT)"; \
	unformatted="$$(git ls-files -z -- '*.go' | xargs -0 "$$goroot/bin/gofmt" -l)"; \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n' 'Go files require gofmt:' "$$unformatted" >&2; \
		exit 1; \
	fi

compile:
	CGO_ENABLED=1 $(GO) test -mod=readonly -run '^$$' -count=1 -timeout 10m ./...
	CGO_ENABLED=0 $(GO) -C tools test -mod=readonly -run '^$$' -count=1 -timeout 10m ./...

generate: build-api-generator
	sh scripts/generate.sh

check-generated: build-api-generator
	sh scripts/generate.sh --check

test: test-unit test-tools test-native

test-unit: build-api-generator
	CGO_ENABLED=1 $(GO) test -race -mod=readonly -count=1 -timeout 10m ./internal/api ./internal/contract

test-tools:
	CGO_ENABLED=0 PM_REPO_ROOT="$(CURDIR)" $(GO) -C tools test -mod=readonly -count=1 -timeout 5m ./...

test-native:
	CGO_ENABLED=1 $(GO) test ./build/nativeprobe -count=1 -timeout 3m

test-setup:
	$(PYTHON) -I -B scripts/ci/check-setup.py --root . --policy scripts/ci/setup-policy.json
	$(PYTHON) -I -B scripts/ci/test_setup.py
	$(PYTHON) -I -B scripts/ci/test_stack_fixture.py
	$(PYTHON) -I -B tools/test_bootstrap.py

test-docs:
	$(PYTHON) -B scripts/test_validate_architecture_docs.py
	$(PYTHON) -B scripts/validate_architecture_docs.py

vet:
	CGO_ENABLED=1 $(GO) vet ./...
	CGO_ENABLED=0 $(GO) -C tools vet ./...

staticcheck: build-analysis-tools
	@set -eu; \
	goroot="$$($(GO) env GOROOT)"; \
	PATH="$$goroot/bin:$$PATH" GOROOT="$$goroot" PM_GO="$$goroot/bin/go" GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=1 .cache/bin/staticcheck ./...
	@set -eu; \
	goroot="$$($(GO) env GOROOT)"; \
	cd tools; \
	PATH="$$goroot/bin:$$PATH" GOROOT="$$goroot" PM_GO="$$goroot/bin/go" GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=0 ../.cache/bin/staticcheck ./...

govulncheck: build-analysis-tools
	@set -eu; \
	goroot="$$($(GO) env GOROOT)"; \
	PATH="$$goroot/bin:$$PATH" GOROOT="$$goroot" PM_GO="$$goroot/bin/go" GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=1 .cache/bin/govulncheck ./...
	@set -eu; \
	goroot="$$($(GO) env GOROOT)"; \
	cd tools; \
	PATH="$$goroot/bin:$$PATH" GOROOT="$$goroot" PM_GO="$$goroot/bin/go" GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=0 ../.cache/bin/govulncheck ./...

boundaries:
	sh scripts/check-boundaries.sh

bazel-build:
	$(BAZEL) --batch build //...

bazel-test:
	$(BAZEL) --batch test //... --test_output=errors --cache_test_results=no

gazelle:
	$(BAZEL) --batch run //:gazelle

benchmark:
	$(PYTHON) scripts/ci/benchmark.py

check-fast: test-setup test-docs check-format build check-generated compile test vet staticcheck govulncheck boundaries

check: check-fast bazel-build bazel-test
