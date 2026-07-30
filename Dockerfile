# Multi-stage build: compile a static binary, ship it on distroless.
FROM golang:1.26 AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go driver, so CGO can stay off -> fully static binary that runs on scratch/distroless.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /sqlpod .

# distroless/static:nonroot — no shell, no package manager, runs as uid 65532.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /sqlpod /sqlpod
# Idle by default so the pod stays warm; queries are run via `kubectl exec /sqlpod ...`.
ENTRYPOINT ["/sqlpod", "idle"]
