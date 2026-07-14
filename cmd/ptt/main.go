// Command ptt is a browser push-to-talk (walkie-talkie) app built on VoiceBlender.
//
// Users sign in with a username only, create public or private rooms, join a
// room, and talk by holding a push-to-talk button. Audio flows over WebRTC and
// only while someone is actively transmitting: there is exactly one speaker at a
// time (single-speaker floor control), and the WebRTC legs — plus the
// VoiceBlender room that mixes them — are created on the press and torn down on
// the release. When a room is quiet, nothing is allocated on the media server.
//
// Like the other examples, every VoiceBlender command is issued over a single
// VSI WebSocket (Client.Events): inbound frames are piped into the client hub so
// Subscribe works; outbound commands (create_room, webrtc_offer, add_leg_to_room,
// delete_leg, delete_room, …) go through the same *EventStream. The REST client
// is never used for commands.
//
// Environment variables:
//
//	VOICEBLENDER_URL   VoiceBlender base URL (default http://localhost:8080/v1)
//	REDIS_URL          Redis connection URL (required), e.g. redis://localhost:6379/0
//	LISTEN_ADDR        Web console listen address (default :8092)
//	APP_ID             Tags this app's rooms and scopes the events it acts on,
//	                   so several examples can share one VoiceBlender (default ptt)
//	VSI_LOG            Log every VSI wire frame (default on; 0/false/off/no to silence)
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	voiceblender "github.com/VoiceBlender/voiceblender-go"
)

