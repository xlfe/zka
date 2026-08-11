package zka

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

type credentialProviderDiagnosticsResponse struct {
	ProviderEnvironment doctorCheck  `json:"provider_environment"`
	CredentialsProvider doctorCheck  `json:"credentials_provider"`
	OpenPGPKeys         doctorCheck  `json:"openpgp_keys"`
	PinentrySession     doctorCheck  `json:"pinentry_session"`
	SSHAgent            sshAgentInfo `json:"ssh_agent"`
}

func (d *Daemon) credentialProviderDiagnostics(ctx context.Context) credentialProviderDiagnosticsResponse {
	providerRunner := d.providerRunner()
	result := credentialProviderDiagnosticsResponse{SSHAgent: d.sshAgent}
	result.CredentialsProvider = credentialsProviderDoctorCheckWithAgent(ctx, d.config, providerRunner, d.sshAgent)
	result.ProviderEnvironment, result.PinentrySession = d.credentialSessionDoctorChecks(ctx)
	if !configHasOpenPGPProviderKeys(d.config) {
		result.OpenPGPKeys = doctorCheck{Name: "openpgp-keys", OK: true, Detail: "no OpenPGP provider signing keys configured"}
		return result
	}
	leaseCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	release, err := d.cardLease.acquire(leaseCtx)
	cancel()
	if err != nil {
		result.OpenPGPKeys = doctorCheck{
			Name: "openpgp-keys", OK: true, Warning: true,
			Detail: "skipped: smart-card lease is busy with another credential operation",
		}
		return result
	}
	defer release()
	result.OpenPGPKeys = openPGPKeysDoctorCheck(ctx, d.config, providerRunner)
	return result
}

