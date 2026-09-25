# workbox — developer ergonomics.
#
# `make check` runs all local validation without touching cloud infrastructure.
# tf-* targets wrap Terraform; only `tf-apply` mutates real resources.

SHELL := /bin/bash
.DEFAULT_GOAL := help

BINARY      := workbox
CMD         := ./cmd/workbox
INFRA       := infra
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -X main.Version=$(VERSION)

XDG_CONFIG_HOME ?= $(HOME)/.config
CONFIG_DIR      := $(XDG_CONFIG_HOME)/workbox
CONFIG_FILE     := $(CONFIG_DIR)/config.yaml

# Config path passed to Terraform (defaults to the canonical user config).
TF_CONFIG ?= $(CONFIG_FILE)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## ------------------------------------------------------------------ config ---

.PHONY: configure
configure: ## Create the user config from workbox.example.yaml (never overwrites)
	@mkdir -p "$(CONFIG_DIR)"
	@if [ -f "$(CONFIG_FILE)" ]; then \
		echo "config already exists: $(CONFIG_FILE) (left unchanged)"; \
	else \
		cp workbox.example.yaml "$(CONFIG_FILE)"; \
		echo "created $(CONFIG_FILE) — edit it with your project, key and tailnet"; \
	fi

## --------------------------------------------------------------------- go ---

.PHONY: build
build: ## Build the workbox binary into ./bin
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)
	@echo "built bin/$(BINARY) ($(VERSION))"

.PHONY: install
install: ## Install workbox into your Go bin directory
	go install -ldflags "$(LDFLAGS)" $(CMD)
	@dir="$$(go env GOBIN)"; [ -n "$$dir" ] || dir="$$(go env GOPATH)/bin"; \
		echo "installed: $$dir/$(BINARY)"; \
		case ":$$PATH:" in *":$$dir:"*) ;; *) echo "note: add $$dir to your PATH";; esac

.PHONY: test
test: ## Run the Go test suite
	go test ./...

.PHONY: fmt
fmt: ## Format Go code
	gofmt -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (enabled set in .golangci.yml)
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found; install v2:"; \
		echo "  curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b \$$(go env GOPATH)/bin v2.13.2"; \
		echo "  (or: brew install golangci-lint) — see https://golangci-lint.run/welcome/install/"; \
		exit 1; \
	}
	golangci-lint run ./...

.PHONY: gofmt-check
gofmt-check: ## Fail if any Go file is not gofmt-clean
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

.PHONY: sh-check
sh-check: ## Syntax-check the cloud-init template and the emitter it installs
	@bash -n $(INFRA)/cloud-init.sh.tftpl
	@# The emitter lives in a quoted heredoc, which bash -n treats as data, so
	@# extract and check it separately. ${...} parses as a parameter expansion,
	@# so no Terraform rendering is needed. An empty extraction means the
	@# heredoc moved or was renamed: fail rather than report a check that did
	@# not run (bash -n accepts empty input).
	@emitter="$$(sed -n "/<<'ACTIVITY'/,/^ACTIVITY$$/p" $(INFRA)/cloud-init.sh.tftpl 		| sed '1d;$$d')"; 	[ -n "$$emitter" ] || 		{ echo "sh-check: no ACTIVITY heredoc in cloud-init.sh.tftpl"; exit 1; }; 	printf '%s\n' "$$emitter" | bash -n /dev/stdin
	@echo "cloud-init.sh.tftpl: syntax OK (script and emitter)"

.PHONY: check
check: gofmt-check lint test build sh-check tf-fmt-check tf-validate ## All local validation (no cloud changes)
	@echo "all checks passed"

## -------------------------------------------------------------- terraform ---

.PHONY: tf-init
tf-init: ## terraform init (remote GCS state; needs infra/backend.hcl)
	@test -f $(INFRA)/backend.hcl || { \
	  echo "error: $(INFRA)/backend.hcl not found."; \
	  echo "Copy $(INFRA)/backend.example.hcl to $(INFRA)/backend.hcl and set your"; \
	  echo "state bucket. Create the bucket first — see docs/operations.md (Remote state)."; \
	  exit 1; }
	terraform -chdir=$(INFRA) init -backend-config=backend.hcl

.PHONY: tf-init-check
tf-init-check: ## terraform init without a backend (for CI/validate)
	terraform -chdir=$(INFRA) init -backend=false

.PHONY: tf-fmt
tf-fmt: ## Format Terraform files
	terraform -chdir=$(INFRA) fmt -recursive

.PHONY: tf-fmt-check
tf-fmt-check: ## Fail if Terraform is not fmt-clean
	terraform -chdir=$(INFRA) fmt -check -recursive

.PHONY: tf-validate
tf-validate: tf-init-check ## Validate the Terraform configuration
	terraform -chdir=$(INFRA) validate

.PHONY: tf-plan
tf-plan: ## Plan infrastructure changes (reads $(TF_CONFIG))
	terraform -chdir=$(INFRA) plan -var 'config_file=$(TF_CONFIG)'

.PHONY: tf-apply
tf-apply: ## Apply infrastructure changes (reads $(TF_CONFIG))
	terraform -chdir=$(INFRA) apply -var 'config_file=$(TF_CONFIG)'
