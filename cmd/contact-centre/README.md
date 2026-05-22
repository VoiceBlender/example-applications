# contact-centre

A VoiceBlender example app that acts as the front door of a contact centre.

## Stage 1 — waiting room

What happens on each inbound SIP call:

1. **Ring.** Early media plays the UK ringback tone (`gb_ringback`) for ~3 seconds.
2. **Answer.** The call is answered. Set `ANSWER_CODECS` to a comma-separated preference order (e.g. `opus,PCMA,PCMU`, from `PCMU`/`PCMA`/`G722`/`opus`) to choose the media codec for the early-media and answer SDP. The app picks the first codec in the list that the caller actually offered (from the `leg.ringing` `offered_codecs`); if none match — or it's unset — VoiceBlender falls back to its own default preference order.
3. **Per-caller waiting room.** The caller is placed alone in a dedicated VoiceBlender room (`waiting-<leg_id>`). VoiceBlender's mixed-minus-self mixer guarantees callers cannot hear each other or each other's announcements.
4. **Hold experience.** Hold music loops in the room. Every `ANNOUNCEMENT_INTERVAL` (default 20 s) the caller hears their live position in the queue: *"You are next in the queue. Thank you for holding."* or *"You are number 2 in the queue…"*. Position drops when an earlier caller hangs up.

Subsequent stages (agent attach, transfer, real dequeueing) will be added later.

## Live panels

Two browser views are served by the same process.

**Supervisor panel** at <http://localhost:8090/> — every inbound call (ringing + queued + ongoing), with caller, callee, state, live duration, queue position, and (for `in_call` rows) a **Listen** button. The **Whisper** button only appears on the call the supervisor is currently listening to. Shows every logged-in agent with their current call, plus a **Call Log** of historical calls (newest first). Streams over a WebSocket; reconnects automatically.

The call log is pluggable — by default it's an in-process ring buffer (`CALL_LOG_MAX=200` entries, lost on restart). Setting `CALL_LOG_BACKEND=redis` plus `CALL_LOG_REDIS_URL=redis://host:port/db` switches it to any Redis-compatible server (Redis, Valkey, KeyDB, Dragonfly, …) — entries persist across restarts and can be shared between multiple instances of this app. The Redis schema is a single list (key `CALL_LOG_REDIS_KEY`, default `contactcentre:call_log`) of JSON-encoded entries; we use `RPUSH` + `LTRIM` to cap and `LRANGE` to read.

When the supervisor clicks **Listen**, the browser opens a WebRTC connection to VoiceBlender with a synthesised silent local audio track (no microphone permission prompt) and is added to the call's room with role `supervisor`. VoiceBlender's role-routing matrix is set once per call room as `customer → [agent]`, `agent → [customer, supervisor]`, `supervisor → [customer, agent]`. The caller's row never lists `supervisor`, so a supervisor in the room is never audible to the caller. The agent's row permits `supervisor`, so a supervisor IS audible to the agent — but the supervisor's mic is muted while they're only listening, so silent frames flow and the agent hears nothing extra.

Clicking **Whisper** acquires the microphone the first time (browser permission prompt), `replaceTrack`s the silent track for the mic on the existing peer connection, and un-mutes it. Subsequent toggles are a pure `track.enabled = !track.enabled` mute/unmute — no server round-trip, no matrix change, no extra WebRTC negotiation. Multiple supervisors monitoring the same call can whisper concurrently; each one's mute state is independent. Clicking **✕** in the header pill mutes while staying in the listen.

**Agent panel** at <http://localhost:8090/agent> — name-only sign-in, then a live view of just the queued calls (ringing calls are hidden). The WebRTC audio leg into VoiceBlender (Opus, trickle ICE) is established **lazily, on the first "Take call" click** — sign-in no longer prompts for the microphone or allocates a server-side leg. When a call ends, the leg is torn down again (the browser remembers the mic grant, so later calls don't re-prompt). This keeps idle agents from holding open media resources. The header pill (`ready / mic init… / audio dialling / mic live / mic muted / mic failed`) reflects the audio state; clicking it after a failure resets to `ready`.

