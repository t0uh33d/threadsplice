// Package relay is the thread splice relay: a small HTTP service that stores and
// forwards end-to-end encrypted messages between agents. It never sees message
// content or room keys and never runs a model.
package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
)

// Config configures a relay.
type Config struct {
	DBPath        string
	AdminToken    string        // required to create rooms
	Retention     time.Duration // messages older than this are deleted; 0 = 30 days
	SendPerMinute int           // per-participant send rate limit; 0 = 60
	Log           *slog.Logger
}

const (
	maxWait      = 30 * time.Second // long-poll cap; stays under common proxy idle timeouts
	onlineWindow = 75 * time.Second // a participant counts as online if seen this recently
	pageSize     = 100
	maxSealed    = 128 << 10 // largest sealed message the relay accepts
)

// Server serves the relay API.
type Server struct {
	cfg   Config
	store *store
	hub   hub

	mu    sync.Mutex
	sends map[string]window   // per-participant send counters
	pres  map[string]presence // per-participant presence
}

// presence is kept in memory: it changes on every poll, doesn't need to
// survive a restart (agents re-report within one poll), and writing it to the
// database would cost a sync on every request.
type presence struct {
	seen  time.Time
	state string
}

type window struct {
	start time.Time
	n     int
}

// New opens (and if needed creates) the relay database.
func New(cfg Config) (*Server, error) {
	if len(cfg.AdminToken) < 16 {
		return nil, errors.New("admin token must be set and at least 16 characters (e.g. THREADSPLICE_ADMIN_TOKEN=$(openssl rand -hex 32))")
	}
	if cfg.Retention == 0 {
		cfg.Retention = 30 * 24 * time.Hour
	}
	if cfg.SendPerMinute == 0 {
		cfg.SendPerMinute = 60
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	st, err := openStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, hub: hub{ch: map[string]chan struct{}{}}, sends: map[string]window{}, pres: map[string]presence{}}, nil
}

// Close closes the database.
func (s *Server) Close() error { return s.store.db.Close() }

// Handler returns the HTTP handler for the relay API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /v1/rooms", s.createRoom)
	mux.HandleFunc("POST /v1/join", s.join)
	mux.HandleFunc("POST /v1/rooms/{room}/invites", s.auth(s.createInvite))
	mux.HandleFunc("GET /v1/rooms/{room}/participants", s.auth(s.participants))
	mux.HandleFunc("DELETE /v1/rooms/{room}/participants/{pid}", s.auth(s.revoke))
	mux.HandleFunc("POST /v1/rooms/{room}/messages", s.auth(s.send))
	mux.HandleFunc("GET /v1/rooms/{room}/messages", s.auth(s.messages))
	mux.HandleFunc("POST /v1/rooms/{room}/ack", s.auth(s.ack))
	return mux
}

