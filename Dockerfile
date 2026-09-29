# syntax=docker/dockerfile:1
# Hardy RMM - server image (API + dashboard + agent binaries for every platform)

# ---- dashboard ----
FROM node:22-alpine AS web
WORKDIR /web
COPY web/ ./
# A prebuilt dist/ ships with the repo; rebuild only if it is missing.
RUN if [ ! -f dist/index.html ]; then npm install --no-audit --no-fund && npm run build; fi

# ---- Go: server + cross-compiled agents ----
FROM golang:1.24-alpine AS go
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS="-trimpath -mod=mod"
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
# -mod=mod fills in any go.sum entries that are missing (verified against sum.golang.org).
RUN go mod download && go build ./... || (echo "regenerating go.sum" && rm -f go.sum && go mod tidy && go build ./...)
ARG VERSION=0.1.0
ENV LDFLAGS="-s -w -X github.com/hardynetworks/hardy-rmm/internal/proto.Version=${VERSION}"
RUN go build -ldflags "$LDFLAGS" -o /out/hardy-server ./cmd/hardy-server
RUN set -e; mkdir -p /out/agents; \
    for t in windows/amd64 windows/arm64 linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64; do \
      os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=".exe"; \
      echo "building agent $os/$arch"; \
      GOOS=$os GOARCH=$arch GOARM=7 go build -ldflags "$LDFLAGS" -o /out/agents/hardy-agent-$os-$arch$ext ./cmd/hardy-agent; \
    done

# ---- runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 hardy
WORKDIR /app
COPY --from=go /out/hardy-server /app/hardy-server
COPY --from=go /out/agents /app/agents
COPY --from=web /web/dist /app/web
ENV LISTEN=:8080 WEB_DIR=/app/web AGENT_DIR=/app/agents
USER hardy
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["/app/hardy-server"]
