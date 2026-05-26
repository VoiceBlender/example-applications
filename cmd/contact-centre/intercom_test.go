package main

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestIntercomCreateRejectsDuplicate(t *testing.T) {
	r := newIntercomRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}

	ic, err := r.Create(ag, intercomTargetSupervisor, "alice", "alice", "leg-1")
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if ic.State != intercomRinging {
		t.Errorf("state = %q, want ringing", ic.State)
	}
	if _, err := r.Create(ag, intercomTargetSupervisor, "alice", "alice", "leg-1"); err == nil {
		t.Fatalf("second Create should have failed (agent already in intercom)")
	}
}

func TestIntercomClaimAnswerIsAtomic(t *testing.T) {
	r := newIntercomRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}
	ic, err := r.Create(ag, intercomTargetSupervisor, "alice", "alice", "leg-1")
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
			if _, ok := r.ClaimAnswer(ic.ID, "sup-leg"); ok {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Errorf("ClaimAnswer winners = %d, want exactly 1", got)
	}
}

func TestIntercomRejectFromRinging(t *testing.T) {
	r := newIntercomRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}
	ic, err := r.Create(ag, intercomTargetSupervisor, "alice", "alice", "leg-1")
	if err != nil {
		t.Fatal(err)
	}
	prev, ok := r.Reject(ic.ID)
	if !ok {
		t.Fatalf("Reject from ringing should succeed")
	}
	if prev.State != intercomEnded {
		t.Errorf("post-reject state = %q, want ended", prev.State)
	}
	// Second reject is a no-op.
	if _, ok := r.Reject(ic.ID); ok {
		t.Errorf("second Reject should fail")
	}
	// And the byAgent slot must be freed so the agent can intercom again.
	if _, err := r.Create(ag, intercomTargetSupervisor, "alice", "alice", "leg-2"); err != nil {
		t.Errorf("Create after Reject should succeed; got %v", err)
	}
}

func TestIntercomEndFreesByAgent(t *testing.T) {
	r := newIntercomRegistry()
	ag := &agent{ID: "a1", Name: "Alice"}
	ic, err := r.Create(ag, intercomTargetSupervisor, "alice", "alice", "leg-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.ClaimAnswer(ic.ID, "sup-leg"); !ok {
		t.Fatal("ClaimAnswer failed")
	}
	if _, ok := r.End(ic.ID); !ok {
		t.Fatal("End failed")
	}
	if r.ByAgent("a1") != nil {
		t.Errorf("ByAgent should be nil after End")
	}
}

func TestIntercomAgentTargetRoundtrip(t *testing.T) {
	// Verify the agent-target kind is preserved end-to-end through
	// Create → ClaimAnswer → activeForTarget lookups (used by the
	// callee-disconnect cleanup path).
	r := newIntercomRegistry()
	caller := &agent{ID: "a-caller", Name: "Alice"}
	calleeID := "a-callee"

	ic, err := r.Create(caller, intercomTargetAgent, calleeID, "Bob", "caller-leg")
	if err != nil {
		t.Fatalf("Create agent-target: %v", err)
	}
	if ic.TargetKind != intercomTargetAgent || ic.Target != calleeID || ic.TargetName != "Bob" {
		t.Errorf("unexpected fields: kind=%q target=%q name=%q", ic.TargetKind, ic.Target, ic.TargetName)
	}
	got := r.activeForTarget(intercomTargetAgent, calleeID)
	if len(got) != 1 || got[0].ID != ic.ID {
		t.Errorf("activeForTarget(agent, %s) = %v, want one entry %s", calleeID, got, ic.ID)
	}
	// Wrong kind shouldn't match.
	if got := r.activeForTarget(intercomTargetSupervisor, calleeID); len(got) != 0 {
		t.Errorf("activeForTarget(supervisor, %s) = %v, want empty", calleeID, got)
	}
	if _, ok := r.ClaimAnswer(ic.ID, "callee-leg"); !ok {
		t.Fatal("ClaimAnswer failed")
	}
	if cur := r.Get(ic.ID); cur == nil || cur.CalleeLeg != "callee-leg" {
		t.Errorf("Get after answer: %+v", cur)
	}
}

func TestSupervisorRegistryUsernames(t *testing.T) {
	r := newSupervisorRegistry()
	a := &supervisorSession{}
	b := &supervisorSession{}
	c := &supervisorSession{}
	r.Add(&supervisorPresence{Session: a, Username: "alice"})
	r.Add(&supervisorPresence{Session: b, Username: "bob"})
	r.Add(&supervisorPresence{Session: c, Username: "alice"})

	got := r.UsernamesOnline()
	want := []string{"alice", "bob"}
	if len(got) != len(want) {
		t.Fatalf("UsernamesOnline = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("UsernamesOnline[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Sessions("alice") covers both a and c.
	if got := len(r.Sessions("alice")); got != 2 {
		t.Errorf("Sessions(alice) = %d, want 2", got)
	}
	r.Remove(c)
	if got := len(r.Sessions("alice")); got != 1 {
		t.Errorf("after Remove, Sessions(alice) = %d, want 1", got)
	}
}
