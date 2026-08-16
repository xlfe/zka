package zka

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Headless marks a machine that never hosts a Kitty view: an origin that
	// runs only zkad, zmx, sshd, and the agents inside its panes. The
	// view-layer commands below stay configured as bare names — the daemon
	// never executes them, since every daemon→Kitty call is gated on a
	// local-node attachment — and doctor skips probing them.
	Headless  bool `json:"headless"`
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
		Command         string            `json:"command"`
		Options         []string          `json:"options"`
		IdentityAgent   string            `json:"identity_agent"`
		ExpectedNodeIDs map[string]string `json:"expected_node_ids"`
	} `json:"ssh"`
	Credentials   CredentialsConfig `json:"credentials"`
	Notifications struct {
		DesktopEnabled      bool   `json:"desktop_enabled"`
		NtfyEnabled         bool   `json:"ntfy_enabled"`
		NtfyIncludeEvidence bool   `json:"ntfy_include_evidence"`
		NtfyCommand         string `json:"ntfy_command"`
	} `json:"notifications"`
	// Focus holds the compositor helper used to raise the window owning a pane.
	// It is a configured command rather than a bare name because zkad runs from
	// a systemd unit whose PATH is the module's servicePath, not a login shell's.
	Focus struct {
		SwayCommand string `json:"sway_command"`
	} `json:"focus"`
	Integrations struct {
		CodexManagedHooks  bool `json:"codex_managed_hooks"`
		ClaudeManagedHooks bool `json:"claude_managed_hooks"`
	} `json:"integrations"`
}

type CredentialsConfig struct {
	DefaultBundle string                              `json:"default_bundle"`
	GnuPG         CredentialGnuPGConfig               `json:"gnupg"`
	PIVB          CredentialPIVBConfig                `json:"pivb"`
	Bundles       map[string]CredentialBundleConfig   `json:"bundles"`
	Providers     map[string]CredentialProviderConfig `json:"providers"`
}

type CredentialPIVBConfig struct {
	ForwardSocket string `json:"forward_socket"`
	Command       string `json:"command"`
	RoutingMode   string `json:"routing_mode"`
	// GrantWindow is the default authorisation window a claim requests when the
	// operator names none. Empty leaves every claim windowless.
	GrantWindow string `json:"grant_window"`
}

type CredentialProviderConfig struct {
	NodeID             string   `json:"node_id"`
	SSHSourceAddresses []string `json:"ssh_source_addresses"`
}

type CredentialGnuPGConfig struct {
	Command                string `json:"command"`
	GPGConfCommand         string `json:"gpgconf_command"`
	GPGConnectAgentCommand string `json:"gpg_connect_agent_command"`
	ConfigureAgent         bool   `json:"configure_agent"`
	OperationTimeout       string `json:"operation_timeout"`
}

type CredentialBundleConfig struct {
	SSHAgent struct {
		Enable bool `json:"enable"`
	} `json:"ssh_agent"`
	OpenPGP struct {
		Enable      bool     `json:"enable"`
		SigningKeys []string `json:"signing_keys"`
	} `json:"openpgp"`
	PIVB struct {
		Enable  bool     `json:"enable"`
		Aliases []string `json:"aliases"`
	} `json:"pivb"`
}

func (c Config) credentialBundle(name string) (CredentialBundleConfig, bool) {
	bundle, ok := c.Credentials.Bundles[name]
	return bundle, ok
}

