package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/e2ee"
)

// Identity is a membership in one room: this machine's own (top level), or a
// session member's, which one agent session joined under it (see SaveChild).
// It holds two secrets — the participant token (relay access) and the room key
// (message confidentiality) — and is stored as JSON readable only by the
// current user.
type Identity struct {
	Alias         string `json:"-"` // the file's name; a session member's is <parent alias>/<name>
	Relay         string `json:"relay"`
	RoomID        string `json:"room_id"`
	RoomName      string `json:"room_name"`
	ParticipantID string `json:"participant_id"`
	Name          string `json:"name"`
	Owner         bool   `json:"owner,omitempty"`
	Machine       bool   `json:"machine,omitempty"`      // top level: this machine's own membership; sessions join under it
	Parent        string `json:"parent,omitempty"`       // session member: the machine membership's participant id
	ParentAlias   string `json:"parent_alias,omitempty"` // session member: the machine membership's local alias
	Token         string `json:"token"`
	RoomKey       string `json:"room_key"`
}

// Home is where identities live: $THREADSPLICE_HOME, else <user config dir>/threadsplice.
func Home() (string, error) {
	if d := os.Getenv("THREADSPLICE_HOME"); d != "" {
		return d, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "threadsplice"), nil
}

var aliasRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func identityPath(alias string) (string, error) {
	if !aliasRe.MatchString(alias) {
		return "", fmt.Errorf("invalid identity name %q", alias)
	}
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "identities", alias+".json"), nil
}

// aliasFile validates alias, a top-level identity's or a session member's
// (<parent alias>/<name>), and returns it as a relative file path.
func aliasFile(alias string) (string, error) {
	if parent, name, ok := strings.Cut(alias, "/"); ok {
		if aliasRe.MatchString(parent) && api.ValidName(name) {
			return filepath.Join(parent, name), nil
		}
	} else if aliasRe.MatchString(alias) {
		return alias, nil
	}
	return "", fmt.Errorf("invalid identity name %q", alias)
}

// Save writes a new identity with 0600 permissions. It never overwrites one:
// the file appears atomically via a hard link, which fails if the name is taken
// (also on case-insensitive disks), so two concurrent saves can't clobber.
func Save(id Identity) error {
	p, err := identityPath(id.Alias)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	if err := writeNew(p, b); errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("identity %q already exists (%s); pick another -as name", id.Alias, p)
	} else if err != nil {
		return err
	}
	return nil
}

// ValidAlias reports whether s can name a top-level local identity. A session
// member's alias is <parent alias>/<name> instead.
func ValidAlias(s string) bool { return aliasRe.MatchString(s) }

// List returns the names of all top-level local identities. Session members
// aren't among them (see ListChildren).
func List() ([]string, error) {
	home, err := Home()
	if err != nil {
		return nil, err
	}
	m, err := filepath.Glob(filepath.Join(home, "identities", "*.json"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range m {
		out = append(out, strings.TrimSuffix(filepath.Base(f), ".json"))
	}
	sort.Strings(out)
	return out, nil
}

// Load reads an identity. An empty alias falls back to $THREADSPLICE_AS, then to
// the only top-level identity if there is exactly one. An alias with a slash,
// <parent alias>/<name>, names a session member (see LoadChild).
func Load(alias string) (Identity, error) {
	if alias == "" {
		alias = os.Getenv("THREADSPLICE_AS")
	}
	if alias == "" {
		names, err := List()
		if err != nil {
			return Identity{}, err
		}
		switch len(names) {
		case 0:
			return Identity{}, errors.New("no identity yet: run `threadsplice room create` or `threadsplice join` first")
		case 1:
			alias = names[0]
		default:
			return Identity{}, fmt.Errorf("several identities exist (%s): choose one with --as or THREADSPLICE_AS", strings.Join(names, ", "))
		}
	}
	if parent, name, ok := strings.Cut(alias, "/"); ok {
		return LoadChild(parent, name)
	}
	p, err := identityPath(alias)
	if err != nil {
		return Identity{}, err
	}
	id, err := readIdentity(p)
	if errors.Is(err, os.ErrNotExist) {
		return Identity{}, fmt.Errorf("no identity named %q", alias)
	} else if err != nil {
		return Identity{}, err
	}
	id.Alias = alias
	return id, nil
}

// readIdentity reads the identity file p. A missing file is an fs.ErrNotExist.
func readIdentity(p string) (Identity, error) {
	var id Identity
	b, err := os.ReadFile(p)
	if err != nil {
		return id, err
	}
	if err := json.Unmarshal(b, &id); err != nil {
		return Identity{}, fmt.Errorf("%s: %w", p, err)
	}
	return id, nil
}

// writeNew creates p holding b, 0600 in a 0700 directory, by hard-linking a
// temp file into place (see Save). It fails with fs.ErrExist if p exists.
func writeNew(p string, b []byte) error {
	tmp, err := tempFile(filepath.Dir(p), b)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Link(tmp, p)
}

// replaceFile atomically replaces p with b, 0600 in a 0700 directory, by
// renaming a temp file over it.
func replaceFile(p string, b []byte) error {
	tmp, err := tempFile(filepath.Dir(p), b)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // gone already once the rename worked
	// On Windows a rename over a file another process has open (the deliverer
	// polls it) fails transiently; retry briefly.
	for attempt := 0; ; attempt++ {
		err = os.Rename(tmp, p)
		if err == nil || attempt == 4 {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// tempFile writes b to a new temp file in dir, creating dir (0700), and
// returns its name.
func tempFile(dir string, b []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".tmp-*") // CreateTemp makes it 0600
	if err != nil {
		return "", err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// Key returns the identity's room key.
func (id Identity) Key() (e2ee.Key, error) { return e2ee.ParseKey(id.RoomKey) }

// FormatInvite builds the string a room owner shares with a new participant:
//
//	<relay URL>#<invite token>.<room key>
//
// The room key rides in the URL fragment, which this tool never sends to the
// relay; only the invite token is redeemed there. Anyone holding the string can
// join (once, before it expires) and read the room, so share it privately.
func FormatInvite(relay, token string, key e2ee.Key) string {
	return strings.TrimRight(relay, "/") + "#" + token + "." + key.String()
}

// ParseInvite splits an invite string into relay URL, invite token and room key.
func ParseInvite(s string) (relay, token string, key e2ee.Key, err error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Fragment == "" {
		return "", "", key, errors.New("invalid invite: expected <relay URL>#<token>.<key>")
	}
	token, k, ok := strings.Cut(u.Fragment, ".")
	if !ok || token == "" {
		return "", "", key, errors.New("invalid invite: missing token or room key")
	}
	if key, err = e2ee.ParseKey(k); err != nil {
		return "", "", key, fmt.Errorf("invalid invite: %w", err)
	}
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), token, key, nil
}
