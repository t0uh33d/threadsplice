package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/t0uh33d/threadsplice/internal/e2ee"
)

// Identity is this machine's membership in one room. It holds two secrets — the
// participant token (relay access) and the room key (message confidentiality) —
// and is stored as JSON readable only by the current user.
type Identity struct {
	Alias         string `json:"-"`
	Relay         string `json:"relay"`
	RoomID        string `json:"room_id"`
	RoomName      string `json:"room_name"`
	ParticipantID string `json:"participant_id"`
	Name          string `json:"name"`
	Owner         bool   `json:"owner,omitempty"`
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

// Save writes the identity atomically with 0600 permissions. It refuses to
// overwrite an existing identity unless replace is set.
func Save(id Identity, replace bool) error {
	p, err := identityPath(id.Alias)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err == nil && !replace {
		return fmt.Errorf("identity %q already exists (%s); pick another --as name", id.Alias, p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// List returns the names of all local identities.
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
// the only identity if there is exactly one.
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
	p, err := identityPath(alias)
	if err != nil {
		return Identity{}, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Identity{}, fmt.Errorf("no identity named %q", alias)
	} else if err != nil {
		return Identity{}, err
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		return Identity{}, fmt.Errorf("%s: %w", p, err)
	}
	id.Alias = alias
	return id, nil
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
