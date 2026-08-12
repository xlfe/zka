package zka

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const Version = "0.9.1"

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		printUsage(stderr)
		return 2, nil
	}
	switch args[0] {
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0, nil
	case "version", "--version":
		fmt.Fprintln(stdout, Version)
		return 0, nil
	}
	if args[0] == "hook" {
		if os.Getenv("ZKA_HOOK_SOCKET") != "" {
			return runHook(args[1:], Paths{}, stdin, stdout)
		}
		if os.Getenv("ZKA_WORKSPACE_ID") == "" || os.Getenv("ZKA_PANE_ID") == "" {
			return hookSuccess(stdout)
		}
	}
	if args[0] == "launch" {
		return runLauncher(args[1:], stdin, stdout, stderr)
	}
	paths, err := DefaultPaths()
	if err != nil {
		if args[0] == "hook" {
			return hookSuccess(stdout)
		}
		return 1, err
	}
	switch args[0] {
	case "daemon":
		return normalizeFlagHelp(runDaemon(args[1:], paths, stderr))
	case "kitty":
		return normalizeFlagHelp(runKitty(args[1:], paths, stdout, stderr))
	case "workspace":
		return normalizeFlagHelp(runWorkspace(args[1:], paths, stdout, stderr))
	case "attention":
		return normalizeFlagHelp(runAttention(args[1:], paths, stdin, stdout, stderr))
	case "pane":
		return normalizeFlagHelp(runPane(args[1:], paths, stdin, stdout, stderr))
	case "pane-host":
		return normalizeFlagHelp(runPaneHost(args[1:], paths, stdin, stdout, stderr))
	case "remote-pane":
		return normalizeFlagHelp(runRemotePane(args[1:], paths, stdin, stdout, stderr))
	case "remote-new-pane":
		return normalizeFlagHelp(runRemoteNewPane(args[1:], paths, stdin, stdout, stderr))
	case "remote-attach":
		return normalizeFlagHelp(runRemoteAttach(args[1:], paths, stdin, stdout, stderr))
	case "remote-control":
		return runRemoteControlCommand(args[1:], paths, stdin, stdout)
	case "doctor":
		return normalizeFlagHelp(runDoctor(args[1:], paths, stdout, stderr))
	case "relay":
		return normalizeFlagHelp(runRelay(args[1:], paths, stdin, stdout, stderr))
	case "hook":
		return runHook(args[1:], paths, stdin, stdout)
	default:
		printUsage(stderr)
		return 2, fmt.Errorf("unknown command %q", args[0])
	}
}

func normalizeFlagHelp(code int, err error) (int, error) {
	if errors.Is(err, flag.ErrHelp) {
		return 0, nil
	}
	return code, err
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `usage: zka COMMAND [OPTIONS]

Commands:
  launch      Choose or create a workspace in the graphical launcher
  attention   Show, watch, focus, or defer panes that need you
  kitty       Create a managed Kitty workspace
  workspace   Manage workspace views, lifecycle, focus, and credential claims
  doctor      Check local or remote integration
  daemon      Run zkad (normally via systemd --user)

Internal commands: pane, pane-host, remote-pane, remote-attach, remote-control, relay, hook`)
}

func runLauncher(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) != 0 {
		return 2, fmt.Errorf("launch accepts no arguments")
	}
	return runLauncherMode("", stdin, stdout, stderr)
}

func runLauncherMode(mode string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	command := os.Getenv("ZKA_LAUNCHER_COMMAND")
	if command == "" {
		command = siblingExecutable("zka-launch")
	}
	var args []string
	if mode != "" {
		args = []string{mode}
	}
	cmd := exec.Command(command, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return processExitCode(err), nil
		}
		return 1, fmt.Errorf("start graphical launcher: %w", err)
	}
	return 0, nil
}

func siblingExecutable(name string) string {
	executable, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(executable), name)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return name
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var options, positionals []string
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--" {
			positionals = append(positionals, args[i:]...)
			break
		}
		if !strings.HasPrefix(token, "-") || token == "-" {
			positionals = append(positionals, token)
			continue
		}
		options = append(options, token)
		name := strings.TrimLeft(token, "-")
		if at := strings.IndexByte(name, '='); at >= 0 {
			continue
		}
		definition := fs.Lookup(name)
		if definition == nil {
			continue
		}
		if boolean, ok := definition.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	return fs.Parse(append(options, positionals...))
}

func runDaemon(args []string, paths Paths, stderr io.Writer) (int, error) {
	fs := newFlagSet("daemon", stderr)
	fs.StringVar(&paths.Socket, "socket", paths.Socket, "Unix socket path")
	fs.StringVar(&paths.StateDir, "state-dir", paths.StateDir, "state directory")
	if err := parseInterspersed(fs, args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("daemon accepts no positional arguments")
	}
	paths.StateFile = filepath.Join(paths.StateDir, "state.json")
	paths.GeneratedDir = filepath.Join(paths.StateDir, "generated")
	d, err := NewDaemon(paths, ExecRunner{}, nil)
	if err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return 0, d.Serve(ctx)
}

func runKitty(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("kitty", stderr)
	name := fs.String("name", "", "optional workspace name")
	cwd := fs.String("cwd", "", "default pane working directory")
	templatePath := fs.String("template", "", "topology-only Kitty session template")
	credentialBundle := fs.String("credential-bundle", "", "credential bundle to bind at creation")
	noCredentials := fs.Bool("no-credentials", false, "leave managed credential endpoints unclaimed and fail-closed")
	if err := parseInterspersed(fs, args); err != nil {
		return 2, err
	}
	if err := validateKittyPassthrough(fs.Args()); err != nil {
		return 2, err
	}
	if *cwd == "" {
		var err error
		*cwd, err = os.Getwd()
		if err != nil {
			return 1, err
		}
	}
	template := DefaultSessionTemplate()
	if *templatePath != "" {
		content, err := os.ReadFile(*templatePath)
		if err != nil {
			return 1, fmt.Errorf("read Kitty template: %w", err)
		}
		template, err = ParseSessionTemplate(string(content))
		if err != nil {
			return 2, err
		}
	}
	specs, err := templatePaneSpecs(template, *cwd)
	if err != nil {
		return 2, err
	}
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	bundle, err := creationCredentialBundle(cfg, *credentialBundle, *noCredentials, false)
	if err != nil {
		return 2, err
	}
	api := NewAPI(paths)
	refreshCredentialSessionForCLI(api, bundle, "create", "", stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspace, err := api.CreateWorkspace(ctx, createWorkspaceRequest{Name: *name, Shell: cfg.Shell.Command, Panes: specs})
	if err != nil {
		return 1, err
	}
	session, err := GenerateManagedSession(template, workspace)
	if err != nil {
		_ = api.DeleteWorkspace(context.Background(), workspace.ID)
		return 1, err
	}
	attachmentID := localAttachmentID(workspace.Origin.ID, workspace.ID)
	attachment := Attachment{
		ID: attachmentID, Node: workspace.Origin, Transport: Transport{Kind: "local"},
		Endpoint: attachmentEndpoint(paths, attachmentID),
	}
	launchedWorkspace, err := launchManagedKitty(ctx, paths, cfg, api, launchAttachmentOptions{
		Workspace: workspace, Attachment: attachment, Session: session, KittyArgs: fs.Args(),
	})
	if err != nil {
		if failedWorkspaceHasBackend(api, workspace.ID) {
			return 1, fmt.Errorf("start managed Kitty (workspace %s retained because a zmx backend started): %w", workspace.ID, err)
		}
		_ = api.DeleteWorkspace(context.Background(), workspace.ID)
		return 1, err
	}
	workspace = launchedWorkspace
	if bundle != "" {
		if _, err := api.ActivateLocalCredentials(ctx, workspace.ID, bundle, attachmentID, false); err != nil {
			return 1, fmt.Errorf("workspace %s attached but credentials were not claimed by attachment %s: %w", workspace.Name, attachmentID, err)
		}
	}
	fmt.Fprintf(stdout, "%s\t%s\n", workspace.ID, workspace.Name)
	return 0, nil
}

func failedWorkspaceHasBackend(api API, workspaceID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	workspace, err := api.Workspace(ctx, workspaceID)
	if err != nil {
		return true
	}
	for _, pane := range workspace.Panes {
		if pane.BackendCreated || pane.BackendStart {
			return true
		}
	}
	return false
}

func runWorkspace(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		printWorkspaceUsage(stderr)
		return 2, nil
	}
	switch args[0] {
	case "help", "--help", "-h":
		printWorkspaceUsage(stdout)
		return 0, nil
	case "list":
		return runWorkspaceList(args[1:], paths, stdout, stderr)
	case "inspect":
		return runWorkspaceInspect(args[1:], paths, stdout, stderr)
	case "reconcile":
		return runWorkspaceReconcile(args[1:], paths, stdout, stderr)
	case "create":
		return runWorkspaceCreate(args[1:], paths, stdout, stderr)
	case "attach":
		return runWorkspaceAttach(args[1:], paths, false, stdout, stderr)
	case "move":
		return runWorkspaceAttach(args[1:], paths, true, stdout, stderr)
	case "detach":
		return runWorkspaceDetach(args[1:], paths, stdout, stderr)
	case "forget":
		return runWorkspaceForget(args[1:], paths, stdout, stderr)
	case "rename":
		return runWorkspaceRename(args[1:], paths, stdout, stderr)
	case "kill":
		return runWorkspaceKill(args[1:], paths, stdout, stderr)
	case "focus":
		return runWorkspaceFocus(args[1:], paths, stdout, stderr)
	case "seen":
		return runWorkspaceSeen(args[1:], paths, stdout, stderr)
	case "credentials":
		return runWorkspaceCredentials(args[1:], paths, stdout, stderr)
	default:
		printWorkspaceUsage(stderr)
		return 2, fmt.Errorf("unknown workspace command %q", args[0])
	}
}

func printWorkspaceUsage(w io.Writer) {
	fmt.Fprintln(w, `usage: zka workspace COMMAND

  list [--origin SSH_ALIAS] [--json]
  inspect [SSH_ALIAS:]REF [--json]
  reconcile [SSH_ALIAS:]REF [--attachment ID] [--recreate-backends]
  create [SSH_ALIAS:]NAME [--template FILE] [--cwd DIR] [--attach] [--credential-bundle NAME] [--no-credentials]
  attach [SSH_ALIAS:]REF [--pane PANE] [--claim-credentials] [--credential-bundle NAME]
  move [SSH_ALIAS:]REF [--pane PANE]
  detach REF
  forget [SSH_ALIAS:]REF
  rename [SSH_ALIAS:]REF NAME
  kill [SSH_ALIAS:]REF
  focus REF [--pane PANE]
  seen REF [--pane PANE]
  credentials claim [--bundle NAME] [--attachment ID] [SSH_ALIAS:]REF
  credentials activate-local [--bundle NAME] [--attachment ID] [--if-unclaimed] REF
  credentials endpoint [--json] REF
  credentials release [SSH_ALIAS:]REF
  credentials status [--json] [[SSH_ALIAS:]REF]`)
}

