// Package mcpinstall registers the gd MCP server into popular MCP clients.
package mcpinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Client describes one supported MCP client app.
type Client struct {
	Key        string // gd add-mcp key
	Name       string // human name
	ConfigPath func() (string, error)
	Format     string // "claude-desktop" | "vscode"
}

// SelfPath is the absolute path of the running gd executable.
func SelfPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "gd"
	}
	return exe
}

// mcpEntry builds the server entry for a client config.
func mcpEntry() map[string]any {
	return map[string]any{
		"command": SelfPath(),
		"args":    []string{"mcp"},
	}
}

// Clients returns the supported client list for the current OS.
func Clients() []Client {
	home, _ := os.UserHomeDir()
	var out []Client

	claudeDesktop := func() (string, error) {
		switch runtime.GOOS {
		case "windows":
			app := os.Getenv("APPDATA")
			if app == "" {
				app = filepath.Join(home, "AppData", "Roaming")
			}
			return filepath.Join(app, "Claude", "claude_desktop_config.json"), nil
		case "darwin":
			return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
		default:
			return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), nil
		}
	}
	claudeCode := func() (string, error) {
		return filepath.Join(home, ".claude.json"), nil
	}
	cursor := func() (string, error) {
		switch runtime.GOOS {
		case "windows":
			app := os.Getenv("APPDATA")
			if app == "" {
				app = filepath.Join(home, "AppData", "Roaming")
			}
			return filepath.Join(app, "Cursor", "mcp.json"), nil
		default:
			return filepath.Join(home, ".cursor", "mcp.json"), nil
		}
	}
	vscode := func() (string, error) {
		switch runtime.GOOS {
		case "windows":
			app := os.Getenv("APPDATA")
			if app == "" {
				app = filepath.Join(home, "AppData", "Roaming")
			}
			return filepath.Join(app, "Code", "User", "settings.json"), nil
		default:
			return filepath.Join(home, ".config", "Code", "User", "settings.json"), nil
		}
	}
	windsurf := func() (string, error) {
		switch runtime.GOOS {
		case "windows":
			app := os.Getenv("APPDATA")
			if app == "" {
				app = filepath.Join(home, "AppData", "Roaming")
			}
			return filepath.Join(app, "Windsurf", "mcp.json"), nil
		default:
			return filepath.Join(home, ".codeium", "windsurf", "mcp.json"), nil
		}
	}

	out = append(out,
		Client{"claude-desktop", "Claude Desktop", claudeDesktop, "claude-desktop"},
		Client{"claude-code", "Claude Code", claudeCode, "claude-code"},
		Client{"cursor", "Cursor", cursor, "claude-desktop"},
		Client{"windsurf", "Windsurf", windsurf, "claude-desktop"},
		Client{"vscode", "VS Code (GitHub Copilot)", vscode, "vscode"},
	)
	return out
}

// FindClient locates a client by key (case-insensitive).
func FindClient(key string) *Client {
	for i := range Clients() {
		if strings.EqualFold(Clients()[i].Key, key) {
			return &Clients()[i]
		}
	}
	return nil
}

// Install merges the gd MCP server entry into the client's config file.
func Install(c *Client) error {
	path, err := c.ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	cfg := map[string]any{}
	if err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		// strip VS Code JSONC comments and trailing commas
		text := stripJSONC(string(raw))
		if err := json.Unmarshal([]byte(text), &cfg); err != nil {
			return fmt.Errorf("%s: parse existing %s: %w (fix the file manually and retry)", c.Name, path, err)
		}
	}

	const key = "gd"
	switch c.Format {
	case "vscode":
		mcp := map[string]any{}
		if v, ok := cfg["mcp"].(map[string]any); ok {
			mcp = v
		}
		mcp[key] = map[string]any{"command": SelfPath(), "args": []string{"mcp"}}
		cfg["mcp"] = mcp
	default: // claude-desktop format: mcpServers
		servers := map[string]any{}
		if v, ok := cfg["mcpServers"].(map[string]any); ok {
			servers = v
		}
		servers[key] = mcpEntry()
		cfg["mcpServers"] = servers
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if len(raw) > 0 {
		if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
			return fmt.Errorf("backup %s: %w", path, err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return nil
}

// stripJSONC removes // and /* */ comments plus trailing commas outside
// strings. It is a compatibility shim for VS Code's JSONC settings.
func stripJSONC(s string) string {
	var b strings.Builder
	inStr := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inStr {
			b.WriteByte(ch)
			if ch == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
				continue
			}
			if ch == '"' {
				inStr = false
			}
			continue
		}
		switch {
		case ch == '"':
			inStr = true
			b.WriteByte(ch)
		case ch == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i < len(s) {
				b.WriteByte('\n')
			}
		case ch == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++
		default:
			b.WriteByte(ch)
		}
	}
	return removeTrailingCommas(b.String())
}

func removeTrailingCommas(s string) string {
	var b strings.Builder
	inStr := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inStr {
			b.WriteByte(ch)
			if ch == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
				continue
			}
			if ch == '"' {
				inStr = false
			}
			continue
		}
		if ch == '"' {
			inStr = true
			b.WriteByte(ch)
			continue
		}
		if ch == ',' {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\r' || s[j] == '\n') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue
			}
		}
		b.WriteByte(ch)
	}
	return b.String()
}