func (d *Daemon) credentialSessionDoctorChecks(ctx context.Context) (doctorCheck, doctorCheck) {
	environmentCheck := doctorCheck{Name: "provider-environment", OK: true}
	pinentryCheck := doctorCheck{Name: "pinentry-session", OK: true}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	required, requirementErr := d.graphicalPinentryRequired(probeCtx)
	if requirementErr != nil {
		environmentCheck.OK = false
		environmentCheck.Detail = requirementErr.Error()
		pinentryCheck.OK = false
		pinentryCheck.Detail = "cannot determine whether the local provider requires pinentry"
		return environmentCheck, pinentryCheck
	}
	if !required {
		environmentCheck.Detail = "provider environment is isolated; no local credential provider requires gpg-agent pinentry"
		if len(d.providerEnvironmentIssues) != 0 {
			environmentCheck.Warning = true
			environmentCheck.Detail += "; " + strings.Join(d.providerEnvironmentIssues, "; ")
		}
		pinentryCheck.Detail = "no local credential provider requires gpg-agent pinentry"
		return environmentCheck, pinentryCheck
	}
	if d.config.Headless {
		pinentryCheck.OK = false
		pinentryCheck.Detail = "headless node is configured or active as an OpenPGP/gpg-agent SSH provider; graphical pinentry cannot prompt"
		environmentCheck.Detail = "provider environment is isolated on a headless node"
		if len(d.providerEnvironmentIssues) != 0 {
			environmentCheck.Warning = true
			environmentCheck.Detail += "; " + strings.Join(d.providerEnvironmentIssues, "; ")
		}
		return environmentCheck, pinentryCheck
	}
	peer := localPeerFromContext(ctx)
	if peer.Err != nil {
		environmentCheck.OK = false
		if errors.Is(peer.Err, errPeerPIDFDUnavailable) {
			environmentCheck.Detail = errPeerPIDFDUnavailable.Error()
		} else {
			environmentCheck.Detail = "cannot capture the local CLI session: " + safeCredentialSessionError(peer.Err)
		}
		pinentryCheck.Warning = true
		pinentryCheck.Detail = "cannot compare the installed pinentry session with this caller"
		return environmentCheck, pinentryCheck
	}

	namesOutput, _, err := runCommandWithOptions(probeCtx, d.runner, d.config.Credentials.GnuPG.GPGConnectAgentCommand,
		[]string{"GETINFO std_env_names", "/bye"}, commandOptions{Environment: d.providerEnvironment, NewSession: true})
	if err != nil {
		environmentCheck.OK = false
		environmentCheck.Detail = "query GnuPG session environment names: " + safeCredentialSessionCommandError(probeCtx, err)
		pinentryCheck.OK = false
		pinentryCheck.Detail = "provider startup environment is unavailable"
		return environmentCheck, pinentryCheck
	}
	standardNames, err := parseAssuanDataNames(namesOutput)
	if err != nil || len(standardNames) == 0 {
		environmentCheck.OK = false
		environmentCheck.Detail = "GnuPG standard session environment list is unavailable"
		pinentryCheck.OK = false
		pinentryCheck.Detail = "provider startup environment is unavailable"
		return environmentCheck, pinentryCheck
	}
	selected, selectedSource, selectErr := d.selectGraphicalCredentialSession(peer.Environment, standardNames)
	if selectErr != nil {
		environmentCheck.OK = false
		environmentCheck.Detail = selectErr.Error()
	} else {
		environmentCheck.Detail = "reachable graphical session from " + selectedSource
		if busErr := probeCredentialSessionBus(selected["DBUS_SESSION_BUS_ADDRESS"]); busErr != nil {
			environmentCheck.Warning = true
			environmentCheck.Detail += "; session bus is unavailable"
		}
		if len(d.providerEnvironmentIssues) != 0 {
			environmentCheck.Warning = true
			environmentCheck.Detail += "; " + strings.Join(d.providerEnvironmentIssues, "; ")
		}
	}

	startupOutput, _, err := runCommandWithOptions(probeCtx, d.runner, d.config.Credentials.GnuPG.GPGConnectAgentCommand,
		[]string{"GETINFO std_startup_env", "/bye"}, commandOptions{Environment: d.providerEnvironment, NewSession: true})
	if err != nil {
		pinentryCheck.OK = false
		pinentryCheck.Detail = "query provider startup environment: " + safeCredentialSessionCommandError(probeCtx, err)
		return environmentCheck, pinentryCheck
	}
	installed, err := parseAssuanDataEnvironment(startupOutput)
	if err != nil {
		pinentryCheck.OK = false
		pinentryCheck.Detail = "provider startup environment is malformed"
		return environmentCheck, pinentryCheck
	}
	// XDG_RUNTIME_DIR is not part of std_startup_env. Resolve a relative
	// WAYLAND_DISPLAY using the current peer's value, which is the runtime
	// namespace the diagnostic is meant to validate. gpg-agent is assumed to
	// share that per-user runtime directory.
	if runtimeDir := peer.Environment["XDG_RUNTIME_DIR"]; runtimeDir != "" {
		installed["XDG_RUNTIME_DIR"] = runtimeDir
	}
	if err := d.credentialSessionProbe(installed); err != nil {
		pinentryCheck.OK = false
		pinentryCheck.Detail = "provider agent has no reachable graphical startup environment"
		return environmentCheck, pinentryCheck
	}
	d.credentialSessionStateMu.Lock()
	owner := d.credentialSessionOwner
	d.credentialSessionStateMu.Unlock()
	for _, name := range []string{"GPG_TTY", "TERM", "INSIDE_EMACS"} {
		if installed[name] == "" {
			continue
		}
		if owner.UpdatedAt.IsZero() {
			pinentryCheck.Warning = true
			pinentryCheck.Detail = "provider agent has a reachable graphical startup environment but still contains pre-refresh terminal routing through " + name
		} else {
			pinentryCheck.OK = false
			pinentryCheck.Detail = "provider agent startup environment regained terminal routing through " + name + " after zka refreshed it"
		}
		return environmentCheck, pinentryCheck
	}
	pinentryCheck.Detail = "provider agent has a reachable graphical startup environment"
	if peerSession := selectCredentialSessionValues(peer.Environment, standardNames); d.credentialSessionProbe(peerSession) == nil && !sameGraphicalCredentialSession(installed, peerSession) {
		pinentryCheck.Warning = true
		pinentryCheck.Detail += "; it differs from the current caller (expected over SSH or from another seat)"
	}
	if !owner.UpdatedAt.IsZero() {
		pinentryCheck.Detail += fmt.Sprintf("; last refreshed by %s for %s at %s", owner.Action, displayCredentialWorkspace(owner.Workspace), owner.UpdatedAt.Format(time.RFC3339))
	} else {
		pinentryCheck.Warning = true
		pinentryCheck.Detail += "; refresh owner is unknown since zkad started"
	}
	return environmentCheck, pinentryCheck
}

