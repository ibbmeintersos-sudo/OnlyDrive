package serve

import (
	"os"
	"syscall"
)

func detachAttr() *syscall.SysProcAttr {
	return detachedSysProcAttr()
}

func killPID(pid int) {
	if !pidAliveCheck(pid) {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
