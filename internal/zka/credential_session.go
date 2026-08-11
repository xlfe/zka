package zka

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const credentialSessionRefreshTimeout = 8 * time.Second

var credentialLocaleEnvironmentNames = []string{
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES",
}

var credentialTerminalEnvironmentNames = map[string]bool{
	"GPG_TTY":      true,
	"TERM":         true,
	"INSIDE_EMACS": true,
}

type credentialSessionRefreshRequest struct {
	Bundle    string `json:"bundle"`
	Action    string `json:"action"`
	Workspace string `json:"workspace,omitempty"`
}

type credentialSessionRefreshResponse struct {
	Required bool   `json:"required"`
	State    string `json:"state"`
	Detail   string `json:"detail"`
}

type credentialSessionOwner struct {
	Action    string
	Workspace string
	Source    string
	UpdatedAt time.Time
}

type environmentCommandRunner struct {
	runner      CommandRunner
	environment []string
}

func (r environmentCommandRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	return r.RunConfigured(ctx, name, args, commandOptions{})
}

func (r environmentCommandRunner) RunConfigured(ctx context.Context, name string, args []string, options commandOptions) (string, string, error) {
	options.Environment = append([]string(nil), r.environment...)
	return runCommandWithOptions(ctx, r.runner, name, args, options)
}

var _ configuredCommandRunner = environmentCommandRunner{}

func (d *Daemon) providerRunner() CommandRunner {
	return environmentCommandRunner{runner: d.runner, environment: append([]string(nil), d.providerEnvironment...)}
}

func sanitizeProviderEnvironment(paths Paths, environment []string) ([]string, []string) {
	result := append([]string(nil), environment...)
	var issues []string
	for _, name := range []string{"ZKA_WORKSPACE_ID", "ZKA_PANE_ID", "ZKA_CREDENTIAL_ENVIRONMENT_VERSION"} {
		if environmentValue(result, name) != "" {
			issues = append(issues, "removed "+name+" inherited from a managed pane")
		}
		result = removeEnvironmentValue(result, name)
	}
	for name := range credentialTerminalEnvironmentNames {
		result = removeEnvironmentValue(result, name)
	}
	if home := environmentValue(result, "GNUPGHOME"); home != "" && pathWithin(filepath.Join(paths.StateDir, "credentials"), home) {
		issues = append(issues, "ignored managed GNUPGHOME")
		result = removeEnvironmentValue(result, "GNUPGHOME")
	}
	if socket := environmentValue(result, "SSH_AUTH_SOCK"); socket != "" && pathWithin(paths.AgentDir, socket) {
		issues = append(issues, "ignored managed SSH_AUTH_SOCK")
		result = removeEnvironmentValue(result, "SSH_AUTH_SOCK")
	}
	return append([]string(nil), result...), issues
}

func pathWithin(root, candidate string) bool {
	if root == "" || candidate == "" || !filepath.IsAbs(root) || !filepath.IsAbs(candidate) {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func environmentValue(environment []string, name string) string {
	prefix := name + "="
	for index := len(environment) - 1; index >= 0; index-- {
		if strings.HasPrefix(environment[index], prefix) {
			return strings.TrimPrefix(environment[index], prefix)
		}
	}
	return ""
}

func environmentMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name != "" {
			result[name] = value
		}
	}
	return result
}

func mapEnvironment(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result
}

