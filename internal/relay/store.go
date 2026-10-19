package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	_ "modernc.org/sqlite"
)

var (
	errNotFound  = errors.New("not found")
	errConflict  = errors.New("conflict")
	errInvite    = errors.New("invite is invalid, expired or already used")
	errFull      = errors.New("too many session members")
	errTokenUsed = errors.New("token hash already in use")
	errRate      = errors.New("rate limited")
)

// A clash refuses a name that would blur who is who in a room, because of
// member, a participant there already (revoked ones too: they keep their
// names). Names are told apart ignoring letter case, and a top-level member's
// name and a dot begin the names of its sessions and of nobody else's.
type clash struct {
	name, member string
	kind         clashKind
}

type clashKind int

const (
	sameName clashKind = iota // member's name in other letter case
	inside                    // a name in member's namespace
	around                    // a name whose namespace would take member in
)

func (c clash) Error() string {
	switch c.kind {
	case sameName:
		return fmt.Sprintf("name %q is already taken in this room, as %q: letter case doesn't tell names apart", c.name, c.member)
	case inside:
		return fmt.Sprintf("names starting with %q belong to member %q's sessions, in any letter case", c.member+".", c.member)
	}
	return fmt.Sprintf("name %q would make member %q, already in this room, look like one of its sessions", c.name, c.member)
}

// maxChildren caps a machine membership's live session members.
const maxChildren = 32

// schemaVersion is the user_version of a current database.
const schemaVersion = 2

// Composite foreign keys on (room_id, participant) make cross-room references
// impossible at the database level, not just in handler code.
const schema = `
CREATE TABLE rooms (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE participants (
	id         TEXT PRIMARY KEY,
	room_id    TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	token_hash BLOB NOT NULL UNIQUE,
	owner      INTEGER NOT NULL DEFAULT 0,
	state      TEXT NOT NULL DEFAULT '',
	last_seen  INTEGER NOT NULL DEFAULT 0,
	acked_seq  INTEGER NOT NULL DEFAULT 0,
	revoked_at INTEGER,
	created_at INTEGER NOT NULL,
	UNIQUE (room_id, name),
	UNIQUE (room_id, id)
);
CREATE TABLE invites (
	token_hash BLOB PRIMARY KEY,
	room_id    TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
	created_by TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER,
	FOREIGN KEY (room_id, created_by) REFERENCES participants (room_id, id) ON DELETE CASCADE
);
CREATE TABLE messages (
	seq          INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id      TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
	id           TEXT NOT NULL,
	sender_id    TEXT NOT NULL,
	recipient_id TEXT,
	sealed       BLOB NOT NULL,
	created_at   INTEGER NOT NULL,
	UNIQUE (sender_id, id),
	FOREIGN KEY (room_id, sender_id) REFERENCES participants (room_id, id) ON DELETE CASCADE,
	FOREIGN KEY (room_id, recipient_id) REFERENCES participants (room_id, id) ON DELETE CASCADE
);
CREATE INDEX messages_room_seq ON messages (room_id, seq);
` + schemaV2 + `
PRAGMA user_version = 2;
`

// schemaV2 adds session members: participants a machine membership creates
// for single agent sessions, one level deep. It is a side table because
// rebuilding participants would break the composite foreign keys into it.
const schemaV2 = `
CREATE TABLE children (
	room_id   TEXT NOT NULL,
	child_id  TEXT PRIMARY KEY,
	parent_id TEXT NOT NULL,
	CHECK (child_id <> parent_id),
	FOREIGN KEY (room_id, child_id)  REFERENCES participants (room_id, id) ON DELETE CASCADE,
	FOREIGN KEY (room_id, parent_id) REFERENCES participants (room_id, id) ON DELETE CASCADE
);
CREATE INDEX children_parent ON children (parent_id);
CREATE TRIGGER children_one_level BEFORE INSERT ON children
WHEN EXISTS (SELECT 1 FROM children WHERE child_id = NEW.parent_id)
	OR EXISTS (SELECT 1 FROM children WHERE parent_id = NEW.child_id)
BEGIN SELECT RAISE(ABORT, 'session members are one level deep'); END;
`

type store struct{ db *sql.DB }

