//go:build !windows

package daemon

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// detachedSysProcAttr returns attributes to run the daemon detached on unix.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// killByPID sends SIGKILL to an rclone process on unix.
func killByPID(pid int) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return
	}
	if !strings.Contains(strings.ToLower(string(out)), "rclone") {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
