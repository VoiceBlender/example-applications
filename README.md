# example-applications

Reference applications built on [VoiceBlender](../VoiceBlender) using the [voiceblender-go](../voiceblender-go) SDK. Each app is a self-contained Go binary under `cmd/`.

## Applications

| Name | Description |
|---|---|
| [contact-centre](./cmd/contact-centre/) | Complete inbound SIP contact centre in one binary. UK ringback → welcome TTS → per-caller waiting room with hold music and live queue-position announcements → one-click *Take call* on the agent dashboard → bridge with mute/hold/resume/hangup → live per-speaker transcription archived into the call log. Supervisor dashboard adds silent monitor (*Listen*), private side-channel (*Whisper*), and a rolling Service KPIs board (SL, ASA, AHT, Abandon, Longest Wait). Pluggable call-log backend (memory / Redis), optional static-password auth, configurable codec preference order. |
| [ivr](./cmd/ivr/) | Multi-department IVR. UK ringback → welcome TTS → DTMF main menu → routes the caller into a department room (sales / support / billing) with looping hold music and a repeating hold message, or hands off to a Deepgram AI voice agent on `0`. |
| [pbx](./cmd/pbx/) | **Multi-tenant SIP PBX** in one binary. Authenticated extensions (REGISTER + INVITE digest), register/IP trunks, extension↔extension and extension→external calls, a dial-by-extension IVR, and a **visual inbound dial-plan editor** (match / gather DTMF+speech / ext / ivr / forward / play / TTS / reject). **Browser WebRTC softphones** act as extra devices per extension — ring alongside the SIP phone, place calls, hold / blind-transfer (incl. "to my desk phone") / DND / mic-selector / ringtone / call-history + contacts. Handles inbound SIP REFER (accept → re-bridge), self-service tenant signup with a cross-tenant superadmin console, on-hold music, and Redis-persisted config/sessions. |
| [ptt](./cmd/ptt/) | **Browser push-to-talk** (walkie-talkie). Username-only login → create public or private rooms → hold a button (or Space) to talk over WebRTC. **Single-speaker floor control** (a second presser gets "busy") and **fully on-demand media**: the VoiceBlender room and every WebRTC leg are created on the press and torn down on the release, so nothing is allocated while a room is quiet. Private rooms use a shareable invite code/link; live presence + "who's talking"; Redis-persisted users/sessions/rooms. |

## Layout

```
example-applications/
├── go.mod
└── cmd/
    └── <app>/        # one directory per binary, with its own README and .env.example
```

## Running an app

From this directory:

```bash
go run ./cmd/<app>
```

See each app's own README for required configuration and prerequisites.

## Docker

Each example has its own multi-stage Dockerfile that produces a single self-contained image. The build context is just this directory — the SDK is pulled from the public Go module proxy, no workspace layout required. Every image is Alpine-based, statically links the binary (`CGO_ENABLED=0`), runs as a non-root user, and uses `tini` for clean signal handling.

- **contact-centre** — [`Dockerfile`](./Dockerfile) (listens on `:8090`):
  ```bash
  docker build -t cc-example .
  ```
- **pbx** — [`Dockerfile.pbx`](./Dockerfile.pbx) (needs Redis; listens on `:8091`):
  ```bash
  docker build -f Dockerfile.pbx -t pbx .
  ```

Typical run:

```bash
docker run --rm -p 8090:8090 \
  -e VOICEBLENDER_URL=http://host.docker.internal:8080/v1 \
  -e SUPERVISOR_PASSWORD=letmein \
  -e AGENT_PASSWORD=letmein \
  cc-example
```

Mount a custom hold-music MP3 without rebuilding:

```bash
-v /path/to/music.mp3:/app/cmd/contact-centre/assets/new_music.mp3:ro
```

### Docker Compose

A worked [`compose.yaml`](./compose.yaml) shows every environment variable the app accepts plus an opt-in Redis sidecar for the persistent call-log backend.

