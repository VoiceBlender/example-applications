package main

import (
	"context"
	"testing"
)

// The "Radio bandwidth" option, chosen at channel creation, enables the fixed
// 500–2000 Hz band-limit filter on the room.
func TestCreateRoomRadio(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()

	on, err := a.rooms.create(ctx, "OnAir", "alice", "public", "classic", true)
	if err != nil {
		t.Fatalf("create radio room: %v", err)
	}
	t.Cleanup(func() { a.rooms.delete(ctx, on.ID) })
	if !on.Radio || on.RadioLow != radioLow || on.RadioHigh != radioHigh {
		t.Fatalf("radio room = %+v, want on %d-%d", on, radioLow, radioHigh)
	}
	if got, ok := a.rooms.get(on.ID); !ok || !got.Radio || got.RadioLow != 500 || got.RadioHigh != 2000 {
		t.Fatalf("persisted radio room = %+v ok=%v", got, ok)
	}

	off, err := a.rooms.create(ctx, "HiFi", "alice", "public", "classic", false)
	if err != nil {
		t.Fatalf("create non-radio room: %v", err)
	}
	t.Cleanup(func() { a.rooms.delete(ctx, off.ID) })
	if off.Radio || off.RadioLow != 0 || off.RadioHigh != 0 {
		t.Fatalf("non-radio room = %+v, want radio off", off)
	}
}
