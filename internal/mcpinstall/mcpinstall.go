// Package mcpinstall registers (and removes) the aispeech MCP server in the
// config of terminal AI agents — Claude Code via its own `mcp` CLI, Codex and
// Gemini via their config files. It registers the stdio bridge
// (`aispeech mcp-proxy`), which every MCP client supports uniformly; each agent
// spawns its own proxy that forwards to the shared HTTP hub. Mirrors aish.
package mcpinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const serverName = "aispeech"

// Status describes one agent's install state.
type Status struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Detected  bool   `json:"detected"`  // the tool/config is present
	Installed bool   `json:"installed"` // our entry is registered
	Note      string `json:"note,omitempty"`
}

// proxyCommand returns the command an agent should run for the MCP server.
// Prefer bare "aispeech" when on PATH (survives NixOS rebuilds and moves);
// otherwise the absolute path to this executable. url is passed through so the
// proxy finds the hub even if the configured port differs; id becomes the
// session's default name at the hub.
func proxyCommand(id, url string) (string, []string) {
	cmd := "aispeech"
	if _, err := exec.LookPath("aispeech"); err != nil {
		if exe, err := os.Executable(); err == nil {
			cmd = exe
		}
	}
	return cmd, []string{"mcp-proxy", "--name", id, "--url", url}
}

// Statuses reports every supported agent's detection and install state.
func Statuses(url string) []Status {
	return []Status{claudeStatus(), codexStatus(), geminiStatus()}
}

// Install registers the stdio bridge for the given agent id.
func Install(id, url string) error {
	cmd, args := proxyCommand(id, url)
	switch id {
	case "claude":
		return claudeInstall(cmd, args)
	case "codex":
		return codexInstall(cmd, args)
	case "gemini":
		return geminiInstall(cmd, args)
	}
	return fmt.Errorf("unknown agent %q", id)
}

// Uninstall removes the entry for the given agent id.
func Uninstall(id string) error {
	switch id {
	case "claude":
		return claudeUninstall()
	case "codex":
		return codexUninstall()
	case "gemini":
		return geminiUninstall()
	}
	return fmt.Errorf("unknown agent %q", id)
}

// --- Claude Code (via its own CLI) ---

func claudeStatus() Status {
	s := Status{ID: "claude", Name: "Claude Code"}
	if _, err := exec.LookPath("claude"); err != nil {
		s.Note = "claude not on PATH"
		return s
	}
	s.Detected = true
	s.Installed = exec.Command("claude", "mcp", "get", serverName).Run() == nil
	return s
}

func claudeInstall(cmd string, args []string) error {
	_ = exec.Command("claude", "mcp", "remove", serverName, "--scope", "user").Run() // idempotent
	add := append([]string{"mcp", "add", serverName, "--scope", "user", "--", cmd}, args...)
	if out, err := exec.Command("claude", add...).CombinedOutput(); err != nil {
		return fmt.Errorf("claude mcp add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func claudeUninstall() error {
	out, err := exec.Command("claude", "mcp", "remove", serverName, "--scope", "user").CombinedOutput()
	if err != nil {
		return fmt.Errorf("claude mcp remove: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- Codex (config.toml) ---

func codexPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

var codexBlock = regexp.MustCompile(`(?ms)^\[mcp_servers\.aispeech\]\n(?:[^\[].*\n?)*`)

func codexStatus() Status {
	s := Status{ID: "codex", Name: "Codex"}
	p := codexPath()
	if _, err := exec.LookPath("codex"); err == nil {
		s.Detected = true
	} else if _, err := os.Stat(filepath.Dir(p)); err == nil {
		s.Detected = true
	}
	if b, err := os.ReadFile(p); err == nil && codexBlock.Match(b) {
		s.Installed = true
	}
	return s
}

func codexInstall(cmd string, args []string) error {
	p := codexPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := os.ReadFile(p)
	body := codexBlock.ReplaceAll(b, nil) // drop any existing block
	block := fmt.Sprintf("[mcp_servers.%s]\ncommand = %q\nargs = %s\n", serverName, cmd, tomlStrArray(args))
	text := strings.TrimRight(string(body), "\n")
	if text != "" {
		text += "\n\n"
	}
	text += block
	return os.WriteFile(p, []byte(text), 0o644)
}

func codexUninstall() error {
	p := codexPath()
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	out := strings.TrimRight(string(codexBlock.ReplaceAll(b, nil)), "\n") + "\n"
	return os.WriteFile(p, []byte(out), 0o644)
}

func tomlStrArray(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// --- Gemini (settings.json) ---

func geminiPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "settings.json")
}

func geminiStatus() Status {
	s := Status{ID: "gemini", Name: "Gemini CLI"}
	p := geminiPath()
	if _, err := exec.LookPath("gemini"); err == nil {
		s.Detected = true
	} else if _, err := os.Stat(filepath.Dir(p)); err == nil {
		s.Detected = true
	}
	if m, err := readJSONObject(p); err == nil {
		if servers, ok := m["mcpServers"].(map[string]any); ok {
			_, s.Installed = servers[serverName]
		}
	}
	return s
}

func geminiInstall(cmd string, args []string) error {
	p := geminiPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	m, err := readJSONObject(p)
	if err != nil {
		m = map[string]any{}
	}
	servers, ok := m["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
		m["mcpServers"] = servers
	}
	servers[serverName] = map[string]any{"command": cmd, "args": args}
	return writeJSONObject(p, m)
}

func geminiUninstall() error {
	p := geminiPath()
	m, err := readJSONObject(p)
	if err != nil {
		return nil
	}
	if servers, ok := m["mcpServers"].(map[string]any); ok {
		delete(servers, serverName)
	}
	return writeJSONObject(p, m)
}

func readJSONObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if len(strings.TrimSpace(string(b))) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func writeJSONObject(path string, m map[string]any) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
