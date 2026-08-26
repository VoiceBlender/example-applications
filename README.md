# example-applications

Reference applications built on [VoiceBlender](../VoiceBlender) using the [voiceblender-go](../voiceblender-go) SDK. Each app is a self-contained Go binary under `cmd/`.

## Applications

| Name | Description |
|---|---|
| [contact-centre](./cmd/contact-centre/) | Complete inbound SIP contact centre in one binary. UK ringback → welcome TTS → per-caller waiting room with hold music and live queue-position announcements → one-click *Take call* on the agent dashboard → bridge with mute/hold/resume/hangup → live per-speaker transcription archived into the call log. Supervisor dashboard adds silent monitor (*Listen*), private side-channel (*Whisper*), and a rolling Service KPIs board (SL, ASA, AHT, Abandon, Longest Wait). Pluggable call-log backend (memory / Redis), optional static-password auth, configurable codec preference order. |
| [ivr](./cmd/ivr/) | Multi-department IVR. UK ringback → welcome TTS → DTMF main menu → routes the caller into a department room (sales / support / billing) with looping hold music and a repeating hold message, or hands off to a Deepgram AI voice agent on `0`. |
| [pbx](./cmd/pbx/) | **Multi-tenant SIP PBX** in one binary. Authenticated extensions (REGISTER + INVITE digest), register/IP trunks, extension↔extension and extension→external calls, a dial-by-extension IVR, and a **visual inbound dial-plan editor** (match / gather DTMF+speech / ext / ivr / forward / play / TTS / reject). **Browser WebRTC softphones** act as extra devices per extension — ring alongside the SIP phone, place calls, hold / blind-transfer (incl. "to my desk phone") / DND / mic-selector / ringtone / call-history + contacts. Handles inbound SIP REFER (accept → re-bridge), self-service tenant signup with a cross-tenant superadmin console, on-hold music, and Redis-persisted config/sessions. |
| [ptt](./cmd/ptt/) | **Browser push-to-talk** (walkie-talkie). Username-only login → create public or private rooms → hold a button (or Space) to talk over WebRTC. **Single-speaker floor control** (a second presser gets "busy") and **fully on-demand media**: the VoiceBlender room and every WebRTC leg are created on the press and torn down on the release, so nothing is allocated while a room is quiet. Private rooms use a shareable invite code/link; live presence + "who's talking"; Redis-persisted users/sessions/rooms. |
| [interpreter](./cmd/interpreter/) | **Live simultaneous interpreter.** Two people, two languages, one WebRTC conversation — each hears **only** the other's translated voice, with live captions of the original and the translation. Cascades Deepgram Flux STT (or Speechmatics, for languages Flux cannot do) → DeepL → ElevenLabs Flash TTS, and uses the room **routing matrix** to silence the direct path between the participants plus per-leg `leg_tts` to inject each translation privately. Hides the synthesis cost behind the speaker's last few hundred milliseconds by translating on Flux's **eager end-of-turn**, staging the audio with `leg_tts_preflight`, and committing it the instant the turn ends (~250–450 ms speaker-stops-to-listener-hears). Each speaker picks a voice gender and is heard in a matching voice on the far side. Optional static login, and idle/duration session limits so an abandoned tab stops billing STT. No datastore. |

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
- **ptt** — [`Dockerfile.ptt`](./Dockerfile.ptt) (needs Redis; listens on `:8092`):
  ```bash
  docker build -f Dockerfile.ptt -t ptt .
  ```
- **interpreter** — [`Dockerfile.interpreter`](./Dockerfile.interpreter) (no datastore; listens on `:8093`):
  ```bash
  docker build -f Dockerfile.interpreter -t interpreter .
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

A worked [`compose.yaml`](./compose.yaml) runs **three apps against one VoiceBlender** — the contact centre, push-to-talk and the live interpreter — plus Caddy and Redis.

```bash
docker compose up --build
```

| URL | App |
|---|---|
| `http://localhost/` | contact-centre — supervisor panel |
| `http://localhost/agent` | contact-centre — agent panel |
| `http://ptt.localhost/` | push-to-talk |
| `http://interp.localhost/` | live interpreter |

Both hostnames are served by Caddy on the **same ports** (80/443) — it routes by `Host`, not by port. Browsers resolve `*.localhost` to `127.0.0.1` themselves, so no `/etc/hosts` entry is needed.

The compose file:

- Brings up five services: **caddy** (the only one with host ports — `80:80` and `443:443`, override with `CADDY_HTTP_PORT` / `CADDY_HTTPS_PORT`), **contact-centre**, **ptt** and **interpreter** (all internal-only, fronted by Caddy), and **redis**.
- Points **all three apps at the same VoiceBlender** (`host.docker.internal:8080`) and keeps them apart with `APP_ID`: each tags the rooms and browser legs it creates, and filters the VSI event stream to its own. See each app's README.
- The **interpreter needs no Redis** but does need outbound internet and API keys for its three speech hops (`DEEPGRAM_API_KEY`, `ELEVENLABS_API_KEY`, `DEEPL_API_KEY`). It defaults to `TRANSLATE_PROVIDER=none` in compose so the stack comes up and the media path works without an MT account — set `TRANSLATE_PROVIDER=deepl` once you have a key.
- The interpreter bills **per minute of wall-clock time** (two live legs = two continuously-streaming STT sessions), so it ships with an idle timeout (`INTERP_IDLE_TIMEOUT`, default 5m) and a hard duration cap (`INTERP_MAX_DURATION`, default 1h). Set `INTERP_PASSWORD` before exposing it — without one there is no login and anyone who finds it can spend your speech budget.
- **Redis is no longer opt-in** — ptt requires it (users, sessions, rooms). The apps use separate databases (contact-centre's call log → `0`, ptt → `1`). The contact centre's call log still defaults to memory; set `CALL_LOG_BACKEND=redis` to persist it.
- Lists **every** env var each app understands inline, with the same defaults as their `.env.example` files, so it doubles as the canonical reference.

**Microphone note:** push-to-talk and the interpreter both need `getUserMedia`, which browsers only grant in a *secure context*. `http://ptt.localhost` and `http://interp.localhost` qualify (localhost is exempt), but any other plain-HTTP hostname does not — to reach them over the network you need HTTPS, i.e. set `CADDY_PTT_DOMAIN` / `CADDY_INTERP_DOMAIN`.

### Caddy reverse proxy

[`Caddyfile`](./Caddyfile) is mounted into the `caddy` service and defines **three sites on the same ports**, told apart by hostname: `CADDY_DOMAIN` (default catch-all `:80`) → `contact-centre:8090`, `CADDY_PTT_DOMAIN` (default `ptt.localhost`) → `ptt:8092`, and `CADDY_INTERP_DOMAIN` (default `interp.localhost`) → `interpreter:8093`. Caddy matches the most specific site first, so each named site wins for its own host while the contact centre remains the catch-all.

WebSocket upgrades (`/api/calls/stream`, `/api/agent/stream`, `/api/ptt/stream`, `/api/lobby/stream`, `/api/interpreter/stream`) pass through transparently — Caddy v2's `reverse_proxy` handles the `Upgrade` header without extra config.

### Public deployment: both apps on :443, one domain each

Copy [`.env.example`](./.env.example) — it is already set up for two Let's Encrypt-secured domains on the same port:

```bash
cp .env.example .env
docker compose up -d --build
```

| URL | App | Certificate |
|---|---|---|
| `https://ccdemo.voiceblender.org/` | contact-centre | Let's Encrypt |
| `https://talky.voiceblender.org/` | push-to-talk | Let's Encrypt |

Both are Caddy sites on the **same `:443` listener**. Caddy reads the SNI from the TLS handshake, serves that domain's own certificate, and proxies to the matching backend — no layer-4 plugin needed. It also issues a `308` redirect from `:80` to HTTPS for both names, and renews the certs automatically. Certs and the ACME account key live in the `caddy-data` volume, so restarts don't re-issue.

**Prerequisites — all three, or issuance fails:**

1. **DNS** `A`/`AAAA` records for **both** names must point at the host.
2. Host ports **80 and 443** must be reachable from the public internet. Port 80 is not optional: Let's Encrypt uses it for the HTTP-01 challenge.
3. **Do not set `CADDY_HTTP_PORT` / `CADDY_HTTPS_PORT`** for a public deployment. Remapping them breaks issuance, because Let's Encrypt always connects to the *public* 80/443. They exist only for local plain-HTTP use.

While testing, point `CADDY_ACME_CA` at the Let's Encrypt **staging** endpoint (commented in `.env.example`) — same flow, untrusted certs, no rate limits. A few failed attempts against production will lock you out for a week.

> **`HOLD_MUSIC_URL` must become the HTTPS URL.** Once `CADDY_DOMAIN` is a real domain there is no catch-all `:80` site left, so VoiceBlender fetching `http://host.docker.internal/moh/…` would match no site, get a 404, and hold music would silently stop working. `.env.example` already points it at `https://ccdemo.voiceblender.org/moh/new_music.mp3`.

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
