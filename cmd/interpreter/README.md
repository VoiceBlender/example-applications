# interpreter — live simultaneous interpreter

Two people, two languages, one conversation. Each opens a shared link, picks the
language they speak, and talks over WebRTC. **Each hears only the other's
translated voice** — the original speech is never mixed through — with live
captions showing the original and the translation side by side.

Latency between the participants is the design constraint everything else bends
to. Everything is driven over a single VoiceBlender **VSI EventStream**
WebSocket (`Client.Events`); the REST client is never used for commands. There
is no login and no Redis: sessions are transient and live in memory.

## How it works

```
        browser A ──WebRTC──┐                    ┌──WebRTC── browser B
                            ▼                    ▼
                 ┌──────────────────────────────────────┐
                 │  room  interpreter-<session>         │
                 │  leg A  role=pa      leg B  role=pb  │
                 │                                      │
                 │  routing matrix {"pa":[], "pb":[]}   │
                 │  → neither hears the other's raw     │
                 └───┬──────────────────────────┬───────┘
                     │ per-leg ingress tap      │
              leg_stt_start(A)           leg_stt_start(B)
                     │                          │
                translate A→B              translate B→A
                     │                          │
              leg_tts → private injection into the OTHER leg
```

Three VoiceBlender primitives carry the whole design:

- **The routing matrix silences the direct path.** `room_routing_set` maps a
  listener's role to the source roles it may hear. An *empty but present* list
  means that listener hears nothing from the mix, so both roles get one. This is
  the load-bearing call: a leg with no role, or a role with no row in the matrix,
  falls back to **full mesh** — and then the participants hear each other
  untranslated, the one outcome the app exists to prevent.
- **`leg_tts` is a private channel.** It injects audio into one leg's
  mixed-minus-self output; nobody else in the room hears it. That is what lets
  both participants share a room while hearing completely different audio.
- **Per-leg STT taps only that leg's own microphone**, before mixing and before
  routing — so a speaker's transcript never contains their peer, even though both
  legs are in one room.

## The latency trick

The obvious cascade waits for the speaker to stop, then transcribes, translates
and synthesizes: a second or more of dead air after every sentence. Instead:

```
speaker still talking
  │
  ├─ eager_end_of_turn ──► translate ──► leg_tts_preflight   (audio buffered,
  │   ("probably done")                                       nothing played)
  │
  ├─ turn_resumed ───────► leg_tts_discard   (they carried on; throw it away)
  │
  └─ end_of_turn ────────► leg_tts_commit    (plays immediately — the audio
                                              already exists, so this costs no
                                              round trip to the TTS vendor)
```

The expensive half happens **during the speaker's last few hundred
milliseconds**. What is left on the critical path is a commit and a mixer tick.
When the guess is wrong — they resumed, or revised their wording — the staged
audio is discarded and the plain path runs, so correctness never depends on the
speculation being right.

| Hop | Expected |
|---|---|
| Browser capture + Opus + WebRTC in | 20–40 ms |
| Ingress tap → Deepgram Flux (80 ms framing) | 80–120 ms |
| Flux `eager_end_of_turn` decision | 150–300 ms after speech ends |
| Translate (DeepL) | 80–150 ms — **off the critical path** |
| ElevenLabs Flash synthesis (preflight) | 100–200 ms — **off the critical path** |
| `end_of_turn` → `leg_tts_commit` → first frame | ~0 ms upstream, ~20 ms mixer |
| Mix → Opus → WebRTC out | 20–40 ms |
| **Speaker stops → listener hears** | **~250–450 ms** |

Set `STT_EAGER_EOT_THRESHOLD=0` to turn the speculation off and hear the
difference — the same path then costs roughly 700–1100 ms.

## Providers

| Hop | Default | Why |
|---|---|---|
| STT | `deepgram_flux` | The only provider that reports **eager** end of turn — the whole low-latency design hangs off it. Covers ten languages. No `language` parameter: it takes `language_hint`, and hints *bias* detection rather than pinning it. |
| STT (fallback) | `speechmatics` | Used automatically for languages Flux cannot do. Pins the language rather than biasing it, at the cost of the eager path. Chosen per participant — see *Languages* below. |
| MT | `deepl` | VoiceBlender's STT providers transcribe but do **not** translate, so this hop lives in the app behind a small `translator` interface (see `translate.go`). `TRANSLATE_PROVIDER=none` runs the whole media path with no key. |
| TTS | `elevenlabs` / `eleven_flash_v2_5` | Flash is the low-latency model, and it is multilingual — so one voice per speaker covers every language here. Which voice is chosen by the speaker's declared gender; see *Voices* below. |

Adding another MT backend is one file and one `case` in `newTranslator`; nothing
else in the app knows which one is in use.

## Edge cases worth knowing about

