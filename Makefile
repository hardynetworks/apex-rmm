# Apex RMM developer tasks
VERSION ?= 0.1.0
LDFLAGS := -s -w -X github.com/hardynetworks/apex-rmm/internal/proto.Version=$(VERSION)
PLATFORMS := windows/amd64 windows/arm64 linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64

.PHONY: all server agents web docker up clean

all: web server agents

server:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/apex-server ./cmd/apex-server

agents:
	sh scripts/build-agents.sh dist/agents "$(LDFLAGS)"

web:
	cd web && npm install && npm run build

docker:
	docker build -t apex-rmm:$(VERSION) -t apex-rmm:latest --build-arg VERSION=$(VERSION) .

up:
	docker compose up -d --build

clean:
	rm -rf dist
