# ptt — browser push-to-talk

A walkie-talkie built on VoiceBlender. Users sign in with a **username and
password**, create **public or private channels**, **watch one or more channels at
once**, and **talk by holding a push-to-talk button**. Audio travels over WebRTC
and is created strictly on-demand: there is **one speaker at a time** per channel
(floor control), and the media legs — plus the VoiceBlender room that mixes them —
exist only while someone is transmitting. When a channel is quiet, nothing is
allocated on the media server.

Everything is driven over a single VoiceBlender **VSI EventStream** WebSocket
(`Client.Events`); the REST client is never used for commands. State is stored in
Redis.

## How it works

- **One VoiceBlender room per talk burst.** On a granted push-to-talk press the
  app `create_room`s, then asks the speaker's browser (un-muted) and every
  present listener's browser (muted) to offer a WebRTC leg, joining each with
  `add_leg_to_room`. On release it `delete_leg`s every leg and `delete_room`s the
  room. Role is set at join time, so no runtime mute/unmute is needed.
- **Single-speaker floor control.** While the floor is held, other presses get a
  "busy" reply. This gives one clean burst boundary that drives the on-demand
  media lifecycle.
- **Signalling** rides a per-room WebSocket (`/api/ptt/stream`): `ptt.press` /
  `ptt.release` for the floor, `webrtc.offer` / `webrtc.answer` /
  `webrtc.candidate` for media (same offer → `webrtc_offer` → answer flow and
  250 ms ICE poll as the pbx softphone), plus `presence` / `speaker` for the UI.

## Channels & activity

The **index (`/`) is your channels** — it shows every channel you watch as a
strip straight away: you **hear them all at once**, one is the **active** channel
that the PTT bar and signal meter act on, and **Talk here** switches which.
`＋ New channel` (a header link) opens a modal to **create** a channel;
`＋ Add a channel to watch` adds an existing one; `✕` stops watching one. The full
**channel browser** (list, join-by-code, delete) lives at **`/channels`**. Channel
creation, invite links, and old `/room/{id}` links all land back on the index
(`/?active=<id>`).

- **Your watched set is remembered per user, server-side** (`ptt:watch:<user>` in
  Redis), so it follows you across reloads, logins and devices — not just the
  browser it was set in. Opening a channel via the lobby or an invite link adds it
  to the set.
- **Activity feed.** Each channel keeps a capped log (`ptt:events:<room>`, newest
  first) of who **joined**, **left**, **talked**, and **rang**. The feed is
  combined across every channel you watch and labelled by channel; recent history
  is replayed when you open a channel, and new events stream in live over the
  per-room WebSocket.

## Channels & access

- **Public** channels are listed for everyone and open to join.
- **Private** channels are reachable only via a **shareable invite code / link**
  the owner copies from the channel's Invite bay. Redeeming a code (or opening
  `/join?code=…`) admits the user permanently, so they can rejoin from the browser.

## Sharing a VoiceBlender with the other examples

All the examples can point at **one** VoiceBlender instance, and its VSI stream
carries everything happening on it. `APP_ID` (default `ptt`) is what keeps them
apart:

- **Everything we create is tagged.** Rooms (`create_room`) *and* browser legs
  (`webrtc_offer`) are created with `app_id: "ptt"`, so every event they emit
  carries it.
- **VoiceBlender filters the stream for us.** The VSI socket is opened with
  `WithAppFilter("^ptt$")`, so the server only ever sends us our own events. The
  event loop has nothing to sort out — anything that arrives is ours.
- **Room ids are namespaced** as `ptt-<room>`. `app_id` separates *events*, but
  room ids are still one flat global space on the server, so the prefix stops two
  examples ever colliding on a room.

Tag everything, or lose it: the filter drops events whose `app_id` doesn't match,
and an untagged leg's `app_id` is empty. If a leg were created without its
`app_id`, its `leg.disconnected` would be filtered out and a dropped browser leg
would never be cleaned up. This is why `floor.go` passes `AppID` on **both**
`create_room` and `webrtc_offer`.

> Tagging WebRTC legs needs VoiceBlender ≥ the `app_id`-on-`webrtc_offer` change
> and voiceblender-go **v0.11.1** (which adds `WebRTCOfferRequest.AppID` and
> `WithAppFilter`). Before that, browser legs could not be tagged at all.

Verified by running two instances (`APP_ID=ptt` and `APP_ID=demo2`) against one
VoiceBlender: each sees only its own `room.*` and `leg.*` events, and neither
observes the other's traffic at all.

## Ring the channel

A **🔔 Ring the channel** button on the channels page sends a radio-style *call
alert* to everyone present — for when you need attention and nobody is
listening. Because an alert is useless if the recipient isn't looking at the
tab, it lands three ways at once:

