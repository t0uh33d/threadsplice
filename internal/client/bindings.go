package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
)

// A Binding ties one Claude Code session to the session member it joined as,
// so that session's MCP server and hooks act as that member while the other
// sessions on the machine stay out. Bindings are local files,
// <home>/bindings/<session id>.json. Next to them, pid-<pid>.json records the
// conversation each Claude Code process shows now, which /clear and resume
// change; a binding is live only while a live process shows its session.
type Binding struct {
	SessionID string    `json:"session_id"`
	Machine   string    `json:"machine"` // the parent's local alias
	Child     string    `json:"child"`   // the session member's participant name
	PID       int       `json:"pid"`     // the Claude Code process that bound it
	UpdatedAt time.Time `json:"updated_at"`
}

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ValidSessionID reports whether s can be a session id. Session ids end up in
// file names: check one before it goes into a path. Ids starting with "pid-"
// are refused too, since that's how the per-process files beside the bindings
// are named.
func ValidSessionID(s string) bool {
	return sessionIDRe.MatchString(s) && !strings.HasPrefix(strings.ToLower(s), "pid-")
}

func bindingsDir() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "bindings"), nil
}

func bindingPath(sid string) (string, error) {
	if !ValidSessionID(sid) {
		return "", fmt.Errorf("invalid session id %q", sid)
	}
	dir, err := bindingsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sid+".json"), nil
}

// SaveBinding records b (0600, atomic replace), replacing any earlier binding
// of the same session.
func SaveBinding(b Binding) error {
	if !aliasRe.MatchString(b.Machine) || !api.ValidName(b.Child) {
		return fmt.Errorf("invalid binding to session member %q", b.Machine+"/"+b.Child)
	}
	p, err := bindingPath(b.SessionID)
	if err != nil {
		return err
	}
	b.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return replaceFile(p, data)
}

// LoadBinding returns session sid's binding, if it has one.
func LoadBinding(sid string) (Binding, bool, error) {
	var b Binding
	p, err := bindingPath(sid)
	if err != nil {
		return b, false, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return b, false, nil
	} else if err != nil {
		return b, false, err
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return Binding{}, false, fmt.Errorf("%s: %w", p, err)
	}
	// Machine and Child name the files read next.
	if b.SessionID != sid || !aliasRe.MatchString(b.Machine) || !api.ValidName(b.Child) {
		return Binding{}, false, fmt.Errorf("%s: invalid binding", p)
	}
	return b, true, nil
}

// DeleteBinding removes session sid's binding; a missing one is fine.
func DeleteBinding(sid string) error {
	p, err := bindingPath(sid)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Bindings returns all bindings, sorted by session id. One that can't be read
// is left out: it can't bind its session either.
func Bindings() ([]Binding, error) {
	dir, err := bindingsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir) // sorted by file name
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Binding
	for _, e := range entries {
		sid, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !ValidSessionID(sid) {
			continue // pid-<pid>.json and temp files
		}
		if b, ok, err := LoadBinding(sid); err == nil && ok {
			out = append(out, b)
		}
	}
	return out, nil
}

// current is the content of pid-<pid>.json.
type current struct {
	SessionID string    `json:"session_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

func currentPath(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid process id %d", pid)
	}
	dir, err := bindingsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pid-"+strconv.Itoa(pid)+".json"), nil
}

// SetCurrent records sid as the conversation the Claude Code process pid
// shows now.
func SetCurrent(pid int, sid string) error {
	if !ValidSessionID(sid) {
		return fmt.Errorf("invalid session id %q", sid)
	}
	p, err := currentPath(pid)
	if err != nil {
		return err
	}
	b, err := json.Marshal(current{SessionID: sid, UpdatedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return replaceFile(p, b)
}

// Current returns the conversation the process pid shows now, if recorded.
func Current(pid int) (string, bool) {
	p, err := currentPath(pid)
	if err != nil {
		return "", false
	}
	var c current
	if b, err := os.ReadFile(p); err != nil || json.Unmarshal(b, &c) != nil || !ValidSessionID(c.SessionID) {
		return "", false
	}
	return c.SessionID, true
}

// ClearCurrent forgets the conversation of the process pid, as it ends.
func ClearCurrent(pid int) {
	if p, err := currentPath(pid); err == nil {
		os.Remove(p)
	}
}

// InUse reports whether a live session other than exceptSID holds the session
// member child of the machine membership machine: a binding to it that is Live.
func InUse(machine, child, exceptSID string) bool {
	bs, _ := Bindings()
	for _, b := range bs {
		if b.Machine == machine && b.Child == child && b.SessionID != exceptSID && b.Live() {
			return true
		}
	}
	return false
}

// Live reports whether a live process shows b's session now: the process that
// bound it, or one that resumed the conversation since, whose pid b doesn't
// record.
// ponytail: a crashed process's pid that the system reuses keeps its session
// looking live until the new process ends; joining then takes another name.
// Record process start times if that bites.
func (b Binding) Live() bool {
	if sid, ok := Current(b.PID); ok && sid == b.SessionID && alive(b.PID) {
		return true
	}
	dir, err := bindingsDir()
	if err != nil {
		return false
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n, ok := strings.CutPrefix(e.Name(), "pid-")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(n, ".json"))
		if err != nil || pid == b.PID {
			continue
		}
		if sid, ok := Current(pid); ok && sid == b.SessionID && alive(pid) {
			return true
		}
	}
	return false
}

// alive reports whether the process pid exists.
func alive(pid int) bool {
	if pid <= 0 {
		return false // signalling 0 or -1 would reach a whole process group
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer p.Release()
	if runtime.GOOS == "windows" {
		// ponytail: FindProcess opened the process, so it exists or still has
		// open handles; Windows also reuses ids quickly. Dead sessions'
		// bindings aren't reliably reclaimed there. Compare start times if
		// native Windows sessions ever need it.
		return true
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}
