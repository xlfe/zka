package zka

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type Config struct {
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
		Command string   `json:"command"`
		Options []string `json:"options"`
	} `json:"ssh"`
	Notifications struct {
		NtfyCommand string `json:"ntfy_command"`
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
	return cfg, nil
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