- an insistent **alert tone** (a 1046+1318 Hz double-chirp, twice — louder and
  longer than any roger beep, so it can't be mistaken for one),
- a pulsing **banner**: *"🔔 alice is ringing the channel"*,
- a **flashing tab title** (*"🔔 alice is calling"*), which clears as soon as you
  focus the tab.

The ringer gets the alert too, as confirmation it went out. Ringing is
**rate-limited to one per 5 seconds per person** (`ringCooldown` in
`presence.go`) — it interrupts everybody, so it must not be spammable; a
throttled ringer is told how long to wait. Alerts never leave the channel they
were sent in.

## Roger tones

When a transmission ends, everyone in the channel hears a **roger beep** (the
courtesy tone a radio sends when the operator releases PTT). It is **configured
per channel**: the owner picks one on the channel's settings bay (or when
creating the channel), it's persisted in Redis, and changing it pushes a `config`
message to everyone currently in the channel so it takes effect live.

The tones are real specs, not invented — see
[`web/static/roger.js`](./web/static/roger.js), which is the single source of
truth (the Go allow-list in `rooms.go` mirrors its keys):

| Style | Sound |
|---|---|
| `off` | no tone |
| `classic` | 880 Hz, 100 ms — the repeater "Beep" (default) |
| `boop` | 440 Hz, 100 ms |
| `honk` | 500 + 700 Hz together, 100 ms |
| `quindar` | 2475 Hz, 250 ms — NASA's Apollo **outro** tone |
| `nasa` | 2450 Hz, 200 ms — the NASA "over" beep |
| `cuckoo` | 1123 Hz/136 ms → 865 Hz/202 ms — the Baofeng two-tone |
| `nextel` | 1760 Hz ×3, 30 ms on / 30 ms off |
| `bumblebee` | 330 → 495 → 660 Hz, 100 ms each |
| `piano` | 660 + 880 Hz together, 100 ms — "Piano Chord" |
| `shootingstar` | 800 → 800 → 540 Hz, 100 ms each |
| `stardust` | 750 Hz/125 ms → 880 Hz/80 ms → 880+1200 Hz/80 ms |
| `wasp` | 660 → 500 → 385 Hz, 100 ms each |
| `tumbleweed` | 1000 → 800 → 600 Hz, 20 ms each |
| `doorbell` | 800 Hz/75 ms → 400 Hz/50 ms |
| `chirp` | 500 → 750 → 1000 Hz, 50 ms each ("Ascending") |
| `descending` | 1000 → 750 → 500 Hz, 50 ms each |

The tone is synthesized in the browser with WebAudio (like the "go ahead" beep
the speaker hears when their leg connects), so it costs no media-server time and
doesn't delay the on-demand teardown. It fires on the end-of-transmission
signal — when the channel's speaker goes from someone to nobody.

Sources for the specs: [repeater-builder courtesy tones](https://www.repeater-builder.com/tech-info/courtesy-tones.html)
and [Quindar tones](https://en.wikipedia.org/wiki/Quindar_tones).

## Login

Username **and password**. There is a single form: signing in with a username
that doesn't exist yet **creates the account** with the password you type; an
existing username has its password verified. Usernames must be 1–24 characters
of letters, digits, `-`, `_`, or `.`; passwords must be 8–72 characters.

Passwords are never stored in plaintext — only a **bcrypt** hash (salt and cost
baked in) is kept in Redis, in the `ptt:users` hash. Sessions still carry only a
username.

> **Upgrading an existing deployment:** earlier builds stored `ptt:users` as a
> Redis *set* of usernames. This version uses a *hash*, so clear the old key once
> (`redis-cli DEL ptt:users`) — otherwise the account lookups return `WRONGTYPE`.
> Existing usernames have no stored password and are simply re-created on next
> login.

## Run

Needs a running VoiceBlender server and a Redis instance.

```bash
docker run -p 6379:6379 redis:7-alpine   # if you don't already have Redis

REDIS_URL=redis://localhost:6379/0 \
VOICEBLENDER_URL=http://localhost:8080/v1 \
go run ./cmd/ptt
```

Then open http://localhost:8092 in two browser tabs, sign in as two different
usernames (each with a password — new usernames are registered on first use),
create a channel, join from both, and hold the button (or press **Space**) to talk.

See [`.env.example`](./.env.example) for all configuration. `VSI_LOG` is on by
default — watch the logs to see `create_room` / `add_leg_to_room` appear only
while a button is held, and `delete_leg` / `delete_room` fire on release.

## Docker

```bash
docker build -f Dockerfile.ptt -t ptt .
docker run --rm -p 8092:8092 \
  -e VOICEBLENDER_URL=http://host.docker.internal:8080/v1 \
  -e REDIS_URL=redis://host.docker.internal:6379/0 \
  ptt
```
