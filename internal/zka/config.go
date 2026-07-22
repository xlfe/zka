package zka

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Attention struct {
		States []AgentState `json:"states"`
	} `json:"attention"`
	Shell struct {
		Command []string `json:"command"`
	} `json:"shell"`
	Kitty struct {
		Command       string   `json:"command"`
		KittenCommand string   `json:"kitten_command"`
		Watcher       string   `json:"watcher"`
		ExtraArgs     []string `json:"extra_args"`
	} `json:"kitty"`
	ZMX struct {
		Command string `json:"command"`
	} `json:"zmx"`
	SSH struct {
		Command       string   `json:"command"`
		Options       []string `json:"options"`
		IdentityAgent string   `json:"identity_agent"`
		ForwardAgent  bool     `json:"forward_agent"`
	} `json:"ssh"`
	Notifications struct {
		DesktopEnabled      bool   `json:"desktop_enabled"`
		NtfyEnabled         bool   `json:"ntfy_enabled"`
		NtfyIncludeEvidence bool   `json:"ntfy_include_evidence"`
		NtfyCommand         string `json:"ntfy_command"`
	} `json:"notifications"`
}

func defaultConfig() Config {
	var cfg Config
	cfg.Shell.Command = []string{"fish"}
	cfg.Kitty.Command = "kitty"
	cfg.Kitty.KittenCommand = "kitten"
	cfg.Kitty.Watcher = findWatcher()
	cfg.ZMX.Command = "zmx"
	cfg.SSH.Command = "ssh"
	cfg.SSH.Options = []string{
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		"-o", "BatchMode=yes",
	}
	cfg.Attention.States = []AgentState{StateBlocked, StateError, StateDone}
	cfg.Notifications.DesktopEnabled = true
	cfg.Notifications.NtfyEnabled = true
	cfg.Notifications.NtfyIncludeEvidence = false
	cfg.Notifications.NtfyCommand = "ntfy-send"
	return cfg
}

func LoadConfig() (Config, error) {
	cfg := defaultConfig()
	path := os.Getenv("ZKA_CONFIG")
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode config %s: %w", path, err)
		}
	}
	applyConfigEnvironment(&cfg)
	if len(cfg.Shell.Command) == 0 || cfg.Shell.Command[0] == "" {
		return Config{}, fmt.Errorf("shell.command must contain an executable")
	}
	if len(cfg.Attention.States) == 0 {
		return Config{}, fmt.Errorf("attention.states must contain at least one state")
	}
	seenStates := map[AgentState]bool{}
	for _, state := range cfg.Attention.States {
		if state != StateBlocked && state != StateError && state != StateDone {
			return Config{}, fmt.Errorf("attention.states contains unsupported state %q", state)
		}
		if seenStates[state] {
			return Config{}, fmt.Errorf("attention.states contains duplicate state %q", state)
		}
		seenStates[state] = true
	}
	for label, command := range map[string]string{
		"kitty.command":              cfg.Kitty.Command,
		"kitty.kitten_command":       cfg.Kitty.KittenCommand,
		"zmx.command":                cfg.ZMX.Command,
		"ssh.command":                cfg.SSH.Command,
		"notifications.ntfy_command": cfg.Notifications.NtfyCommand,
	} {
		if command == "" {
			return Config{}, fmt.Errorf("%s must not be empty", label)
		}
	}
	if cfg.SSH.IdentityAgent != "" {
		cfg.SSH.Options = append([]string{"-o", "IdentityAgent=" + cfg.SSH.IdentityAgent}, cfg.SSH.Options...)
	}
	if cfg.SSH.ForwardAgent {
		cfg.SSH.Options = append([]string{"-o", "ForwardAgent=yes"}, cfg.SSH.Options...)
	}
	return cfg, nil
}

type sshAgentInfo struct {
	InheritedSocket string `json:"inherited_socket,omitempty"`
	IdentityAgent   string `json:"identity_agent,omitempty"`
	EffectiveSocket string `json:"effective_socket,omitempty"`
}

func newSSHAgentInfo(cfg Config, inheritedSocket string) sshAgentInfo {
	identityAgent := cfg.SSH.IdentityAgent
	if identityAgent == "" {
		identityAgent = sshIdentityAgentOption(cfg.SSH.Options)
	}
	effectiveSocket := inheritedSocket
	if identityAgent != "" {
		effectiveSocket = expandSSHIdentityAgent(identityAgent, inheritedSocket)
	}
	return sshAgentInfo{
		InheritedSocket: inheritedSocket,
		IdentityAgent:   identityAgent,
		EffectiveSocket: effectiveSocket,
	}
}

