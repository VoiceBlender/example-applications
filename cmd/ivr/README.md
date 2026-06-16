# Example: Company IVR

A multi-department IVR (Interactive Voice Response) that answers inbound calls, greets the caller with a TTS prompt, presents a DTMF menu, and routes the caller to a department room.

## Call flow

```
Inbound call
  └─ leg.ringing      → answer the call
  └─ leg.connected    → "Thank you for calling Acme Corp. Please hold…"
  └─ tts.finished     → main menu prompt
  └─ dtmf.received
       1 → Sales queue
       2 → Support queue
       3 → Billing queue
       0 → Operator queue
       9 → Repeat menu
       * → Goodbye → hang up
       ? → "Invalid option, please try again" (max 3 attempts, then goodbye)
  └─ leg.disconnected → cleanup
```

Once a caller is routed, they are added to the department's persistent room where agents can join to handle the call. Hold music is played in the room while they wait.

## Prerequisites

- A running [VoiceBlender](https://github.com/VoiceBlender/voiceblender) instance
- An [ElevenLabs](https://elevenlabs.io) API key for TTS prompts, unless already configured in VoiceBlender

Events and commands both travel over a single VoiceBlender VSI WebSocket connection, so no public webhook URL, HTTP tunnel, or REST round-trips are needed.

## Configuration

| Environment variable | Required | Default | Description |
|----------------------|----------|---------|-------------|
| `ELEVENLABS_API_KEY` | no | — | ElevenLabs API key for TTS; omit if already configured in VoiceBlender |
| `VOICEBLENDER_URL` | no | `http://localhost:8080/v1` | VoiceBlender API base URL (the VSI WebSocket is derived from this) |
| `TTS_VOICE` | no | `Rachel` | ElevenLabs voice name |
| `COMPANY_NAME` | no | `Acme Corp` | Company name spoken in the greeting |
| `DEEPGRAM_API_KEY` | no | — | Deepgram API key for the AI agent on the operator queue (digit `0`) |
| `ANSWER_CODECS` | no | _(server default)_ | Comma-separated codec preference for the 183 (early media) and 200 (answer) SDPs, e.g. `opus,PCMA,PCMU`. The first codec in the list that the caller also offered is selected; if none match, VoiceBlender's default order is used. |

## Running

From the repository root:

```bash
export ELEVENLABS_API_KEY=your_key_here

go run ./cmd/ivr
```

VoiceBlender must be configured to send inbound SIP calls to the same instance. The IVR opens a VSI WebSocket to receive events and creates the four department rooms (`sales`, `support`, `billing`, `operator`) on startup if they do not already exist.

## Architecture

```
SIP carrier
    │  inbound INVITE
    ▼
VoiceBlender ◀─── VSI WebSocket (full-duplex) ────▶ IVR server  (this program)
                  events:  leg.ringing,
                           dtmf.received,
                           tts.finished, …
                  commands: answer_leg, leg_tts,
                            add_leg_to_room,
                            room_play_start, …
```

A single `*voiceblender.EventStream` carries both directions: inbound frames are piped into the client's hub (so `Subscribe` works) and outbound commands are sent via the stream's typed methods (`AnswerLeg`, `LegTTS`, `AddLegToRoom`, `RoomPlayStart`, …). No REST round-trips are performed at runtime.

Each active call is represented by a `call` struct that holds the current IVR state (`greeting → menu → routed/goodbye`). The VSI event stream is consumed in a single loop that dispatches each event to a goroutine; all state transitions are protected by a per-call mutex.
