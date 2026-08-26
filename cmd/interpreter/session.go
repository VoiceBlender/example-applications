package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// A session is one interpreted conversation: two participants, two languages,
// one VoiceBlender room. Sessions are transient — they live in memory and vanish
// when the last participant leaves — so there is nothing to persist and no login
// to gate them. The session id is the secret: whoever has the link is in.

const (
	maxParticipants = 2

	// roleA / roleB are the VoiceBlender leg roles that drive the room's routing
	// matrix. They must never be empty: a leg with no role, or with a role that
	// has no row in the matrix, falls back to hearing EVERYONE — which in this
	// app means the participants hear each other's untranslated voice, the one
	// failure the whole design exists to prevent.
	roleA = "pa"
	roleB = "pb"
)

var (
	errSessionNotFound = errors.New("session not found")
	errSessionFull     = errors.New("session is full")
)

// staged is one utterance synthesized speculatively at eager_end_of_turn and
// held on the listener's leg, waiting for the speaker's turn to actually end.
type staged struct {
	ttsID   string        // the id to commit or discard
	src     string        // the eager transcript, to detect revision at end_of_turn
	text    string        // the translation that was synthesized
	at      time.Time     // when it was preflighted, for local TTL expiry
	ready   bool          // tts.staged seen — synthesis finished
	readyCh chan struct{} // closed when ready flips, so a commit can wait for it
}

// participant is one side of a session.
//
// Two pieces of per-leg bookkeeping matter and are easy to confuse:
//
//   - staged is keyed by THIS participant's turn_index, but each entry's tts_id
//     lives on the PEER's leg — it is this speaker's words, waiting to be spoken
//     to the listener.
//   - inflight is a tts_id playing on THIS participant's own leg, i.e. the peer's
//     translated speech. VoiceBlender does not queue or interrupt concurrent
//     leg_tts on one leg — they simply overlap and both are audible — so this
//     field is what lets us stop the previous utterance before starting the next.
type participant struct {
	id   string // opaque per-browser id
	name string
	slot int // 0 or 1; fixes the role and the TTS voice

	mu sync.Mutex
	// lang is what this participant speaks; gender selects the voice their words
	// are spoken in on the OTHER side.
	lang   string
	gender string
	legID  string
	live   bool // the leg has connected (STT may start)
	sttOn  bool
	// sttLang is the language the RUNNING transcriber was started with, as
	// opposed to `lang`, which is what the participant has currently selected.
	// Keeping both is what makes a drift between them detectable: without it a
	// failed restart leaves the wrong language running and nothing notices.
	sttLang string
	// sttProvider is the engine this leg's transcription actually runs on.
	// With per-language routing the two participants may differ, and the two
	// engines report turns differently — see interpreter.onText.
	sttProvider string
	inflight    string
	staged      map[int]*staged
	// stagedByID indexes the same entries by tts id. It outlives removal from
	// `staged`, because tts.staged can land AFTER the turn has ended and pulled
	// the entry out — and a commit that is blocked waiting on synthesis still
	// needs to be woken by it.
	stagedByID map[string]*staged
	// pending is a barrier per turn: present while a speculative translation is
	// in flight, closed when it finishes. end_of_turn waits on it so it never
	// races the eager pass and runs the whole cascade a second time.
	pending map[int]chan struct{}
	// maxDone is the highest turn index already finished, so a speculative pass
	// that lands late can tell it is stale instead of staging audio nobody will
	// ever commit.
	maxDone int
	c       *conn

	// sttMu serializes transcriber start/stop for this leg. mu is released
	// across the VSI round trip, so without this a leg connecting and a
	// language change can interleave and leave the transcriber running in a
	// language nobody selected.
	sttMu sync.Mutex
}

// putStaged records a speculative utterance under both its turn index and its
// tts id.
func (p *participant) putStaged(turnIndex int, st *staged) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.staged[turnIndex] = st
	p.stagedByID[st.ttsID] = st
}

// takeStaged pulls a turn's staged utterance out of the turn map, but leaves it
// in the id index so a late tts.staged can still mark it ready. The caller must
// releaseStaged it once finished with it.
func (p *participant) takeStaged(turnIndex int) *staged {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.staged[turnIndex]
	delete(p.staged, turnIndex)
	return st
}

// releaseStaged forgets an utterance entirely.
func (p *participant) releaseStaged(st *staged) {
	if st == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.stagedByID, st.ttsID)
}

// dropAllStaged removes and returns every staged utterance.
func (p *participant) dropAllStaged() []*staged {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*staged, 0, len(p.staged))
	for idx, st := range p.staged {
		out = append(out, st)
		delete(p.staged, idx)
		delete(p.stagedByID, st.ttsID)
	}
	return out
}

// markStagedReady wakes a commit that is waiting on synthesis. Reports false if
// the id is unknown — already resolved, or never ours.
func (p *participant) markStagedReady(ttsID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.stagedByID[ttsID]
	if !ok || st.ready {
		return false
	}
	st.ready = true
	close(st.readyCh)
	return true
}

func (p *participant) role() string {
	if p.slot == 0 {
		return roleA
	}
	return roleB
}

