# kista. Every target runs with GOWORK=off, so a parent go.work cannot interfere.
export GOWORK := off

E2E_BUILD ?= $(CURDIR)/e2e/.build

.PHONY: build image docs console console-test console-e2e reserved test test-db test-db-down test-s3 test-s3-down test-azurite test-azurite-down test-moto test-moto-down test-gcs test-gcs-down lint e2e-duckdb e2e-runner e2e-build e2e sdk-test

build:
	go build -o bin/kista ./cmd/kista

# The image (spec 0013) for this machine's platform, as kista:dev, then its smoke test: uid 65532, a
# read-only root filesystem, /readyz on the probe listener. Needs Docker with buildx.
image:
	docker buildx build --build-arg VERSION=dev --provenance=false --load -t kista:dev .
	deploy/docker/smoke.sh kista:dev dev

# The administration console (spec 0015), embedded by the next make build; without it kista serves a
# placeholder at /ui/. Needs Node 22.
console:
	cd web/console && npm ci && npm run build

console-test:
	cd web/console && npm run typecheck && npm test && npm run licenses

# The console end to end: Keycloak in Docker, kista serve, Playwright (Chromium: npx playwright
# install chromium in web/console once). Run make console first.
console-e2e:
	web/console/e2e/run.sh

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

# moto, an AWS emulator, for the awskms source (spec 0004 phase 3; pinned by digest); then run make
# test with KISTA_TEST_MOTO=http://127.0.0.1:55000. It hashes a DIGEST message again, so the known
# answer is tested against a fake (see the spec).
MOTO := motoserver/moto:5.1.22@sha256:1e3802c95726373544967b428201c548f0247c15b00db2d96a5ba0a77d8643b8

test-moto:
	-docker rm -f kista-test-moto >/dev/null 2>&1
	@$(call pull,$(MOTO))
	docker run -d --name kista-test-moto -p 127.0.0.1:55000:5000 $(MOTO)
	@for i in $$(seq 1 60); do curl -sf http://127.0.0.1:55000/moto-api/ >/dev/null && exit 0; sleep 1; done; \
		docker logs kista-test-moto; exit 1

test-moto-down:
	docker rm -f kista-test-moto

# fake-gcs-server for the gcs store (spec 0005 phase 3b; pinned by digest); then run make test with
# KISTA_TEST_GCS=http://127.0.0.1:54443.
FAKEGCS := fsouza/fake-gcs-server:1.52.2@sha256:d47b4cf8b87006cab8fbbecfa5f06a2a3c5722e464abddc0d107729663d40ec4

test-gcs:
	-docker rm -f kista-test-gcs >/dev/null 2>&1
	@$(call pull,$(FAKEGCS))
	docker run -d --name kista-test-gcs -p 127.0.0.1:54443:4443 $(FAKEGCS) \
		-scheme http -port 4443 -public-host 127.0.0.1:54443 -external-url http://127.0.0.1:54443
	@for i in $$(seq 1 60); do curl -sf http://127.0.0.1:54443/storage/v1/b >/dev/null && exit 0; sleep 1; done; \
		docker logs kista-test-gcs; exit 1

test-gcs-down:
	docker rm -f kista-test-gcs

lint:
	go vet ./...
	golangci-lint run ./...

# DuckDB at e2e/DUCKDB_PIN with the e2e extensions (30-45 min cold, minutes with ccache).
e2e-duckdb:
	./e2e/build-duckdb.sh $(E2E_BUILD)

e2e-runner:
	./e2e/build-runner.sh $(E2E_BUILD)

e2e-build: e2e-duckdb e2e-runner

# The C++ SDK (sdk/cpp, spec 0012) against DuckDB at the pin (make e2e-duckdb first): the licence
# vectors through the C++ verifier.
SDK_BUILD ?= $(E2E_BUILD)/sdk-build
sdk-test:
	cmake -S sdk/cpp -B $(SDK_BUILD) -G Ninja -DKISTA_DUCKDB_DEPS=$(abspath $(E2E_BUILD))/sdk
	ninja -C $(SDK_BUILD)
	ctest --test-dir $(SDK_BUILD) --output-on-failure

e2e:
	KISTA_E2E_DUCKDB=$(E2E_BUILD) go test -count=1 ./e2e/...

# The docs site (website/, Docusaurus): built as the docs-build workflow does; `cd website && npm start`
# serves it with live reload. Needs Node 22.
docs:
	cd website && npm ci && npx docusaurus build
