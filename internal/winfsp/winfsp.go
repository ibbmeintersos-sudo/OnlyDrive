// Package winfsp detects and installs WinFsp on Windows.
package winfsp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"gd/internal/config"
	"gd/internal/rclone"
)

// PinnedWinFspSHA256 is the SHA-256 of winfsp-2.1.25156.msi.
const PinnedWinFspSHA256 = "073a70e00f77423e34bed98b86e600def93393ba5822204fac57a29324db9f7a"

// msiURL returns the pinned WinFsp MSI download URL.
func msiURL() string {
	ver := config.PinnedWinFspVersion
	// winfsp-2.1.25156.msi for v2.1
	switch ver {
	case "2.1":
		return "https://github.com/winfsp/winfsp/releases/download/v2.1/winfsp-2.1.25156.msi"
	default:
		return fmt.Sprintf("https://github.com/winfsp/winfsp/releases/download/v%s/winfsp-%s.msi", ver, ver)
	}
}

// Installed reports whether WinFsp is available. We check the DLL in several
// known locations AND fall back to a live FUSE probe via rclone, because some
// WinFsp builds place the DLL outside System32 (e.g. WinFsp 2.x 64-bit only).
func Installed() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	if dllInstalled() {
		return true
	}
	// live probe: if mounting actually works, WinFsp must be present
	return mountProbe()
}

func dllInstalled() bool {
	sysDir := os.Getenv("SystemRoot")
	if sysDir == "" {
		sysDir = `C:\Windows`
	}
	candidates := []string{
		filepath.Join(sysDir, "System32", "winfsp-x64.dll"),
		filepath.Join(sysDir, "System32", "winfsp-arm64.dll"),
		filepath.Join(sysDir, "SysWOW64", "winfsp-x64.dll"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return true
		}
	}
	// program-files installs ship the DLL under bin/
	for _, pf := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if pf == "" {
			continue
		}
		c := filepath.Join(pf, "WinFsp", "bin", "winfsp-x64.dll")
		if _, err := os.Stat(c); err == nil {
			return true
		}
	}
	return false
}

// mountProbe starts a throwaway rcd and tries mounting a tiny local remote.
var probeOnce struct {
	sync.Once
	result bool
}

func mountProbe() bool {
	probeOnce.Do(func() {
		m, err := rclone.New()
		if err != nil || !m.Installed() {
			probeOnce.result = false
			return
		}
		probeOnce.result = m.MountProbe()
	})
	return probeOnce.result
}

// Ensure installs WinFsp via its MSI if it is not already present.
// The MSI triggers a single UAC prompt which the user must accept.
func Ensure(ctx context.Context) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("winfsp is only needed on Windows")
	}
	if Installed() {
		return nil
	}
	dir, _, _, _, err := config.Paths()
	if err != nil {
		return err
	}
	msiPath := filepath.Join(dir, "winfsp.msi")
	if err := download(ctx, msiURL(), msiPath); err != nil {
		return fmt.Errorf("download winfsp: %w", err)
	}
	if config.PinnedWinFspVersion == "2.1" {
		if err := verifySHA256(msiPath, PinnedWinFspSHA256); err != nil {
			_ = os.Remove(msiPath)
			return fmt.Errorf("verify winfsp MSI: %w", err)
		}
	}
	// msiexec returns 3010 when a reboot is required; both 0 and 3010 are success.
	cmd := exec.CommandContext(ctx, "msiexec", "/i", msiPath, "/qn", "/norestart")
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	out, err := cmd.CombinedOutput()
	if err != nil {
		// fall back to an interactive install (UI) if silent failed
		cmd2 := exec.CommandContext(ctx, "msiexec", "/i", msiPath)
		err2 := cmd2.Run()
		if err2 != nil {
			return fmt.Errorf("install winfsp: %v (msi output: %s)", err, truncate(string(out), 300))
		}
	}
	if !Installed() {
		return fmt.Errorf("winfsp installed but DLL still not found; reboot may be required")
	}
	return nil
}

// InstalledVersion returns a human string for doctor output.
func InstalledVersion() string {
	if !Installed() {
		return "not installed"
	}
	return "installed"
}

func download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: http %d", url, resp.StatusCode)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func verifySHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, want)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}
