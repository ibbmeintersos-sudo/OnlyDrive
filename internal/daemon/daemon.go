// Package daemon runs and supervises the rclone RC daemon for gd.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gd/internal/config"
	"gd/internal/rclone"
)

// PassFile returns the path where the generated RC password is stored.
func passFile() (string, error) {
	dir, _, _, _, err := config.Paths()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "rcpass"), nil
}

// LoadPass returns the stored RC password without creating one.
func LoadPass() (string, error) {
	p, err := passFile()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	pass := strings.TrimSpace(string(data))
	if len(pass) < 16 {
		return "", fmt.Errorf("rc password in %s is too short", p)
	}
	return pass, nil
}

// LoadOrCreatePass returns the stored RC password, generating one if needed.
func LoadOrCreatePass() (string, error) {
	if pass, err := LoadPass(); err == nil {
		return pass, nil
	}
	p, err := passFile()
	if err != nil {
		return "", err
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	pass := hex.EncodeToString(buf)
	return pass, os.WriteFile(p, []byte(pass), 0o600)
}

// pidFile returns the pid file path for the background daemon.
func pidFile() (string, error) {
	dir, _, _, _, err := config.Paths()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gd-daemon.pid"), nil
}

// Start launches the rclone RC daemon detached from this console and writes a pid file.
func Start() (string, error) {
	m, err := rclone.New()
	if err != nil {
		return "", err
	}
	if !m.Installed() {
		return "", fmt.Errorf("rclone is not installed yet; run: gd setup")
	}
	if existing, err := LoadPass(); err == nil {
		m.SetAuth("gd", existing)
	}
	if m.RCAlive() {
		return "already running", nil
	}
	pass, err := LoadOrCreatePass()
	if err != nil {
		return "", err
	}
	dir, rcloneConf, _, _, err := config.Paths()
	if err != nil {
		return "", err
	}
	logPath := filepath.Join(dir, "rclone.log")
	args := []string{
		"rcd",
		"--rc-addr", "127.0.0.1:" + strconv.Itoa(config.RcloneRCPort),
		"--config", rcloneConf,
		"--log-file", logPath,
		"--log-level", "INFO",
	}
	cmd := exec.Command(m.BinPath, args...)
	// Keep the RC password out of the process command line.
	cmd.Env = append(os.Environ(), "RCLONE_RC_USER=gd", "RCLONE_RC_PASS="+pass)
	cmd.SysProcAttr = detachedSysProcAttr()
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start rclone rcd: %w", err)
	}
	pf, err := pidFile()
	if err != nil {
		return "", err
	}
	_ = os.WriteFile(pf, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	// wait for readiness
	m2, _ := rclone.New()
	m2.SetAuth("gd", pass)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if m2.RCAlive() {
			return "started (pid " + strconv.Itoa(cmd.Process.Pid) + ")", nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	killByPID(cmd.Process.Pid)
	_ = os.Remove(pf)
	return "", fmt.Errorf("rclone rcd did not become ready; log: %s", logPath)
}

// Stop terminates the background daemon (via RC, then by pid as fallback).
func Stop() error {
	m, err := rclone.New()
	if err != nil {
		return err
	}
	pass, err := LoadOrCreatePass()
	if err == nil {
		m.SetAuth("gd", pass)
		_ = m.RCStop()
	}
	pf, err := pidFile()
	if err != nil {
		return nil
	}
	if data, err := os.ReadFile(pf); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			killByPID(pid)
		}
		_ = os.Remove(pf)
	}
	return nil
}

// Running reports whether the RC daemon responds.
func Running() bool {
	m, err := rclone.New()
	if err != nil {
		return false
	}
	if pass, err := LoadPass(); err == nil {
		m.SetAuth("gd", pass)
	}
	return m.RCAlive()
}

// EnsureDaemon starts the daemon if it is not running.
func EnsureDaemon() error {
	if Running() {
		return nil
	}
	_, err := Start()
	return err
}

// EnsureMounted re-mounts registered mounts after daemon start (e.g. reboot).
// Returns the list of mount points that are (still) active.
func EnsureMounted() ([]config.Mount, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := EnsureDaemon(); err != nil {
		return nil, err
	}
	m, err := rclone.New()
	if err != nil {
		return nil, err
	}
	pass, err := LoadOrCreatePass()
	if err != nil {
		return nil, err
	}
	m.SetAuth("gd", pass)

	active, _ := m.ListMounts(context.Background())
	activePts := map[string]bool{}
	for _, mm := range active {
		if pt, ok := mm["MountPoint"].(string); ok {
			activePts[pt] = true
		}
	}
	live := make([]config.Mount, 0, len(cfg.Mounts))
	changed := false
	for i := range cfg.Mounts {
		reg := &cfg.Mounts[i]
		if activePts[reg.Letter] {
			live = append(live, *reg)
			continue
		}
		// Keep the original target on the first remount; after that pick a
		// fresh one (a free letter on Windows, a fresh mountpoint elsewhere).
		target := reg.Letter
		if target == "" || !config.IsDriveLetter(target) {
			t, err := config.MountTarget(reg.Account)
			if err != nil {
				// Keep the registration so the next start can retry.
				continue
			}
			target = t
		}
		if err := m.MountRemote(context.Background(), reg.Remote, target, reg.VolName); err != nil {
			// Keep the registration so the next start can retry.
			continue
		}
		if reg.Letter != target {
			reg.Letter = target
			changed = true
		}
		live = append(live, *reg)
	}
	if changed {
		_ = cfg.Save()
	}
	return live, nil
}

// UnmountAccount unmounts every active mount registered for an account.
func UnmountAccount(cfg *config.Config, account string) {
	m, err := rclone.New()
	if err != nil {
		return
	}
	pass, err := LoadPass()
	if err != nil {
		return
	}
	m.SetAuth("gd", pass)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, mm := range cfg.Mounts {
		if strings.EqualFold(mm.Account, account) && mm.Letter != "" {
			_ = m.UnmountRemote(ctx, mm.Letter)
		}
	}
}

// ConfPath returns the rclone.conf path (used by serve and setup).
func ConfPath() string {
	_, rcloneConf, _, _, _ := config.Paths()
	return rcloneConf
}
