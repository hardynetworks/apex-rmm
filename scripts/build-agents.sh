#!/bin/sh
# Cross-compiles the agent for every platform: build-agents.sh <out dir> "<ldflags>"
#
# The linux/amd64 and linux/arm64 agents load the OpenH264 encoder at run time, which makes them
# dynamically linked against the system's C library. The ELF interpreter is set explicitly to
# glibc's loader: left to itself, Go's linker uses musl's loader when the build runs on Alpine
# (as in the Docker image), and such a binary won't start on Ubuntu/Debian ("not found").
# A fully static "-static" build (no H.264) is made too, for systems without glibc (Alpine).
set -e
OUT="$1"
LDFLAGS="$2"
mkdir -p "$OUT"
for t in windows/amd64 windows/arm64 linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64; do
  os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=".exe"
  ldso=""
  case "$t" in
    linux/amd64) ldso=/lib64/ld-linux-x86-64.so.2 ;;
    linux/arm64) ldso=/lib/ld-linux-aarch64.so.1 ;;
  esac
  echo "building agent $os/$arch"
  if [ -n "$ldso" ]; then
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS -I $ldso" -o "$OUT/apex-agent-$os-$arch" ./cmd/apex-agent
    echo "building agent $os/$arch-static"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -tags nodlopen -ldflags "$LDFLAGS" -o "$OUT/apex-agent-$os-$arch-static" ./cmd/apex-agent
  else
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=7 go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/apex-agent-$os-$arch$ext" ./cmd/apex-agent
  fi
done