func openStore(path string) (*store, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// ponytail: one connection serializes all DB access, which also makes every
	// check-then-write below race-free. Plenty for a small relay; add a read pool
	// if profiles ever show contention.
	db.SetMaxOpenConns(1)
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// Each step runs in one transaction, user_version included, so a failed
	// migration leaves the database as it was.
	var ddl string
	switch v {
	case 0:
		ddl = schema
	case 1:
		ddl = schemaV2 + "PRAGMA user_version = 2;"
	case schemaVersion:
	default:
		db.Close()
		return nil, fmt.Errorf("database schema v%d is newer than this relay (v%d); upgrade threadsplice", v, schemaVersion)
	}
	if ddl != "" {
		if err := inTx(context.Background(), db, func(tx *sql.Tx) error {
			_, err := tx.Exec(ddl)
			return err
		}); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &store{db}, nil
}

func inTx(ctx context.Context, db *sql.DB, f func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func (s *store) createRoom(ctx context.Context, name, owner string, tokenHash []byte) (roomID, pid string, err error) {
	roomID, pid = rand.Text(), rand.Text()
	now := ms(time.Now())
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rooms (id, name, created_at) VALUES (?, ?, ?)`, roomID, name, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO participants (id, room_id, name, token_hash, owner, created_at) VALUES (?, ?, ?, ?, 1, ?)`,
			pid, roomID, owner, tokenHash, now)
		return err
	})
	return roomID, pid, err
}

func (s *store) createInvite(ctx context.Context, roomID, by string, tokenHash []byte, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO invites (token_hash, room_id, created_by, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, roomID, by, ms(expires))
	return err
}

