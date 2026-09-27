# syntax=docker/dockerfile:1

# Stage 1: build
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache dependencies: copy module manifests first, download, then copy source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary, stripped of symbols/debug info.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/anti-loop-proxy ./cmd/anti-loop-proxy

# Stage 2: runtime
# distroless/static:nonroot — minimal, no shell, runs as uid 65532 (nonroot).
# No HEALTHCHECK here: distroless has no shell/wget. The binary serves
# GET /healthz for orchestrator (k8s) probes; see docker-compose.yaml.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/anti-loop-proxy /usr/local/bin/anti-loop-proxy

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/anti-loop-proxy"]
