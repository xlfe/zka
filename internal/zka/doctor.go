package zka

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func runDoctor(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("doctor", stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	origin := fs.String("origin", "", "test an origin through its SSH alias")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("doctor accepts no positional arguments")
	}
	cfg, cfgErr := LoadConfig()
	checks := []doctorCheck{{Name: "config", OK: cfgErr == nil, Detail: doctorDetail(cfgErr, envOr("ZKA_CONFIG", "built-in defaults"))}}
	if cfgErr != nil {
		return writeDoctorResult(checks, *jsonOut, stdout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := NewAPI(paths)
	_, err := api.Ping(ctx)
	checks = append(checks, doctorCheck{Name: "daemon", OK: err == nil, Detail: doctorDetail(err, paths.Socket)})
	stateErr := NewStore(paths).Ensure()
	checks = append(checks, doctorCheck{Name: "state-dir", OK: stateErr == nil, Detail: doctorDetail(stateErr, paths.StateDir)})
	commands := []struct{ name, command string }{
		{"kitty", cfg.Kitty.Command}, {"kitten", cfg.Kitty.KittenCommand},
		{"zmx", cfg.ZMX.Command}, {"ssh", cfg.SSH.Command},
		{"ntfy-send", cfg.Notifications.NtfyCommand},
	}
	if cfg.Integrations.CodexManagedHooks {
		commands = append(commands, struct{ name, command string }{"codex", "codex"})
	}
	if cfg.Integrations.ClaudeManagedHooks {
		commands = append(commands, struct{ name, command string }{"claude", "claude"})
	}
	for _, item := range commands {
		path, lookupErr := exec.LookPath(item.command)
		checks = append(checks, doctorCheck{Name: item.name, OK: lookupErr == nil, Detail: doctorDetail(lookupErr, path)})
	}
	watcherExists, watcherErr := configExists(cfg.Kitty.Watcher)
	if watcherErr == nil && !watcherExists {
		watcherErr = fmt.Errorf("not found")
	}
	checks = append(checks, doctorCheck{Name: "kitty-watcher", OK: watcherErr == nil, Detail: doctorDetail(watcherErr, cfg.Kitty.Watcher)})
	checks = append(checks,
		managedHookDoctorCheck("codex-hooks", "/etc/codex/requirements.toml", "hook codex", cfg.Integrations.CodexManagedHooks),
		managedHookDoctorCheck("claude-hooks", "/etc/claude-code/managed-settings.d/50-zka.json", "hook claude", cfg.Integrations.ClaudeManagedHooks),
	)
	if *origin != "" {
		forwardingConfigDetail := "disabled; enable services.zka.ssh.forwardAgent"
		if cfg.SSH.ForwardAgent {
			forwardingConfigDetail = "enabled"
		}
		checks = append(checks, doctorCheck{
			Name: "ssh-agent-forwarding-config", OK: cfg.SSH.ForwardAgent,
			Detail: forwardingConfigDetail,
		})
		daemonAgent, agentErr := api.SSHAgent(ctx)
		if agentErr != nil {
			checks = append(checks, doctorCheck{Name: "zkad-ssh-agent", OK: false, Detail: agentErr.Error() + " (restart zkad after upgrading)"})
		} else {
			sshAdd := siblingSSHCommand(cfg.SSH.Command, "ssh-add")
			checks = append(checks, doctorSSHAgentChecks(ctx, daemonAgent, os.Getenv("SSH_AUTH_SOCK"), func(ctx context.Context, socket string) ([]string, error) {
				return inspectSSHAgent(ctx, sshAdd, socket)
			})...)
		}
		var workspaces []*Workspace
		remoteErr := api.RemoteCall(ctx, *origin, "list", nil, &workspaces)
		detail := fmt.Sprintf("%s (%d workspaces)", *origin, len(workspaces))
		checks = append(checks, doctorCheck{Name: "remote-control", OK: remoteErr == nil, Detail: doctorDetail(remoteErr, detail)})
		var forwarding remoteAgentForwardingStatus
		forwardingErr := api.RemoteCall(ctx, *origin, "agent_forwarding", nil, &forwarding)
		remoteConfigOK := forwardingErr == nil && forwarding.Enabled && forwarding.RelayVersion == agentRelayVersion
		remoteConfigDetail := fmt.Sprintf("relay version %d", forwarding.RelayVersion)
		if forwardingErr != nil {
			remoteConfigDetail = forwardingErr.Error()
		} else if !forwarding.Enabled {
			remoteConfigDetail = "disabled on origin"
		}
		checks = append(checks, doctorCheck{Name: "remote-agent-relay", OK: remoteConfigOK, Detail: remoteConfigDetail})
		forwardedOK := forwardingErr == nil && forwarding.ForwardedSocket
		forwardedDetail := "forwarded agent socket is available"
		if forwardingErr != nil {
			forwardedDetail = forwardingErr.Error()
		} else if !forwarding.ForwardedSocket {
			forwardedDetail = "control SSH did not receive a dialable forwarded agent"
		}
		checks = append(checks, doctorCheck{Name: "remote-forwarded-agent", OK: forwardedOK, Detail: forwardedDetail})
	}
	return writeDoctorResult(checks, *jsonOut, stdout)
}

func managedHookDoctorCheck(name, path, command string, enabled bool) doctorCheck {
	if !enabled {
		return doctorCheck{Name: name, OK: true, Detail: "disabled in zka configuration"}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return doctorCheck{Name: name, Detail: err.Error()}
	}
	if !strings.Contains(string(b), command) {
		return doctorCheck{Name: name, Detail: "managed zka hook not found in " + path}
	}
	return doctorCheck{Name: name, OK: true, Detail: path}
}

type sshAgentInspector func(context.Context, string) ([]string, error)

func doctorSSHAgentChecks(ctx context.Context, daemonAgent sshAgentInfo, callerSocket string, inspect sshAgentInspector) []doctorCheck {
	daemonFingerprints, daemonErr := inspect(ctx, daemonAgent.EffectiveSocket)
	callerFingerprints, callerErr := daemonFingerprints, daemonErr
	if !sameSSHAgentSocket(daemonAgent.EffectiveSocket, callerSocket) {
		callerFingerprints, callerErr = inspect(ctx, callerSocket)
	}
	daemonDetail := sshAgentDetail(daemonAgent.EffectiveSocket, daemonFingerprints, daemonErr)
	if daemonAgent.IdentityAgent != "" {
		daemonDetail = fmt.Sprintf("configured %s; effective %s", daemonAgent.IdentityAgent, daemonDetail)
	} else if daemonAgent.InheritedSocket != "" {
		daemonDetail = "inherited " + daemonDetail
	}
	checks := []doctorCheck{
		{Name: "zkad-ssh-agent", OK: daemonErr == nil, Detail: daemonDetail},
		{Name: "caller-ssh-agent", OK: callerErr == nil, Detail: sshAgentDetail(callerSocket, callerFingerprints, callerErr)},
	}
	match := doctorCheck{Name: "ssh-agent-match"}
	switch {
	case daemonErr != nil || callerErr != nil:
		match.Detail = fmt.Sprintf("cannot compare agents: zkad uses %s; caller uses %s", displaySSHAgentSocket(daemonAgent.EffectiveSocket), displaySSHAgentSocket(callerSocket))
	case sameSSHAgentSocket(daemonAgent.EffectiveSocket, callerSocket):
		match.OK = true
		match.Detail = "zkad and caller use " + displaySSHAgentSocket(callerSocket)
	case equalStrings(daemonFingerprints, callerFingerprints):
		match.OK = true
		match.Detail = fmt.Sprintf("different sockets expose the same identities: zkad uses %s; caller uses %s", displaySSHAgentSocket(daemonAgent.EffectiveSocket), displaySSHAgentSocket(callerSocket))
	default:
		match.Detail = fmt.Sprintf("zkad uses %s; caller uses %s; agents expose different identities", displaySSHAgentSocket(daemonAgent.EffectiveSocket), displaySSHAgentSocket(callerSocket))
	}
	return append(checks, match)
}

func inspectSSHAgent(ctx context.Context, sshAddCommand, socket string) ([]string, error) {
	if socket == "" {
		return nil, fmt.Errorf("SSH_AUTH_SOCK is not set")
	}
	if socket == "none" {
		return nil, fmt.Errorf("disabled by IdentityAgent=none")
	}
	info, err := os.Stat(socket)
	if err != nil {
		return nil, fmt.Errorf("inspect socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("not a Unix socket")
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(inspectCtx, sshAddCommand, "-L")
	cmd.Env = sshAgentEnvironment(socket)
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return nil, fmt.Errorf("list identities: %w: %s", err, detail)
		}
		return nil, fmt.Errorf("list identities: %w", err)
	}
	return sshPublicKeyFingerprints(string(output))
}

func sshPublicKeyFingerprints(output string) ([]string, error) {
	var fingerprints []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("invalid public key from ssh-agent")
		}
		blob, err := base64.StdEncoding.DecodeString(fields[1])
		if err != nil {
			return nil, fmt.Errorf("decode public key from ssh-agent: %w", err)
		}
		digest := sha256.Sum256(blob)
		fingerprints = append(fingerprints, "SHA256:"+base64.RawStdEncoding.EncodeToString(digest[:]))
	}
	if len(fingerprints) == 0 {
		return nil, fmt.Errorf("agent contains no identities")
	}
	sort.Strings(fingerprints)
	return fingerprints, nil
}