// join redeems a single-use invite and creates the participant atomically. A
// name clash rolls back, so the invite stays usable.
func (s *store) join(ctx context.Context, inviteHash []byte, name string, tokenHash []byte) (api.Membership, error) {
	m := api.Membership{ParticipantID: rand.Text()}
	now := ms(time.Now())
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT i.room_id, r.name FROM invites i JOIN rooms r ON r.id = i.room_id
			WHERE i.token_hash = ? AND i.used_at IS NULL AND i.expires_at > ?`, inviteHash, now).Scan(&m.RoomID, &m.RoomName)
		if errors.Is(err, sql.ErrNoRows) {
			return errInvite
		} else if err != nil {
			return err
		}
		// No newcomer may pass as another member, nor as one of a member's
		// sessions, whoever joined first (see clash).
		var other string
		err = tx.QueryRowContext(ctx, `SELECT name FROM participants WHERE room_id = ? AND lower(name) = lower(?) ORDER BY name <> ? LIMIT 1`,
			m.RoomID, name, name).Scan(&other)
		switch {
		case err == nil && other == name:
			return errConflict
		case err == nil:
			return clash{name, other, sameName}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT p.name FROM participants p WHERE p.room_id = ? AND substr(lower(?), 1, length(p.name) + 1) = lower(p.name) || '.'
			AND NOT EXISTS (SELECT 1 FROM children c WHERE c.child_id = p.id) ORDER BY length(p.name) LIMIT 1`, m.RoomID, name).Scan(&other)
		if err == nil {
			return clash{name, other, inside}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// A dotted name may have joined first: then nobody takes the name it
		// would pass for a session of.
		err = tx.QueryRowContext(ctx, `SELECT name FROM participants WHERE room_id = ? AND substr(lower(name), 1, length(?) + 1) = lower(?) || '.'
			ORDER BY length(name) LIMIT 1`, m.RoomID, name, name).Scan(&other)
		if err == nil {
			return clash{name, other, around}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invites SET used_at = ? WHERE token_hash = ?`, now, inviteHash); err != nil {
			return err
		}
		// A newcomer starts at the room's current cursor: retained history is
		// readable via the history view, but must not arrive as a flood of new
		// messages (each one would wake the joining agent's session).
		_, err = tx.ExecContext(ctx, `INSERT INTO participants (id, room_id, name, token_hash, acked_seq, created_at)
			VALUES (?, ?, ?, ?, (SELECT coalesce(max(seq), 0) FROM messages WHERE room_id = ?), ?)`,
			m.ParticipantID, m.RoomID, name, tokenHash, m.RoomID, now)
		return err
	})
	return m, err
}

// createChild creates the session member name under parent, the caller's
// machine membership, as long as allow, which charges the parent's creation
// budget, agrees. A retry with the same token hash returns the member it
// created, with created false, and costs nothing: it is how a client recovers
// from a lost answer, so the budget must not stand in its way.
func (s *store) createChild(ctx context.Context, roomID, parent, name string, tokenHash []byte, allow func() bool) (m api.Membership, created bool, err error) {
	m = api.Membership{RoomID: roomID}
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		// auth accepted the parent, but the owner may have revoked it since: a
		// member created now would have escaped the cascade.
		var parentName string
		err := tx.QueryRowContext(ctx, `SELECT p.name, r.name FROM participants p JOIN rooms r ON r.id = p.room_id
			WHERE p.id = ? AND p.room_id = ? AND p.revoked_at IS NULL`, parent, roomID).Scan(&parentName, &m.RoomName)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		// A taken name may be this very member: the retry of a request whose
		// response was lost.
		var id, taken, under string
		var hash []byte
		var revoked bool
		err = tx.QueryRowContext(ctx, `SELECT p.id, p.name, p.token_hash, p.revoked_at IS NOT NULL, coalesce(c.parent_id, '')
			FROM participants p LEFT JOIN children c ON c.child_id = p.id WHERE p.room_id = ? AND lower(p.name) = lower(?)
			ORDER BY p.name <> ? LIMIT 1`, roomID, name, name).Scan(&id, &taken, &hash, &revoked, &under)
		switch {
		case err == nil && taken == name && !revoked && under == parent && bytes.Equal(hash, tokenHash):
			m.ParticipantID = id
			return nil
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		case !allow():
			return errRate
		case err == nil && taken == name:
			return errConflict
		case err == nil:
			return clash{name, taken, sameName}
		}
		// The name starts with the caller's and a dot (the handler checked).
		// Rooms from before these rules can still hold a member named like the
		// caller in other letter case, or like the caller plus a dot and more,
		// whom a member with this name would pass for a session of. One whose
		// namespace holds the caller's own name doesn't count: the caller's
		// sessions look no more like its than the caller does.
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT p.name FROM participants p WHERE p.room_id = ? AND p.id <> ? AND length(p.name) >= length(?)
			AND substr(lower(?), 1, length(p.name) + 1) = lower(p.name) || '.'
			AND NOT EXISTS (SELECT 1 FROM children c WHERE c.child_id = p.id) ORDER BY length(p.name) DESC LIMIT 1`,
			roomID, parent, parentName, name).Scan(&owner)
		if err == nil {
			return clash{name, owner, inside}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM children c JOIN participants p ON p.id = c.child_id
			WHERE c.parent_id = ? AND p.revoked_at IS NULL`, parent).Scan(&n); err != nil {
			return err
		}
		if n >= maxChildren {
			return errFull
		}
		// The client picked the token, so it may have reused one; the unique
		// index would refuse it below, as an internal error.
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM participants WHERE token_hash = ?`, tokenHash).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errTokenUsed
		}
		m.ParticipantID = rand.Text()
		// Like a newcomer through join, a session member starts at the room's cursor.
		if _, err := tx.ExecContext(ctx, `INSERT INTO participants (id, room_id, name, token_hash, acked_seq, created_at)
			VALUES (?, ?, ?, ?, (SELECT coalesce(max(seq), 0) FROM messages WHERE room_id = ?), ?)`,
			m.ParticipantID, roomID, name, tokenHash, roomID, ms(time.Now())); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO children (room_id, child_id, parent_id) VALUES (?, ?, ?)`, roomID, m.ParticipantID, parent)
		created = err == nil
		return err
	})
	return m, created, err
}

type caller struct {
	id, room, name string
	owner          bool
	parent         string // a session member's machine membership; empty at the top level
}

// auth resolves a bearer token hash to an active participant: not revoked,
// and for a session member, under a parent that isn't revoked either.
// Revoking a parent revokes its members too; the parent check is defence in depth.
func (s *store) auth(ctx context.Context, tokenHash []byte) (caller, error) {
	var c caller
	err := s.db.QueryRowContext(ctx, `SELECT p.id, p.room_id, p.name, p.owner, coalesce(c.parent_id, '') FROM participants p
		LEFT JOIN children c ON c.child_id = p.id LEFT JOIN participants pp ON pp.id = c.parent_id
		WHERE p.token_hash = ? AND p.revoked_at IS NULL AND pp.revoked_at IS NULL`,
		tokenHash).Scan(&c.id, &c.room, &c.name, &c.owner, &c.parent)
	if errors.Is(err, sql.ErrNoRows) {
		return c, errNotFound
	}
	return c, err
}

// active reports whether pid is still a participant that auth accepts.
func (s *store) active(ctx context.Context, pid string) bool {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM participants p
		LEFT JOIN children c ON c.child_id = p.id LEFT JOIN participants pp ON pp.id = c.parent_id
		WHERE p.id = ? AND p.revoked_at IS NULL AND pp.revoked_at IS NULL`, pid).Scan(&n)
	return err == nil && n == 1
}