The agent's WS at `/api/agent/stream` is the single channel: it carries queue snapshots **and** all WebRTC signaling. Sessions are in-memory; restart the server and agents need to sign in again.

Endpoints exposed on the same HTTP server:

| Path | Purpose |
|---|---|
| `GET /` | Supervisor panel HTML |
| `GET /api/calls` | One-shot JSON snapshot of active calls |
| `GET /api/calls/stream` | **Bidirectional WebSocket** for the supervisor. Server → `snapshot {calls, stats, at, agents, self.listening_room_id}`, `webrtc.answer`, `webrtc.candidate`, `webrtc.error`, `listen.error`. Client → `webrtc.offer`, `webrtc.candidate`, `webrtc.hangup`, `listen.start {room_id}`, `listen.stop`. The supervisor's WebRTC leg is bound to this socket — it's hung up when the socket closes. Whisper has no WS messages: it's a browser-side mic mute toggle on the existing listen leg. |
| `GET /agent` | Agent panel HTML (login + queue dashboard) |
| `POST /api/agents/login` | `{name}` → `{agent_id, name, logged_in_at}` |
| `POST /api/agents/logout` | `{agent_id}` |
| `GET /api/agents/whoami?agent_id=…` | Returns the agent record, or 404 if the session is gone |
| `GET /api/agent/stream?agent_id=…` | **Bidirectional WebSocket** for the agent (rejects unknown `agent_id`). Server → `snapshot` (carries `self.current_call` including `on_hold`), `webrtc.answer`, `webrtc.candidate`, `webrtc.error`, `call.error`. Client → `webrtc.offer`, `webrtc.candidate`, `webrtc.hangup`, `call.answer {leg_id}` (claim a queued call and bridge), `call.hangup` (end the current call), `call.hold` (drop the agent's leg from the room and start hold music; caller hears music alone), `call.resume` (stop hold music and re-add the agent's leg). The agent's WebRTC leg is bound to this socket — when it closes the leg is hung up automatically. Mute is purely client-side (toggles `track.enabled` on the local audio stream) — no server message. |
| `GET /moh/new_music.mp3` | Hold-music file VoiceBlender fetches |

## How it talks to VoiceBlender

Events arrive over **VSI** — the WebSocket Streaming Interface VoiceBlender exposes at `/vsi`. No HTTP webhook server is required.

## Prerequisites

- A running [VoiceBlender] server with SIP and the VSI endpoint enabled.
- TTS credentials — either pre-configured inside VoiceBlender, or `TTS_API_KEY` set here so it's forwarded with each TTS request.
- The hold-music MP3 dropped at [`./assets/new_music.mp3`](./assets/). This file is **not committed** — copy or symlink your own. The same file is used by [`voiceblender-go/examples/ivr`](../../../voiceblender-go/examples/ivr).

## Configuration

See [`.env.example`](./.env.example). All variables are optional and have sensible localhost defaults.

## Run

From the repo root:

```bash
cp cmd/contact-centre/.env.example cmd/contact-centre/.env   # then edit
go run ./cmd/contact-centre
```

Expected startup logs:

```
http listening addr=[::]:8090
contact centre ready voiceblender_url=http://localhost:8080/v1 operator_panel=http://localhost:8090/ agent_panel=http://localhost:8090/agent …
```

Then open <http://localhost:8090/> (operator) and/or <http://localhost:8090/agent> (agent sign-in).

## Manual test

Open the operator panel at <http://localhost:8090/> and the agent panel at <http://localhost:8090/agent> in two browser tabs alongside your softphone(s). On the agent tab, sign in with any name (e.g. "Alice").

1. Place a SIP call to your VoiceBlender SIP endpoint (linphone, baresip, etc.).
   - Expect ~3 s of UK ringback before answer.
   - After answer: hold music + *"You are next in the queue. Thank you for holding."*
   - The **operator** panel shows the call appear as **ringing**, then transition to **queued**, position `01`.
   - The **agent** panel shows the call appear only after it reaches the queue (ringing is hidden), at position `01`.
2. Place a second call in parallel from a different softphone.
   - Caller A still hears *"You are next…"*; caller B hears *"You are number 2…"*.
   - Both panels show two rows; the second is at position `02`.
