# syntax=docker/dockerfile:1.7
# One image with all backend binaries in /app; the chart picks the command per workload.
# Multi-arch: the Go toolchain runs on the build platform and cross-compiles (no emulation).

ARG GO_VERSION=1.25.5

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY backend/ ./
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/ ./cmd/api ./cmd/worker ./cmd/jobs ./cmd/migrate ./cmd/maiakctl

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /app/
USER 65532:65532
CMD ["/app/api"]