func runWorkspaceCredentials(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("workspace credentials requires claim, activate-local, endpoint, release, or status")
	}
	api := NewAPI(paths)
	switch args[0] {
	case "claim":
		fs := newFlagSet("workspace credentials claim", stderr)
		bundleName := fs.String("bundle", "", "credential bundle to claim")
		attachmentID := fs.String("attachment", "", "active controlling attachment id")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return 2, err
		}
		if fs.NArg() != 1 {
			return 2, fmt.Errorf("workspace credentials claim requires one workspace reference")
		}
		cfg, err := LoadConfig()
		if err != nil {
			return 1, err
		}
		if *bundleName == "" {
			*bundleName = cfg.Credentials.DefaultBundle
		}
		if *bundleName == "" {
			return 2, fmt.Errorf("--bundle is required because credentials.default_bundle is not set")
		}
		resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 15*time.Second)
		host, _ := splitWorkspaceRef(fs.Arg(0))
		workspace, err := resolveWorkspace(resolveCtx, api, fs.Arg(0))
		resolveCancel()
		if err != nil {
			return 1, err
		}
		if host == "" {
			host = workspace.RemoteHost
		}
		node, err := api.Node(context.Background())
		if err != nil {
			return 1, err
		}
		*attachmentID, err = credentialOwnerAttachment(workspace, node.ID, *attachmentID)
		if err != nil {
			return 1, err
		}
		if host == "" {
			return 1, fmt.Errorf("workspace %q is authoritative here; use credentials activate-local", workspace.Name)
		}
		refreshCredentialSessionForCLI(api, *bundleName, "claim", workspace.ID, stderr)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var status workspaceCredentialStatus
		err = api.RemoteCall(ctx, host, "credentials_claim", workspaceCredentialRequest{
			Workspace: workspace.ID, Bundle: *bundleName, OwnerAttachmentID: *attachmentID,
		}, &status)
		if err != nil {
			return 1, err
		}
		var refreshed Workspace
		_ = api.RemoteCall(ctx, host, "get", refRequest{Ref: workspace.ID}, &refreshed)
		writeWorkspaceCredentialStatus(stdout, status)
		return 0, nil
	case "activate-local":
		fs := newFlagSet("workspace credentials activate-local", stderr)
		bundleName := fs.String("bundle", "", "local credential bundle to activate")
		attachmentID := fs.String("attachment", "", "active controlling attachment id")
		ifUnclaimed := fs.Bool("if-unclaimed", false, "leave an existing workspace credential binding unchanged")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return 2, err
		}
		if fs.NArg() != 1 {
			return 2, fmt.Errorf("workspace credentials activate-local requires one workspace reference")
		}
		cfg, err := LoadConfig()
		if err != nil {
			return 1, err
		}
		if *bundleName == "" {
			*bundleName = cfg.Credentials.DefaultBundle
		}
		if *bundleName == "" {
			return 2, fmt.Errorf("--bundle is required because credentials.default_bundle is not set")
		}
		resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 15*time.Second)
		workspace, err := resolveWorkspace(resolveCtx, api, fs.Arg(0))
		resolveCancel()
		if err != nil {
			return 1, err
		}
		if workspace.RemoteHost != "" {
			return 1, fmt.Errorf("activate-local must run on workspace origin %s", workspace.Origin.Name)
		}
		node, err := api.Node(context.Background())
		if err != nil {
			return 1, err
		}
		*attachmentID, err = credentialActivationOwnerAttachment(workspace, node.ID, *attachmentID, *ifUnclaimed)
		if err != nil {
			return 1, err
		}
		if !*ifUnclaimed || workspace.CredentialClaim == nil {
			refreshCredentialSessionForCLI(api, *bundleName, "activate-local", workspace.ID, stderr)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		status, err := api.ActivateLocalCredentials(ctx, workspace.ID, *bundleName, *attachmentID, *ifUnclaimed)
		if err != nil {
			return 1, err
		}
		writeWorkspaceCredentialStatus(stdout, status)
		return 0, nil
	case "endpoint":
		fs := newFlagSet("workspace credentials endpoint", stderr)
		jsonOut := fs.Bool("json", false, "emit JSON")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return 2, err
		}
		if fs.NArg() != 1 {
			return 2, fmt.Errorf("workspace credentials endpoint requires one workspace reference")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		workspace, err := resolveWorkspace(ctx, api, fs.Arg(0))
		if err != nil {
			return 1, err
		}
		endpoint, err := api.PIVBEndpoint(ctx, workspace.ID)
		if err != nil {
			return 1, err
		}
		if *jsonOut {
			return 0, writeJSON(stdout, endpoint)
		}
		if endpoint.State != "ready" {
			detail := endpoint.Detail
			if detail == "" {
				detail = "activate or claim a PIVB-enabled credential bundle first"
			}
			return 1, fmt.Errorf("workspace PIVB route is %s: %s", endpoint.State, detail)
		}
		fmt.Fprintln(stdout, endpoint.Socket)
		return 0, nil
	case "release", "status":
		fs := newFlagSet("workspace credentials "+args[0], stderr)
		jsonOut := fs.Bool("json", false, "emit JSON")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return 2, err
		}
		if fs.NArg() > 1 || args[0] == "release" && fs.NArg() != 1 {
			return 2, fmt.Errorf("workspace credentials %s accepts %s workspace reference", args[0], map[bool]string{true: "one", false: "at most one"}[args[0] == "release"])
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if fs.NArg() == 0 {
			status, err := api.CredentialStatus(ctx, "")
			if err != nil {
				return 1, err
			}
			if *jsonOut {
				return 0, writeJSON(stdout, status)
			}
			for _, workspace := range status.Workspaces {
				writeWorkspaceCredentialStatus(stdout, workspace)
			}
			return 0, nil
		}
		host, _ := splitWorkspaceRef(fs.Arg(0))
		workspace, err := resolveWorkspace(ctx, api, fs.Arg(0))
		if err != nil {
			return 1, err
		}
		if host == "" {
			host = workspace.RemoteHost
		}
		if host != "" {
			var response credentialStatusResponse
			if args[0] == "release" {
				var released workspaceCredentialStatus
				err = api.RemoteCall(ctx, host, "credentials_release", workspaceCredentialRequest{Workspace: workspace.ID}, &released)
				response.Workspaces = []workspaceCredentialStatus{released}
			} else {
				err = api.RemoteCall(ctx, host, "credentials_status", workspaceCredentialRequest{Workspace: workspace.ID}, &response)
			}
			if err != nil {
				return 1, err
			}
			if *jsonOut {
				return 0, writeJSON(stdout, response)
			}
			for _, item := range response.Workspaces {
				writeWorkspaceCredentialStatus(stdout, item)
			}
			return 0, nil
		}
		if args[0] == "release" {
			released, err := api.ReleaseWorkspaceCredentials(ctx, workspace.ID)
			if err != nil {
				return 1, err
			}
			writeWorkspaceCredentialStatus(stdout, released)
			return 0, nil
		}
		response, err := api.CredentialStatus(ctx, workspace.ID)
		if err != nil {
			return 1, err
		}
		if *jsonOut {
			return 0, writeJSON(stdout, response)
		}
		for _, item := range response.Workspaces {
			writeWorkspaceCredentialStatus(stdout, item)
		}
		return 0, nil
	default:
		return 2, fmt.Errorf("unknown workspace credentials command %q", args[0])
	}
}

func writeWorkspaceCredentialStatus(w io.Writer, status workspaceCredentialStatus) {
	fmt.Fprintf(w, "credentials=%s workspace=%s", status.State, status.WorkspaceName)
	if status.Bundle != "" {
		fmt.Fprintf(w, " bundle=%s", status.Bundle)
	}
	if status.OwnerNode != "" {
		fmt.Fprintf(w, " node=%s", shortID(status.OwnerNode))
	}
	capabilities := make([]string, 0, len(status.Capabilities))
	for name, capability := range status.Capabilities {
		value := name + ":" + capability.State
		if capability.Detail != "" {
			value += "(" + capability.Detail + ")"
		}
		capabilities = append(capabilities, value)
	}
	sort.Strings(capabilities)
	if len(capabilities) != 0 {
		fmt.Fprintf(w, " capabilities=%s", strings.Join(capabilities, ","))
	}
	if len(status.RecreatePaneIDs) != 0 {
		fmt.Fprintf(w, " recreate_panes=%s", strings.Join(status.RecreatePaneIDs, ","))
	}
	if status.RecreationDetail != "" {
		fmt.Fprintf(w, " recreation_detail=%q", status.RecreationDetail)
	}
	fmt.Fprintln(w)
}

