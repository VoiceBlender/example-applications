# syntax=docker/dockerfile:1.7
#
# Multi-stage Docker build for the VoiceBlender example applications.
# Self-contained: the build context is this directory, and the SDK is
# pulled from the public Go module proxy.
#
#   docker build -t cc-example .
#
# Run:
#
#   docker run --rm -p 8090:8090 \
#     -e VOICEBLENDER_URL=http://host.docker.internal:8080/v1 \
#     -e SUPERVISOR_PASSWORD=letmein \
#     -e AGENT_PASSWORD=letmein \
#     cc-example
#
# Run a different binary (once more land under cmd/):
#
#   docker run --rm cc-example /app/<other-binary>

ARG GO_VERSION=1.24
ARG ALPINE_VERSION=3.20

# ---- builder ---------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Pre-fetch modules in their own layer so source changes don't bust the
# dependency cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Now copy the full source.
COPY . .

# Static, CGO-disabled build so the binary runs on a minimal base.
# Builds every command under cmd/, so adding a new example is just
# `mkdir cmd/<name>/ && rebuild` — no Dockerfile edit needed.
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out && \
    for cmd in cmd/*/; do \
        name=$(basename "$cmd"); \
        echo "==> building $name"; \
        go build -ldflags="-s -w" -o "/out/$name" "./$cmd"; \
    done

# ---- runtime ---------------------------------------------------------------
FROM alpine:${ALPINE_VERSION}

# ca-certificates: the example doesn't hit HTTPS itself today, but
# leaving the bundle in keeps a future TLS dependency from silently
# failing. tini lets the container handle SIGTERM cleanly so
# /api/calls/stream subscribers get a tidy disconnect on `docker stop`.
RUN apk add --no-cache ca-certificates tini && \
    adduser -D -u 10001 -g '' cc

# The binary references `cmd/contact-centre/assets/` relatively. Layout
# WORKDIR + COPY so that path resolves under the runtime root.
WORKDIR /app
COPY --from=builder /out/ /app/
COPY cmd/contact-centre/assets/ /app/cmd/contact-centre/assets/
RUN chown -R cc:cc /app

USER cc
EXPOSE 8090

# tini reaps zombies and forwards signals. The default command runs
# the contact-centre; override to launch another example binary.
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/app/contact-centre"]