```bash
# Memory-backed call log (default):
docker compose up --build

# Redis-backed call log (brings up the sidecar):
CALL_LOG_BACKEND=redis docker compose --profile redis up --build
```

The compose file:

- Brings up three services connected on the default compose network: **caddy** (the only one with host ports — `80:80` and `443:443` by default, override with `CADDY_HTTP_PORT` / `CADDY_HTTPS_PORT` if those are taken), **contact-centre** (built from the local `Dockerfile`, internal-only), and **redis** (opt-in via the `redis` profile).
- Adds `host.docker.internal:host-gateway` to contact-centre so it can reach a VoiceBlender server on the host (the Linux equivalent of macOS/Windows behaviour).
- Lists **every** env var the app understands inline, with the same defaults as [`.env.example`](./cmd/contact-centre/.env.example), so it doubles as the canonical reference.
- Wraps the variables you're most likely to override (`SUPERVISOR_PASSWORD`, `AGENT_PASSWORD`, `CALL_LOG_BACKEND`, `CALL_LOG_REDIS_URL`, `CALL_LOG_REDIS_KEY`) in `${VAR:-default}` so a sibling `.env` file or shell exports take precedence without editing the YAML.
- Includes a Redis 7 sidecar gated behind the `redis` Compose profile, with a healthcheck and a named volume for persistence across `docker compose down`.

### Caddy reverse proxy

[`Caddyfile`](./Caddyfile) is mounted into the `caddy` service. By default it terminates plain HTTP on the container's port 80 (published to host `:80`) and proxies everything to `contact-centre:8090`. WebSocket upgrades (`/api/calls/stream`, `/api/agent/stream`) are passed through transparently — Caddy v2's `reverse_proxy` handles the `Upgrade` header without extra config. If host port 80 is already taken, override with `CADDY_HTTP_PORT=8090 CADDY_HTTPS_PORT=8443 docker compose up`.

#### Enabling HTTPS with Let's Encrypt

The Caddyfile and compose file are templated for a one-shot switch to automatic HTTPS. Set `CADDY_DOMAIN` to a real public hostname (DNS pointing at the host running Caddy) and Caddy provisions a Let's Encrypt cert on first request.

```bash
CADDY_DOMAIN=cc.example.com \
  CADDY_ACME_EMAIL=admin@example.com \
  HOLD_MUSIC_URL=https://cc.example.com/moh/new_music.mp3 \
  docker compose up --build -d
```

| Env var | Default | Purpose |
|---|---|---|
| `CADDY_DOMAIN` | _(unset)_ → `http://:80` | Domain Caddy serves on. Setting a hostname here trips auto-HTTPS. |
| `CADDY_ACME_EMAIL` | _placeholder_ | Account email for Let's Encrypt renewal notifications. |
| `CADDY_ACME_CA` | LE production | ACME directory URL. Override with the LE staging endpoint while testing. |
| `CADDY_HTTP_PORT` | `80` | Host port mapped to Caddy's container port 80. Override if something else on the host already binds 80. Let's Encrypt's HTTP-01 challenge requires this to be reachable at the *public* port 80. |
| `CADDY_HTTPS_PORT` | `443` | Host port mapped to Caddy's container port 443. Override if something else on the host already binds 443. Let's Encrypt's TLS-ALPN-01 challenge requires this to be reachable at the *public* port 443. |
| `HOLD_MUSIC_URL` | local HTTP | URL VoiceBlender fetches the hold-music file from. Point this at the same domain over HTTPS so VoiceBlender's outbound fetch matches the public site. |

Certificates and ACME state persist in the `caddy-data` named volume — restarting or recreating the container doesn't trigger a re-issuance and won't burn rate-limits. Wipe the volume (`docker compose down -v`) to start from scratch.

While iterating, point `CADDY_ACME_CA` at the Let's Encrypt staging endpoint (`https://acme-staging-v02.api.letsencrypt.org/directory`) — same flow, untrusted certs, no rate-limit risk.