func sshIdentityAgentOption(options []string) string {
	for index := 0; index < len(options); index++ {
		option := options[index]
		if option == "-o" && index+1 < len(options) {
			if value := sshConfigOptionValue(options[index+1], "IdentityAgent"); value != "" {
				return value
			}
			index++
			continue
		}
		if strings.HasPrefix(option, "-o") {
			if value := sshConfigOptionValue(strings.TrimPrefix(option, "-o"), "IdentityAgent"); value != "" {
				return value
			}
		}
	}
	return ""
}

func sshConfigOptionValue(option, name string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(option), func(r rune) bool { return r == '=' || r == ' ' || r == '\t' })
	if len(parts) == 2 && strings.EqualFold(parts[0], name) {
		return parts[1]
	}
	return ""
}

func expandSSHIdentityAgent(value, inheritedSocket string) string {
	switch value {
	case "SSH_AUTH_SOCK", "$SSH_AUTH_SOCK", "${SSH_AUTH_SOCK}":
		return inheritedSocket
	}
	const escapedPercent = "\x00"
	expanded := strings.ReplaceAll(value, "%%", escapedPercent)
	expanded = strings.ReplaceAll(expanded, "%i", strconv.Itoa(os.Getuid()))
	return strings.ReplaceAll(expanded, escapedPercent, "%")
}

func sameSSHAgentSocket(left, right string) bool {
	if left == right {
		return true
	}
	if left == "" || right == "" || left == "none" || right == "none" {
		return false
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func withSSHAgentMismatchHint(err error, daemonAgent sshAgentInfo, callerSocket string) error {
	if err == nil || !sshAuthenticationFailure(err) || sameSSHAgentSocket(daemonAgent.EffectiveSocket, callerSocket) {
		return err
	}
	return fmt.Errorf("%w\nSSH agent mismatch: zkad uses %s; caller uses %s. Configure services.zka.ssh.identityAgent or import SSH_AUTH_SOCK and restart zkad", err, displaySSHAgentSocket(daemonAgent.EffectiveSocket), displaySSHAgentSocket(callerSocket))
}

func sshAuthenticationFailure(err error) bool {
	detail := strings.ToLower(err.Error())
	return strings.Contains(detail, "permission denied") || strings.Contains(detail, "agent refused operation")
}

func displaySSHAgentSocket(socket string) string {
	if socket == "" {
		return "SSH_AUTH_SOCK is not set"
	}
	if socket == "none" {
		return "no agent (IdentityAgent=none)"
	}
	return socket
}

func applyConfigEnvironment(cfg *Config) {
	if value := os.Getenv("ZKA_KITTY_COMMAND"); value != "" {
		cfg.Kitty.Command = value
	}
	if value := os.Getenv("ZKA_KITTEN_COMMAND"); value != "" {
		cfg.Kitty.KittenCommand = value
	}
	if value := os.Getenv("ZKA_KITTY_WATCHER"); value != "" {
		cfg.Kitty.Watcher = value
	}
	if value := os.Getenv("ZKA_ZMX_COMMAND"); value != "" {
		cfg.ZMX.Command = value
	}
	if value := os.Getenv("ZKA_SSH_COMMAND"); value != "" {
		cfg.SSH.Command = value
	}
	if value := os.Getenv("ZKA_NTFY_COMMAND"); value != "" {
		cfg.Notifications.NtfyCommand = value
	}
}

func findWatcher() string {
	if value := os.Getenv("ZKA_KITTY_WATCHER"); value != "" {
		return value
	}
	exe, err := os.Executable()
	if err == nil {
		candidate := filepath.Clean(filepath.Join(filepath.Dir(exe), "..", "share", "zka", "kitty-watcher.py"))
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
	}
	for _, candidate := range []string{"kitty/watcher.py", "./kitty/watcher.py"} {
		if _, err := os.Stat(candidate); err == nil {
			absolute, absErr := filepath.Abs(candidate)
			if absErr == nil {
				return absolute
			}
		}
	}
	return "kitty-watcher.py"
}

func configExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}
