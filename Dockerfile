# kista's image (spec 0013): one static binary with the console embedded, in a distroless image, as
# uid and gid 65532. buildx builds linux/amd64 and linux/arm64; Go cross-compiles, so no emulation.
# Every base image is pinned by digest (Dependabot updates them).

# The console: files, not a binary, so built once on the build's platform. The same gates as CI:
# the licence check and the audit, then the build.
FROM --platform=$BUILDPLATFORM node:26-alpine@sha256:143494b1da2945f061539253adc65e4f1569ddf07da2d384c022c791a9d90a4a AS console
# its packages end up in the binary: the SBOM scans this stage too
ARG BUILDKIT_SBOM_SCAN_STAGE=true
WORKDIR /web
COPY web/console/package.json web/console/package-lock.json ./
# no install scripts: nothing a dependency ships runs before the gates
RUN npm ci --no-fund --ignore-scripts
COPY web/console ./
# then only what the binary embeds stays, so the SBOM lists the shipped packages, not the tooling
RUN npm run licenses && npm audit --omit=dev --audit-level=high && npm run build && npm prune --omit=dev

FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS build
ARG TARGETOS TARGETARCH VERSION=dev
ENV GOWORK=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web/console/console.go ./web/console/
COPY --from=console /web/dist ./web/console/dist
RUN test -f web/console/dist/app/index.html && test -f web/console/dist/mfe/kista.js
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/hugr-lab/duckdb-extension-repository/internal/app.Version=$VERSION" \
      -o /out/kista ./cmd/kista \
 && mkdir -p /out/var/lib/kista/tmp

# The release binaries (release.yml): the same build for linux and darwin, written out with
# --target binary --output type=local.
FROM scratch AS binary
COPY --from=build /out/kista /kista

# The image: the last stage, the default target.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/kista /kista
# the state directory, writable by kista when nothing is mounted there (a read-only root filesystem
# mounts a volume or a tmpfs instead)
COPY --from=build --chown=65532:65532 /out/var/lib/kista /var/lib/kista
USER 65532:65532
ARG VERSION=dev
LABEL org.opencontainers.image.title="kista" \
      org.opencontainers.image.description="A trusted repository for DuckDB extensions" \
      org.opencontainers.image.source="https://github.com/hugr-lab/duckdb-extension-repository" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="$VERSION"
# SQLite's temporary files go to the writable volume, not to /tmp on the root filesystem
ENV TMPDIR=/var/lib/kista/tmp
ENTRYPOINT ["/kista"]
CMD ["serve", "-config", "/etc/kista/kista.yaml"]
