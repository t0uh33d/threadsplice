package client

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// SessionInfo records where the live agent session using an identity can be
// reached. A SessionStart hook writes it; the MCP server's deliverer reads it.
// It is local-only and never leaves the machine.
type SessionInfo struct {
	Agent     string    `json:"agent"`            // "claude", "codex" or "copilot"
	SessionID string    `json:"session_id"`       // agent session id (for Codex: the thread id)
	Socket    string    `json:"socket,omitempty"` // Claude Code: CLAUDE_CODE_MESSAGING_SOCKET
	Cwd       string    `json:"cwd,omitempty"`
	PID       int       `json:"pid,omitempty"` // the agent process, when known
	UpdatedAt time.Time `json:"updated_at"`
}

func sessionPath(alias string) (string, error) {
	p, err := identityPath(alias) // validates alias
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(filepath.Dir(p)), "sessions", alias+".json"), nil
}

// SaveSession records the live session for alias (0600, atomic replace).
func SaveSession(alias string, s SessionInfo) error {
	p, err := sessionPath(alias)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	s.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// LoadSession returns the live session recorded for alias, if any.
func LoadSession(alias string) (SessionInfo, bool, error) {
	var s SessionInfo
	p, err := sessionPath(alias)
	if err != nil {
		return s, false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return s, false, nil
	} else if err != nil {
		return s, false, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false, err
	}
	return s, true, nil
}