func (c Config) credentialsEnabled() bool { return len(c.Credentials.Bundles) != 0 }

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
	cfg.Credentials.GnuPG.Command = "gpg"
	cfg.Credentials.GnuPG.GPGConfCommand = "gpgconf"
	cfg.Credentials.GnuPG.GPGConnectAgentCommand = "gpg-connect-agent"
	cfg.Credentials.GnuPG.OperationTimeout = "45s"
	cfg.Credentials.PIVB.Command = "pivb"
	cfg.Credentials.PIVB.RoutingMode = pivbRoutingEnvironment
	cfg.Credentials.PIVB.GrantWindow = ""
	cfg.Credentials.Bundles = map[string]CredentialBundleConfig{}
	cfg.Credentials.Providers = map[string]CredentialProviderConfig{}
	cfg.SSH.ExpectedNodeIDs = map[string]string{}
	cfg.Attention.States = []AgentState{StateBlocked, StateError, StateDone}
	cfg.Notifications.DesktopEnabled = true
	cfg.Notifications.NtfyEnabled = true
	cfg.Notifications.NtfyIncludeEvidence = false
	cfg.Notifications.NtfyCommand = "ntfy-send"
	cfg.Focus.SwayCommand = "swaymsg"
	cfg.Integrations.CodexManagedHooks = true
	cfg.Integrations.ClaudeManagedHooks = true
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
		decoder := json.NewDecoder(strings.NewReader(string(b)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("decode config %s: %w", path, err)
		}
	}
	applyConfigEnvironment(&cfg)
	if cfg.Credentials.Bundles == nil {
		cfg.Credentials.Bundles = map[string]CredentialBundleConfig{}
	}
	if cfg.Credentials.Providers == nil {
		cfg.Credentials.Providers = map[string]CredentialProviderConfig{}
	}
	if cfg.SSH.ExpectedNodeIDs == nil {
		cfg.SSH.ExpectedNodeIDs = map[string]string{}
	}
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
		"kitty.command":                               cfg.Kitty.Command,
		"kitty.kitten_command":                        cfg.Kitty.KittenCommand,
		"zmx.command":                                 cfg.ZMX.Command,
		"ssh.command":                                 cfg.SSH.Command,
		"notifications.ntfy_command":                  cfg.Notifications.NtfyCommand,
		"focus.sway_command":                          cfg.Focus.SwayCommand,
		"credentials.gnupg.command":                   cfg.Credentials.GnuPG.Command,
		"credentials.gnupg.gpgconf_command":           cfg.Credentials.GnuPG.GPGConfCommand,
		"credentials.gnupg.gpg_connect_agent_command": cfg.Credentials.GnuPG.GPGConnectAgentCommand,
		"credentials.pivb.command":                    cfg.Credentials.PIVB.Command,
	} {
		if command == "" {
			return Config{}, fmt.Errorf("%s must not be empty", label)
		}
	}
	timeout, err := time.ParseDuration(cfg.Credentials.GnuPG.OperationTimeout)
	if err != nil || timeout <= 0 {
		return Config{}, fmt.Errorf("credentials.gnupg.operation_timeout must be a positive Go duration")
	}
	if cfg.Credentials.PIVB.GrantWindow != "" {
		if _, err := parseCredentialGrantWindow(credentialGrantWindowConfigLabel, cfg.Credentials.PIVB.GrantWindow); err != nil {
			return Config{}, err
		}
	}
	if sshForwardAgentEnabled(cfg.SSH.Options) {
		return Config{}, fmt.Errorf("ssh.options must not enable ForwardAgent; use a credential bundle with ssh_agent enabled")
	}
	for host, nodeID := range cfg.SSH.ExpectedNodeIDs {
		if err := validateSSHHost(host); err != nil {
			return Config{}, fmt.Errorf("ssh.expected_node_ids host %q: %w", host, err)
		}
		if err := validateNodeID(nodeID); err != nil {
			return Config{}, fmt.Errorf("ssh.expected_node_ids[%q]: %w", host, err)
		}
	}
	providerNodeIDs := map[string]string{}
	for name, provider := range cfg.Credentials.Providers {
		if err := validateCredentialBundleName(name); err != nil {
			return Config{}, fmt.Errorf("credential provider: %w", err)
		}
		if err := validateNodeID(provider.NodeID); err != nil {
			return Config{}, fmt.Errorf("credential provider %q: %w", name, err)
		}
		if previous := providerNodeIDs[provider.NodeID]; previous != "" {
			return Config{}, fmt.Errorf("credential providers %q and %q repeat node id %s", previous, name, provider.NodeID)
		}
		providerNodeIDs[provider.NodeID] = name
		for index, source := range provider.SSHSourceAddresses {
			if _, err := parseCredentialSourceNetwork(source); err != nil {
				return Config{}, fmt.Errorf("credential provider %q ssh_source_addresses[%d]: %w", name, index, err)
			}
		}
	}
	for name, bundle := range cfg.Credentials.Bundles {
		if err := validateCredentialBundleName(name); err != nil {
			return Config{}, err
		}
		if !bundle.SSHAgent.Enable && !bundle.OpenPGP.Enable && !bundle.PIVB.Enable {
			return Config{}, fmt.Errorf("credential bundle %q enables no capabilities", name)
		}
		if bundle.PIVB.Enable && len(bundle.PIVB.Aliases) == 0 {
			return Config{}, fmt.Errorf("credential bundle %q enables PIVB without an alias allowlist", name)
		}
		seenPIVBAliases := map[string]bool{}
		for index, alias := range bundle.PIVB.Aliases {
			if !validPIVBAlias(alias) {
				return Config{}, fmt.Errorf("credential bundle %q pivb.aliases[%d] is invalid", name, index)
			}
			if seenPIVBAliases[alias] {
				return Config{}, fmt.Errorf("credential bundle %q repeats PIVB alias %q", name, alias)
			}
			seenPIVBAliases[alias] = true
		}
		seen := map[string]bool{}
		for index, fingerprint := range bundle.OpenPGP.SigningKeys {
			canonical, err := canonicalOpenPGPFingerprint(fingerprint)
			if err != nil {
				return Config{}, fmt.Errorf("credential bundle %q signing_keys[%d]: %w", name, index, err)
			}
			if seen[canonical] {
				return Config{}, fmt.Errorf("credential bundle %q repeats signing fingerprint %s", name, canonical)
			}
			seen[canonical] = true
			bundle.OpenPGP.SigningKeys[index] = canonical
		}
		cfg.Credentials.Bundles[name] = bundle
	}
	if cfg.Credentials.DefaultBundle != "" {
		if _, ok := cfg.Credentials.Bundles[cfg.Credentials.DefaultBundle]; !ok {
			return Config{}, fmt.Errorf("credentials.default_bundle %q is not defined", cfg.Credentials.DefaultBundle)
		}
	}
	if cfg.Credentials.PIVB.ForwardSocket != "" && !filepath.IsAbs(cfg.Credentials.PIVB.ForwardSocket) {
		return Config{}, fmt.Errorf("credentials.pivb.forward_socket must be absolute")
	}
	if cfg.Credentials.PIVB.RoutingMode != pivbRoutingEnvironment {
		return Config{}, fmt.Errorf("credentials.pivb.routing_mode must be %q; enforced provenance is not available yet", pivbRoutingEnvironment)
	}
	if cfg.SSH.IdentityAgent != "" {
		cfg.SSH.Options = append([]string{"-o", "IdentityAgent=" + cfg.SSH.IdentityAgent}, cfg.SSH.Options...)
	}
	return cfg, nil
}

