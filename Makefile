# kista. Every target runs with GOWORK=off, so a parent go.work cannot interfere.
export GOWORK := off

E2E_BUILD ?= $(CURDIR)/e2e/.build

.PHONY: build test lint e2e-duckdb e2e-runner e2e-build e2e

build:
	go build -o bin/kista ./cmd/kista

test:
	go test ./...

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
