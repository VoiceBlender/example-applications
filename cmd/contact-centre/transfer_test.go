package main

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestTransferCreateRejectsDoubleForSameCall(t *testing.T) {
	r := newTransferRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}
	if _, err := r.Create("call-1", "waiting-call-1", ag.ID, ag.Name, intercomTargetAgent, "a2", "Bob", "leg-a", false); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := r.Create("call-1", "waiting-call-1", ag.ID, ag.Name, intercomTargetSupervisor, "alice", "alice", "leg-a", false); err == nil {
		t.Fatal("second Create on same call should have failed")
	}
}

func TestTransferCreateUnknownKindFails(t *testing.T) {
	r := newTransferRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}
	if _, err := r.Create("call-1", "room-1", ag.ID, ag.Name, "robot", "x", "X", "leg-a", false); err == nil {
		t.Fatal("unknown target kind should have errored")
	}
}

func TestTransferClaimAnswerIsAtomic(t *testing.T) {
	r := newTransferRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}
	tr, err := r.Create("call-1", "room-1", ag.ID, ag.Name, intercomTargetAgent, "a2", "Bob", "leg-a", false)
	if err != nil {
		t.Fatal(err)
	}
	const N = 20
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := r.ClaimAnswer(tr.ID, "leg-target"); ok {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Errorf("ClaimAnswer winners = %d, want 1", got)
	}
}

func TestTransferCancelFromRingingFreesByCall(t *testing.T) {
	r := newTransferRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}
	tr, err := r.Create("call-1", "room-1", ag.ID, ag.Name, intercomTargetAgent, "a2", "Bob", "leg-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled, ok := r.Cancel(tr.ID); !ok || cancelled.State != transferCancelled {
		t.Fatalf("Cancel from ringing should yield cancelled state; got %+v ok=%v", cancelled, ok)
	}
	if _, ok := r.Cancel(tr.ID); ok {
		t.Errorf("second Cancel should fail")
	}
	// Cancel keeps the record in the registry until Settle, so the call
	// slot is still occupied here.
	if _, err := r.Create("call-1", "room-1", ag.ID, ag.Name, intercomTargetSupervisor, "alice", "alice", "leg-a", false); err == nil {
		t.Errorf("Create after Cancel (no Settle) should fail — slot still held")
	}
	r.Settle(tr.ID)
	if _, err := r.Create("call-1", "room-1", ag.ID, ag.Name, intercomTargetSupervisor, "alice", "alice", "leg-a", false); err != nil {
		t.Errorf("Create after Settle should succeed; got %v", err)
	}
}

func TestTransferByCallAndByTarget(t *testing.T) {
	r := newTransferRegistry()
	a1 := &agent{ID: "a1", Name: "Alice"}
	a2 := &agent{ID: "a2", Name: "Bob"}
	t1, _ := r.Create("call-1", "room-1", a1.ID, a1.Name, intercomTargetAgent, "a3", "Carol", "leg-a1", false)
	t2, _ := r.Create("call-2", "room-2", a2.ID, a2.Name, intercomTargetSupervisor, "alice", "alice", "leg-a2", false)

	if got := r.ByCall("call-1"); got == nil || got.ID != t1.ID {
		t.Errorf("ByCall(call-1) = %+v, want %s", got, t1.ID)
	}
	if got := r.ByCall("call-missing"); got != nil {
		t.Errorf("ByCall(missing) = %+v, want nil", got)
	}
	if got := r.ByTarget(intercomTargetAgent, "a3"); len(got) != 1 || got[0].ID != t1.ID {
		t.Errorf("ByTarget(agent, a3) = %v, want one entry %s", got, t1.ID)
	}
	if got := r.ByTarget(intercomTargetSupervisor, "alice"); len(got) != 1 || got[0].ID != t2.ID {
		t.Errorf("ByTarget(supervisor, alice) = %v, want one entry %s", got, t2.ID)
	}
}

func TestTransferByFromAgent(t *testing.T) {
	r := newTransferRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}
	if got := r.ByFromAgent("a1"); got != nil {
		t.Errorf("ByFromAgent before Create should be nil; got %+v", got)
	}
	tr, _ := r.Create("call-1", "room-1", ag.ID, ag.Name, intercomTargetSupervisor, "alice", "alice", "leg-a", false)
	if got := r.ByFromAgent("a1"); got == nil || got.ID != tr.ID {
		t.Errorf("ByFromAgent after Create = %+v, want %s", got, tr.ID)
	}
	r.Cancel(tr.ID)
	r.Settle(tr.ID)
	if got := r.ByFromAgent("a1"); got != nil {
		t.Errorf("ByFromAgent after Cancel+Settle should be nil; got %+v", got)
	}
}