- **Staged utterances are capped at 3 per leg**, and VoiceBlender *refuses* the
  fourth rather than evicting one — so the app evicts its own oldest first.
- **Concurrent `leg_tts` on one leg overlap audibly.** There is no queueing and
  no interrupt, so the app tracks the in-flight `tts_id` and `leg_play_stop`s it
  before starting the next. Talk over a translation and you will hear it cut.
- **A revised transcript** at `end_of_turn` discards the staged audio and
  re-translates, rather than speaking words that were never said.
- **Changing language mid-session** restarts transcription on that leg and drops
  anything staged from the old language.
- **One participant present** starts no transcription at all — there is nobody to
  translate for, and it would just burn STT minutes.
- **An idle session ends itself** after `SESSION_IDLE_TIMEOUT`, and every session
  after `SESSION_MAX_DURATION`. Both browsers are told why before the audio
  stops. See *Access and cost control* above.
- **Comfort noise** (`COMFORT_NOISE_ENABLED`, server-side, on by default) is what
  you hear between translations, because each participant's personal mix is
  otherwise silent. It reads as a phone line; turn it off server-side for
  complete silence.

## Languages — and which engine transcribes them

Deepgram Flux is the fastest transcriber here — it is the only one that reports
**eager** end of turn, which is what the whole low-latency design hangs off — but
`flux-general-multi` covers exactly ten languages:

```
en  es  fr  de  hi  ru  pt  ja  it  nl
```

and rejects the connection outright (`400 INVALID_PARAMETER`) for anything else.
Offering an eleventh would mean that participant is silently never transcribed:
their side of the conversation does nothing while the other direction keeps
working.

So the transcriber is chosen **per participant, by their language** — not once
per deployment:

```
STT_PROVIDER=deepgram_flux            # preferred: fast, ten languages
STT_FALLBACK_PROVIDER=speechmatics    # covers the rest
DEEPGRAM_API_KEY=...
SPEECHMATICS_API_KEY=...
```

With both keys set, an English↔Polish call runs **both engines at once**:

```
stt started participant=alice lang=en provider=deepgram_flux
stt started participant=bob   lang=pl provider=speechmatics
```

Alice keeps Flux's eager path, so English→Polish stays fast; only the Polish→
English direction pays the extra turn of latency that Speechmatics costs. One
participant's language never drags the other off the fast engine.

**Polish, Ukrainian, Turkish, Korean and Mandarin need `SPEECHMATICS_API_KEY`.**
Without it they are simply not offered — the selector, the join and
`leg_stt_start` all agree, so nobody can pick a language that will transcribe
nothing. The startup log says exactly what is routed where:

```
languages offered provider=deepgram_flux count=10 codes=en,es,fr,de,it,pt,nl,hi,ru,ja
languages offered provider=speechmatics count=5  codes=pl,uk,tr,ko,zh
```

Note the asymmetry: translating **to** a language only needs DeepL and
ElevenLabs, both of which cover all fifteen. It is transcribing **from** one that
constrains the list.

Each language carries a separate code per hop in [`langs.go`](./langs.go),
because every provider spells them differently and a wrong one fails quietly:
Mandarin is `cmn` to Speechmatics but `ZH` to DeepL, and English and Portuguese
need a regional variant (`EN-GB`, `PT-PT`) as a DeepL *target* or the request is
rejected.

## Voices

Each participant picks **Female**, **Male**, or **Prefer not to say** — the
person who starts the session chooses on the landing page, and whoever opens the
invite link is asked before they join — and their words are spoken **to the
other side** in the matching voice. The voice follows
the *speaker*: when Alice talks, Bob hears Alice's words in Alice's voice. That
matters more than it sounds — a man's words arriving in a woman's voice is
jarring, and occasionally misleading, to sit through.

| Env | Default | |
|---|---|---|
| `TTS_VOICE_FEMALE` | Rachel | |
| `TTS_VOICE_MALE` | Adam | |
| `TTS_VOICE_DEFAULT` | Rachel | What "prefer not to say" is heard in |

These are ElevenLabs **voice IDs**, not names. There is no stock voice that is
convincingly androgynous, so `TTS_VOICE_DEFAULT` simply falls back to the female
one — set it to whatever suits your deployment rather than reading intent into
that choice.

Both the language and the voice ride the signalling socket's query string, so a
participant is seated with their choices from the first frame rather than at the
defaults with a correction to follow — otherwise the opening words of a call are
transcribed in the wrong language.

The setting can be changed mid-call. Unlike a language change it does **not**
restart transcription — only the synthesis of that person's translated words
changes — but anything already staged for the peer is discarded, so a single
sentence never arrives in the wrong voice. Because the model is multilingual, the
voice does not change when someone switches language.

## Access and cost control

