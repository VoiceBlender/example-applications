package main

import (
	"context"
	"time"
)

// Event kinds recorded in a channel's activity log.
const (
	eventJoin  = "join"
	eventLeave = "leave"
	eventTalk  = "talk"
	eventRing  = "ring"
)

// recordEvent appends an activity event to a room's log and pushes it live to
// everyone currently in the room, so each watcher's combined feed updates in
// real time. Persistence is best-effort — a Redis hiccup must not break the live
// path — so a store error is logged, not surfaced.
func (a *app) recordEvent(roomID, actor, kind string) {
	name := roomID
	if room, ok := a.rooms.get(roomID); ok {
		name = room.Name
	}
	ev := channelEvent{
		Room:  roomID,
		Name:  name,
		Actor: actor,
		Kind:  kind,
		At:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := a.store.AppendEvent(context.Background(), ev); err != nil {
		a.log.Warn("record event", "room", roomID, "kind", kind, "error", err)
	}
	msg := map[string]any{"type": "event", "event": ev}
	for _, m := range a.presence.membersInRoom(roomID) {
		m.send(msg)
	}
}

// recentEvents returns a room's stored activity log, newest first (empty on
// error). Sent to a browser when it opens the channel so the feed shows what
// happened before it arrived.
func (a *app) recentEvents(roomID string) []channelEvent {
	evs, err := a.store.LoadEvents(context.Background(), roomID, maxEvents)
	if err != nil {
		a.log.Warn("load events", "room", roomID, "error", err)
		return nil
	}
	return evs
}