func (p *participant) snapshot() (lang, legID string, live bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lang, p.legID, p.live
}

func (p *participant) getLang() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lang
}

// runningLang is the language the transcriber is actually running in, which is
// not necessarily the one selected — see startSTT.
func (p *participant) runningLang() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sttLang
}

// usesTurnEvents reports whether this leg's transcriber reports turn boundaries
// (Deepgram Flux). Those legs are driven by stt.turn and must ignore final
// stt.text, or every utterance would be spoken twice.
func (p *participant) usesTurnEvents() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sttProvider == "deepgram_flux"
}

func (p *participant) getGender() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gender
}

func (p *participant) leg() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.legID
}

// hangUp closes this participant's browser socket, if it still has one.
func (p *participant) hangUp() {
	p.mu.Lock()
	c := p.c
	p.mu.Unlock()
	if c != nil {
		c.hangUp()
	}
}

// send queues a message to this participant's browser, if it still has a socket.
func (p *participant) send(msg any) {
	p.mu.Lock()
	c := p.c
	p.mu.Unlock()
	if c != nil {
		c.send(msg)
	}
}

// conn is one browser signalling WebSocket. Writes go through a buffered outbox
// so a slow or wedged client can never block the VSI event loop.
type conn struct {
	outbox chan any
	// quit is closed to hang up on this browser from outside its own request
	// goroutine — which is how a timed-out session evicts someone who is just
	// sitting there with the tab open.
	quit chan struct{}
	once sync.Once
}

func newConn() *conn { return &conn{outbox: make(chan any, 32), quit: make(chan struct{})} }

// hangUp asks this browser's request goroutine to unwind. Idempotent.
func (c *conn) hangUp() {
	c.once.Do(func() { close(c.quit) })
}

func (c *conn) send(msg any) {
	select {
	case c.outbox <- msg:
	default: // client is not draining; drop rather than stall the event loop
	}
}

// session is one interpreted conversation.
type session struct {
	id string
	// invite is the bearer token that lets the OTHER person in without an
	// account. It is scoped to this session and dies with it — see
	// app.invitedTo. Kept separate from the id so the id can appear in logs
	// without handing out access.
	invite  string
	created time.Time

	mu      sync.Mutex
	parts   [maxParticipants]*participant
	roomUp  bool // create_room succeeded
	routing bool // room_routing_set applied

	// startedAt is when interpreting actually began (both legs up), which is
	// what the maximum-duration cap measures — not when the link was minted.
	startedAt time.Time
	// lastActivity is the last time anyone spoke. The idle timeout measures
	// from here, and it is the number that actually maps to money: while the
	// legs are up, audio is streaming to the STT vendor whether or not anyone
	// is saying anything.
	lastActivity time.Time
	// emptySince is when the last participant left (or the session was minted
	// and never joined). Zero while someone is present.
	emptySince time.Time
	// closed marks a session already torn down, so a reaper and a departing
	// participant cannot both try to end it.
	closed bool
}

// touch records speech activity, holding off the idle timeout.
func (s *session) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

// markStarted stamps the clock the maximum-duration cap runs from, once.
func (s *session) markStarted() {
	s.mu.Lock()
	if s.startedAt.IsZero() {
		s.startedAt = time.Now()
	}
	s.mu.Unlock()
}

// close marks the session torn down. It reports false if someone got there
// first, so the teardown runs exactly once.
func (s *session) close() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.closed = true
	return true
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// expiry reports why a session should be reaped now, or "" to leave it alone.
func (s *session) expiry(now time.Time, cfg config) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ""
	}
	if cfg.emptyTimeout > 0 && !s.emptySince.IsZero() && now.Sub(s.emptySince) > cfg.emptyTimeout {
		return "abandoned"
	}
	// The duration cap only makes sense once interpreting actually started.
	if cfg.maxDuration > 0 && !s.startedAt.IsZero() && now.Sub(s.startedAt) > cfg.maxDuration {
		return "time limit reached"
	}
	// The idle clock starts at join, not at first speech, so a session whose
	// media never came up at all — a denied microphone, a failed ICE — is
	// reaped too instead of holding sockets open forever.
	if cfg.idleTimeout > 0 && !s.lastActivity.IsZero() && now.Sub(s.lastActivity) > cfg.idleTimeout {
		return "no speech for a while"
	}
	return ""
}

// both returns the two participants when the session is full, so callers that
// only make sense with a peer (starting STT, translating) can bail early.
func (s *session) both() (*participant, *participant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parts[0] == nil || s.parts[1] == nil {
		return nil, nil, false
	}
	return s.parts[0], s.parts[1], true
}

// peerOf returns the other participant, or nil if the session is half-empty.
func (s *session) peerOf(p *participant) *participant {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, other := range s.parts {
		if other != nil && other != p {
			return other
		}
	}
	return nil
}

// members returns the participants currently present.
func (s *session) members() []*participant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*participant, 0, maxParticipants)
	for _, p := range s.parts {
		if p != nil {
			out = append(out, p)
		}
	}
	return out
}

// broadcast sends a message to every participant in the session.
func (s *session) broadcast(msg any) {
	for _, p := range s.members() {
		p.send(msg)
	}
}

