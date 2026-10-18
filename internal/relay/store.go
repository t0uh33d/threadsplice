package relay

import (
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
	errNotFound = errors.New("not found")
	errConflict = errors.New("conflict")
	errInvite   = errors.New("invite is invalid, expired or already used")
)

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
PRAGMA user_version = 1;
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
	switch v {
	case 0:
		if err := inTx(context.Background(), db, func(tx *sql.Tx) error {
			_, err := tx.Exec(schema)
			return err
		}); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	case 1:
	default:
		db.Close()
		return nil, fmt.Errorf("database schema v%d is newer than this relay (v1); upgrade threadsplice", v)
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
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM participants WHERE room_id = ? AND name = ?`, m.RoomID, name).Scan(&taken); err != nil {
			return err
		}
		if taken > 0 {
			return errConflict
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

type caller struct {
	id, room string
	owner    bool
}

// auth resolves a bearer token hash to an active (non-revoked) participant.
func (s *store) auth(ctx context.Context, tokenHash []byte) (caller, error) {
	var c caller
	err := s.db.QueryRowContext(ctx, `SELECT id, room_id, owner FROM participants WHERE token_hash = ? AND revoked_at IS NULL`,
		tokenHash).Scan(&c.id, &c.room, &c.owner)
	if errors.Is(err, sql.ErrNoRows) {
		return c, errNotFound
	}
	return c, err
}

// active reports whether pid is still a non-revoked participant.
func (s *store) active(ctx context.Context, pid string) bool {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM participants WHERE id = ? AND revoked_at IS NULL`, pid).Scan(&n)
	return err == nil && n == 1
}

// participants lists a room's members. Presence (state, last seen) is filled
// in by the server from memory; the schema's state and last_seen columns are
// unused since presence moved there.
func (s *store) participants(ctx context.Context, roomID string) ([]api.Participant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, owner, acked_seq, revoked_at IS NOT NULL
		FROM participants WHERE room_id = ? ORDER BY created_at`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Participant
	for rows.Next() {
		var p api.Participant
		if err := rows.Scan(&p.ID, &p.Name, &p.Owner, &p.AckedSeq, &p.Revoked); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *store) revoke(ctx context.Context, roomID, pid string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE participants SET revoked_at = ? WHERE room_id = ? AND id = ? AND revoked_at IS NULL`,
		ms(time.Now()), roomID, pid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
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