Two features exist for the same reason: this app spends money per minute of
wall-clock time, not per action. Two connected legs mean two Deepgram streams
billed continuously, whether or not anyone is talking.

### Login

One shared static credential guards the console:

```
AUTH_USERNAME=interpreter
AUTH_PASSWORD=something-long
```

**Leaving `AUTH_PASSWORD` empty disables the login entirely.** That is the
default, so local trials stay friction-free — but on anything reachable it means
every passer-by can start a session on your budget.

It is a gate, not an identity system: both participants sign in with the same
credential and still choose their own display name for the call. Page loads
redirect to the form; `fetch` calls and the signalling WebSocket get a 401
instead, so a JS client never tries to parse the login page as its response.
Credentials are compared with `crypto/subtle`, and the token is invalidated
server-side on sign-out rather than merely cleared from the browser.

### Session limits

| Setting | Default | What it stops |
|---|---|---|
| `SESSION_IDLE_TIMEOUT` | `5m` | Nobody has spoken for a while → end the session. **The one that actually saves money** — it catches the tab someone walked away from. |
| `SESSION_MAX_DURATION` | `1h` | A hard ceiling on any one conversation, for when something keeps making noise into the microphone and "activity" never stops. |
| `SESSION_EMPTY_TIMEOUT` | `5m` | A link nobody opened, or one everyone has left. |

Any of them set to `0` is disabled. Durations are Go strings (`90s`, `10m`,
`1h30m`).

When a limit is hit the app stops the transcribers **first** — they are the meter
that is running — then discards staged audio, tells both browsers why, deletes
the legs and the room, and retires the link. The page shows what happened rather
than just going silent, and says up front that the session will end itself.

The clock the maximum-duration cap measures runs from when interpreting actually
begins (both legs up), not from when the link was minted. The idle clock starts
at join and resets on every transcript or turn event — so a session whose media
never came up at all (a denied microphone, a failed ICE) is reaped too.

**Shutdown counts as a limit.** On `SIGINT`/`SIGTERM` the app ends every live
session before exiting. A leg left behind on the media server keeps streaming
audio to the STT vendor, and with the app gone there is nobody left to stop it —
so killing the process must not leave a meter running.

## Sharing a VoiceBlender with the other examples

Same rules as the other apps. `APP_ID` (default `interpreter`) keeps them apart:
rooms *and* browser legs are created with it, the VSI socket is opened with
`WithAppFilter("^interpreter$")`, and room ids are namespaced `interpreter-<id>`.
Tag everything, or lose it — an untagged leg's `leg.connected` would be filtered
out and transcription would never start.

## Run

Needs a running VoiceBlender server. No Redis.

```bash
VOICEBLENDER_URL=http://localhost:8080/v1 \
DEEPGRAM_API_KEY=... \
ELEVENLABS_API_KEY=... \
DEEPL_API_KEY=... \
AUTH_PASSWORD=letmein \
go run ./cmd/interpreter
```

Drop `AUTH_PASSWORD` to run without a login.

Open <http://localhost:8093> in two browsers (or two profiles — each needs its
own microphone permission). In the first, pick your name, language and voice and
press **Start**. Paste the invite link into the second: it asks the same three
questions before joining, so both sides choose for themselves. Then talk.

To try it with no MT account at all:

```bash
TRANSLATE_PROVIDER=none go run ./cmd/interpreter
```

Every hop still runs; you just hear your own words read back in the other seat's
voice, which is enough to confirm the routing, the STT and the TTS all work.

See [`.env.example`](./.env.example) for the full configuration.

**Microphone note:** browsers only release a microphone in a *secure context*.
`http://localhost` qualifies; any other plain-HTTP hostname does not, so reaching
this over a network needs HTTPS.

## Docker

```bash
docker build -f Dockerfile.interpreter -t interpreter .
docker run --rm -p 8093:8093 \
  -e VOICEBLENDER_URL=http://host.docker.internal:8080/v1 \
  -e DEEPGRAM_API_KEY=... -e ELEVENLABS_API_KEY=... -e DEEPL_API_KEY=... \
  interpreter
```

## Tests

```bash
go test ./cmd/interpreter/
```

Auth and the session limits have their own tests (`auth_test.go`,
`timeout_test.go`) — including that a WebSocket upgrade gets a 401 rather than a
redirect, that the post-login `next` cannot become an open redirect, and that
teardown stops transcription *before* deleting the legs.

The turn state machine runs against a fake VSI recorder (`fake_vsi_test.go`), so
the interesting cases — eager→commit, resume→discard, revised text, the staging
cap, barge-in, commit failure — are all covered without a media server. The
routing matrix has its own test asserting it serializes as `[]` and never `null`,
because those two differ by one character on the wire and `null` silently means
everyone hears everyone.