const (
	// A window shorter than a minute buys nothing a single touch does not
	// already cover, and one longer than half a day outlives the working
	// session an operator was thinking about when they opened it.
	credentialGrantWindowMin = time.Minute
	credentialGrantWindowMax = 12 * time.Hour

	credentialGrantWindowConfigLabel = "credentials.pivb.grant_window"
	credentialGrantWindowFlagLabel   = "--window"
)

// parseCredentialGrantWindow converts an operator-facing duration into the
// whole seconds the claim records. Zero is always legal and means the window
// is closed; every other value has to sit inside the bounds above and land on
// a second boundary, because a window is quoted back to operators and stamped
// into provider requests as an integer.
func parseCredentialGrantWindow(label, value string) (int64, error) {
	window, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration", label)
	}
	if window == 0 {
		return 0, nil
	}
	if window < credentialGrantWindowMin || window > credentialGrantWindowMax {
		return 0, fmt.Errorf("%s must be 0 or between %s and %s", label, credentialGrantWindowMin, credentialGrantWindowMax)
	}
	if window%time.Second != 0 {
		return 0, fmt.Errorf("%s must be a whole number of seconds", label)
	}
	return int64(window / time.Second), nil
}

// resolveCredentialWindowSeconds picks the window one claim asks for. An
// omitted flag inherits the configured default; any explicit value overrides
// it, including "0", so an operator can always close the window on a node
// whose configuration opens one by default.
func resolveCredentialWindowSeconds(flagValue string, cfg Config) (int64, error) {
	if strings.TrimSpace(flagValue) == "" {
		if strings.TrimSpace(cfg.Credentials.PIVB.GrantWindow) == "" {
			return 0, nil
		}
		return parseCredentialGrantWindow(credentialGrantWindowConfigLabel, cfg.Credentials.PIVB.GrantWindow)
	}
	return parseCredentialGrantWindow(credentialGrantWindowFlagLabel, flagValue)
}

func validPIVBAlias(value string) bool {
	if len(value) == 0 || len(value) > 32 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0 && i < len(value)-1 {
			continue
		}
		return false
	}
	return true
}

func validateNodeID(id string) error {
	if len(id) != 32 {
		return fmt.Errorf("node id must be 32 lowercase hexadecimal characters")
	}
	for _, character := range id {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return fmt.Errorf("node id must be 32 lowercase hexadecimal characters")
		}
	}
	return nil
}

func parseCredentialSourceNetwork(value string) (netip.Prefix, error) {
	if address, err := netip.ParseAddr(value); err == nil {
		address = address.Unmap()
		return netip.PrefixFrom(address, address.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("must be an IP address or CIDR network")
	}
	return prefix.Masked(), nil
}

func validateCredentialBundleName(name string) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("invalid credential bundle name %q", name)
	}
	for index, r := range name {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !valid || index == 0 && (r == '.' || r == '_' || r == '-') {
			return fmt.Errorf("invalid credential bundle name %q", name)
		}
	}
	return nil
}

func canonicalOpenPGPFingerprint(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 40 && len(value) != 64 {
		return "", fmt.Errorf("fingerprint must contain 40 or 64 hexadecimal characters")
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'F') {
			return "", fmt.Errorf("fingerprint must contain only hexadecimal characters")
		}
	}
	return value, nil
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

func sshForwardAgentEnabled(options []string) bool {
	for index := 0; index < len(options); index++ {
		option := options[index]
		if option == "-A" {
			return true
		}
		if option == "-o" && index+1 < len(options) {
			if strings.EqualFold(sshConfigOptionValue(options[index+1], "ForwardAgent"), "yes") {
				return true
			}
			index++
			continue
		}
		if strings.HasPrefix(option, "-o") && strings.EqualFold(sshConfigOptionValue(strings.TrimPrefix(option, "-o"), "ForwardAgent"), "yes") {
			return true
		}
	}
	return false
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
	if value := os.Getenv("ZKA_SWAY_COMMAND"); value != "" {
		cfg.Focus.SwayCommand = value
	}
	if value := os.Getenv("ZKA_HEADLESS"); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			cfg.Headless = parsed
		}
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
