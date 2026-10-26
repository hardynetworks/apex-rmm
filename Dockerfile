# syntax=docker/dockerfile:1
# Apex RMM - server image (API + dashboard + agent binaries for every platform)

# ---- dashboard ----
# The dashboard and Go stages run on the build machine's own CPU (Go cross-compiles),
# so building the arm64 image doesn't emulate npm or the compiler.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /web
COPY web/ ./
RUN rm -rf dist node_modules && npm install --no-audit --no-fund && npm run build

# ---- Go: server + cross-compiled agents ----
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS go
ARG TARGETOS=linux TARGETARCH
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS="-trimpath -mod=mod"
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
# -mod=mod fills in any go.sum entries that are missing (verified against sum.golang.org).
RUN go mod download && go build ./... || (echo "regenerating go.sum" && rm -f go.sum && go mod tidy && go build ./...)
ARG VERSION=0.1.0
ENV LDFLAGS="-s -w -X github.com/hardynetworks/apex-rmm/internal/proto.Version=${VERSION}"
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "$LDFLAGS" -o /out/apex-server ./cmd/apex-server
COPY scripts/build-agents.sh ./scripts/
RUN sh scripts/build-agents.sh /out/agents "$LDFLAGS"

# ---- runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 apex
WORKDIR /app
COPY --from=go /out/apex-server /app/apex-server
COPY --from=go /out/agents /app/agents
COPY --from=web /web/dist /app/web
ENV LISTEN=:8080 WEB_DIR=/app/web AGENT_DIR=/app/agents
USER apex
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["/app/apex-server"]