// Run serves the relay on addr until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, addr string, cfg Config) error {
	s, err := New(cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	// Cancelling base ends in-flight long-polls promptly on shutdown.
	base, endPolls := context.WithCancel(context.Background())
	defer endPolls()
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      maxWait + 15*time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return base },
	}
	go s.janitor(ctx)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.cfg.Log.Info("relay listening", "addr", addr, "db", cfg.DBPath)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	s.cfg.Log.Info("relay shutting down")
	endPolls()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if err := s.store.prune(ctx, time.Now().Add(-s.cfg.Retention)); err != nil && ctx.Err() == nil {
			s.cfg.Log.Error("retention prune failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// hub wakes long-polls when a room gets a new message.
type hub struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

// wait returns a channel closed on the room's next message. Subscribe before
// querying, so a message landing between the query and the wait isn't missed.
func (h *hub) wait(room string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.ch[room]
	if !ok {
		c = make(chan struct{})
		h.ch[room] = c
	}
	return c
}

func (h *hub) notify(room string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.ch[room]; ok {
		close(c)
		delete(h.ch, room)
	}
}

// ---- handlers ----

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.db.PingContext(r.Context()); err != nil {
		httpError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	w.Write([]byte("ok\n"))
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	tok, _ := bearer(r)
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AdminToken)) != 1 {
		httpError(w, http.StatusUnauthorized, "relay admin token required to create rooms")
		return
	}
	var req api.CreateRoomRequest
	if !decode(w, r, &req) {
		return
	}
	if !validName(req.Name) || !validName(req.Owner) {
		httpError(w, http.StatusBadRequest, nameRule)
		return
	}
	token := newToken()
	roomID, pid, err := s.store.createRoom(r.Context(), req.Name, req.Owner, hashToken(token))
	if err != nil {
		s.internal(w, err)
		return
	}
	s.cfg.Log.Info("room created", "room", roomID, "owner", pid)
	writeJSON(w, http.StatusCreated, api.Membership{RoomID: roomID, RoomName: req.Name, ParticipantID: pid, Token: token})
}

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var req api.JoinRequest
	if !decode(w, r, &req) {
		return
	}
	if !validName(req.Name) {
		httpError(w, http.StatusBadRequest, nameRule)
		return
	}
	token := newToken()
	m, err := s.store.join(r.Context(), hashToken(req.Invite), req.Name, hashToken(token))
	switch {
	case errors.Is(err, errInvite):
		httpError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, errConflict):
		httpError(w, http.StatusConflict, fmt.Sprintf("name %q is already taken in this room", req.Name))
		return
	case err != nil:
		s.internal(w, err)
		return
	}
	m.Token = token
	s.cfg.Log.Info("participant joined", "room", m.RoomID, "participant", m.ParticipantID)
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request, c caller) {
	if !c.owner {
		httpError(w, http.StatusForbidden, "only the room owner can invite")
		return
	}
	var req api.CreateInviteRequest
	if !decode(w, r, &req) {
		return
	}
	ttl := 24 * time.Hour
	if req.TTLSeconds > 0 {
		ttl = min(time.Duration(req.TTLSeconds)*time.Second, 7*24*time.Hour)
	}
	inv := api.Invite{Token: newToken(), ExpiresAt: time.Now().Add(ttl).UTC().Truncate(time.Millisecond)}
	if err := s.store.createInvite(r.Context(), c.room, c.id, hashToken(inv.Token), inv.ExpiresAt); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, inv)
}

func (s *Server) participants(w http.ResponseWriter, r *http.Request, c caller) {
	ps, err := s.store.participants(r.Context(), c.room)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.mu.Lock()
	for i, p := range ps {
		pr := s.pres[p.ID]
		if !pr.seen.IsZero() {
			ps[i].LastSeen, ps[i].State = pr.seen.UTC().Truncate(time.Millisecond), pr.state
		}
		ps[i].Online = !p.Revoked && time.Since(pr.seen) < onlineWindow
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, ps)
}

func (s *Server) touch(pid, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pres[pid]
	p.seen = time.Now()
	if state != "" {
		p.state = state
	}
	s.pres[pid] = p
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request, c caller) {
	if !c.owner {
		httpError(w, http.StatusForbidden, "only the room owner can revoke participants")
		return
	}
	switch err := s.store.revoke(r.Context(), c.room, r.PathValue("pid")); {
	case errors.Is(err, errNotFound):
		httpError(w, http.StatusNotFound, "no such active participant in this room")
	case err != nil:
		s.internal(w, err)
	default:
		s.cfg.Log.Info("participant revoked", "room", c.room, "participant", r.PathValue("pid"), "by", c.id)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) send(w http.ResponseWriter, r *http.Request, c caller) {
	var req api.SendRequest
	if !decode(w, r, &req) {
		return
	}
	switch {
	case !validID(req.ID):
		httpError(w, http.StatusBadRequest, "id must be 1-64 characters of [A-Za-z0-9_-]")
		return
	case len(req.Sealed) == 0 || len(req.Sealed) > maxSealed:
		httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("sealed message must be 1-%d bytes", maxSealed))
		return
	case req.To == c.id:
		httpError(w, http.StatusBadRequest, "cannot send a message to yourself")
		return
	case !s.allowSend(c.id):
		httpError(w, http.StatusTooManyRequests, fmt.Sprintf("rate limit: %d messages per minute", s.cfg.SendPerMinute))
		return
	}
	seq, dup, err := s.store.insertMessage(r.Context(), c.room, c.id, req.To, req.ID, req.Sealed)
	switch {
	case errors.Is(err, errNotFound):
		httpError(w, http.StatusNotFound, "recipient is not an active participant in this room")
		return
	case err != nil:
		s.internal(w, err)
		return
	}
	if !dup {
		s.hub.notify(c.room)
	}
	writeJSON(w, http.StatusOK, api.SendResponse{Seq: seq, Duplicate: dup})
}

