# kista. Every target runs with GOWORK=off, so a parent go.work cannot interfere.
export GOWORK := off

E2E_BUILD ?= $(CURDIR)/e2e/.build

.PHONY: build reserved test test-db test-db-down test-s3 test-s3-down test-azurite test-azurite-down lint e2e-duckdb e2e-runner e2e-build e2e

build:
	go build -o bin/kista ./cmd/kista

test:
	go test ./...

# The reserved extension names (spec 0008), aliases and DuckDB's keys (spec 0009) from DuckDB at e2e/DUCKDB_PIN and community-extensions at
# internal/reserved/COMMUNITY_PIN; CI checks the committed lists are current.
reserved:
	./internal/reserved/generate.sh

# PostgreSQL 17 and SQL Server 2022 for the store suite (Docker); then run make test with:
#   KISTA_TEST_POSTGRES=postgres://postgres@127.0.0.1:56432/postgres?sslmode=disable KISTA_TEST_POSTGRES_PASSWORD=kista-test
#   KISTA_TEST_SQLSERVER=sqlserver://sa@127.0.0.1:52433?encrypt=disable KISTA_TEST_SQLSERVER_PASSWORD='Kista-Test-2026!'
test-db:
	docker compose -f internal/store/testdb/compose.yaml -p kista-test up -d --wait

test-db-down:
	docker compose -f internal/store/testdb/compose.yaml -p kista-test down

# An S3-compatible server for the blob suite (SeaweedFS, pinned by digest: MinIO no longer publishes
# images); then run make test with:
#   KISTA_TEST_S3=http://kista-test:kista-test-secret@127.0.0.1:58333/kista-test
SEAWEEDFS := chrislusf/seaweedfs:4.48@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d
# A registry that times out once should not fail a run: pull with retries first.
pull = for i in 1 2 3 4; do docker pull -q $(1) >/dev/null && break; [ $$i = 4 ] && exit 1; sleep $$((i * 10)); done

test-s3:
	-docker rm -f kista-test-seaweedfs >/dev/null 2>&1
	@$(call pull,$(SEAWEEDFS))
	docker run -d --name kista-test-seaweedfs -p 127.0.0.1:58333:8333 \
		-v $(CURDIR)/internal/blob/s3/testdata/seaweedfs-s3.json:/etc/kista-s3.json:ro \
		$(SEAWEEDFS) server -s3 -s3.config=/etc/kista-s3.json -dir=/data -ip.bind=0.0.0.0
	@for i in $$(seq 1 60); do \
		case $$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:58333/) in 200|403) exit 0;; esac; sleep 1; done; \
		docker logs kista-test-seaweedfs; exit 1

test-s3-down:
	docker rm -f kista-test-seaweedfs

# Azurite (the Azure Storage emulator) for the azureblob suite, with its well-known development
# account; then run make test with:
#   KISTA_TEST_AZUREBLOB=http://127.0.0.1:50000/devstoreaccount1
AZURITE := mcr.microsoft.com/azure-storage/azurite:3.37.0@sha256:830430c1da1a2d537e08f3e6764dd1f5ae00cf0346bcaf625b968ec3f0971fd5
test-azurite:
	-docker rm -f kista-test-azurite >/dev/null 2>&1
	@$(call pull,$(AZURITE))
	docker run -d --name kista-test-azurite -p 127.0.0.1:50000:10000 $(AZURITE) \
		azurite-blob --blobHost 0.0.0.0 --skipApiVersionCheck --loose --inMemoryPersistence
	@for i in $$(seq 1 60); do \
		case $$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:50000/devstoreaccount1?comp=list) in 200|403) exit 0;; esac; sleep 1; done; \
		docker logs kista-test-azurite; exit 1

test-azurite-down:
	docker rm -f kista-test-azurite

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