// participants lists a room's members. Presence (state, last seen) is filled
// in by the server from memory; the schema's state and last_seen columns are
// unused since presence moved there.
func (s *store) participants(ctx context.Context, roomID string) ([]api.Participant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.name, p.owner, coalesce(c.parent_id, ''), p.acked_seq, p.revoked_at IS NOT NULL
		FROM participants p LEFT JOIN children c ON c.child_id = p.id WHERE p.room_id = ? ORDER BY p.created_at`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Participant
	for rows.Next() {
		var p api.Participant
		if err := rows.Scan(&p.ID, &p.Name, &p.Owner, &p.Parent, &p.AckedSeq, &p.Revoked); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// revoke revokes an active participant and, in the same transaction, its
// session members.
func (s *store) revoke(ctx context.Context, roomID, pid string) error {
	now := ms(time.Now())
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE participants SET revoked_at = ? WHERE room_id = ? AND id = ? AND revoked_at IS NULL`,
			now, roomID, pid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errNotFound
		}
		_, err = tx.ExecContext(ctx, `UPDATE participants SET revoked_at = ?
			WHERE revoked_at IS NULL AND id IN (SELECT child_id FROM children WHERE parent_id = ?)`, now, pid)
		return err
	})
}

// insertMessage durably accepts a message. Retries with the same (sender, id)
// return the original seq with dup=true instead of storing a second copy.
func (s *store) insertMessage(ctx context.Context, roomID, from, to, id string, sealed []byte) (seq int64, dup bool, err error) {
	var recipient any // NULL addresses the whole room
	if to != "" {
		var ok int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM participants WHERE room_id = ? AND id = ? AND revoked_at IS NULL`,
			roomID, to).Scan(&ok); err != nil {
			return 0, false, err
		}
		if ok == 0 {
			return 0, false, errNotFound
		}
		recipient = to
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO messages (room_id, id, sender_id, recipient_id, sealed, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (sender_id, id) DO NOTHING`, roomID, id, from, recipient, sealed, ms(time.Now()))
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		seq, err = res.LastInsertId()
		return seq, false, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT seq FROM messages WHERE sender_id = ? AND id = ?`, from, id).Scan(&seq)
	return seq, true, err
}

// view selects which messages a reader gets.
type view int

const (
	inbox    view = iota // room-wide and addressed to me, not my own: what agents consume
	history              // inbox plus what I sent: a participant's full view
	everyone             // every message in the room: the owner's audit view
)

// messages returns the messages after seq `after` that `me` gets in view v.
// Callers enforce that only the owner asks for everyone.
func (s *store) messages(ctx context.Context, roomID, me string, after int64, limit int, v view) ([]api.Envelope, error) {
	const cols = `SELECT seq, id, sender_id, coalesce(recipient_id, ''), sealed, created_at FROM messages `
	q, args := cols+`WHERE room_id = ? AND seq > ? AND sender_id <> ? AND (recipient_id IS NULL OR recipient_id = ?) ORDER BY seq LIMIT ?`,
		[]any{roomID, after, me, me, limit}
	switch v {
	case history:
		q, args = cols+`WHERE room_id = ? AND seq > ? AND (recipient_id IS NULL OR recipient_id = ? OR sender_id = ?) ORDER BY seq LIMIT ?`,
			[]any{roomID, after, me, me, limit}
	case everyone:
		q, args = cols+`WHERE room_id = ? AND seq > ? ORDER BY seq LIMIT ?`, []any{roomID, after, limit}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Envelope{}
	for rows.Next() {
		var e api.Envelope
		var at int64
		if err := rows.Scan(&e.Seq, &e.ID, &e.From, &e.To, &e.Sealed, &at); err != nil {
			return nil, err
		}
		e.CreatedAt = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *store) ackedSeq(ctx context.Context, pid string) (seq int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT acked_seq FROM participants WHERE id = ?`, pid).Scan(&seq)
	return seq, err
}

func (s *store) ack(ctx context.Context, pid string, seq int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE participants SET acked_seq = max(acked_seq, ?) WHERE id = ?`, seq, pid)
	return err
}

// prune enforces retention: old messages and spent or expired invites go.
func (s *store) prune(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE created_at < ?`, ms(before))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM invites WHERE expires_at < ? OR used_at < ?`, ms(time.Now()), ms(before))
	return err
}