// messages long-polls: it returns as soon as anything is readable after the
// cursor, or an empty page once `wait` (capped at 30s) elapses. Without
// `after`, it resumes from the caller's last acknowledged seq. With all=1 it
// returns a read-only history from an explicit cursor, touching nobody's
// acknowledgements: the whole room (direct messages between others included)
// for the owner, and for anyone else what they may read plus what they sent.
func (s *Server) messages(w http.ResponseWriter, r *http.Request, c caller) {
	q := r.URL.Query()
	all := q.Get("all") == "1"
	v := inbox
	switch {
	case all && c.owner:
		v = everyone
	case all:
		v = history
	}
	var after int64
	var err error
	if v := q.Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			httpError(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
	} else if all {
		httpError(w, http.StatusBadRequest, "all=1 needs an explicit after cursor")
		return
	} else if after, err = s.store.ackedSeq(r.Context(), c.id); err != nil {
		s.internal(w, err)
		return
	}
	var wait time.Duration
	if v := q.Get("wait"); v != "" {
		sec, err := strconv.Atoi(v)
		if err != nil || sec < 0 {
			httpError(w, http.StatusBadRequest, "wait must be a non-negative number of seconds")
			return
		}
		wait = min(time.Duration(sec)*time.Second, maxWait)
	}
	deadline := time.Now().Add(wait)
	for {
		woke := s.hub.wait(c.room)
		msgs, err := s.store.messages(r.Context(), c.room, c.id, after, pageSize, v)
		if err != nil {
			if r.Context().Err() == nil {
				s.internal(w, err)
			}
			return
		}
		if len(msgs) > 0 || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, api.MessagesResponse{Messages: msgs})
			return
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-woke:
			if !s.store.active(r.Context(), c.id) { // revoked while waiting: deliver nothing more
				timer.Stop()
				httpError(w, http.StatusUnauthorized, "invalid or revoked credential")
				return
			}
		case <-timer.C:
		case <-r.Context().Done(): // client gone or relay shutting down
			timer.Stop()
			writeJSON(w, http.StatusOK, api.MessagesResponse{Messages: []api.Envelope{}})
			return
		}
		timer.Stop()
	}
}

func (s *Server) ack(w http.ResponseWriter, r *http.Request, c caller) {
	var req api.AckRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Seq < 0 {
		httpError(w, http.StatusBadRequest, "seq must be non-negative")
		return
	}
	if err := s.store.ack(r.Context(), c.id, req.Seq); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers ----

// auth authenticates the bearer token, confines the caller to its own room, and
// records presence (an optional ?state= reports busy/idle).
func (s *Server) auth(h func(http.ResponseWriter, *http.Request, caller)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearer(r)
		if !ok {
			httpError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		c, err := s.store.auth(r.Context(), hashToken(tok))
		if errors.Is(err, errNotFound) {
			httpError(w, http.StatusUnauthorized, "invalid or revoked credential")
			return
		} else if err != nil {
			s.internal(w, err)
			return
		}
		if r.PathValue("room") != c.room {
			httpError(w, http.StatusNotFound, "room not found") // don't confirm other rooms exist
			return
		}
		state := r.URL.Query().Get("state")
		if state != "" && !validState(state) {
			httpError(w, http.StatusBadRequest, "state must be 1-16 characters of [a-z_]")
			return
		}
		s.touch(c.id, state)
		h(w, r, c)
	}
}

func (s *Server) allowSend(pid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// ponytail: fixed one-minute window per participant; switch to a token bucket if bursts at window edges matter.
	now := time.Now()
	win := s.sends[pid]
	if now.Sub(win.start) >= time.Minute {
		win = window{start: now}
	}
	if win.n >= s.cfg.SendPerMinute {
		return false
	}
	win.n++
	s.sends[pid] = win
	return true
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.cfg.Log.Error("request failed", "err", err)
	httpError(w, http.StatusInternalServerError, "internal error")
}

func bearer(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok, ok && tok != ""
}

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

const nameRule = "names must be 1-32 characters of [A-Za-z0-9._-], starting with a letter or digit"

var (
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	idRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	stateRe = regexp.MustCompile(`^[a-z_]{1,16}$`)
)

func validName(s string) bool  { return nameRe.MatchString(s) }
func validID(s string) bool    { return idRe.MatchString(s) }
func validState(s string) bool { return stateRe.MatchString(s) }

// decode reads a strictly validated JSON body of at most 256 KiB.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			httpError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		}
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.Error{Error: msg})
}