func (d *Daemon) refreshCredentialSession(ctx context.Context, req credentialSessionRefreshRequest) credentialSessionRefreshResponse {
	bundle, ok := d.config.credentialBundle(req.Bundle)
	if !ok {
		return credentialSessionRefreshResponse{State: "warning", Detail: fmt.Sprintf("credential bundle %q is not configured", req.Bundle)}
	}
	refreshCtx, cancel := context.WithTimeout(ctx, credentialSessionRefreshTimeout)
	defer cancel()
	select {
	case <-d.credentialSessionGate:
		defer func() { d.credentialSessionGate <- struct{}{} }()
	case <-refreshCtx.Done():
		return credentialSessionRefreshResponse{State: "warning", Detail: safeCredentialSessionError(refreshCtx.Err())}
	}

	peer := localPeerFromContext(ctx)
	callerSocket := ""
	if peer.Err == nil {
		callerSocket = peer.Environment["SSH_AUTH_SOCK"]
		if pathWithin(d.paths.AgentDir, callerSocket) {
			callerSocket = ""
		}
	}
	sshSource := d.credentialSSHSocketForCaller(callerSocket)
	required, err := d.credentialBundleRequiresGraphicalPinentry(refreshCtx, bundle, sshSource)
	if err != nil {
		return credentialSessionRefreshResponse{State: "warning", Detail: "cannot identify the provider gpg-agent SSH socket: " + safeCredentialSessionCommandError(refreshCtx, err)}
	}
	if !required {
		return credentialSessionRefreshResponse{State: "not-required", Detail: "selected bundle does not use gpg-agent pinentry"}
	}
	if d.config.Headless {
		return credentialSessionRefreshResponse{Required: true, State: "headless", Detail: "graphical pinentry refresh is disabled on a headless node"}
	}
	if peer.Err != nil {
		return credentialSessionRefreshResponse{Required: true, State: "warning", Detail: safeCredentialSessionError(peer.Err)}
	}

	namesOutput, _, err := runCommandWithOptions(refreshCtx, d.runner, d.config.Credentials.GnuPG.GPGConnectAgentCommand,
		[]string{"GETINFO std_env_names", "/bye"}, commandOptions{Environment: d.providerEnvironment, NewSession: true})
	if err != nil {
		return credentialSessionRefreshResponse{Required: true, State: "warning", Detail: "query GnuPG session environment names: " + safeCredentialSessionCommandError(refreshCtx, err)}
	}
	standardNames, err := parseAssuanDataNames(namesOutput)
	if err != nil || len(standardNames) == 0 {
		if err == nil {
			err = errors.New("GnuPG returned no standard session environment names")
		}
		return credentialSessionRefreshResponse{Required: true, State: "warning", Detail: safeCredentialSessionError(err)}
	}

	selected, source, err := d.selectGraphicalCredentialSession(peer.Environment, standardNames)
	if err != nil {
		return credentialSessionRefreshResponse{Required: true, State: "warning", Detail: safeCredentialSessionError(err)}
	}
	commandEnvironment := environmentMap(d.providerEnvironment)
	for _, name := range standardNames {
		delete(commandEnvironment, name)
	}
	for _, name := range credentialLocaleEnvironmentNames {
		delete(commandEnvironment, name)
	}
	for name := range credentialTerminalEnvironmentNames {
		delete(commandEnvironment, name)
	}
	for _, name := range standardNames {
		if credentialTerminalEnvironmentNames[name] {
			continue
		}
		if value, ok := selected[name]; ok {
			commandEnvironment[name] = value
		}
	}
	for _, name := range credentialLocaleEnvironmentNames {
		if value, ok := selected[name]; ok {
			commandEnvironment[name] = value
		}
	}
	// XDG_RUNTIME_DIR is not copied into gpg-agent's startup_env. It is used
	// here only so relative Wayland display names are interpreted consistently
	// with the peer session; gpg-agent is assumed to have the same per-user
	// runtime directory.
	if value := selected["XDG_RUNTIME_DIR"]; value != "" {
		commandEnvironment["XDG_RUNTIME_DIR"] = value
	}
	_, _, err = runCommandWithOptions(refreshCtx, d.runner, d.config.Credentials.GnuPG.GPGConnectAgentCommand,
		[]string{"updatestartuptty", "/bye"}, commandOptions{Environment: mapEnvironment(commandEnvironment), NewSession: true})
	if err != nil {
		return credentialSessionRefreshResponse{Required: true, State: "warning", Detail: "refresh provider graphical pinentry session: " + safeCredentialSessionCommandError(refreshCtx, err)}
	}
	d.credentialSessionStateMu.Lock()
	d.credentialSessionOwner = credentialSessionOwner{
		Action: req.Action, Workspace: req.Workspace, Source: source, UpdatedAt: time.Now().UTC(),
	}
	d.credentialSessionStateMu.Unlock()
	return credentialSessionRefreshResponse{Required: true, State: "refreshed", Detail: "provider graphical pinentry session refreshed from " + source}
}

