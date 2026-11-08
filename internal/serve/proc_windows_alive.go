//go:build windows

package serve

import (
	"os/exec"
	"strconv"
	"strings"
)

// windowsProcAlive checks pid existence via tasklist (cheap enough here).
func windowsProcAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
	if err != nil {
		return false
	}
	text := strings.ToLower(string(out))
	return strings.Contains(text, "rclone") && strings.Contains(text, strconv.Itoa(pid))
}
