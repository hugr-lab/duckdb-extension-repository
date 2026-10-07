# kista. Every target runs with GOWORK=off, so a parent go.work cannot interfere.
export GOWORK := off

E2E_BUILD ?= $(CURDIR)/e2e/.build

.PHONY: build test test-db test-db-down lint e2e-duckdb e2e-runner e2e-build e2e

build:
	go build -o bin/kista ./cmd/kista

test:
	go test ./...

# PostgreSQL 17 and SQL Server 2022 for the store suite (Docker); then run make test with:
#   KISTA_TEST_POSTGRES=postgres://postgres@127.0.0.1:56432/postgres?sslmode=disable KISTA_TEST_POSTGRES_PASSWORD=kista-test
#   KISTA_TEST_SQLSERVER=sqlserver://sa@127.0.0.1:52433?encrypt=disable KISTA_TEST_SQLSERVER_PASSWORD='Kista-Test-2026!'
test-db:
	docker compose -f internal/store/testdb/compose.yaml -p kista-test up -d --wait

test-db-down:
	docker compose -f internal/store/testdb/compose.yaml -p kista-test down

lint:
	go vet ./...
	golangci-lint run ./...

# DuckDB at e2e/DUCKDB_PIN with the e2e extensions (30-45 min cold, minutes with ccache).
e2e-duckdb:
	./e2e/build-duckdb.sh $(E2E_BUILD)

e2e-runner:
	./e2e/build-runner.sh $(E2E_BUILD)

e2e-build: e2e-duckdb e2e-runner

e2e:
	KISTA_E2E_DUCKDB=$(E2E_BUILD) go test -count=1 ./e2e/...
