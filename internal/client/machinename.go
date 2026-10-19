package client

import (
	"os"
	"runtime"
	"strings"
)

// DefaultMachineName is the name a new machine membership gets unless the user
// picks one: the kind of machine, never its hostname or the user's login, since
// every room member and the relay see it.
func DefaultMachineName() string {
	switch runtime.GOOS {
	case "darwin":
		return "mac"
	case "windows":
		return "windows"
	case "linux":
		if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft") {
			return "wsl"
		}
		return "linux"
	}
	return "machine"
}
