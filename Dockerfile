# Multi-stage build: compile a static binary, ship it on distroless.
FROM golang:1.26 AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go driver, so CGO can stay off -> fully static binary that runs on scratch/distroless.
# TARGETOS/TARGETARCH are set per-platform by buildx for multi-arch release
# builds; under a plain `docker build` they are empty and Go targets the build
# container's own platform. No --platform=$BUILDPLATFORM pin on the build
# stage: it would break the classic (non-BuildKit) builder some operators
# still run, so release builds emulate instead (QEMU in the workflow).
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w -X main.version=${VERSION}" -o /sqlpod .

# distroless/static:nonroot — no shell, no package manager, runs as uid 65532.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /sqlpod /sqlpod
# Idle by default so the pod stays warm; queries are run via `kubectl exec /sqlpod ...`.
ENTRYPOINT ["/sqlpod", "idle"]