func sshAgentEnvironment(socket string) []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "SSH_AUTH_SOCK=") {
			environment = append(environment, entry)
		}
	}
	return append(environment, "SSH_AUTH_SOCK="+socket)
}

func siblingSSHCommand(sshCommand, name string) string {
	if strings.ContainsRune(sshCommand, filepath.Separator) {
		return filepath.Join(filepath.Dir(sshCommand), name)
	}
	return name
}

func sshAgentDetail(socket string, fingerprints []string, err error) string {
	detail := displaySSHAgentSocket(socket)
	if err != nil {
		return detail + ": " + err.Error()
	}
	return detail + " (" + strings.Join(fingerprints, ", ") + ")"
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func writeDoctorResult(checks []doctorCheck, jsonOut bool, stdout io.Writer) (int, error) {
	failed := false
	for _, check := range checks {
		failed = failed || !check.OK
	}
	if jsonOut {
		if err := writeJSON(stdout, checks); err != nil {
			return 1, err
		}
	} else {
		for _, check := range checks {
			status := "ok"
			if !check.OK {
				status = "FAIL"
			}
			fmt.Fprintf(stdout, "%-5s %-16s %s\n", status, check.Name, check.Detail)
		}
	}
	if failed {
		return 1, nil
	}
	return 0, nil
}

func doctorDetail(err error, success string) string {
	if err != nil {
		return err.Error()
	}
	return success
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