// runWorkspaceCreate births a workspace without launching Kitty, locally or on
// a remote origin. The workspace comes out dormant — fully attachable, with no
// view anywhere — which is the whole point: agents on a headless origin can be
// set up from any machine and attached to later.
func runWorkspaceCreate(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("workspace create", stderr)
	cwd := fs.String("cwd", "", "default pane working directory on the origin")
	templatePath := fs.String("template", "", "topology-only Kitty session template")
	attach := fs.Bool("attach", false, "attach the workspace here after creating it")
	claimCredentials := fs.Bool("claim-credentials", false, "deprecated alias for binding the creation credential bundle")
	credentialBundle := fs.String("credential-bundle", "", "credential bundle to bind at creation")
	noCredentials := fs.Bool("no-credentials", false, "leave managed credential endpoints unclaimed and fail-closed")
	if err := parseInterspersed(fs, args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("workspace create requires one [SSH_ALIAS:]NAME argument")
	}
	host, name := splitWorkspaceRef(fs.Arg(0))
	if host == "" && strings.TrimSpace(name) == "" {
		return 2, fmt.Errorf("workspace create requires a name (append SSH_ALIAS: for an automatic remote name)")
	}
	if *noCredentials && (*claimCredentials || *credentialBundle != "") {
		return 2, fmt.Errorf("--no-credentials cannot be combined with --claim-credentials or --credential-bundle")
	}
	if *cwd != "" && !filepath.IsAbs(*cwd) {
		if host != "" {
			// A relative path resolved on this machine would silently mean a
			// different directory on the origin.
			return 2, fmt.Errorf("--cwd %q must be an absolute path on %s", *cwd, host)
		}
		resolved, err := filepath.Abs(*cwd)
		if err != nil {
			return 1, err
		}
		*cwd = resolved
	}
	template := DefaultSessionTemplate()
	if *templatePath != "" {
		content, err := os.ReadFile(*templatePath)
		if err != nil {
			return 1, fmt.Errorf("read Kitty template: %w", err)
		}
		template, err = ParseSessionTemplate(string(content))
		if err != nil {
			return 2, err
		}
	}
	plan, err := TemplateGenesis(template, *cwd)
	if err != nil {
		return 2, err
	}
	key, err := randomID()
	if err != nil {
		return 1, err
	}
	// Shell is deliberately left empty: the origin's configured shell must
	// win, and for a remote create this machine's config describes the wrong
	// host. Empty pane directories default to the origin's home the same way.
	request := createWorkspaceRequest{
		Name: name, Panes: plan.Panes, Topology: plan.Topology,
		FocusPane: plan.FocusPane, CreationKey: key,
	}
	api := NewAPI(paths)
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	bundle, err := creationCredentialBundle(cfg, *credentialBundle, *noCredentials, *claimCredentials)
	if err != nil {
		return 2, err
	}
	// Even for a remote workspace, the credentials originate at this node. The
	// local daemon owns both the provider and the peer-derived graphical session.
	refreshCredentialSessionForCLI(api, bundle, "create", "", stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var workspace *Workspace
	if host == "" {
		workspace, err = api.CreateWorkspace(ctx, request)
	} else {
		var created Workspace
		if err = api.RemoteCall(ctx, host, "create_workspace", request, &created); err != nil {
			err = fmt.Errorf("create workspace on %s: %w", host, err)
		} else {
			workspace = &created
		}
	}
	if err != nil {
		if host != "" && strings.Contains(err.Error(), "already exists") {
			fmt.Fprintf(stderr, "zka: attach the existing workspace with: zka workspace attach %s:%s\n", host, name)
		}
		return 1, err
	}
	if !*attach {
		fmt.Fprintf(stdout, "%s\t%s\n", workspace.ID, workspace.Name)
		return 0, nil
	}
	ref := workspace.ID
	if host != "" {
		ref = host + ":" + workspace.ID
	}
	attachArgs := []string{ref}
	if bundle != "" {
		attachArgs = append([]string{"--claim-credentials", "--credential-bundle", bundle}, attachArgs...)
	}
	code, err := runWorkspaceAttach(attachArgs, paths, false, stdout, stderr)
	if err != nil {
		retry := "zka workspace attach " + ref
		return code, fmt.Errorf("workspace %s created but its attach sequence did not complete; retry with: %s: %w", workspace.Name, retry, err)
	}
	return code, nil
}

func creationCredentialBundle(cfg Config, explicit string, noCredentials, compatibilityClaim bool) (string, error) {
	if noCredentials {
		if explicit != "" || compatibilityClaim {
			return "", fmt.Errorf("--no-credentials cannot be combined with --credential-bundle or --claim-credentials")
		}
		return "", nil
	}
	bundle := explicit
	if bundle == "" {
		bundle = cfg.Credentials.DefaultBundle
	}
	if bundle == "" && compatibilityClaim {
		return "", fmt.Errorf("--claim-credentials requires --credential-bundle because credentials.default_bundle is not set")
	}
	if bundle != "" {
		if _, ok := cfg.credentialBundle(bundle); !ok {
			return "", fmt.Errorf("credential bundle %q is not configured", bundle)
		}
	}
	return bundle, nil
}

func runWorkspaceReconcile(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("workspace reconcile", stderr)
	attachmentID := fs.String("attachment", "", "local attachment id")
	recreateBackends := fs.Bool("recreate-backends", false, "stop and recreate route-unsafe managed backends")
	if err := parseInterspersed(fs, args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("workspace reconcile requires one workspace reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	api := NewAPI(paths)
	workspace, err := resolveWorkspace(ctx, api, fs.Arg(0))
	if err != nil {
		return 1, err
	}
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	if *recreateBackends {
		if workspace.RemoteHost != "" {
			var recreated Workspace
			err = api.RemoteCall(ctx, workspace.RemoteHost, "recreate_credential_backends", refRequest{Ref: workspace.ID}, &recreated)
			workspace = &recreated
		} else {
			workspace, err = api.RecreateCredentialBackends(ctx, workspace.ID)
		}
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(stdout, "%s\tcredential backends recreated\n", workspace.ID)
		return 0, nil
	}
	if cfg.Headless && *attachmentID == "" && workspace.RemoteHost == "" {
		// No Kitty can ever run here, so there is no view to recapture — but
		// the backend census is still meaningful.
		if _, err := api.ReconcileBackends(ctx, workspace.ID); err != nil {
			return 1, err
		}
		fmt.Fprintf(stdout, "%s\tbackends reconciled (headless origin: no local Kitty attachments)\n", workspace.ID)
		return 0, nil
	}
	workspace, err = api.ReconcileTopology(ctx, workspace.ID, *attachmentID)
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "%s\tgeneration=%d\tdigest=%s\n",
		workspace.ID, workspace.Topology.Generation, shortDigest(workspace.Topology.Digest))
	return 0, nil
}

func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func runWorkspaceList(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("workspace list", stderr)
	origin := fs.String("origin", "", "SSH host alias")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := parseInterspersed(fs, args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("workspace list accepts no references")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := NewAPI(paths)
	var workspaces []*Workspace
	var err error
	if *origin != "" {
		err = api.RemoteCall(ctx, *origin, "list", nil, &workspaces)
	} else {
		workspaces, err = api.Workspaces(ctx)
	}
	if err != nil {
		return 1, err
	}
	if *jsonOut {
		return 0, writeJSON(stdout, workspaces)
	}
	writeWorkspaceTable(stdout, workspaces)
	return 0, nil
}

func runWorkspaceInspect(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("workspace inspect", stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := parseInterspersed(fs, args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("workspace inspect requires one workspace reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	workspace, err := resolveWorkspace(ctx, NewAPI(paths), fs.Arg(0))
	if err != nil {
		return 1, err
	}
	if *jsonOut {
		return 0, writeJSON(stdout, workspace)
	}
	writeWorkspaceDetail(stdout, workspace)
	return 0, nil
}

func runWorkspaceAttach(args []string, paths Paths, move bool, stdout, stderr io.Writer) (int, error) {
	name := "workspace attach"
	if move {
		name = "workspace move"
	}
	fs := newFlagSet(name, stderr)
	paneRef := fs.String("pane", "", "pane to focus after attaching")
	claimCredentials := fs.Bool("claim-credentials", false, "claim a credential bundle after attaching")
	credentialBundle := fs.String("credential-bundle", "", "credential bundle to claim")
	if err := parseInterspersed(fs, args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("%s requires one workspace reference", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	api := NewAPI(paths)
	host, ref := splitWorkspaceRef(fs.Arg(0))
	if *credentialBundle != "" && !*claimCredentials {
		return 2, fmt.Errorf("--credential-bundle requires --claim-credentials")
	}
	if *claimCredentials && *credentialBundle == "" {
		cfg, err := LoadConfig()
		if err != nil {
			return 1, err
		}
		if cfg.Credentials.DefaultBundle == "" {
			return 2, fmt.Errorf("--claim-credentials requires --credential-bundle because credentials.default_bundle is not set")
		}
	}
	var workspace *Workspace
	var err error
	if host == "" {
		workspace, err = api.Workspace(ctx, ref)
		if err == nil && workspace.RemoteHost == "" {
			_, err = api.ReconcileBackends(ctx, workspace.ID)
			if err == nil {
				workspace, err = api.Workspace(ctx, workspace.ID)
			}
		}
	} else {
		var reconciled backendReconcileResponse
		err = api.RemoteCall(ctx, host, "reconcile_backends", backendReconcileRequest{Workspace: ref}, &reconciled)
		var remote Workspace
		if err == nil {
			err = api.RemoteCall(ctx, host, "get", refRequest{Ref: ref}, &remote)
		}
		if err == nil {
			workspace, err = api.Workspace(ctx, remote.ID)
		}
	}
	if err != nil {
		return 1, err
	}
	if workspace.DeletionPending {
		return 1, fmt.Errorf("workspace %q is being deleted", workspace.Name)
	}
	if *paneRef != "" {
		pane, err := resolvePaneFromCopy(workspace, *paneRef)
		if err != nil {
			return 1, err
		}
		*paneRef = pane.ID
		if pane.BackendDead {
			return 1, fmt.Errorf("pane %q cannot be opened: zmx backend is dead", pane.Title)
		}
	}
	node, err := api.Node(ctx)
	if err != nil {
		return 1, err
	}
	attachmentID := localAttachmentID(node.ID, workspace.ID)
	existing := preferredLocalAttachment(workspace, node.ID)
	if existing != nil {
		attachmentID = existing.ID
	}
	if attachmentUsable(existing) && !attachmentTopologyCurrent(workspace, existing) {
		reconciled, reconcileErr := api.ReconcileTopology(ctx, workspace.ID, existing.ID)
		if reconcileErr == nil {
			workspace = reconciled
			existing = workspace.Attachments[existing.ID]
		} else {
			fmt.Fprintf(stderr, "zka: topology reconciliation failed for attachment %s: %v; rebuilding the view\n",
				shortID(existing.ID), reconcileErr)
		}
	}
	if attachmentUsable(existing) && attachmentTopologyCurrent(workspace, existing) {
		if move && workspace.PrimaryAttachmentID != existing.ID {
			var moved *Workspace
			if host != "" {
				moved, err = commitRemoteMove(ctx, api, host, workspace, existing)
			} else {
				moved, err = commitLocalMove(ctx, api, workspace, existing)
			}
			if err != nil {
				return 1, err
			}
			if err := validateWorkspaceTransition(workspace, moved); err != nil {
				return 1, err
			}
			workspace = moved
			existing = workspace.Attachments[existing.ID]
			if existing == nil {
				return 1, fmt.Errorf("moved workspace %s lost attachment %s", workspace.ID, attachmentID)
			}
		}
		if err := focusAttachment(ctx, paths, workspace, existing, *paneRef); err != nil {
			return 1, err
		}
		if *claimCredentials {
			if err := claimAttachedWorkspaceCredentials(api, host, workspace, existing.ID, *credentialBundle, stderr); err != nil {
				return 1, err
			}
		}
		fmt.Fprintf(stdout, "%s\t%s\n", workspace.ID, workspace.Name)
		return 0, nil
	}
	if existing != nil && strings.HasPrefix(existing.Endpoint, "unix:") && existing.Status != AttachmentDetached {
		if workspace.PrimaryAttachmentID == existing.ID && !existing.Revoked {
			attachmentID, err = randomID()
			if err != nil {
				return 1, err
			}
		} else {
			if err := closeAndDetachLocal(ctx, api, workspace.ID, existing); err != nil {
				var closeErr *kittyCloseError
				if !errors.As(err, &closeErr) {
					return 1, err
				}
				fmt.Fprintf(stderr, "zka: %v; rebuilding the detached view\n", closeErr)
			}
			workspace, err = api.Workspace(ctx, workspace.ID)
			if err != nil {
				return 1, err
			}
		}
	}
	if len(workspace.Topology.Roots) == 0 && strings.TrimSpace(workspace.Manifest.Session) == "" {
		return 1, fmt.Errorf("workspace %s has no captured Kitty manifest", workspace.Name)
	}
	transport := Transport{Kind: "local"}
	if host != "" {
		transport = Transport{Kind: "ssh", Host: host}
	}
	attachment := Attachment{ID: attachmentID, Node: node, Transport: transport, Endpoint: attachmentEndpoint(paths, attachmentID)}
	session, err := RenderAttachmentSession(workspace, transport, attachmentID)
	if err != nil {
		return 1, err
	}
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	if host != "" {
		remoteAttachment := attachment
		remoteAttachment.Endpoint = "ssh:" + node.Name + ":" + attachment.ID
		if err := api.RemoteCall(ctx, host, "register_attachment", attachmentRequest{Workspace: workspace.ID, Attachment: remoteAttachment}, new(Attachment)); err != nil {
			return 1, err
		}
	}
	attached, err := launchManagedKitty(ctx, paths, cfg, api, launchAttachmentOptions{Workspace: workspace, Attachment: attachment, Session: session})
	if err != nil {
		if host != "" {
			err = joinWorkspaceAttachRollback(err, rollbackRemoteAttachment(api, host, workspace.ID, attachmentID))
		}
		return 1, err
	}
	if err := validateWorkspaceTransition(workspace, attached); err != nil {
		return 1, joinWorkspaceAttachRollback(err,
			rollbackLaunchedAttachment(liveWorkspaceAttachOperations{api: api}, host, workspace.ID, &attachment))
	}
	localAttachment := attached.Attachments[attachmentID]
	if localAttachment == nil {
		err := fmt.Errorf("launched workspace %s lost attachment %s", attached.ID, attachmentID)
		return 1, joinWorkspaceAttachRollback(err,
			rollbackLaunchedAttachment(liveWorkspaceAttachOperations{api: api}, host, attached.ID, &attachment))
	}
	finalized, err := finalizeLaunchedWorkspaceAttach(
		ctx,
		liveWorkspaceAttachOperations{api: api},
		host,
		move,
		attached,
		localAttachment,
	)
	if err != nil {
		return 1, err
	}
	workspace = finalized
	if err := focusAttachment(ctx, paths, workspace, workspace.Attachments[attachmentID], *paneRef); err != nil {
		return 1, err
	}
	if *claimCredentials {
		if err := claimAttachedWorkspaceCredentials(api, host, workspace, attachmentID, *credentialBundle, stderr); err != nil {
			return 1, err
		}
	}
	fmt.Fprintf(stdout, "%s\t%s\n", workspace.ID, workspace.Name)
	return 0, nil
}

func claimAttachedWorkspaceCredentials(api API, host string, workspace *Workspace, ownerAttachmentID, bundleName string, stderr io.Writer) error {
	if workspace == nil {
		return fmt.Errorf("workspace attached but its credential owner could not be verified: workspace is unavailable")
	}
	if ownerAttachmentID == "" {
		return fmt.Errorf("workspace %s attached but its credential owner could not be verified: attachment is unavailable", workspace.Name)
	}
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if bundleName == "" {
		bundleName = cfg.Credentials.DefaultBundle
	}
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 15*time.Second)
	var authoritative *Workspace
	if host == "" {
		authoritative, err = api.Workspace(resolveCtx, workspace.ID)
	} else {
		var remote Workspace
		err = api.RemoteCall(resolveCtx, host, "get", refRequest{Ref: workspace.ID}, &remote)
		authoritative = &remote
	}
	resolveCancel()
	if err != nil {
		return fmt.Errorf("workspace %s attached but its credential owner could not be verified: %w", workspace.Name, err)
	}
	node, err := api.Node(context.Background())
	if err != nil {
		return err
	}
	ownerAttachment, err := credentialOwnerAttachment(authoritative, node.ID, ownerAttachmentID)
	if err != nil {
		return fmt.Errorf("workspace %s attached but its credential owner could not be verified: %w", workspace.Name, err)
	}
	if bundleName == "" {
		return fmt.Errorf("workspace %s attached but no credential bundle was selected", workspace.Name)
	}
	refreshCredentialSessionForCLI(api, bundleName, "attach-claim", workspace.ID, stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var status workspaceCredentialStatus
	if host == "" {
		_, err := api.ActivateLocalCredentials(ctx, workspace.ID, bundleName, ownerAttachment, false)
		if err != nil {
			return fmt.Errorf("workspace %s attached but credential bundle %s was not claimed: %w", workspace.Name, bundleName, err)
		}
		return nil
	}
	if err := api.RemoteCall(ctx, host, "credentials_claim", workspaceCredentialRequest{
		Workspace: workspace.ID, Bundle: bundleName, OwnerAttachmentID: ownerAttachment,
	}, &status); err != nil {
		return fmt.Errorf("workspace %s attached but credential bundle %s was not claimed: %w", workspace.Name, bundleName, err)
	}
	return nil
}

func credentialOwnerAttachment(workspace *Workspace, nodeID, explicit string) (string, error) {
	if workspace == nil {
		return "", fmt.Errorf("workspace is unavailable")
	}
	if explicit != "" {
		attachment := workspace.Attachments[explicit]
		if attachment == nil || attachment.Node.ID != nodeID || attachment.Status != AttachmentReady || attachment.Revoked {
			return "", fmt.Errorf("attachment %s is not an active attachment owned by this node", explicit)
		}
		return explicit, nil
	}
	var candidates []string
	for id, attachment := range workspace.Attachments {
		if attachment != nil && attachment.Node.ID == nodeID && attachment.Status == AttachmentReady && !attachment.Revoked {
			candidates = append(candidates, id)
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", fmt.Errorf("credential claim requires an active attachment owned by this node")
	}
	if len(candidates) > 1 {
		return "", fmt.Errorf("multiple active attachments are owned by this node; pass --attachment with one of: %s", strings.Join(candidates, ","))
	}
	return candidates[0], nil
}

func credentialActivationOwnerAttachment(workspace *Workspace, nodeID, explicit string, ifUnclaimed bool) (string, error) {
	ownerAttachment, err := credentialOwnerAttachment(workspace, nodeID, explicit)
	if err == nil {
		return ownerAttachment, nil
	}
	if ifUnclaimed && workspace != nil && workspace.CredentialClaim != nil {
		return "", nil
	}
	return "", err
}

func refreshCredentialSessionForCLI(api API, bundle, action, workspace string, stderr io.Writer) {
	if strings.TrimSpace(bundle) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := api.RefreshCredentialSession(ctx, credentialSessionRefreshRequest{
		Bundle: bundle, Action: action, Workspace: workspace,
	})
	if err != nil {
		if isUnknownDaemonOperation(err, "credential_session_refresh") {
			fmt.Fprintln(stderr, "zka: warning: zkad does not support graphical pinentry refresh; upgrade and restart the local daemon")
			return
		}
		fmt.Fprintf(stderr, "zka: warning: graphical pinentry session was not refreshed: %v\n", err)
		return
	}
	if response.State == "warning" {
		fmt.Fprintf(stderr, "zka: warning: graphical pinentry session was not refreshed: %s\n", response.Detail)
	}
}

func attachmentUsable(attachment *Attachment) bool {
	return attachment != nil && attachment.Status == AttachmentReady && strings.HasPrefix(attachment.Endpoint, "unix:") && !attachment.Revoked
}

func attachmentTopologyCurrent(workspace *Workspace, attachment *Attachment) bool {
	if workspace == nil || attachment == nil {
		return false
	}
	if workspace.Topology.Generation == 0 {
		return attachment.AppliedRevision == workspace.Revision
	}
	return attachment.AppliedTopologyGeneration == workspace.Topology.Generation &&
		attachment.AppliedTopologyDigest == workspace.Topology.Digest &&
		topologyMatchesDesired(workspace, attachment.ObservedTopology)
}

func preferredLocalAttachment(workspace *Workspace, nodeID string) *Attachment {
	primary := workspace.Attachments[workspace.PrimaryAttachmentID]
	if primary != nil && primary.Node.ID == nodeID && attachmentUsable(primary) {
		return primary
	}
	for _, attachment := range workspace.SortedAttachments() {
		if attachment.Node.ID == nodeID && attachmentUsable(attachment) {
			return workspace.Attachments[attachment.ID]
		}
	}
	if primary != nil && primary.Node.ID == nodeID && strings.HasPrefix(primary.Endpoint, "unix:") && primary.Status != AttachmentDetached {
		return primary
	}
	if deterministic := workspace.Attachments[localAttachmentID(nodeID, workspace.ID)]; deterministic != nil {
		return deterministic
	}
	for _, attachment := range workspace.SortedAttachments() {
		if attachment.Node.ID == nodeID && strings.HasPrefix(attachment.Endpoint, "unix:") {
			return workspace.Attachments[attachment.ID]
		}
	}
	return nil
}

func focusableLocalAttachment(workspace *Workspace, nodeID, paneID string) *Attachment {
	preferred := preferredLocalAttachment(workspace, nodeID)
	if localAttachmentCanFocus(preferred, nodeID, paneID) {
		return preferred
	}
	for _, attachment := range workspace.SortedAttachments() {
		if localAttachmentCanFocus(attachment, nodeID, paneID) {
			return workspace.Attachments[attachment.ID]
		}
	}
	return nil
}

func localAttachmentCanFocus(attachment *Attachment, nodeID, paneID string) bool {
	if attachment == nil || attachment.Node.ID != nodeID || attachment.Status == AttachmentDetached ||
		attachment.Revoked || !strings.HasPrefix(attachment.Endpoint, "unix:") {
		return false
	}
	if paneID == "" {
		return true
	}
	view, ok := attachment.Views[paneID]
	return ok && view.Ready
}

const workspaceAttachRollbackTimeout = 15 * time.Second

type workspaceAttachOperations interface {
	readyRemote(context.Context, string, *Workspace, *Attachment) (*Workspace, error)
	commitRemote(context.Context, string, *Workspace, *Attachment) (*Workspace, error)
	commitLocal(context.Context, *Workspace, *Attachment) (*Workspace, error)
	rollback(context.Context, string, string, *Attachment) error
}

type liveWorkspaceAttachOperations struct {
	api API
}

func (o liveWorkspaceAttachOperations) readyRemote(ctx context.Context, host string, workspace *Workspace, attachment *Attachment) (*Workspace, error) {
	return readyRemoteAttachment(ctx, o.api, host, workspace, attachment)
}

func (o liveWorkspaceAttachOperations) commitRemote(ctx context.Context, host string, workspace *Workspace, attachment *Attachment) (*Workspace, error) {
	return commitRemoteMove(ctx, o.api, host, workspace, attachment)
}

func (o liveWorkspaceAttachOperations) commitLocal(ctx context.Context, workspace *Workspace, attachment *Attachment) (*Workspace, error) {
	return commitLocalMove(ctx, o.api, workspace, attachment)
}

func (o liveWorkspaceAttachOperations) rollback(ctx context.Context, host, workspaceID string, attachment *Attachment) error {
	if workspaceID == "" {
		return fmt.Errorf("rollback workspace id is empty")
	}
	if attachment == nil {
		return fmt.Errorf("rollback attachment does not exist")
	}
	steps := []workspaceAttachRollbackStep{
		{
			name: "detach local attachment state",
			run: func(ctx context.Context) error {
				_, err := o.api.DetachAttachment(ctx, workspaceID, attachment.ID)
				return err
			},
		},
		{
			name: "close local Kitty view",
			run: func(ctx context.Context) error {
				return closeLocalWorkspaceView(ctx, workspaceID, attachment)
			},
		},
	}
	if host != "" {
		steps = append(steps, workspaceAttachRollbackStep{
			name: "detach origin attachment state",
			run: func(ctx context.Context) error {
				return o.api.RemoteCall(ctx, host, "detach_attachment", attachmentRefRequest{
					Workspace: workspaceID, Attachment: attachment.ID,
				}, nil)
			},
		})
	}
	return runWorkspaceAttachRollbackSteps(ctx, steps...)
}

type workspaceAttachRollbackStep struct {
	name string
	run  func(context.Context) error
}

func runWorkspaceAttachRollbackSteps(ctx context.Context, steps ...workspaceAttachRollbackStep) error {
	var failures []error
	for _, step := range steps {
		if err := step.run(ctx); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", step.name, err))
		}
	}
	return errors.Join(failures...)
}

func validateWorkspaceTransition(current, next *Workspace) error {
	if current == nil {
		return fmt.Errorf("workspace transition started without a workspace")
	}
	if current.ID == "" {
		return fmt.Errorf("workspace transition started without a workspace id")
	}
	if next == nil {
		return fmt.Errorf("workspace transition for %s returned a nil workspace", current.ID)
	}
	if next.ID != current.ID {
		return fmt.Errorf("workspace transition for %s returned workspace %s", current.ID, next.ID)
	}
	return nil
}

func finalizeLaunchedWorkspaceAttach(
	ctx context.Context,
	operations workspaceAttachOperations,
	host string,
	move bool,
	workspace *Workspace,
	attachment *Attachment,
) (*Workspace, error) {
	if workspace == nil {
		return nil, fmt.Errorf("launched workspace does not exist")
	}
	if attachment == nil {
		return nil, joinWorkspaceAttachRollback(
			fmt.Errorf("launched workspace %s lost its local attachment", workspace.ID),
			rollbackLaunchedAttachment(operations, host, workspace.ID, attachment),
		)
	}
	workspaceID := workspace.ID
	attachmentID := attachment.ID
	fail := func(cause error) (*Workspace, error) {
		return nil, joinWorkspaceAttachRollback(
			cause,
			rollbackLaunchedAttachment(operations, host, workspaceID, attachment),
		)
	}

	if host != "" {
		ready, err := operations.readyRemote(ctx, host, workspace, attachment)
		if err != nil {
			return fail(err)
		}
		if err := validateWorkspaceTransition(workspace, ready); err != nil {
			return fail(err)
		}
		workspace = ready
		if move {
			destination := workspace.Attachments[attachmentID]
			if destination == nil {
				return fail(fmt.Errorf("workspace %s lost destination attachment %s before move", workspaceID, attachmentID))
			}
			moved, err := operations.commitRemote(ctx, host, workspace, destination)
			if err != nil {
				return fail(err)
			}
			if err := validateWorkspaceTransition(workspace, moved); err != nil {
				return fail(err)
			}
			workspace = moved
		}
	} else if move && workspace.PrimaryAttachmentID != attachmentID {
		moved, err := operations.commitLocal(ctx, workspace, attachment)
		if err != nil {
			return fail(err)
		}
		if err := validateWorkspaceTransition(workspace, moved); err != nil {
			return fail(err)
		}
		workspace = moved
	}
	return workspace, nil
}

func rollbackLaunchedAttachment(operations workspaceAttachOperations, host, workspaceID string, attachment *Attachment) error {
	ctx, cancel := context.WithTimeout(context.Background(), workspaceAttachRollbackTimeout)
	defer cancel()
	return operations.rollback(ctx, host, workspaceID, attachment)
}

func rollbackRemoteAttachment(api API, host, workspaceID, attachmentID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), workspaceAttachRollbackTimeout)
	defer cancel()
	if err := api.RemoteCall(ctx, host, "detach_attachment", attachmentRefRequest{
		Workspace: workspaceID, Attachment: attachmentID,
	}, nil); err != nil {
		return fmt.Errorf("detach origin attachment state: %w", err)
	}
	return nil
}

func joinWorkspaceAttachRollback(cause, rollbackErr error) error {
	if rollbackErr == nil {
		return cause
	}
	if cause == nil {
		return fmt.Errorf("workspace attach rollback failed: %w", rollbackErr)
	}
	return errors.Join(cause, fmt.Errorf("workspace attach rollback failed: %w", rollbackErr))
}

func readyRemoteAttachment(ctx context.Context, api API, host string, workspace *Workspace, attachment *Attachment) (*Workspace, error) {
	if attachment == nil {
		return nil, fmt.Errorf("local attachment disappeared before remote readiness")
	}
	var remote Workspace
	err := api.RemoteCall(ctx, host, "update_attachment", attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID,
		TopologyGeneration: attachment.AppliedTopologyGeneration,
		TopologyDigest:     attachment.AppliedTopologyDigest,
		ObservedTopology:   attachment.ObservedTopology,
		Status:             AttachmentReady, Views: attachment.Views,
	}, &remote)
	if err != nil {
		return nil, err
	}
	return api.Workspace(ctx, remote.ID)
}

func commitRemoteMove(ctx context.Context, api API, host string, workspace *Workspace, attachment *Attachment) (*Workspace, error) {
	if attachment == nil {
		return nil, fmt.Errorf("destination attachment does not exist")
	}
	if attachment.AppliedRevision != workspace.Revision {
		ready, err := readyRemoteAttachment(ctx, api, host, workspace, attachment)
		if err != nil {
			return nil, err
		}
		if err := validateWorkspaceTransition(workspace, ready); err != nil {
			return nil, err
		}
		workspace = ready
		attachment = workspace.Attachments[attachment.ID]
		if attachment == nil {
			return nil, fmt.Errorf("workspace %s lost destination attachment before move", workspace.ID)
		}
	}
	var result moveCommitResponse
	if err := api.RemoteCall(ctx, host, "commit_move", moveCommitRequest{
		Workspace: workspace.ID, Destination: attachment.ID, ExpectedRevision: workspace.Revision,
	}, &result); err != nil {
		return nil, err
	}
	return api.Workspace(ctx, workspace.ID)
}

func commitLocalMove(ctx context.Context, api API, workspace *Workspace, attachment *Attachment) (*Workspace, error) {
	if attachment == nil {
		return nil, fmt.Errorf("destination attachment does not exist")
	}
	result, err := api.CommitMove(ctx, moveCommitRequest{
		Workspace: workspace.ID, Destination: attachment.ID, ExpectedRevision: workspace.Revision,
	})
	if err != nil {
		return nil, err
	}
	return result.Workspace, nil
}

func runWorkspaceDetach(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("workspace detach", stderr)
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("workspace detach requires one local workspace reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := NewAPI(paths)
	workspace, err := api.Workspace(ctx, fs.Arg(0))
	if err != nil {
		return 1, err
	}
	node, err := api.Node(ctx)
	if err != nil {
		return 1, err
	}
	var attachments []*Attachment
	for _, attachment := range workspace.SortedAttachments() {
		if attachment.Node.ID == node.ID && strings.HasPrefix(attachment.Endpoint, "unix:") && attachment.Status != AttachmentDetached {
			attachments = append(attachments, attachment)
		}
	}
	if len(attachments) == 0 {
		fmt.Fprintln(stdout, "already detached")
		return 0, nil
	}
	var firstErr error
	operations := liveWorkspaceDetachOperations{api: api}
	for _, attachment := range attachments {
		if err := detachWorkspaceAttachment(ctx, operations, workspace.RemoteHost, workspace.ID, attachment); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return 1, firstErr
	}
	fmt.Fprintln(stdout, workspace.Name)
	return 0, nil
}

type workspaceDetachOperations interface {
	detachLocal(context.Context, string, *Attachment) error
	detachRemote(context.Context, string, string, *Attachment) error
}

type liveWorkspaceDetachOperations struct {
	api API
}

func (o liveWorkspaceDetachOperations) detachLocal(ctx context.Context, workspaceID string, attachment *Attachment) error {
	return closeAndDetachLocal(ctx, o.api, workspaceID, attachment)
}

func (o liveWorkspaceDetachOperations) detachRemote(ctx context.Context, host, workspaceID string, attachment *Attachment) error {
	if host == "" {
		return nil
	}
	return o.api.RemoteCall(ctx, host, "detach_attachment", attachmentRefRequest{
		Workspace: workspaceID, Attachment: attachment.ID,
	}, nil)
}

func detachWorkspaceAttachment(
	ctx context.Context,
	operations workspaceDetachOperations,
	host, workspaceID string,
	attachment *Attachment,
) error {
	localErr := operations.detachLocal(ctx, workspaceID, attachment)
	if localErr != nil {
		var closeErr *kittyCloseError
		if !errors.As(localErr, &closeErr) {
			return localErr
		}
	}
	remoteErr := operations.detachRemote(ctx, host, workspaceID, attachment)
	return errors.Join(localErr, remoteErr)
}

func runWorkspaceForget(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("workspace forget", stderr)
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("workspace forget requires one cached remote workspace reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := NewAPI(paths)
	host, ref := splitWorkspaceRef(fs.Arg(0))
	if ref == "" {
		return 2, fmt.Errorf("workspace forget requires a workspace reference after the SSH host alias")
	}
	var workspace *Workspace
	var err error
	if host == "" {
		workspace, err = api.Workspace(ctx, ref)
	} else {
		if err = validateSSHHost(host); err == nil {
			var workspaces []*Workspace
			workspaces, err = api.Workspaces(ctx)
			if err == nil {
				workspace, err = resolveCachedWorkspaceFromCopies(workspaces, host, ref)
			}
		}
	}
	if err != nil {
		return 1, err
	}
	if workspace.RemoteHost == "" {
		return 1, fmt.Errorf("workspace %q is authoritative on this host; use workspace kill to destroy it", workspace.Name)
	}
	if err := api.DeleteWorkspace(ctx, workspace.ID); err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", workspace.ID, workspace.Name, workspace.RemoteHost)
	return 0, nil
}

func resolveCachedWorkspaceFromCopies(workspaces []*Workspace, host, ref string) (*Workspace, error) {
	var found *Workspace
	for _, workspace := range workspaces {
		if workspace == nil || workspace.RemoteHost != host {
			continue
		}
		if workspace.ID == ref {
			return workspace, nil
		}
		if workspace.Name != ref && !strings.HasPrefix(workspace.ID, ref) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("cached workspace reference %q from %s is ambiguous", ref, host)
		}
		found = workspace
	}
	if found == nil {
		return nil, fmt.Errorf("unknown cached workspace %q from %s", ref, host)
	}
	return found, nil
}

func runWorkspaceRename(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("workspace rename", stderr)
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 2 {
		return 2, fmt.Errorf("workspace rename requires a workspace reference and a new name")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := NewAPI(paths)
	host, _ := splitWorkspaceRef(fs.Arg(0))
	workspace, err := resolveWorkspace(ctx, api, fs.Arg(0))
	if err != nil {
		return 1, err
	}
	request := renameWorkspaceRequest{Workspace: workspace.ID, Name: fs.Arg(1), ExpectedRevision: workspace.Revision}
	if host == "" {
		workspace, err = api.RenameWorkspace(ctx, request)
	} else {
		var renamed Workspace
		err = api.RemoteCall(ctx, host, "rename_workspace", request, &renamed)
		workspace = &renamed
	}
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "%s\t%s\n", workspace.ID, workspace.Name)
	return 0, nil
}

func runWorkspaceKill(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("workspace kill", stderr)
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("workspace kill requires one workspace reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	api := NewAPI(paths)
	host, _ := splitWorkspaceRef(fs.Arg(0))
	workspace, err := resolveWorkspace(ctx, api, fs.Arg(0))
	if err != nil {
		return 1, err
	}
	killCtx, killCancel := context.WithTimeout(ctx, 15*time.Second)
	defer killCancel()
	api.client.Timeout = 15 * time.Second
	var response workspaceDeletionResponse
	if host == "" {
		response, err = api.KillWorkspace(killCtx, workspace.ID)
	} else {
		err = api.RemoteCall(killCtx, host, "kill_workspace", killWorkspaceRequest{WorkspaceID: workspace.ID}, &response)
	}
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "%s\t%s\n", response.DeletedWorkspaceID, response.Name)
	return 0, nil
}

func closeAndDetachLocal(ctx context.Context, api API, workspaceID string, attachment *Attachment) error {
	if workspaceID == "" {
		return fmt.Errorf("workspace id is required")
	}
	if attachment == nil {
		return fmt.Errorf("local attachment does not exist")
	}
	if _, err := api.DetachAttachment(ctx, workspaceID, attachment.ID); err != nil {
		return err
	}
	if err := closeLocalWorkspaceView(ctx, workspaceID, attachment); err != nil {
		return &kittyCloseError{err: err}
	}
	return nil
}

func closeLocalWorkspaceView(ctx context.Context, workspaceID string, attachment *Attachment) error {
	if workspaceID == "" {
		return fmt.Errorf("workspace id is required")
	}
	if attachment == nil {
		return fmt.Errorf("local attachment does not exist")
	}
	if !strings.HasPrefix(attachment.Endpoint, "unix:") {
		return nil
	}
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	kitty := KittyClient{Runner: ExecRunner{}, Command: cfg.Kitty.KittenCommand}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return kitty.CloseWorkspace(callCtx, attachment.Endpoint, workspaceID)
}

type kittyCloseError struct{ err error }

func (e *kittyCloseError) Error() string {
	return "Kitty was unreachable; attachment was still detached: " + e.err.Error()
}
func (e *kittyCloseError) Unwrap() error { return e.err }

func runWorkspaceFocus(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("workspace focus", stderr)
	pane := fs.String("pane", "", "pane reference")
	if err := parseInterspersed(fs, args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("workspace focus requires one local workspace reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	api := NewAPI(paths)
	workspace, err := api.Workspace(ctx, fs.Arg(0))
	if err != nil {
		return 1, err
	}
	if workspace.DeletionPending {
		return 1, fmt.Errorf("workspace %q is being deleted", workspace.Name)
	}
	if *pane != "" {
		resolved, err := resolvePaneFromCopy(workspace, *pane)
		if err != nil {
			return 1, err
		}
		*pane = resolved.ID
	}
	node, err := api.Node(ctx)
	if err != nil {
		return 1, err
	}
	attachment := focusableLocalAttachment(workspace, node.ID, *pane)
	if attachment == nil {
		return 1, fmt.Errorf("workspace has no attached pane on this node")
	}
	if err := focusAttachment(ctx, paths, workspace, attachment, *pane); err != nil {
		return 1, err
	}
	fmt.Fprintln(stdout, workspace.Name)
	return 0, nil
}

func focusAttachment(ctx context.Context, paths Paths, workspace *Workspace, attachment *Attachment, paneRef string) error {
	if attachment == nil || attachment.Endpoint == "" {
		return fmt.Errorf("workspace has no local Kitty attachment")
	}
	paneID := paneRef
	if paneRef != "" {
		pane, err := resolvePaneFromCopy(workspace, paneRef)
		if err != nil {
			return err
		}
		paneID = pane.ID
	}
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	runner := ExecRunner{}
	kitty := KittyClient{Runner: runner, Command: cfg.Kitty.KittenCommand}
	if err := kitty.FocusPane(ctx, attachment.Endpoint, workspace.ID, paneID); err != nil {
		return err
	}
	if err := focusSwayWindow(ctx, runner, cfg.Focus.SwayCommand, attachment.PID); err != nil {
		return err
	}
	api := NewAPI(paths)
	if workspace.RemoteHost != "" {
		_ = api.RemoteCall(ctx, workspace.RemoteHost, "seen", workspacePaneRequest{Workspace: workspace.ID, Pane: paneID}, nil)
	} else {
		_, _ = api.Seen(ctx, workspace.ID, paneID)
	}
	return nil
}

func runWorkspaceSeen(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("workspace seen", stderr)
	pane := fs.String("pane", "", "pane reference")
	if err := parseInterspersed(fs, args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("workspace seen requires one workspace reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := NewAPI(paths)
	host, ref := splitWorkspaceRef(fs.Arg(0))
	var workspace Workspace
	var err error
	if host == "" {
		result, callErr := api.Seen(ctx, ref, *pane)
		err = callErr
		if result != nil {
			workspace = *result
		}
	} else {
		err = api.RemoteCall(ctx, host, "seen", workspacePaneRequest{Workspace: ref, Pane: *pane}, &workspace)
	}
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "%s\t%s\n", workspace.Attention, workspace.Name)
	return 0, nil
}

func resolveWorkspace(ctx context.Context, api API, ref string) (*Workspace, error) {
	host, localRef := splitWorkspaceRef(ref)
	if host == "" {
		return api.Workspace(ctx, localRef)
	}
	var workspace Workspace
	if err := api.RemoteCall(ctx, host, "get", refRequest{Ref: localRef}, &workspace); err != nil {
		return nil, err
	}
	return &workspace, nil
}

func splitWorkspaceRef(ref string) (host, workspace string) {
	if at := strings.IndexByte(ref, ':'); at > 0 {
		return ref[:at], ref[at+1:]
	}
	return "", ref
}

func resolvePaneFromCopy(workspace *Workspace, ref string) (*Pane, error) {
	return resolvePaneLocked(workspace, ref)
}

func writeWorkspaceTable(w io.Writer, workspaces []*Workspace) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATE\tNAME\tID\tORIGIN\tREV\tPANES\tATTACHMENTS")
	for _, workspace := range workspaces {
		origin := workspace.Origin.Name
		if workspace.RemoteHost != "" {
			origin = workspace.RemoteHost
		}
		state := string(workspace.Attention)
		if len(workspace.Attachments) == 0 {
			// Created, never attached anywhere: waiting for its first view.
			state = "dormant"
		}
		if workspace.DeletionPending {
			state = "deleting"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\n", state, workspace.Name, shortID(workspace.ID), origin, workspace.Revision, len(workspace.Panes), len(workspace.Attachments))
	}
	_ = tw.Flush()
}

func writeWorkspaceDetail(w io.Writer, workspace *Workspace) {
	fmt.Fprintf(w, "workspace=%s\nname=%s\norigin=%s\nrevision=%d\nattention=%s\nprimary_attachment=%s\n",
		workspace.ID, workspace.Name, workspace.Origin.Name, workspace.Revision, workspace.Attention, workspace.PrimaryAttachmentID)
	fmt.Fprintf(w, "topology_generation=%d\ntopology_digest=%s\ntopology_panes=%d\ntopology_pending=%d\n",
		workspace.Topology.Generation, shortDigest(workspace.Topology.Digest),
		len(desiredPaneIDs(workspace)), pendingTopologyPaneCount(workspace))
	if len(workspace.Attachments) == 0 {
		fmt.Fprintln(w, "dormant=true")
	}
	if workspace.DeletionPending {
		fmt.Fprintf(w, "deletion_pending=true\ndeletion_error=%s\n", workspace.DeletionError)
	}
	if legacy := workspace.PIVBProvider; legacy != nil {
		fmt.Fprintf(w, "credential_migration_conflict=legacy_pivb source=%s bundle=%s owner=%s generation=%d serial=%d key=%s\n",
			legacy.Source, legacy.Bundle, legacy.OwnerNodeID, legacy.Generation, legacy.Manifest.Card.Serial, legacy.Manifest.Card.KeyID)
	}
	for _, pane := range workspace.SortedPanes() {
		fmt.Fprintf(w, "pane[%s]=%s backend=%s state=%s evidence=%s/%s\n", shortID(pane.ID), pane.Title, pane.Backend.Ref, pane.State, pane.Evidence.Source, pane.Evidence.Event)
		if pane.BackendDead {
			fmt.Fprintf(w, "pane_backend[%s]=dead error=%s\n", shortID(pane.ID), pane.BackendError)
		}
		if pane.Retiring() {
			fmt.Fprintf(w, "pane_removal[%s]=pending error=%s\n", shortID(pane.ID), pane.RemovalError)
		}
		if pane.Proposed() {
			fmt.Fprintf(w, "pane_topology[%s]=proposed since=%s endpoint=%s window=%d\n",
				shortID(pane.ID), pane.PhaseAt.Format(time.RFC3339),
				pane.Admission.Endpoint, pane.Admission.WindowID)
		}
		// Sorted and keyed, so two inspect runs of the same state can be diffed,
		// and "pending" is printed at all: a reservation that was never attempted
		// used to produce no output whatsoever.
		for _, record := range pane.SortedNotifications() {
			status := notificationRecordStatus(record)
			if status == "sent" {
				continue
			}
			fmt.Fprintf(w, "notification[%s/%s]=%s key=%s attempts=%d next_retry=%s error=%s\n",
				shortID(pane.ID), record.Channel, status, record.Key, record.Attempts,
				formatOptionalTime(record.NextRetryAt), record.LastError)
		}
	}
	for _, attachment := range workspace.SortedAttachments() {
		fmt.Fprintf(w, "attachment[%s]=%s node=%s transport=%s status=%s revision=%d topology=%d/%s reconcile=%s target=%d\n",
			shortID(attachment.ID), attachment.Role, attachment.Node.Name, attachment.Transport.Kind,
			attachment.Status, attachment.AppliedRevision, attachment.AppliedTopologyGeneration,
			shortDigest(attachment.AppliedTopologyDigest), attachment.ReconcileStatus,
			attachment.ReconcileTargetGeneration)
		if attachment.LastError != "" {
			fmt.Fprintf(w, "attachment_error[%s]=%s\n", shortID(attachment.ID), attachment.LastError)
		}
	}
}

func pendingTopologyPaneCount(workspace *Workspace) int {
	count := 0
	for _, pane := range workspace.Panes {
		if !pane.Admitted() {
			count++
		}
	}
	return count
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func localPaneCommandEnvironment(workspaceID, paneID string) []string {
	environment := append([]string(nil), os.Environ()...)
	environment = replaceEnvironmentValue(environment, "ZKA_WORKSPACE_ID", workspaceID)
	environment = replaceEnvironmentValue(environment, "ZKA_PANE_ID", paneID)
	// Ambient credentials are removed by managedPaneCommandEnvironment when a
	// backend is created. Keeping this helper identity-only also prevents a zka
	// command launched from another managed pane from leaking its marker.
	return removeEnvironmentValue(environment, "ZKA_CREDENTIAL_ENVIRONMENT_VERSION")
}

func managedPaneCommandEnvironment(cfg Config, paths Paths, workspaceID, paneID string, creating bool) []string {
	environment := localPaneCommandEnvironment(workspaceID, paneID)
	if creating && cfg.credentialsEnabled() {
		environment = removeEnvironmentValue(environment, "SSH_AUTH_SOCK")
		environment = removeEnvironmentValue(environment, "GNUPGHOME")
		for _, name := range []string{"PIVB_ATTACHMENT_MODE", "PIVB_ROUTE_SOCKET", "PIVB_ATTACHMENT_PROTOCOL"} {
			environment = removeEnvironmentValue(environment, name)
		}
		sshEnabled, openPGPEnabled, pivbEnabled := false, false, false
		for _, bundle := range cfg.Credentials.Bundles {
			sshEnabled = sshEnabled || bundle.SSHAgent.Enable
			openPGPEnabled = openPGPEnabled || bundle.OpenPGP.Enable
			pivbEnabled = pivbEnabled || bundle.PIVB.Enable
		}
		if sshEnabled {
			environment = replaceEnvironmentValue(environment, "SSH_AUTH_SOCK", agentRelaySocketPath(paths.AgentDir, workspaceID))
		}
		if openPGPEnabled {
			if home, err := credentialOpenPGPHome(paths, workspaceID); err == nil {
				environment = replaceEnvironmentValue(environment, "GNUPGHOME", home)
			}
		}
		if pivbEnabled {
			environment = replaceEnvironmentValue(environment, "PIVB_ATTACHMENT_MODE", "route-required")
			environment = replaceEnvironmentValue(environment, "PIVB_ROUTE_SOCKET", pivbRelaySocketPath(paths, workspaceID))
			environment = replaceEnvironmentValue(environment, "PIVB_ATTACHMENT_PROTOCOL", strconv.Itoa(managedPIVBAttachmentProtocol(cfg)))
		}
		environment = replaceEnvironmentValue(environment, "ZKA_CREDENTIAL_ENVIRONMENT_VERSION", strconv.Itoa(credentialEnvironmentVersionForConfig(cfg)))
	}
	return environment
}

func remotePaneCommandEnvironment(cfg Config, paths Paths, workspaceID, paneID string, creating bool) []string {
	return managedPaneCommandEnvironment(cfg, paths, workspaceID, paneID, creating)
}

func localPaneBackendCommand(cfg Config, paths Paths, prepared preparePaneResponse) *exec.Cmd {
	return paneBackendCommand(cfg, prepared,
		managedPaneCommandEnvironment(cfg, paths, prepared.Workspace.ID, prepared.Pane.ID, prepared.Create))
}

func remotePaneBackendCommand(cfg Config, paths Paths, prepared preparePaneResponse) *exec.Cmd {
	return paneBackendCommand(cfg, prepared,
		remotePaneCommandEnvironment(cfg, paths, prepared.Workspace.ID, prepared.Pane.ID, prepared.Create))
}

func paneBackendCommand(cfg Config, prepared preparePaneResponse, environment []string) *exec.Cmd {
	args := []string{"attach", prepared.Pane.Backend.Ref}
	if prepared.Create {
		args = append(args, "zka", "pane-host", "--workspace", prepared.Workspace.ID, "--pane", prepared.Pane.ID, "--")
		args = append(args, prepared.Workspace.Shell...)
	}
	cmd := exec.Command(cfg.ZMX.Command, args...)
	cmd.Env = environment
	// exec fails the whole launch when Dir does not exist, so a directory that
	// has since been removed must become "no directory" rather than a dead pane.
	if prepared.Create && usableDirectory(prepared.Pane.CWD) {
		cmd.Dir = prepared.Pane.CWD
	}
	return cmd
}

func replaceEnvironmentValue(environment []string, name, value string) []string {
	environment = removeEnvironmentValue(environment, name)
	return append(environment, name+"="+value)
}

func removeEnvironmentValue(environment []string, name string) []string {
	prefix := name + "="
	result := environment[:0]
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return result
}

func runPane(args []string, paths Paths, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("pane", stderr)
	workspaceRef := fs.String("workspace", "", "workspace id")
	paneRef := fs.String("pane", "", "existing pane id")
	sourceWindow := fs.String("source-window", "", "Kitty window id this pane was opened from")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *workspaceRef == "" || fs.NArg() != 0 {
		return 2, fmt.Errorf("pane requires --workspace and optional --pane")
	}
	api := NewAPI(paths)
	cwd, _ := os.Getwd()
	// The Kitty window identity is read before preparing so the daemon can
	// record which window this pane belongs to. That provenance is what lets
	// admission be decided from evidence instead of from a timer.
	windowID, parseErr := strconv.ParseInt(os.Getenv("KITTY_WINDOW_ID"), 10, 64)
	endpoint := os.Getenv("KITTY_LISTEN_ON")
	if endpoint == "" || parseErr != nil || windowID <= 0 {
		return 1, fmt.Errorf("managed Kitty endpoint and window id are required")
	}
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	kitty := KittyClient{Runner: ExecRunner{}, Command: cfg.Kitty.KittenCommand}
	inheritCtx, inheritCancel := context.WithTimeout(context.Background(), 2*time.Second)
	inheritFrom := kitty.PaneForWindow(inheritCtx, endpoint, *workspaceRef, sourceWindowID(*sourceWindow))
	inheritCancel()
	prepared, err := api.PreparePane(context.Background(), workspacePaneRequest{
		Workspace: *workspaceRef, Pane: *paneRef, CWD: cwd,
		Endpoint: endpoint, WindowID: windowID, InheritFromPane: inheritFrom,
	})
	if err != nil {
		return 1, err
	}
	if prepared.Create {
		capabilityCtx, capabilityCancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = ensurePaneVisiblePIVBCapability(capabilityCtx, cfg)
		capabilityCancel()
		if err != nil {
			return 1, err
		}
	}
	identityCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	identityErr := kitty.SetIdentity(identityCtx, endpoint, windowID, prepared.Workspace.ID, prepared.Pane.ID)
	cancel()
	if identityErr != nil {
		return 1, fmt.Errorf("tag Kitty pane: %w", identityErr)
	}
	if !prepared.Create {
		if prepared.Pane.BackendDead {
			if paneBackendExitedCleanly(prepared.Pane) {
				return 0, nil
			}
			return runLocalDeadPane(api, kitty, endpoint, windowID, prepared.Workspace, prepared.Pane,
				paneBackendError(prepared.Pane), stdin, stdout)
		}
		exists, err := zmxSessionExists(context.Background(), cfg.ZMX.Command, prepared.Pane.Backend.Ref)
		if err != nil {
			return 1, fmt.Errorf("query zmx sessions: %w", err)
		}
		if !exists {
			return runLocalDeadPane(api, kitty, endpoint, windowID, prepared.Workspace, prepared.Pane,
				fmt.Errorf("zmx session %q no longer exists", prepared.Pane.Backend.Ref), stdin, stdout)
		}
	}
	cmd := localPaneBackendCommand(cfg, paths, prepared)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Start(); err != nil {
		return runLocalDeadPane(api, kitty, endpoint, windowID, prepared.Workspace, prepared.Pane, err, stdin, stdout)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 8*time.Second)
	runErr, exited, readyErr := waitForLocalPaneReady(readyCtx, api, prepared.Workspace.ID, prepared.Pane.ID, done)
	readyCancel()
	if readyErr != nil {
		_ = cmd.Process.Kill()
		if !exited {
			runErr = <-done
		}
		return finishLocalPaneAttach(api, cfg, kitty, endpoint, windowID, prepared.Workspace, prepared.Pane,
			fmt.Errorf("wait for zmx attachment readiness: %w", readyErr), stdin, stdout)
	}
	if exited {
		return finishLocalPaneAttach(api, cfg, kitty, endpoint, windowID, prepared.Workspace, prepared.Pane, runErr, stdin, stdout)
	}
	readyCtx, readyCancel = context.WithTimeout(context.Background(), 2*time.Second)
	readyErr = kitty.SetPaneReady(readyCtx, endpoint, windowID, true)
	readyCancel()
	if readyErr != nil {
		_ = cmd.Process.Kill()
		<-done
		return 1, fmt.Errorf("mark Kitty pane ready: %w", readyErr)
	}
	// Ask the daemon to commit this pane into the desired topology now that the
	// window is tagged and ready. Advisory only: the background admission
	// worker reaches the same state, so a failure here must not fail the pane.
	admitCtx, admitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, _ = api.AdmitPane(admitCtx, prepared.Workspace.ID, prepared.Pane.ID, endpoint)
	admitCancel()
	runErr = <-done
	return finishLocalPaneAttach(api, cfg, kitty, endpoint, windowID, prepared.Workspace, prepared.Pane, runErr, stdin, stdout)
}

func waitForLocalPaneReady(ctx context.Context, api API, workspaceID, paneID string, done <-chan error) (error, bool, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	started := time.Now()
	var lastErr error
	for {
		select {
		case runErr := <-done:
			return runErr, true, nil
		case <-ctx.Done():
			if lastErr != nil {
				return nil, false, lastErr
			}
			return nil, false, ctx.Err()
		case <-ticker.C:
			workspace, err := api.Workspace(ctx, workspaceID)
			if err != nil {
				lastErr = err
				continue
			}
			pane := workspace.Panes[paneID]
			if pane != nil && pane.BackendReady && time.Since(started) >= 100*time.Millisecond {
				select {
				case runErr := <-done:
					return runErr, true, nil
				default:
					return nil, false, nil
				}
			}
		}
	}
}

func finishLocalPaneAttach(api API, cfg Config, kitty KittyClient, endpoint string, windowID int64, workspace *Workspace, pane *Pane, runErr error, stdin io.Reader, stdout io.Writer) (int, error) {
	if recorded := recordedDeadBackend(api, workspace.ID, pane.ID); recorded != nil {
		if paneBackendExitedCleanly(recorded) {
			return 0, nil
		}
		return runLocalDeadPane(api, kitty, endpoint, windowID, workspace, recorded, paneBackendError(recorded), stdin, stdout)
	}
	exists, queryErr := zmxSessionExists(context.Background(), cfg.ZMX.Command, pane.Backend.Ref)
	if queryErr == nil && exists {
		return processExitCode(runErr), nil
	}
	if queryErr != nil {
		return 1, fmt.Errorf("query zmx session after attachment exited: %w", queryErr)
	}
	if runErr == nil {
		runErr = fmt.Errorf("zmx session %q exited", pane.Backend.Ref)
	}
	return runLocalDeadPane(api, kitty, endpoint, windowID, workspace, pane, runErr, stdin, stdout)
}

func zmxSessionExists(ctx context.Context, command, name string) (bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, _, err := (ExecRunner{}).Run(callCtx, command, "list", "--short")
	if err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 0 && fields[0] == name {
			return true, nil
		}
	}
	return false, scanner.Err()
}

func recordedDeadBackend(api API, workspaceID, paneID string) *Pane {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	workspace, err := api.Workspace(ctx, workspaceID)
	if err != nil {
		return nil
	}
	pane := workspace.Panes[paneID]
	if pane == nil || !pane.BackendDead {
		return nil
	}
	return pane
}

func paneBackendExitedCleanly(pane *Pane) bool {
	return pane != nil && pane.BackendDead && pane.Evidence.Event == "process_exit" &&
		pane.Process.ExitCode != nil && *pane.Process.ExitCode == 0
}

func paneBackendError(pane *Pane) error {
	if pane.BackendError != "" {
		return errors.New(pane.BackendError)
	}
	return fmt.Errorf("zmx backend %q is dead", pane.Backend.Ref)
}

func runLocalDeadPane(api API, kitty KittyClient, endpoint string, windowID int64, workspace *Workspace, pane *Pane, cause error, stdin io.Reader, stdout io.Writer) (int, error) {
	if !pane.BackendDead {
		_, _ = api.Event(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "backend_error", Source: "zmx", Detail: cause.Error()})
	}
	readyCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := kitty.SetPaneReady(readyCtx, endpoint, windowID, true)
	cancel()
	if err != nil {
		return 1, fmt.Errorf("mark dead Kitty pane ready: %w", err)
	}
	if err := writeDeadPaneMessage(stdout, workspace, pane, cause); err != nil {
		return 1, err
	}
	if err := waitForDeadPaneDismiss(stdin); err != nil {
		return 1, err
	}
	return 0, nil
}

func writeDeadPaneMessage(w io.Writer, workspace *Workspace, pane *Pane, cause error) error {
	detail := "zmx backend is unavailable"
	if cause != nil {
		detail = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(cause.Error()))
	}
	_, err := fmt.Fprintf(w, "\x1b[2J\x1b[H\n  zka: zmx backend is dead\n\n  workspace: %s\n  pane:      %s\n  backend:   %s\n  reason:    %s\n\n  Press Ctrl-C to remove this pane.\n", workspace.Name, shortID(pane.ID), pane.Backend.Ref, detail)
	return err
}

func waitForDeadPaneDismiss(stdin io.Reader) error {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	input := make(chan error, 1)
	go func() {
		buffer := make([]byte, 64)
		for {
			n, err := stdin.Read(buffer)
			for _, value := range buffer[:n] {
				if value == 3 {
					input <- nil
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				input <- err
				return
			}
		}
	}()
	select {
	case <-interrupt:
		return nil
	case err := <-input:
		return err
	}
}

// sourceWindowID parses Kitty's @active-kitty-window-id substitution. The flag
// is a string, and a value that will not parse is ignored rather than
// rejected: when there is no active window Kitty leaves the placeholder
// literal, and an int flag would make flag parsing fail and kill the pane.
func sourceWindowID(value string) int64 {
	windowID, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || windowID <= 0 {
		return 0
	}
	return windowID
}

// newRemotePaneAllocation builds the origin-side allocation for a pane opened
// from a replica's Kitty. It deliberately carries no CWD: the replica's own
// directory is a path on the wrong machine, and storing it in an origin pane
// is how remote panes ended up in arbitrary directories. The origin resolves
// the real directory from the source pane instead.
func newRemotePaneAllocation(workspaceID, attachmentID, allocationID, sourcePaneID string) allocatePaneRequest {
	return allocatePaneRequest{
		Workspace:       workspaceID,
		Key:             attachmentID + ":" + allocationID,
		InheritFromPane: sourcePaneID,
	}
}

func runPaneHost(args []string, paths Paths, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("pane-host", stderr)
	workspaceID := fs.String("workspace", "", "workspace id")
	paneID := fs.String("pane", "", "pane id")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	command := fs.Args()
	if *workspaceID == "" || *paneID == "" || len(command) == 0 {
		return 2, fmt.Errorf("pane-host requires --workspace, --pane, and a command after --")
	}
	api := NewAPI(paths)
	credentialVersion, _ := strconv.Atoi(os.Getenv("ZKA_CREDENTIAL_ENVIRONMENT_VERSION"))
	workspace, err := api.PaneProcessEvent(context.Background(), Event{
		WorkspaceID: *workspaceID, PaneID: *paneID, Kind: "process_started", Source: "pane-host",
		PID: os.Getpid(), CredentialEnvironmentVersion: credentialVersion,
	})
	if err != nil {
		return 1, err
	}
	pane := workspace.Panes[*paneID]
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if pane != nil && usableDirectory(pane.CWD) {
		cmd.Dir = pane.CWD
	}
	cmd.Env = append(os.Environ(), "ZKA_WORKSPACE_ID="+*workspaceID, "ZKA_PANE_ID="+*paneID)
	err = cmd.Run()
	exitCode := processExitCode(err)
	_, eventErr := api.PaneProcessEvent(context.Background(), Event{WorkspaceID: *workspaceID, PaneID: *paneID, Kind: "process_exit", Source: "pane-host", ExitCode: &exitCode, Detail: fmt.Sprintf("exit code %d", exitCode)})
	if eventErr != nil {
		fmt.Fprintf(stderr, "zka: report process exit: %v\n", eventErr)
	}
	if exitCode != 0 {
		return exitCode, nil
	}
	return 0, nil
}

func runRemoteAttach(args []string, paths Paths, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("remote-attach", stderr)
	workspaceRef := fs.String("workspace", "", "workspace id")
	paneRef := fs.String("pane", "", "pane id")
	attachmentID := fs.String("attachment", "", "destination attachment id")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *workspaceRef == "" || *paneRef == "" || *attachmentID == "" || fs.NArg() != 0 {
		return 2, fmt.Errorf("remote-attach requires --workspace, --pane, and --attachment")
	}
	ctx := context.Background()
	api := NewAPI(paths)
	prepared, err := api.PreparePane(ctx, workspacePaneRequest{Workspace: *workspaceRef, Pane: *paneRef})
	if err != nil {
		return 1, err
	}
	workspace, pane := prepared.Workspace, prepared.Pane
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	if prepared.Create {
		capabilityCtx, capabilityCancel := context.WithTimeout(ctx, 5*time.Second)
		err = ensurePaneVisiblePIVBCapability(capabilityCtx, cfg)
		capabilityCancel()
		if err != nil {
			return 1, err
		}
	}
	if !prepared.Create {
		if pane.BackendDead {
			if paneBackendExitedCleanly(pane) {
				clearRemotePaneHeartbeat(api, workspace.ID, *attachmentID, pane.ID)
				return 0, nil
			}
			return runRemoteDeadPane(api, workspace, pane, *attachmentID, paneBackendError(pane), stdin, stdout)
		}
		exists, err := zmxSessionExists(ctx, cfg.ZMX.Command, pane.Backend.Ref)
		if err != nil {
			return 1, err
		}
		if !exists {
			return runRemoteDeadPane(api, workspace, pane, *attachmentID,
				fmt.Errorf("remote zmx session %q is missing", pane.Backend.Ref), stdin, stdout)
		}
	}
	cmd := remotePaneBackendCommand(cfg, paths, prepared)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Start(); err != nil {
		return runRemoteDeadPane(api, workspace, pane, *attachmentID, err, stdin, stdout)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	readyCtx, readyCancel := context.WithTimeout(ctx, 8*time.Second)
	runErr, exited, readyErr := waitForLocalPaneReady(readyCtx, api, workspace.ID, pane.ID, done)
	readyCancel()
	if readyErr != nil {
		_ = cmd.Process.Kill()
		if !exited {
			runErr = <-done
		}
		exists, queryErr := zmxSessionExists(ctx, cfg.ZMX.Command, pane.Backend.Ref)
		if queryErr == nil && !exists {
			return runRemoteDeadPane(api, workspace, pane, *attachmentID,
				fmt.Errorf("wait for remote zmx client readiness: %w", readyErr), stdin, stdout)
		}
		return 1, fmt.Errorf("wait for remote zmx client readiness: %w", readyErr)
	}
	if exited {
		return finishRemotePaneAttach(api, cfg, workspace, pane, *attachmentID, runErr, stdin, stdout)
	}
	heartbeat := attachmentPaneReadyRequest{
		Workspace: workspace.ID, Attachment: *attachmentID, Pane: pane.ID, Ready: true,
	}
	heartbeatCtx, heartbeatCancel := context.WithTimeout(ctx, 2*time.Second)
	_, heartbeatErr := api.SetAttachmentPaneReady(heartbeatCtx, heartbeat)
	heartbeatCancel()
	if heartbeatErr != nil {
		_ = cmd.Process.Kill()
		<-done
		return 1, fmt.Errorf("publish remote pane readiness: %w", heartbeatErr)
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case runErr = <-done:
			return finishRemotePaneAttach(api, cfg, workspace, pane, *attachmentID, runErr, stdin, stdout)
		case <-ticker.C:
			heartbeatCtx, heartbeatCancel := context.WithTimeout(ctx, time.Second)
			_, _ = api.SetAttachmentPaneReady(heartbeatCtx, heartbeat)
			heartbeatCancel()
		}
	}
}

func finishRemotePaneAttach(api API, cfg Config, workspace *Workspace, pane *Pane, attachmentID string, runErr error, stdin io.Reader, stdout io.Writer) (int, error) {
	if recorded := recordedDeadBackend(api, workspace.ID, pane.ID); recorded != nil {
		if paneBackendExitedCleanly(recorded) {
			clearRemotePaneHeartbeat(api, workspace.ID, attachmentID, pane.ID)
			return 0, nil
		}
		return runRemoteDeadPane(api, workspace, recorded, attachmentID, paneBackendError(recorded), stdin, stdout)
	}
	exists, queryErr := zmxSessionExists(context.Background(), cfg.ZMX.Command, pane.Backend.Ref)
	if queryErr != nil {
		clearRemotePaneHeartbeat(api, workspace.ID, attachmentID, pane.ID)
		return 1, fmt.Errorf("query remote zmx session after attachment exited: %w", queryErr)
	}
	if exists {
		clearRemotePaneHeartbeat(api, workspace.ID, attachmentID, pane.ID)
		return processExitCode(runErr), nil
	}
	if runErr == nil {
		runErr = fmt.Errorf("zmx session %q exited", pane.Backend.Ref)
	}
	return runRemoteDeadPane(api, workspace, pane, attachmentID, runErr, stdin, stdout)
}

func runRemoteDeadPane(api API, workspace *Workspace, pane *Pane, attachmentID string, cause error, stdin io.Reader, stdout io.Writer) (int, error) {
	if !pane.BackendDead {
		_, _ = api.Event(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "backend_error", Source: "zmx", Detail: cause.Error()})
	}
	heartbeat := attachmentPaneReadyRequest{Workspace: workspace.ID, Attachment: attachmentID, Pane: pane.ID, Ready: true}
	heartbeatCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, err := api.SetAttachmentPaneReady(heartbeatCtx, heartbeat)
	cancel()
	if err != nil {
		return 1, fmt.Errorf("publish dead remote pane readiness: %w", err)
	}
	defer clearRemotePaneHeartbeat(api, workspace.ID, attachmentID, pane.ID)
	if err := writeDeadPaneMessage(stdout, workspace, pane, cause); err != nil {
		return 1, err
	}
	dismissed := make(chan error, 1)
	go func() { dismissed <- waitForDeadPaneDismiss(stdin) }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-dismissed:
			if err != nil {
				return 1, err
			}
			return 0, nil
		case <-ticker.C:
			heartbeatCtx, heartbeatCancel := context.WithTimeout(context.Background(), time.Second)
			_, _ = api.SetAttachmentPaneReady(heartbeatCtx, heartbeat)
			heartbeatCancel()
		}
	}
}

func clearRemotePaneHeartbeat(api API, workspaceID, attachmentID, paneID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = api.SetAttachmentPaneReady(ctx, attachmentPaneReadyRequest{
		Workspace: workspaceID, Attachment: attachmentID, Pane: paneID, Ready: false,
	})
}

func runRemoteNewPane(args []string, paths Paths, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("remote-new-pane", stderr)
	host := fs.String("origin", "", "origin SSH alias")
	workspaceID := fs.String("workspace", "", "workspace id")
	attachment := fs.String("attachment", "", "attachment id")
	sourceWindow := fs.String("source-window", "", "Kitty window id this pane was opened from")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *host == "" || *workspaceID == "" || *attachment == "" || fs.NArg() != 0 {
		return 2, fmt.Errorf("remote-new-pane requires origin, workspace, and attachment")
	}
	if err := validateSSHHost(*host); err != nil {
		return 2, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := NewAPI(paths)
	windowID, err := strconv.ParseInt(os.Getenv("KITTY_WINDOW_ID"), 10, 64)
	if err != nil || windowID <= 0 {
		return 1, fmt.Errorf("KITTY_WINDOW_ID is unavailable for the new remote pane")
	}
	allocationID, err := randomID()
	if err != nil {
		return 1, err
	}
	endpoint := os.Getenv("KITTY_LISTEN_ON")
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	kitty := KittyClient{Runner: ExecRunner{}, Command: cfg.Kitty.KittenCommand}
	inheritFrom := kitty.PaneForWindow(ctx, endpoint, *workspaceID, sourceWindowID(*sourceWindow))
	var allocated allocatePaneResponse
	if err := api.RemoteCall(ctx, *host, "allocate_pane",
		newRemotePaneAllocation(*workspaceID, *attachment, allocationID, inheritFrom), &allocated); err != nil {
		return 1, err
	}
	if err := kitty.SetIdentity(ctx, endpoint, windowID, allocated.Workspace.ID, allocated.Pane.ID); err != nil {
		return 1, err
	}
	return runRemotePane([]string{"--origin", *host, "--workspace", allocated.Workspace.ID, "--pane", allocated.Pane.ID, "--attachment", *attachment}, paths, stdin, stdout, stderr)
}

func runRemotePane(args []string, paths Paths, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("remote-pane", stderr)
	host := fs.String("origin", "", "origin SSH alias")
	workspace := fs.String("workspace", "", "workspace id")
	pane := fs.String("pane", "", "pane id")
	attachment := fs.String("attachment", "", "attachment id")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *host == "" || *workspace == "" || *pane == "" || *attachment == "" || fs.NArg() != 0 {
		return 2, fmt.Errorf("remote-pane requires origin, workspace, pane, and attachment")
	}
	if err := validateSSHHost(*host); err != nil {
		return 2, err
	}
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	windowID, parseErr := strconv.ParseInt(os.Getenv("KITTY_WINDOW_ID"), 10, 64)
	endpoint := os.Getenv("KITTY_LISTEN_ON")
	if endpoint == "" || parseErr != nil || windowID <= 0 {
		return 1, fmt.Errorf("managed Kitty endpoint and window id are required")
	}
	kitty := KittyClient{Runner: ExecRunner{}, Command: cfg.Kitty.KittenCommand}
	markCtx, markCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err = kitty.SetPaneReady(markCtx, endpoint, windowID, false)
	markCancel()
	if err != nil {
		return 1, fmt.Errorf("mark remote Kitty pane preparing: %w", err)
	}
	api := NewAPI(paths)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backoff := 250 * time.Millisecond
	for {
		sshArgs := append([]string(nil), cfg.SSH.Options...)
		sshArgs = append(sshArgs, "-tt", "--", *host, "exec", "zka", "remote-attach",
			"--workspace", *workspace, "--pane", *pane, "--attachment", *attachment)
		cmd := exec.CommandContext(ctx, cfg.SSH.Command, sshArgs...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		if err := cmd.Start(); err != nil {
			return 1, fmt.Errorf("start SSH pane attachment: %w", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		readyCtx, readyCancel := context.WithTimeout(ctx, 8*time.Second)
		runErr, exited, readyErr := waitForRemotePaneReady(readyCtx, api, *host, *workspace, *attachment, *pane, done)
		readyCancel()
		if readyErr != nil {
			_ = cmd.Process.Kill()
			if !exited {
				runErr = <-done
			}
			return 1, fmt.Errorf("wait for remote zmx attachment readiness: %w", readyErr)
		}
		if !exited {
			markCtx, markCancel := context.WithTimeout(ctx, 2*time.Second)
			readyErr = kitty.SetPaneReady(markCtx, endpoint, windowID, true)
			markCancel()
			if readyErr != nil {
				_ = cmd.Process.Kill()
				<-done
				return 1, fmt.Errorf("mark remote Kitty pane ready: %w", readyErr)
			}
			backoff = 250 * time.Millisecond
			runErr = <-done
			markCtx, markCancel = context.WithTimeout(context.Background(), time.Second)
			_ = kitty.SetPaneReady(markCtx, endpoint, windowID, false)
			markCancel()
		}
		code := processExitCode(runErr)
		if runErr == nil {
			return 0, nil
		}
		if ctx.Err() != nil {
			return 130, nil
		}
		if code != 255 {
			return code, nil
		}
		select {
		case <-ctx.Done():
			return 130, nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func waitForRemotePaneReady(ctx context.Context, api API, host, workspaceID, attachmentID, paneID string, done <-chan error) (error, bool, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	started := time.Now()
	var lastErr error
	for {
		select {
		case runErr := <-done:
			return runErr, true, nil
		case <-ctx.Done():
			if lastErr != nil {
				return nil, false, lastErr
			}
			return nil, false, ctx.Err()
		case <-ticker.C:
			callCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			var readiness paneReadinessResponse
			err := api.RemoteCall(callCtx, host, "pane_readiness", paneReadinessRequest{
				Workspace: workspaceID, Attachment: attachmentID, Pane: paneID,
			}, &readiness)
			cancel()
			if err != nil {
				lastErr = err
				continue
			}
			if (readiness.BackendReady || readiness.BackendDead) && readiness.ClientReady && time.Since(started) >= 150*time.Millisecond {
				select {
				case runErr := <-done:
					return runErr, true, nil
				default:
					return nil, false, nil
				}
			}
		}
	}
}

func runRemoteControlCommand(args []string, paths Paths, stdin io.Reader, stdout io.Writer) (int, error) {
	if len(args) != 0 {
		return 2, fmt.Errorf("remote-control accepts no arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return 0, runRemoteControlMux(ctx, paths, stdin, stdout)
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 127
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	code := exitErr.ExitCode()
	if code < 0 {
		return 129
	}
	return code
}
