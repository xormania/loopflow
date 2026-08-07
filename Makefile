# Local development commands. AGENTS.md is authoritative: `go vet ./...` and
# `go test ./...` must be green before every commit.

GO   ?= go
BIN  := $(CURDIR)/bin
SQLC := $(BIN)/sqlc

.PHONY: all build vet test check tools sqlc clean

all: check

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

# The pre-commit floor.
check: vet test build

# sqlc is a dev-time tool only. It is pinned in tools/go.mod — a separate
# module — so the application's own go.mod keeps exactly one direct
# dependency, modernc.org/sqlite (decisions.md D4). Nothing sqlc depends on
# can reach a shipped binary.
tools: $(SQLC)

$(SQLC):
	GOBIN=$(BIN) $(GO) -C tools install github.com/sqlc-dev/sqlc/cmd/sqlc

# Regenerate the typed query layer from internal/store/queries/*.sql against
# the schema in internal/store/migrations/. No queries exist yet; the query
# layer lands with the Phase 1 store implementation.
sqlc: $(SQLC)
	$(SQLC) generate

clean:
	rm -rf $(BIN)