func (d *Daemon) graphicalPinentryRequired(ctx context.Context) (bool, error) {
	activeOpenPGP := d.activeLocalOpenPGPBundles()
	for name, bundle := range d.config.Credentials.Bundles {
		openPGPProvider := (bundle.OpenPGP.Enable && len(bundle.OpenPGP.SigningKeys) != 0) || activeOpenPGP[name]
		if openPGPProvider {
			return true, nil
		}
	}
	sshRequired := false
	for _, bundle := range d.config.Credentials.Bundles {
		if !bundle.SSHAgent.Enable {
			continue
		}
		bundle.OpenPGP.Enable = false
		required, err := d.credentialBundleRequiresGraphicalPinentry(ctx, bundle, d.sshAgent.EffectiveSocket)
		if err != nil {
			return false, fmt.Errorf("identify provider gpg-agent SSH socket: %w", err)
		}
		sshRequired = sshRequired || required
	}
	return sshRequired, nil
}

func (d *Daemon) activeLocalOpenPGPBundles() map[string]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	active := map[string]bool{}
	for _, workspace := range d.state.Workspaces {
		claim := workspace.CredentialClaim
		if claim == nil || claim.OwnerNodeID != d.state.Node.ID || claim.ProviderSource != "local" && claim.ProviderSource != "" {
			continue
		}
		if bundle, ok := d.config.credentialBundle(claim.Bundle); ok && bundle.OpenPGP.Enable {
			active[claim.Bundle] = true
		}
	}
	return active
}

func parseAssuanDataEnvironment(output string) (map[string]string, error) {
	result := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(output, "\x00", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "D ") {
			continue
		}
		entry, err := url.PathUnescape(strings.TrimSpace(strings.TrimPrefix(line, "D ")))
		if err != nil {
			return nil, err
		}
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return nil, errors.New("malformed GnuPG startup environment entry")
		}
		result[name] = value
	}
	return result, nil
}

func probeCredentialSessionBus(address string) error {
	if address == "" {
		return errors.New("DBUS_SESSION_BUS_ADDRESS is not set")
	}
	endpoint := strings.SplitN(address, ";", 2)[0]
	if strings.HasPrefix(endpoint, "unix:path=") {
		path, err := url.PathUnescape(strings.TrimPrefix(endpoint, "unix:path="))
		if err != nil {
			return err
		}
		conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	if strings.HasPrefix(endpoint, "unix:abstract=") {
		name, err := url.PathUnescape(strings.TrimPrefix(endpoint, "unix:abstract="))
		if err != nil {
			return err
		}
		conn, err := net.DialTimeout("unix", "\x00"+name, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	return errors.New("session bus address is not a supported Unix endpoint")
}

func sameGraphicalCredentialSession(left, right map[string]string) bool {
	for _, name := range []string{"WAYLAND_DISPLAY", "DISPLAY", "DBUS_SESSION_BUS_ADDRESS"} {
		if left[name] != right[name] {
			return false
		}
	}
	return true
}

func displayCredentialWorkspace(workspace string) string {
	if workspace == "" {
		return "the selected workspace"
	}
	return shortID(workspace)
}