func TestTransferAttendedClaimAnswerKeepsConsulting(t *testing.T) {
	r := newTransferRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}
	tr, err := r.Create("call-1", "room-1", ag.ID, ag.Name, intercomTargetAgent, "a2", "Bob", "leg-a", true)
	if err != nil {
		t.Fatalf("Create attended: %v", err)
	}
	if tr.ConsultRoomID == "" {
		t.Fatal("attended transfer should have ConsultRoomID set")
	}
	claimed, ok := r.ClaimAnswer(tr.ID, "leg-b")
	if !ok || claimed.State != transferConsulting || claimed.TargetLegID != "leg-b" {
		t.Fatalf("attended ClaimAnswer = %+v ok=%v, want consulting + target leg recorded", claimed, ok)
	}
	// Transfer must remain in registry while consulting (byCall still mapped).
	if got := r.ByCall("call-1"); got == nil || got.ID != tr.ID || got.State != transferConsulting {
		t.Fatalf("ByCall during consulting = %+v, want consulting transfer", got)
	}
	completed, ok := r.Complete(tr.ID)
	if !ok || completed.State != transferCompleted {
		t.Fatalf("Complete attended = %+v ok=%v, want completed", completed, ok)
	}
	// ByCall must still resolve the transfer until Settle — the bridge
	// loop relies on this during the finalize hand-off window.
	if got := r.ByCall("call-1"); got == nil || got.ID != tr.ID || got.State != transferCompleted {
		t.Fatalf("ByCall during finalize = %+v, want completed transfer", got)
	}
	r.Settle(tr.ID)
	if got := r.ByCall("call-1"); got != nil {
		t.Errorf("ByCall after Settle should be nil; got %+v", got)
	}
}

func TestTransferAttendedCancelDuringConsulting(t *testing.T) {
	r := newTransferRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}
	tr, _ := r.Create("call-1", "room-1", ag.ID, ag.Name, intercomTargetAgent, "a2", "Bob", "leg-a", true)
	if _, ok := r.ClaimAnswer(tr.ID, "leg-b"); !ok {
		t.Fatal("ClaimAnswer should succeed")
	}
	cancelled, ok := r.Cancel(tr.ID)
	if !ok || cancelled.State != transferCancelled {
		t.Fatalf("Cancel from consulting = %+v ok=%v, want cancelled", cancelled, ok)
	}
	// Still visible until Settle (gives failure cleanup a window).
	if got := r.ByCall("call-1"); got == nil || got.ID != tr.ID {
		t.Errorf("ByCall during fail-cleanup = %+v, want cancelled transfer", got)
	}
	r.Settle(tr.ID)
	if got := r.ByCall("call-1"); got != nil {
		t.Errorf("ByCall after Settle should be nil; got %+v", got)
	}
}

func TestTransferBlindClaimAnswerCompletes(t *testing.T) {
	r := newTransferRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}
	tr, _ := r.Create("call-1", "room-1", ag.ID, ag.Name, intercomTargetAgent, "a2", "Bob", "leg-a", false)
	if tr.ConsultRoomID != "" {
		t.Fatal("blind transfer should not have ConsultRoomID")
	}
	claimed, ok := r.ClaimAnswer(tr.ID, "leg-b")
	if !ok || claimed.State != transferCompleted {
		t.Fatalf("blind ClaimAnswer = %+v ok=%v, want completed", claimed, ok)
	}
	// Stays in the registry until Settle (the bridge-loop safety net
	// needs it visible during finalize).
	if got := r.ByCall("call-1"); got == nil || got.State != transferCompleted {
		t.Fatalf("ByCall after blind ClaimAnswer = %+v, want completed transfer", got)
	}
	r.Settle(tr.ID)
	if got := r.ByCall("call-1"); got != nil {
		t.Errorf("ByCall after Settle should be nil; got %+v", got)
	}
	// Complete on an already-completed/settled transfer is a no-op.
	if _, ok := r.Complete(tr.ID); ok {
		t.Errorf("Complete on settled transfer should fail")
	}
}
