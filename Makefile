PYTHON ?= python3
TOOLS = $(PYTHON) tools/bootstrap.py
GO = $(TOOLS) exec go --
BAZEL = $(TOOLS) exec bazelisk --
PM_TOOLS_CACHE ?= $(CURDIR)/.cache/pm-tools
export PM_TOOLS_CACHE

.PHONY: all hooks acceptance build build-api-generator generate check-generated test test-tools test-native test-setup test-docs vet boundaries bazel-build bazel-test gazelle benchmark check
.PHONY: browser-deps build-browser generate-browser check-generated-browser

all: build

# Install the git hooks for this clone: pre-commit, commit-msg and pre-push.
hooks:
	git config core.hooksPath scripts/dev/git-hooks

# The Gherkin acceptance suite. CI runs it with the dependency stack.
acceptance:
	CGO_ENABLED=1 $(GO) test -count=1 ./tests/acceptance/...

build: build-api-generator
	mkdir -p .cache/bin
	CGO_ENABLED=1 $(GO) build -o .cache/bin/nativeprobe ./build/nativeprobe
	CGO_ENABLED=0 $(GO) build -o .cache/bin/pm-server ./cmd/pm-server
	CGO_ENABLED=0 $(GO) build -o .cache/bin/pm-worker ./cmd/pm-worker
	CGO_ENABLED=0 $(GO) build -o .cache/bin/pm ./cmd/pm
	CGO_ENABLED=0 $(GO) build -o .cache/bin/pmgen ./cmd/pmgen
	CGO_ENABLED=0 $(GO) -C tools build -o ../.cache/bin/pm-dependency-check ./cmd/pm-dependency-check
	CGO_ENABLED=0 $(GO) -C tools build -o ../.cache/bin/oapi-codegen github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
	CGO_ENABLED=0 $(GO) -C tools build -o ../.cache/bin/staticcheck honnef.co/go/tools/cmd/staticcheck
	CGO_ENABLED=0 $(GO) -C tools build -o ../.cache/bin/govulncheck golang.org/x/vuln/cmd/govulncheck

build-api-generator:
	mkdir -p .cache/bin
	CGO_ENABLED=0 $(GO) -C tools build -o ../.cache/bin/pm-api-generate ./cmd/pm-api-generate

browser-deps:
	PYTHON="$(PYTHON)" sh scripts/browser.sh install

build-browser: browser-deps
	PYTHON="$(PYTHON)" sh scripts/browser.sh build

generate-browser: browser-deps
	PYTHON="$(PYTHON)" sh scripts/browser.sh generate

check-generated-browser: browser-deps
	PYTHON="$(PYTHON)" sh scripts/browser.sh check

generate: build-api-generator
	sh scripts/generate.sh
	$(MAKE) generate-browser

check-generated: build-api-generator
	sh scripts/generate.sh --check
	$(MAKE) check-generated-browser

test: test-tools test-native

test-tools:
	CGO_ENABLED=0 PM_REPO_ROOT="$(CURDIR)" $(GO) -C tools test . -run '^TestResolvedDependencyPolicy$$' -count=1 -timeout 5m

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

boundaries:
	sh scripts/check-boundaries.sh

bazel-build: build-browser
	$(BAZEL) --batch build //...

bazel-test: build-browser
	$(BAZEL) --batch test //... --test_output=errors --cache_test_results=no

gazelle:
	$(BAZEL) --batch run //:gazelle

benchmark:
	$(PYTHON) scripts/ci/benchmark.py

check: test-setup test-docs build check-generated test vet boundaries bazel-build bazel-test
