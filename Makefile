# Backend developer and CI entry points. Generators are pinned in backend/tools/go.mod and run
# with `go tool`, so nothing has to be installed globally (golangci-lint excepted, see lint).
ROOT    := $(abspath .)
BACKEND := $(ROOT)/backend
TOOLS   := go -C $(BACKEND)/tools tool

VERSION ?= dev
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# Paths written by `generate`; check-generated fails if they drift from what is committed.
GENERATED := backend/internal/api/gen backend/internal/auth/authdb backend/internal/tenants/tenantsdb backend/internal/audit/auditdb \
	backend/internal/scheduler/schedulerdb backend/internal/monitors/monitorsdb backend/internal/incidents/incidentsdb backend/internal/results/resultsdb \
	backend/internal/notify/notifydb

.PHONY: generate check-generated lint test build tidy

generate:
	$(TOOLS) sqlc generate -f $(ROOT)/sqlc.yaml
	$(TOOLS) oapi-codegen -config $(BACKEND)/internal/api/gen/oapi-codegen.yaml \
		-o $(BACKEND)/internal/api/gen/api.gen.go $(ROOT)/api/openapi.yaml

check-generated: generate
	@if [ -n "$$(git status --porcelain -- $(GENERATED))" ]; then \
		echo "generated code is out of date; run 'make generate' and commit:"; \
		git status --porcelain -- $(GENERATED); \
		git --no-pager diff --stat -- $(GENERATED); \
		exit 1; \
	fi

# Integration tests run when TEST_DATABASE_ADMIN_URL points at a scratch Postgres superuser.
test:
	cd $(BACKEND) && go test -race -count=1 ./...

lint:
	cd $(BACKEND) && golangci-lint run ./...

build:
	cd $(BACKEND) && CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ \
		./cmd/api ./cmd/worker ./cmd/jobs ./cmd/migrate ./cmd/maiakctl

tidy:
	cd $(BACKEND) && go mod tidy
	cd $(BACKEND)/tools && go mod tidy
