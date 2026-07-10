# pbx

A small SIP **PBX** built on VoiceBlender and the [voiceblender-go](../../../voiceblender-go) SDK. One binary, one VSI WebSocket, a Redis-backed config store, and a web management console themed like the [contact-centre](../contact-centre/) example.

## What it does

| Capability | How |
|---|---|
| **Outbound trunks (VoiceBlender registers)** | A `register`-type trunk makes VoiceBlender REGISTER to an upstream provider. Status (registered / failed / expired) is shown live on the console from `sip.outbound_registration_*` events. |
| **Inbound IP-authenticated trunks** | An `ip`-type trunk lists trusted peer IPs. Inbound calls whose **source IP** (from the `leg.ringing` event) matches are trusted with **no digest challenge** and routed to the IVR. |
| **Authenticated extensions (REGISTER + INVITE)** | Inbound REGISTER is challenged against the extension's password (registrar). Every extension INVITE is digest-challenged on the ringing leg before it's routed. |
| **WebRTC softphones (browser "virtual devices")** | Each extension can have **WebRTC accounts** (username/password/label). Signing into one at `/phone/login` opens a browser softphone that acts as an **extra device for the same extension number**: it rings alongside the SIP registration(s) — first to answer wins — and can place calls as that extension. The browser's WebRTC leg is created once at sign-in and reused for every call (detached from a call's room on hangup, never deleted). See below. |
| **Extension ↔ extension calls** | Bridged through a room; the caller hears ringback until the callee answers. Rings the callee's SIP phone **and** any signed-in WebRTC softphones simultaneously. |
| **Extension → external calls** | Numbers that don't match an extension are dialed out via a trunk. |
| **Call transfer (SIP REFER)** | When a phone transfers a call, VoiceBlender surfaces the REFER as `leg.transfer_requested` (it does not auto-dial). The app completes it by re-bridging: the other party is connected to the requested target (an extension's endpoints or a trunk) and the transferor is dropped. Blind transfers are seamless; attended transfers are handled semi-attended (the target extension is rung fresh). Softphones also transfer via their own UI (see below). |
| **Inbound trunk calls → dial plan** | Inbound calls from trunks are routed by a **configurable dial plan** edited as a visual flow graph in the console (see below). The default flow sends every inbound call to the dial-by-extension IVR. |
| **Dial-by-extension IVR** | Greet, collect the extension number (submit with `#`, or auto-submit on a match, `*` clears), then ring that extension. Reachable as a dial-plan action. |
| **On-hold music** | When an extension puts a call on hold, the other party hears the configured hold-music URL (looped) until it's taken off hold. The URL is set in the console's **Global configuration** section (persisted in Redis; seeded from `HOLD_MUSIC_URL`). |

There is no dialplan/PBX abstraction in the SDK — routing, the extension registry, credential lookup, and trunk lifecycle are all implemented here, reacting to VSI events.

## Multi-tenancy

The PBX is **multi-tenant**: each tenant is an isolated workspace with its own extensions, trunks, dial plan, config, and softphone accounts. **Extension numbers may repeat across tenants** (acme's `1001` ≠ globex's `1001`).

- **Sign up** at `/signup` to create a workspace (a tenant + its admin user). Sign in at `/login` with the admin username + password. The admin session carries the tenant, and every console page / API / snapshot is scoped to it — a tenant never sees another's data.
- **SIP identity.** The dialable **Number** is tenant-scoped and may repeat; the underlying SIP **Username** is globally unique, auto-derived as `<tenant>-<number>` (e.g. `acme-1001`). Phones register with that username, so two tenants' identical Numbers never collide on the registrar. The console shows the SIP username to configure the device with; users dial plain Numbers.
- **Tenant resolution.** An extension INVITE is attributed to a tenant via the caller's (globally-unique) authenticated SIP username; an inbound trunk call via the trunk it arrived on (each trunk belongs to a tenant). Dialed numbers are then resolved *within* that tenant.
- **Softphone accounts** are globally unique, so `/phone/login` needs no tenant field; the account resolves to its extension and hence its tenant.
- **Storage.** Tenants/users live in Redis hashes `pbx:tenants` / `pbx:users`; config and dial plans are per-tenant (`pbx:config:<tenant>` / `pbx:dialplan:<tenant>`); extensions/trunks share the existing hashes and carry a `tenant_id`. Live call state is keyed by server-unique leg ids and simply tagged with a tenant for console isolation.
- **Superadmin.** Set `SUPERADMIN=<user>:<pass>` to enable a cross-tenant operator: sign in with those credentials at `/login` to reach **`/admin`**, a console that manages **every tenant** — create/delete tenants (delete cascades all their extensions, trunks, config, dial plan, and users), add/remove each tenant's console users, and create/edit/delete any tenant's extensions and trunks (choosing the owning tenant). Regular tenant admins never see it.
- For local dev, `SEED_TENANT=<slug>:<name>:<user>:<pass>` seeds a workspace on startup. Passwords (console, extension, trunk, superadmin) are stored **plaintext** — demo-grade; public signup has no email verification or rate limiting.

## WebRTC softphones

A softphone is a browser "virtual device" for an extension. In the console's **Extensions** page, add one or more **WebRTC accounts** to an extension (each is a `username` / `password` / `label`). Someone opens `/phone/login`, signs in with those credentials, and gets a browser phone (mic + `<audio>` sink) that:

- **Rings alongside the SIP phone.** When the extension is dialed — from another extension, a dial-plan `ext` node, or the IVR — every registered SIP contact **and** every signed-in WebRTC device rings at once (a *fork*); the first to answer wins and the rest stop.
- **Places calls as the extension.** The dial field routes through the same extension/trunk logic as a SIP INVITE (rings another extension's endpoints, or goes out a trunk), with the browser hearing ringback.
- **Persists across calls.** The WebRTC leg is established once at sign-in (`webrtc_offer` + trickle ICE over the phone WS) and reused; on hangup it is *detached* from the call's room (`remove_leg_from_room`), never deleted, so it's ready for the next call. It's deleted only on sign-out / WS close.

Signalling (`webrtc.offer`/`webrtc.answer`/`webrtc.candidate`, the 250 ms ICE-candidate poll, the `<audio autoplay playsinline>` sink) mirrors the [contact-centre](../contact-centre/) example's agent stream, driven entirely over the VSI stream. The softphone is a **separate identity** from the admin console: its own login (`/phone/login`), cookie (`pbx_phone`), and signalling WebSocket (`/api/phone/stream`) — a softphone user never sees the console.

## Console

The management console is split into pages behind a shared top nav — **Live calls** (`/`), **Extensions** (`/extensions`), **Trunks** (`/trunks`), **Dial plan** (`/dialplan`), **Config** (`/config`) — all fed by one snapshot WebSocket (`/api/stream`). The theme and shared plumbing live in `/static/app.css` + `/static/app.js`; the visual dial-plan editor is `/static/dialplan.js`.

**Per-extension codecs.** Each extension can specify an ordered codec preference list (e.g. `opus, PCMA, PCMU`) in the console. When a call rings that extension, the outbound INVITE offers exactly those codecs in that priority order (`CreateLegRequest.Codecs`); blank means the server/global default. (This is distinct from `ANSWER_CODECS`, which is the answer-codec order for inbound trunk calls landing in the IVR/dial plan.)

## Inbound dial plan (visual flow editor)

The server hands every inbound INVITE to the app to decide, so inbound-call routing is entirely app-defined. The **Inbound dial plan** board in the console is a visual node/flow editor: inbound trunk calls walk the graph from the `start` node.

- **Nodes**: `start` (entry) · `match` (branch on **trunk + dialed number/DID**, with match/nomatch outputs) · `answer` (answer the call, then continue) · `wait` (pause N seconds, then continue) · `gather` (play a prompt, collect DTMF/speech, branch on the entry) · `ext` (ring one or more extensions — a comma-separated list forks/rings them simultaneously, first to answer wins; a **ring time** bounds it and a **noanswer** output continues the flow if nobody picks up) · `ivr` (dial-by-extension IVR) · `forward` (bridge out to an external number via a trunk — with a **ring time** and a **noanswer** output, like `ext`) · `play` (play an audio-file URL) · `tts` (speak text) · `reject` (hang up). `answer`/`play`/`tts` chain to a `next` node; `ext`/`ivr`/`forward`/`reject` are terminal.
- **Gather**: plays an optional prompt (TTS or audio URL) and collects input — **DTMF, speech, or both** (input mode). Each comma-separated *option* becomes an output port — wire them to their destinations, plus a `default` port for no match / timeout / no input.
  - **DTMF**: options are digits (`1,2,0`). Auto-submits on an option match, on reaching *max digits*, or on `#`; `*` clears.
  - **Speech**: options are keywords (`sales,support,billing`). The spoken transcript is matched to an option by whole-phrase, exact word, number-word (`one`→`1`), or fuzzy close-word (`sails`→`sales`) matching. Uses `STT_*` config (`STT_PROVIDER`/`STT_LANGUAGE`/`STT_API_KEY`). A per-node **language** overrides the default.
  - **Both**: accepts a keypress or a spoken keyword, whichever comes first.
- **Editing**: drag a node's header to move it; click an output dot then an input dot to connect (one edge per output); click a node body to edit its settings; click a wire to delete it; **Save** persists the graph (Redis key `pbx:dialplan`), **Reload** re-pulls it.
- **Trunk identification**: the server tags register-trunk inbound calls with `TrunkID` (source IP matched the trunk's captured peer socket); the app falls back to a source-IP match for ip trunks. Calls that can't be tied to a trunk still run the graph, so a catch-all rule handles them — inbound calls are never blindly rejected.
- **Default**: a fresh install seeds `start → ivr`, so inbound trunk calls reach the IVR out of the box. Edit the graph to route specific DIDs to extensions, play announcements, forward, etc.
- `play`/`tts` answer the call before playing; `tts` uses the app's `TTS_*` config. REST: `GET`/`PUT /api/dialplan`.

## Files

| File | Role |
|---|---|
| `main.go` | app wiring, VSI event loop, startup |
| `registrar.go` | REGISTER challenge / accept / reject; registration status |
| `dialplan.go` | classify + authenticate inbound legs; bridge internal / external calls |
| `ivr.go` | dial-by-extension IVR for inbound trunk calls |
| `extensions.go` / `trunks.go` | in-memory registries + live status; extensions carry WebRTC accounts |
| `phones.go` | live WebRTC softphone registry (sessions, `byLeg` index, ext fan-out) |
| `phone.go` | softphone: login/logout, page, and the signalling WebSocket (offer/answer/ICE, answer/reject/dial/hangup/mute) |
| `tenants.go` | tenant + console-user registry (self-service signup), Redis-backed |
| `superadmin.go` | cross-tenant superadmin console (`/admin`): list/manage all tenants' extensions & trunks |
| `store_redis.go` | Redis persistence (`pbx:extensions`, `pbx:trunks` hashes; `pbx:tenants`/`pbx:users`; per-tenant `pbx:config:*`/`pbx:dialplan:*`) |
| `auth.go` / `web.go` | console: tenant-scoped admin + softphone sessions, signup/login, CRUD REST, snapshot WebSocket, page templates |
| `web/` | `layout.html` + `page_*.html` console pages, `phone.html` / `phone_login.html` softphone, `login.html` / `signup.html`, `static/` shared assets |

## Running

Requires a VoiceBlender server and a Redis instance.

```bash
cp cmd/pbx/.env.example cmd/pbx/.env   # then edit
# from the repo root:
REDIS_URL=redis://localhost:6379/0 \
PBX_DOMAIN=pbx.local \
SEED_TENANT=acme:Acme:admin:letmein \
go run ./cmd/pbx
```

Open the console at <http://localhost:8091/>, **create a workspace at `/signup`** (or sign in with the `SEED_TENANT` admin), and add extensions and trunks. Configure phones with the extension's shown SIP username (`<tenant>-<number>`, e.g. `acme-1001`) registering as `sip:<username>@<PBX_DOMAIN>`; call between them by dialing plain Numbers, or dial an external number to go out a trunk.

## Configuration

See [`.env.example`](./.env.example). Key variables: `VOICEBLENDER_URL`, `REDIS_URL` (required), `LISTEN_ADDR`, `PBX_DOMAIN`, `PBX_REALM`, `SEED_TENANT` (optional dev seed), `SUPERADMIN` (optional cross-tenant admin), `COMPANY_NAME`, `TTS_*`, `ANSWER_CODECS`.

## Notes & caveats

- **Demo-grade credentials.** Extension and trunk passwords are stored in Redis in **plaintext** for simplicity. Don't reuse this store for anything real.
- **Outbound external routing** targets `sip:<number>@<trunk-host>`, relying on the server to route via the matching trunk. For a register trunk the caller-leg carries the trunk credentials so the provider's 401/407 can be answered.
- **Inbound IP-trunk auth** is decided entirely from the ringing event's `source_address` — no server-side allowlist is required.
- **Multi-tenant** (see above): sign up at `/signup`; console auth is always required (no anonymous bypass). `SEED_TENANT` optionally provisions a workspace for local dev.
- **VSI wire tracing.** Every command sent and event/reply received over the VSI WebSocket is logged (`msg=vsi dir=send|recv type=… frame=…`), on by default. Set `VSI_LOG=0` to silence it. Implemented via the SDK's `voiceblender.WithFrameLogger` option.
