package client

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/t0uh33d/threadsplice/internal/api"
)

// A session member is an agent session that joined a room as a participant of
// its own, under this machine's membership in that room: the relay names it
// <machine name>.<label> and keeps it one level deep. Locally it is an Identity
// at <home>/children/<parent alias>/<name>.json whose Alias is
// <parent alias>/<name>. List and Load("") never see these, so flagless
// commands keep resolving to the machine's own identity.

// NewToken returns a new bearer token, made like the relay's own: 32 random
// bytes, base64url. A session member's client picks its token and the relay
// only ever sees its TokenHash, so creating the member is safe to retry.
func NewToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// TokenHash is the form in which the relay knows a token: its SHA-256, in
// lowercase hex.
func TokenHash(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func childrenDir(parentAlias string) (string, error) {
	if !aliasRe.MatchString(parentAlias) {
		return "", fmt.Errorf("invalid identity name %q", parentAlias)
	}
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "children", parentAlias), nil
}

func childPath(parentAlias, name string) (string, error) {
	dir, err := childrenDir(parentAlias)
	if err != nil {
		return "", err
	}
	if !api.ValidName(name) {
		return "", fmt.Errorf("invalid session member name %q", name)
	}
	return filepath.Join(dir, name+".json"), nil
}

// SaveChild writes a new session member: id.Name under the machine membership
// id.ParentAlias, 0600 in 0700 directories. Like Save it never overwrites a
// member, with one exception that loses nothing. A member is saved before the
// relay creates it, so that its token is on disk first; that pending record has
// no ParticipantID, and saving the same record with the ParticipantID set
// completes it. Saving what is on disk already is a no-op; anything else that
// exists is an error wrapping fs.ErrExist.
func SaveChild(id Identity) error {
	if id.ParentAlias == "" {
		return errors.New("a session member needs its parent's alias")
	}
	alias := id.ParentAlias + "/" + id.Name
	if id.Alias != "" && id.Alias != alias {
		return fmt.Errorf("session member alias %q doesn't match %q", id.Alias, alias)
	}
	id.Alias = alias
	p, err := childPath(id.ParentAlias, id.Name)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	if err := writeNew(p, b); !errors.Is(err, fs.ErrExist) {
		return err
	}
	// ponytail: read, compare, then rename, without a lock. Racing joins of one
	// name can at worst drop a pending token the relay then refuses (409).
	if old, err := LoadChild(id.ParentAlias, id.Name); err == nil {
		if old == id {
			return nil
		}
		if old.ParticipantID == "" && id.ParticipantID != "" { // pending: complete it
			if old.ParticipantID = id.ParticipantID; old == id {
				return replaceFile(p, b)
			}
		}
	}
	return fmt.Errorf("session member %q already exists (%s): %w", alias, p, fs.ErrExist)
}

// LoadChild reads the session member name of the machine membership
// parentAlias. A missing one is an error wrapping fs.ErrNotExist; a pending one
// (see SaveChild) has no ParticipantID.
func LoadChild(parentAlias, name string) (Identity, error) {
	p, err := childPath(parentAlias, name)
	if err != nil {
		return Identity{}, err
	}
	id, err := readIdentity(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Identity{}, fmt.Errorf("no session member %q: %w", parentAlias+"/"+name, fs.ErrNotExist)
	} else if err != nil {
		return Identity{}, err
	}
	id.Alias, id.ParentAlias = parentAlias+"/"+name, parentAlias
	return id, nil
}

// MemberOf reports whether id, a session member, belongs to the machine
// membership m. Member files outlive their membership's file: an alias whose
// file is gone and that now names a membership in another room still has the
// old one's members under it.
func (id Identity) MemberOf(m Identity) bool {
	return id.ParentAlias == m.Alias && id.Parent == m.ParticipantID && id.RoomID == m.RoomID && id.Relay == m.Relay
}

// LoadMember reads the session member name of the machine membership machine
// for a session to act as: one the relay confirmed (it has a ParticipantID)
// and that belongs to that membership (MemberOf).
func LoadMember(machine, name string) (Identity, error) {
	id, err := LoadChild(machine, name)
	if err != nil {
		return Identity{}, err
	}
	if id.ParticipantID == "" {
		return Identity{}, fmt.Errorf("session member %q is unfinished: the relay never confirmed it", id.Alias)
	}
	m, err := Load(machine)
	if err != nil {
		return Identity{}, err
	}
	if !id.MemberOf(m) {
		return Identity{}, fmt.Errorf("session member %q belongs to another membership than %q's, in room %q", id.Alias, machine, m.RoomName)
	}
	return id, nil
}

// ListChildren returns the session members of the machine membership
// parentAlias, sorted by name.
func ListChildren(parentAlias string) ([]Identity, error) {
	dir, err := childrenDir(parentAlias)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Identity
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !api.ValidName(name) {
			continue // temp files
		}
		id, err := LoadChild(parentAlias, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted meanwhile
		} else if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	// Not the file names' order: "x-2.json" sorts before "x.json".
	slices.SortFunc(out, func(a, b Identity) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// DeleteChild removes a pending session member (no ParticipantID), for when
// creating it failed for good. It refuses to remove any other, since the file
// holds the only copy of that member's token. A missing one is fine.
func DeleteChild(parentAlias, name string) error {
	id, err := LoadChild(parentAlias, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if id.ParticipantID != "" {
		return fmt.Errorf("session member %q exists on the relay; its file holds its only token, so it stays", id.Alias)
	}
	p, err := childPath(parentAlias, name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

var (
	labelChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)
	labelDash  = regexp.MustCompile(`-{2,}`)
)

// SessionLabel is the default label of a session working in dir: the base name
// of the nearest directory from dir up that holds .git, else of dir. Characters
// outside [A-Za-z0-9._-] become dashes, runs of dashes collapse, and leading
// characters that aren't letters or digits go; "session" if nothing is left.
// The home directory is never used, since its name is usually the user's
// login: the walk stops below it, and dir being home gives "session". Callers
// shorten the label to fit the name rule.
func SessionLabel(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		home = filepath.Clean(home)
	}
	pick := dir
	for d := dir; d != home; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil { // a worktree's .git is a file
			pick = d
			break
		}
		up := filepath.Dir(d)
		if up == d {
			break
		}
		d = up
	}
	if pick == home {
		return "session"
	}
	label := labelDash.ReplaceAllString(labelChars.ReplaceAllString(filepath.Base(pick), "-"), "-")
	if label = strings.TrimLeft(label, "._-"); label == "" {
		return "session"
	}
	return label
}
