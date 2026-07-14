# syntax=docker/dockerfile:1.7
#
# Docker build for the VoiceBlender contact-centre example.
# Self-contained: the build context is this directory, and the SDK is pulled
# from the public Go module proxy. Each example has its own Dockerfile — see
# Dockerfile.pbx for the PBX.
#
#   docker build -t contact-centre .
#
# Run:
#
#   docker run --rm -p 8090:8090 \
#     -e VOICEBLENDER_URL=http://host.docker.internal:8080/v1 \
#     -e SUPERVISOR_PASSWORD=letmein \
#     -e AGENT_PASSWORD=letmein \
#     contact-centre

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
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/contact-centre ./cmd/contact-centre

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
COPY --from=builder /out/contact-centre /app/contact-centre
COPY cmd/contact-centre/assets/ /app/cmd/contact-centre/assets/
RUN chown -R cc:cc /app

USER cc
EXPOSE 8090

# tini reaps zombies and forwards signals.
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/app/contact-centre"]