3. Hang up caller A.
   - Both panels: the first row fades out; caller B jumps to position `01`.
   - The agent's "longest wait" stat re-anchors to caller B's clock.
4. On the agent panel the pill sits at `ready` — no mic prompt yet, no WebRTC leg.
5. With a queued call visible, click **Take call** on a row. The pill now runs `mic init… → audio dialling → mic live` (grant microphone permission on the first call), the server logs `agent webrtc leg created`, and the call bridges. When the call ends the leg is released and the pill returns to `ready`; the next **Take call** re-establishes audio without re-prompting.
   - Hold music stops in the caller's room; the agent's WebRTC leg is added to the same room.
   - Operator panel: the row's state badge flips to **on call · &lt;agent name&gt;** (green).
   - Agent panel: the queue row vanishes (queue only shows waiting callers); a green "ON CALL" card appears at the top of the dashboard showing the caller and a live duration. Remaining queued calls disable their "Take call" buttons.
   - Caller hears the agent's microphone; agent hears the caller through the browser audio element.
6. **Mute / unmute.** Click **Mute** on the call card — the mic pill flips to `mic muted` (amber, blinking). The caller hears silence from your end. Click **Unmute** to restore. Mute is enforced client-side; nothing changes on the server.
7. **Hold / resume.** Click **Hold** — the call card border turns amber, the tag flips to `on hold`, the operator panel's state badge becomes `on hold · Alice`. Caller hears the looping hold-music MP3 alone; you hear silence. Click **Resume** — the music stops, you're put back in the room, conversation continues.
8. Click **End call** in the card to drop the caller. Server logs show `agent hung up caller`; the call row vanishes from both panels and the agent's "Take call" buttons re-enable.
9. Click **Log out** — the panel returns to the sign-in screen, the WebSocket disconnects, and the agent's WebRTC leg is hung up. Server logs show `agent webrtc leg hung up` and `agent logged out`.

### Supervisor listen-in

10. On the supervisor panel, in the row of an `on call · <agent>` entry, click **Listen**. Header pill: `opening audio… → audio ready → listening`. Server logs show `supervisor webrtc leg created` and `supervisor listening`. You should hear the live conversation in your browser.
11. Click the same row again (button now says **Stop**), the **✕** in the header pill, or a different row's **Listen** to switch. Closing the supervisor tab also tears the leg down.

### Supervisor whisper

12. While listening to a call, click **Whisper** in the same row (it's only shown while you're listening to that call). First time: browser prompts for mic permission; accept.
   - Header pill flips to amber `whispering · <agent>`. The button label becomes `Mute`.
   - Speak — only the agent hears you. Confirm on the SIP softphone that the caller still hears nothing extra. The agent still hears the caller; you still hear caller + agent.
13. Click **Mute** in the row, or the **✕** in the pill — mic is muted, pill returns to `listening`; agent no longer hears you; caller is undisturbed throughout. Click **Whisper** again to un-mute — no permission prompt this time.
14. With a second supervisor tab, listen + whisper to the same call. Both whisperers' mic audio mixes for the agent. Each supervisor's mute toggle is independent.

### Authentication

Both panels can be gated behind a static password. Set `SUPERVISOR_PASSWORD` and/or `AGENT_PASSWORD` (independently — either side stays open if its variable is empty). When a password is configured, the panel redirects unauthenticated visitors to `/login?role=…`; on success the server issues a 12-hour rolling session cookie (`cc_supervisor` or `cc_agent`) and the underlying WebSocket inherits it (modern browsers include cookies on same-origin upgrades).

The login form asks for a **username** and a **password**. Only the password is validated against the configured value; the username is free-form — anyone can sign in as any username as long as they know the shared password. The username is captured on each successful sign-in (logged with `role` + `remote`) so you have a light audit trail of who claimed which session, and is also reused by the agent panel as the agent's display name (so there's no second name-prompt screen when `AGENT_PASSWORD` is configured). The agent panel falls back to the original in-page name form when auth is disabled.