func (d *Daemon) credentialBundleRequiresGraphicalPinentry(ctx context.Context, bundle CredentialBundleConfig, sshSource string) (bool, error) {
	if bundle.OpenPGP.Enable {
		return true, nil
	}
	if !bundle.SSHAgent.Enable {
		return false, nil
	}
	agentSocket, _, err := d.providerRunner().Run(ctx, d.config.Credentials.GnuPG.GPGConfCommand, "--list-dirs", "agent-ssh-socket")
	if err != nil {
		return false, err
	}
	return sameSSHAgentSocket(sshSource, strings.TrimSpace(agentSocket)), nil
}

func (d *Daemon) selectGraphicalCredentialSession(peer map[string]string, standardNames []string) (map[string]string, string, error) {
	peerSession := selectCredentialSessionValues(peer, standardNames)
	if err := d.credentialSessionProbe(peerSession); err == nil {
		return peerSession, "local CLI session", nil
	}
	fallback := selectCredentialSessionValues(environmentMap(d.providerEnvironment), standardNames)
	if err := d.credentialSessionProbe(fallback); err == nil {
		return fallback, "zkad startup session", nil
	}
	return nil, "", errors.New("no reachable graphical pinentry session is available")
}

func selectCredentialSessionValues(source map[string]string, standardNames []string) map[string]string {
	result := map[string]string{}
	for _, name := range standardNames {
		if credentialTerminalEnvironmentNames[name] {
			continue
		}
		if value, ok := source[name]; ok {
			if name == "DISPLAY" {
				if _, local := localX11DisplayNumber(value); !local {
					continue
				}
			}
			result[name] = value
		}
	}
	for _, name := range credentialLocaleEnvironmentNames {
		if value, ok := source[name]; ok {
			result[name] = value
		}
	}
	if value := source["XDG_RUNTIME_DIR"]; value != "" {
		result["XDG_RUNTIME_DIR"] = value
	}
	return result
}

func parseAssuanDataNames(output string) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(strings.ReplaceAll(output, "\x00", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "D ") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, "D "))
		if name == "" || strings.ContainsAny(name, "= \t\r\n") {
			return nil, errors.New("GnuPG returned a malformed standard session environment name")
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

func probeGraphicalCredentialSession(environment map[string]string) error {
	if wayland := environment["WAYLAND_DISPLAY"]; wayland != "" {
		path := wayland
		if !filepath.IsAbs(path) {
			runtimeDir := environment["XDG_RUNTIME_DIR"]
			if runtimeDir == "" || !filepath.IsAbs(runtimeDir) {
				return errors.New("WAYLAND_DISPLAY is relative but the peer XDG_RUNTIME_DIR is unavailable")
			}
			path = filepath.Join(runtimeDir, wayland)
		}
		conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
	}
	if display := environment["DISPLAY"]; display != "" {
		if number, ok := localX11DisplayNumber(display); ok {
			conn, dialErr := net.DialTimeout("unix", filepath.Join("/tmp/.X11-unix", "X"+number), 250*time.Millisecond)
			if dialErr == nil {
				_ = conn.Close()
				return nil
			}
		}
	}
	return errors.New("neither WAYLAND_DISPLAY nor DISPLAY names a reachable local display")
}

func localX11DisplayNumber(display string) (string, bool) {
	colon := strings.LastIndex(display, ":")
	if colon < 0 {
		return "", false
	}
	host := display[:colon]
	if host != "" && host != "unix" && host != "localhost" {
		return "", false
	}
	number := strings.SplitN(display[colon+1:], ".", 2)[0]
	parsed, err := strconv.Atoi(number)
	return number, err == nil && parsed >= 0
}

func safeCredentialSessionError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "graphical pinentry refresh timed out"
	}
	return err.Error()
}

func safeCredentialSessionCommandError(ctx context.Context, err error) string {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return safeCredentialSessionError(ctxErr)
	}
	return safeCredentialSessionError(err)
}