// ── registry ──────────────────────────────────────────────────────────────────

// sessionRegistry holds every live session, plus a leg → participant index so a
// VoiceBlender event (which knows only a leg id) can be routed back to a person.
type sessionRegistry struct {
	mu     sync.Mutex
	byID   map[string]*session
	byLeg  map[string]*participant
	legSes map[string]*session
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{
		byID:   make(map[string]*session),
		byLeg:  make(map[string]*participant),
		legSes: make(map[string]*session),
	}
}

// create makes an empty session with a fresh id.
func (r *sessionRegistry) create() *session {
	// emptySince starts ticking immediately: a link that is minted and never
	// opened should not linger forever.
	s := &session{
		id: newID(6),
		// 192 bits: this token is the only thing guarding the conversation for
		// whoever joins by link, so it has to be unguessable.
		invite:  newID(24),
		created: time.Now(), emptySince: time.Now(),
	}
	r.mu.Lock()
	r.byID[s.id] = s
	r.mu.Unlock()
	return s
}

func (r *sessionRegistry) get(id string) (*session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	return s, ok
}

// join seats a participant in a session, or reports why it could not.
//
// The language and voice are settled HERE, at join, rather than being corrected
// by a follow-up message. Seating someone at the defaults and fixing it a moment
// later leaves a window in which they are transcribed in the wrong language —
// short, but real, and it is the first moment of the call.
func (r *sessionRegistry) join(id, name, lang, gender string, cfg config) (*session, *participant, error) {
	s, ok := r.get(id)
	if !ok {
		return nil, nil, errSessionNotFound
	}
	// Fall back rather than reject: a stale tab sending an unknown code should
	// land somewhere sensible, not fail to join.
	if !cfg.langSupported(lang) {
		lang = defaultLang
	}
	if !knownGender(gender) {
		gender = defaultGender
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, errSessionNotFound
	}
	for slot, existing := range s.parts {
		if existing != nil {
			continue
		}
		p := &participant{
			id:         newID(8),
			name:       name,
			slot:       slot,
			lang:       lang,
			gender:     gender,
			staged:     make(map[int]*staged),
			stagedByID: make(map[string]*staged),
			pending:    make(map[int]chan struct{}),
			maxDone:    -1, // turn indexes start at 0
			c:          newConn(),
		}
		s.parts[slot] = p
		s.emptySince = time.Time{}
		// Start the idle clock now rather than at first speech: someone who
		// joins and never speaks should still time out.
		s.lastActivity = time.Now()
		return s, p, nil
	}
	return nil, nil, errSessionFull
}

// leave removes a participant and reports whether the session is now empty (so
// the caller can delete the VoiceBlender room).
func (r *sessionRegistry) leave(s *session, p *participant) (nowEmpty bool) {
	s.mu.Lock()
	for slot, existing := range s.parts {
		if existing == p {
			s.parts[slot] = nil
		}
	}
	nowEmpty = s.parts[0] == nil && s.parts[1] == nil
	if nowEmpty {
		s.emptySince = time.Now()
	}
	s.mu.Unlock()

	if nowEmpty {
		r.mu.Lock()
		delete(r.byID, s.id)
		r.mu.Unlock()
	}
	return nowEmpty
}

// bindLeg records the participant's media leg and indexes it, so leg-keyed
// VoiceBlender events can find their way back to a person.
func (r *sessionRegistry) bindLeg(s *session, p *participant, legID string) {
	p.mu.Lock()
	old := p.legID
	p.legID = legID
	p.live = false
	p.mu.Unlock()

	r.mu.Lock()
	if old != "" {
		delete(r.byLeg, old)
		delete(r.legSes, old)
	}
	r.byLeg[legID] = p
	r.legSes[legID] = s
	r.mu.Unlock()
}

// unbindLeg drops the leg index for a participant.
func (r *sessionRegistry) unbindLeg(p *participant) string {
	p.mu.Lock()
	legID := p.legID
	p.legID = ""
	p.live = false
	p.sttOn = false
	p.mu.Unlock()

	if legID != "" {
		r.mu.Lock()
		delete(r.byLeg, legID)
		delete(r.legSes, legID)
		r.mu.Unlock()
	}
	return legID
}

// forLeg resolves a VoiceBlender leg id back to its participant and session.
func (r *sessionRegistry) forLeg(legID string) (*session, *participant, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byLeg[legID]
	if !ok {
		return nil, nil, false
	}
	return r.legSes[legID], p, true
}

// drop unregisters a session by id, so its link stops working.
func (r *sessionRegistry) drop(id string) {
	r.mu.Lock()
	delete(r.byID, id)
	r.mu.Unlock()
}

// all returns every live session, for the staged-TTS sweeper.
func (r *sessionRegistry) all() []*session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*session, 0, len(r.byID))
	for _, s := range r.byID {
		out = append(out, s)
	}
	return out
}

// newID returns a URL-safe random identifier of n bytes, hex-encoded.
func newID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail in practice; a time-based fallback keeps the
		// app running rather than panicking a request handler.
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000")))
	}
	return hex.EncodeToString(b)
}