// app holds the shared PTT resources.
type app struct {
	client *voiceblender.Client
	// stream is the current VSI WebSocket (events + commands). It's swapped on
	// reconnect, so read it via vsi() — never touch the field directly.
	stream atomic.Pointer[voiceblender.EventStream]
	log    *slog.Logger

	// appID tags everything this demo creates on VoiceBlender and scopes the
	// events it acts on. Several examples share one VoiceBlender instance, so
	// the stream carries their traffic too — see runEventLoop.
	appID string

	store    *redisStore
	sessions *sessionStore     // username-only browser sessions
	rooms    *roomRegistry     // durable room metadata + membership
	presence *presenceRegistry // live in-memory presence (who is in which room)
	floor    *floorManager     // single-speaker floor control + VB orchestration
	hub      *webHub           // lobby snapshot fan-out
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	baseURL := envOr("VOICEBLENDER_URL", "http://localhost:8080/v1")
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		log.Error("REDIS_URL is required")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	store, err := newRedisStore(ctx, redisURL)
	if err != nil {
		log.Error("connect redis", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	a := &app{
		client: voiceblender.New(voiceblender.WithBaseURL(baseURL)),
		log:    log,
		appID:  envOr("APP_ID", "ptt"),
		store:  store,
		hub:    newWebHub(),
	}
	a.sessions = newSessionStore(store)
	a.rooms = newRoomRegistry(store)
	a.rooms.notify = a.notifyChanged
	a.presence = newPresenceRegistry()
	a.floor = newFloorManager(a)

	// Restore sessions and rooms from Redis so a restart doesn't sign everyone
	// out or lose their rooms.
	a.sessions.load(ctx)
	if err := a.rooms.load(ctx); err != nil {
		log.Error("load rooms", "error", err)
		os.Exit(1)
	}

	// VSI WebSocket (events + commands). Several examples can share one
	// VoiceBlender, so ask the server to send us only our own app's events —
	// everything we create (rooms and WebRTC legs alike) is tagged with app_id,
	// so nothing of ours is lost to the filter and nothing of theirs reaches us.
	streamOpts := []voiceblender.EventStreamOption{
		voiceblender.WithAppFilter(a.appFilter()),
	}
	// Frame logging is on by default; set VSI_LOG=0 to silence it.
	if vsiFrameLogEnabled() {
		streamOpts = append(streamOpts, voiceblender.WithFrameLogger(a.logVSIFrame))
		log.Info("VSI frame logging enabled (set VSI_LOG=0 to disable)")
	}
	// manageStream connects (retrying) and reconnects if the stream drops. Block
	// until the first connect so a.vsi() is live before the console starts.
	ready := make(chan struct{})
	go a.manageStream(ctx, streamOpts, ready)
	select {
	case <-ready:
	case <-ctx.Done():
		return
	}

	srv := &http.Server{Addr: envOr("LISTEN_ADDR", ":8092"), Handler: a.serveHTTP()}
	go func() {
		log.Info("push-to-talk console", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "error", err)
			cancel()
		}
	}()
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("PTT ready", "base_url", baseURL, "app_id", a.appID, "rooms", a.rooms.count())

	a.runEventLoop(ctx)
}

// vsi returns the current VSI stream. It is non-nil once main() has passed the
// first-connect barrier, and is only ever swapped (never cleared) afterwards.
func (a *app) vsi() *voiceblender.EventStream { return a.stream.Load() }

// manageStream keeps a live VSI connection: connect (retrying with backoff),
// pump events until the stream errors, then reconnect — so the app survives a
// VoiceBlender restart or a dropped socket instead of exiting.
func (a *app) manageStream(ctx context.Context, opts []voiceblender.EventStreamOption, ready chan struct{}) {
	backoff := time.Second
	first := true
	for ctx.Err() == nil {
		stream, err := a.client.Events(ctx, opts...)
		if err != nil {
			a.log.Warn("VSI connect failed, retrying", "error", err, "retry_in", backoff.String())
			if !sleepCtx(ctx, backoff) {
				return
			}
			if backoff < 15*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		a.stream.Store(stream)
		a.log.Info("VSI connected")
		if first {
			first = false
			close(ready)
		}
		// Pump events + demux command replies until the stream drops.
		err = stream.PipeTo(ctx, a.client)
		_ = stream.Close()
		if ctx.Err() != nil {
			return
		}
		a.log.Warn("VSI stream dropped, reconnecting", "error", err)
		if !sleepCtx(ctx, time.Second) {
			return
		}
	}
}

// sleepCtx sleeps for d or until ctx is cancelled; returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// runEventLoop consumes the VSI event stream. The only event the PTT app acts on
// is LegDisconnected: a media leg dropping (e.g. ICE failure) should clean up the
// burst it belonged to, exactly as a browser-driven release would.
//
// Several examples share one VoiceBlender instance, but the stream is filtered
// server-side to our app_id (see manageStream), so everything arriving here is
// already ours — there is nothing to sort out in the loop.
func (a *app) runEventLoop(ctx context.Context) {
	sub := a.client.Subscribe()
	defer sub.Close()

	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			return
		}
		if e, ok := ev.(*voiceblender.LegDisconnectedEvent); ok {
			go a.floor.legGone(e.LegID)
		}
	}
}

// appFilter is the VSI app_id filter for this app: an anchored, literal match on
// our app id, so a neighbouring example called e.g. "ptt-staging" can't leak in.
func (a *app) appFilter() string { return "^" + regexp.QuoteMeta(a.appID) + "$" }

// vbRoom namespaces a PTT room id into the VoiceBlender room id. app_id keeps
// the *events* separate, but room ids are still one flat global space on the
// server, so prefixing keeps this demo from colliding with a room another
// example happens to name the same.
func (a *app) vbRoom(roomID string) string { return a.appID + "-" + roomID }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// logVSIFrame logs one raw VSI wire frame. dir is "send" (a command we issued)
// or "recv" (an event or command reply from the server).
func (a *app) logVSIFrame(dir string, data []byte) {
	var env struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &env)
	a.log.Info("vsi", "dir", dir, "type", env.Type, "frame", string(data))
}

// vsiFrameLogEnabled reports whether raw VSI frame logging is on. Default on;
// VSI_LOG=0/false/off/no disables it.
func vsiFrameLogEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VSI_LOG"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// isVSINotFound reports whether err is a VSI error with code 404.
func isVSINotFound(err error) bool {
	e, ok := err.(*voiceblender.VSIError)
	return ok && e.Code == 404
}

// isVSIConflict reports whether err is a VSI error with code 409.
func isVSIConflict(err error) bool {
	e, ok := err.(*voiceblender.VSIError)
	return ok && e.Code == 409
}
