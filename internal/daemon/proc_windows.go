//go:build windows

package daemon

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// detachedSysProcAttr returns attributes to run the daemon detached on Windows.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 /*DETACHED_PROCESS*/}
}

// killByPID forcefully terminates an rclone process by id (best effort).
func killByPID(pid int) {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
	if err != nil {
		return
	}
	text := strings.ToLower(string(out))
	if !strings.Contains(text, "rclone") || !strings.Contains(text, strconv.Itoa(pid)) {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
