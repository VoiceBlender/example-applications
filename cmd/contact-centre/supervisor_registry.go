package main

import (
	"sort"
	"sync"
)

// supervisorPresence is one connected supervisor WebSocket — the
// session struct (carrying the WebRTC leg id), the username derived
// from the auth session, and a non-blocking outbox the intercom
// pump uses to deliver ad-hoc events like incoming-call modals.
type supervisorPresence struct {
	Session  *supervisorSession
	Username string
	Outbox   chan<- any
}

// supervisorRegistry tracks every currently-connected supervisor WS.
// Two things use it:
//   - the agent panel needs a deduplicated list of online usernames
//     to populate its "Call supervisor" dropdown;
//   - the intercom flow needs to fan messages out to every session of
//     a particular username (multiple browser tabs = same username).
type supervisorRegistry struct {
	mu       sync.Mutex
	sessions map[*supervisorSession]*supervisorPresence
	notify   func() // invoked on every add/remove so dependent snapshots refresh
}

func newSupervisorRegistry() *supervisorRegistry {
	return &supervisorRegistry{sessions: make(map[*supervisorSession]*supervisorPresence)}
}

// onChange installs the change notifier (typically a.reg.NotifyChanged).
func (r *supervisorRegistry) onChange(fn func()) { r.notify = fn }

func (r *supervisorRegistry) fireNotify() {
	if r.notify != nil {
		r.notify()
	}
}

func (r *supervisorRegistry) Add(p *supervisorPresence) {
	if p == nil || p.Session == nil {
		return
	}
	r.mu.Lock()
	r.sessions[p.Session] = p
	r.mu.Unlock()
	r.fireNotify()
}

func (r *supervisorRegistry) Remove(s *supervisorSession) {
	if s == nil {
		return
	}
	r.mu.Lock()
	_, existed := r.sessions[s]
	delete(r.sessions, s)
	r.mu.Unlock()
	if existed {
		r.fireNotify()
	}
}

// UsernamesOnline returns the unique sorted set of usernames currently
// connected. Sorted so the agent's dropdown order is stable.
func (r *supervisorRegistry) UsernamesOnline() []string {
	r.mu.Lock()
	seen := make(map[string]struct{}, len(r.sessions))
	for _, p := range r.sessions {
		if p.Username != "" {
			seen[p.Username] = struct{}{}
		}
	}
	r.mu.Unlock()
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// Sessions returns every presence matching the given username so the
// caller can fan out an outbox message to all of them.
func (r *supervisorRegistry) Sessions(username string) []*supervisorPresence {
	if username == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*supervisorPresence
	for _, p := range r.sessions {
		if p.Username == username {
			out = append(out, p)
		}
	}
	return out
}

// Each iterates every connected supervisor presence under the lock.
// Used by disconnect-cleanup paths that need to push to "supervisors
// involved in this intercom" regardless of username.
func (r *supervisorRegistry) Each(fn func(*supervisorPresence)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.sessions {
		fn(p)
	}
}