- **Supervisor:** a small **Sign out** button appears in the topbar once a session cookie is present. Clicking it clears the cookie server-side and redirects to the login form.
- **Agent:** the existing **Log out** button still ends the agent's name-based identity, and additionally clears the auth session if one is configured. With auth on, the page bounces to `/login?role=agent` after logout instead of the in-page re-login form.

The login form lives at `/login` and re-uses the same dark-grid IBM-Plex aesthetic. The password comparison uses `crypto/subtle.ConstantTimeCompare`; failed attempts pause for 250 ms to slow a naive scripted attacker — this is *not* a real defence, just a hint that you should add proper rate-limiting if you take this beyond a localhost example. The `/moh/` hold-music endpoint is intentionally left public so the VoiceBlender server can fetch it server-to-server.

### Service KPIs

The supervisor page surfaces a row of standard contact-centre metrics — **Service Level**, **ASA**, **AHT**, **Abandon Rate**, and **Longest Wait** — recomputed on every snapshot. They're all derived from data the app already tracks (the active call registry + the call log), so they work with both `memory` and `redis` log backends without any extra wiring.

| KPI | Meaning |
|---|---|
| **Service Level** | % of calls (answered + abandoned in window) whose pickup beat `SLA_THRESHOLD` (default 20s). Abandoned calls count as failing SL — they never met threshold. Colour-coded: ≥80% green, 50–80% amber, <50% red. |
| **ASA** | Mean *Answer Speed* — wait between call arrival and agent pickup, averaged across answered calls in the window. |
| **AHT** | Mean *Handle Time* — duration from agent pickup to disconnect, across answered calls in the window. |
| **Abandon Rate** | Share of offered calls that hung up before being answered. Colour-coded: <5% green, <15% amber, ≥15% red. |
| **Longest Wait** | Peak wait observed anywhere in the window: across answered calls (`AnsweredAt − StartedAt`), abandoned calls (`EndedAt − StartedAt`), and callers currently in queue (`now − StartedAt`). It doesn't reset when the queue empties — the peak persists for the rolling window. Ticks every second client-side when a live caller's wait exceeds the recorded peak. Green <30s, amber <2m, red ≥2m. |

`METRICS_WINDOW` (default `30m`) controls the rolling window for the four historical KPIs; only entries whose `ended_at` falls inside the window count. The board sub-header shows `offered N · answered N · abandoned N` for context (a "100% Service Level" on 2 calls is less meaningful than on 200, and the counters make that obvious).

If the window has no eligible calls, the cell shows `—` rather than a misleading `0.0%`.

### Live transcription

The moment an agent answers a call, the app starts room-wide STT on the caller's room (`room.STT`, language from `STT_LANGUAGE`, provider from `STT_PROVIDER`, optional `STT_API_KEY`). VoiceBlender's STT layer spins up one transcriber per leg, so caller, agent, and any later-joining supervisor each get their own `stt.text` events attributed by `leg_id`. The supervisor app maps those leg ids to a friendly speaker label at capture time (`customer`, the agent's name, or `Supervisor` if it came from a listening supervisor) and buffers the line in memory under the caller's leg id.

15. With an `on call · <agent>` row visible, click anywhere on the row (outside the Listen/Whisper buttons) — the **transcript modal** opens in `LIVE` mode. Speak from both ends and watch lines appear in real time, colour-coded per speaker (cyan = customer, green = agent, amber = supervisor whispers). Press `Esc`, click the **✕**, or click outside the modal to close. The modal stays in sync across snapshots, so you can leave it open through the whole conversation.
16. Hang up the call. The modal silently switches to `ARCHIVE` mode pointed at the now-saved log entry — no blanking. Close it.
17. Open any past row in the **Call Log** table at the bottom of the panel — the same modal opens in `ARCHIVE` mode populated from the persisted transcript. Switch to `CALL_LOG_BACKEND=redis` and the saved transcript survives restarts.

Finals only: partials are disabled (`partial: false`) so each line appears once it has been committed by the provider. There's no in-place flicker; if you want a "typing" feel you can flip `Partial: true` in [`bridgeAndMonitor`'s `room.STT()`](main.go) and adjust the modal to update existing lines by id.


