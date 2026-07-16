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
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const Version = "0.5.0"

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
	if args[0] == "hook" && (os.Getenv("ZKA_WORKSPACE_ID") == "" || os.Getenv("ZKA_PANE_ID") == "") {
		return hookSuccess(stdout)
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
  kitty       Create a managed Kitty workspace
  workspace   List, inspect, attach, move, detach, rename, kill, focus, or acknowledge workspaces
  doctor      Check local or remote integration
  daemon      Run zkad (normally via systemd --user)

Internal commands: pane, pane-host, remote-pane, remote-attach, remote-control, hook`)
}

func runLauncher(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) != 0 {
		return 2, fmt.Errorf("launch accepts no arguments")
	}
	command := os.Getenv("ZKA_LAUNCHER_COMMAND")
	if command == "" {
		command = siblingExecutable("zka-launch")
	}
	cmd := exec.Command(command)
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := NewAPI(paths)
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
	case "attach":
		return runWorkspaceAttach(args[1:], paths, false, stdout, stderr)
	case "move":
		return runWorkspaceAttach(args[1:], paths, true, stdout, stderr)
	case "detach":
		return runWorkspaceDetach(args[1:], paths, stdout, stderr)
	case "rename":
		return runWorkspaceRename(args[1:], paths, stdout, stderr)
	case "kill":
		return runWorkspaceKill(args[1:], paths, stdout, stderr)
	case "focus":
		return runWorkspaceFocus(args[1:], paths, stdout, stderr)
	case "seen":
		return runWorkspaceSeen(args[1:], paths, stdout, stderr)
	default:
		printWorkspaceUsage(stderr)
		return 2, fmt.Errorf("unknown workspace command %q", args[0])
	}
}

func printWorkspaceUsage(w io.Writer) {
	fmt.Fprintln(w, `usage: zka workspace COMMAND

  list [--origin SSH_ALIAS] [--json]
  inspect [SSH_ALIAS:]REF [--json]
  attach [SSH_ALIAS:]REF [--pane PANE]
  move [SSH_ALIAS:]REF [--pane PANE]
  detach REF
  rename [SSH_ALIAS:]REF NAME
  kill [SSH_ALIAS:]REF
  focus REF [--pane PANE]
  seen REF [--pane PANE]`)
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
		if _, err := resolvePaneFromCopy(workspace, *paneRef); err != nil {
			return 1, err
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
	if attachmentUsable(existing) && existing.AppliedRevision == workspace.Revision {
		if move && workspace.PrimaryAttachmentID != existing.ID {
			if host != "" {
				workspace, err = commitRemoteMove(ctx, api, host, workspace, existing)
			} else {
				workspace, err = commitLocalMove(ctx, api, workspace, existing)
			}
			if err != nil {
				return 1, err
			}
		}
		if err := focusAttachment(ctx, paths, workspace, existing, *paneRef); err != nil {
			return 1, err
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
			if err := closeAndDetachLocal(ctx, paths, api, workspace, existing); err != nil {
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
	if strings.TrimSpace(workspace.Manifest.Session) == "" {
		return 1, fmt.Errorf("workspace %s has no captured Kitty manifest", workspace.Name)
	}
	transport := Transport{Kind: "local"}
	if host != "" {
		transport = Transport{Kind: "ssh", Host: host}
	}
	attachment := Attachment{ID: attachmentID, Node: node, Transport: transport, Endpoint: attachmentEndpoint(paths, attachmentID)}
	if host != "" {
		remoteAttachment := attachment
		remoteAttachment.Endpoint = "ssh:" + node.Name + ":" + attachment.ID
		if err := api.RemoteCall(ctx, host, "register_attachment", attachmentRequest{Workspace: workspace.ID, Attachment: remoteAttachment}, new(Attachment)); err != nil {
			return 1, err
		}
	}
	session, err := RenderAttachmentSession(workspace, transport, attachmentID)
	if err != nil {
		return 1, err
	}
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	attached, err := launchManagedKitty(ctx, paths, cfg, api, launchAttachmentOptions{Workspace: workspace, Attachment: attachment, Session: session})
	if err != nil {
		if host != "" {
			_ = api.RemoteCall(context.Background(), host, "detach_attachment", attachmentRefRequest{Workspace: workspace.ID, Attachment: attachmentID}, nil)
		}
		return 1, err
	}
	workspace = attached
	localAttachment := workspace.Attachments[attachmentID]
	if host != "" {
		workspace, err = readyRemoteAttachment(ctx, api, host, workspace, localAttachment)
		if err != nil {
			_ = closeAndDetachLocal(context.Background(), paths, api, workspace, localAttachment)
			_ = api.RemoteCall(context.Background(), host, "detach_attachment", attachmentRefRequest{Workspace: workspace.ID, Attachment: attachmentID}, nil)
			return 1, err
		}
		if move {
			workspace, err = commitRemoteMove(ctx, api, host, workspace, workspace.Attachments[attachmentID])
			if err != nil {
				_ = closeAndDetachLocal(context.Background(), paths, api, workspace, workspace.Attachments[attachmentID])
				_ = api.RemoteCall(context.Background(), host, "detach_attachment", attachmentRefRequest{Workspace: workspace.ID, Attachment: attachmentID}, nil)
				return 1, err
			}
		}
	} else if move && workspace.PrimaryAttachmentID != attachmentID {
		workspace, err = commitLocalMove(ctx, api, workspace, localAttachment)
		if err != nil {
			_ = closeAndDetachLocal(context.Background(), paths, api, workspace, localAttachment)
			return 1, err
		}
	}
	if err := focusAttachment(ctx, paths, workspace, workspace.Attachments[attachmentID], *paneRef); err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "%s\t%s\n", workspace.ID, workspace.Name)
	return 0, nil
}

func attachmentUsable(attachment *Attachment) bool {
	return attachment != nil && attachment.Status == AttachmentReady && strings.HasPrefix(attachment.Endpoint, "unix:") && !attachment.Revoked
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

func readyRemoteAttachment(ctx context.Context, api API, host string, workspace *Workspace, attachment *Attachment) (*Workspace, error) {
	if attachment == nil {
		return nil, fmt.Errorf("local attachment disappeared before remote readiness")
	}
	var remote Workspace
	err := api.RemoteCall(ctx, host, "update_attachment", attachmentUpdateRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, ExpectedRevision: workspace.Revision,
		Status: AttachmentReady, Views: attachment.Views,
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
		var err error
		workspace, err = readyRemoteAttachment(ctx, api, host, workspace, attachment)
		if err != nil {
			return nil, err
		}
		attachment = workspace.Attachments[attachment.ID]
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
	for _, attachment := range attachments {
		if workspace.RemoteHost != "" {
			if err := api.RemoteCall(ctx, workspace.RemoteHost, "detach_attachment", attachmentRefRequest{Workspace: workspace.ID, Attachment: attachment.ID}, nil); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
		}
		if err := closeAndDetachLocal(ctx, paths, api, workspace, attachment); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return 1, firstErr
	}
	fmt.Fprintln(stdout, workspace.Name)
	return 0, nil
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

func closeAndDetachLocal(ctx context.Context, paths Paths, api API, workspace *Workspace, attachment *Attachment) error {
	if attachment == nil {
		return fmt.Errorf("local attachment does not exist")
	}
	if _, err := api.DetachAttachment(ctx, workspace.ID, attachment.ID); err != nil {
		return err
	}
	var closeErr error
	if attachment != nil && strings.HasPrefix(attachment.Endpoint, "unix:") {
		cfg, err := LoadConfig()
		if err != nil {
			return err
		}
		kitty := KittyClient{Runner: ExecRunner{}, Command: cfg.Kitty.KittenCommand}
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		closeErr = kitty.CloseWorkspace(callCtx, attachment.Endpoint, workspace.ID)
		cancel()
	}
	if closeErr != nil {
		return &kittyCloseError{err: closeErr}
	}
	return nil
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
	attachment := preferredLocalAttachment(workspace, node.ID)
	if !attachmentUsable(attachment) {
		return 1, fmt.Errorf("workspace has no ready attachment on this node")
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
	kitty := KittyClient{Runner: ExecRunner{}, Command: cfg.Kitty.KittenCommand}
	if err := kitty.FocusPane(ctx, attachment.Endpoint, workspace.ID, paneID); err != nil {
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
	if workspace.DeletionPending {
		fmt.Fprintf(w, "deletion_pending=true\ndeletion_error=%s\n", workspace.DeletionError)
	}
	for _, pane := range workspace.SortedPanes() {
		fmt.Fprintf(w, "pane[%s]=%s backend=%s state=%s evidence=%s/%s\n", shortID(pane.ID), pane.Title, pane.Backend.Ref, pane.State, pane.Evidence.Source, pane.Evidence.Event)
		if pane.BackendDead {
			fmt.Fprintf(w, "pane_backend[%s]=dead error=%s\n", shortID(pane.ID), pane.BackendError)
		}
		if pane.RemovalPending {
			fmt.Fprintf(w, "pane_removal[%s]=pending error=%s\n", shortID(pane.ID), pane.RemovalError)
		}
		for _, record := range pane.Notifications {
			if record.LastError != "" {
				fmt.Fprintf(w, "notification_error[%s]=%s\n", record.Channel, record.LastError)
			}
		}
	}
	for _, attachment := range workspace.SortedAttachments() {
		fmt.Fprintf(w, "attachment[%s]=%s node=%s transport=%s status=%s revision=%d\n", shortID(attachment.ID), attachment.Role, attachment.Node.Name, attachment.Transport.Kind, attachment.Status, attachment.AppliedRevision)
	}
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func runPane(args []string, paths Paths, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("pane", stderr)
	workspaceRef := fs.String("workspace", "", "workspace id")
	paneRef := fs.String("pane", "", "existing pane id")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *workspaceRef == "" || fs.NArg() != 0 {
		return 2, fmt.Errorf("pane requires --workspace and optional --pane")
	}
	api := NewAPI(paths)
	cwd, _ := os.Getwd()
	prepared, err := api.PreparePane(context.Background(), *workspaceRef, *paneRef, cwd)
	if err != nil {
		return 1, err
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
	identityCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	identityErr := kitty.SetIdentity(identityCtx, endpoint, windowID, prepared.Workspace.ID, prepared.Pane.ID)
	cancel()
	if identityErr != nil {
		return 1, fmt.Errorf("tag Kitty pane: %w", identityErr)
	}
	if !prepared.Create {
		if prepared.Pane.BackendDead {
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
	commandArgs := []string{"attach", prepared.Pane.Backend.Ref}
	if prepared.Create {
		commandArgs = append(commandArgs, "zka", "pane-host", "--workspace", prepared.Workspace.ID, "--pane", prepared.Pane.ID, "--")
		commandArgs = append(commandArgs, prepared.Workspace.Shell...)
	}
	cmd := exec.Command(cfg.ZMX.Command, commandArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if prepared.Create && prepared.Pane.CWD != "" {
		cmd.Dir = prepared.Pane.CWD
	}
	cmd.Env = append(os.Environ(), "ZKA_WORKSPACE_ID="+prepared.Workspace.ID, "ZKA_PANE_ID="+prepared.Pane.ID)
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
	if recorded := recordedBackendError(api, workspace.ID, pane.ID); recorded != nil {
		return runLocalDeadPane(api, kitty, endpoint, windowID, workspace, pane, recorded, stdin, stdout)
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

func recordedBackendError(api API, workspaceID, paneID string) error {
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
	return paneBackendError(pane)
}

func paneBackendError(pane *Pane) error {
	if pane.BackendError != "" {
		return errors.New(pane.BackendError)
	}
	return fmt.Errorf("zmx backend %q is dead", pane.Backend.Ref)
}

func runLocalDeadPane(api API, kitty KittyClient, endpoint string, windowID int64, workspace *Workspace, pane *Pane, cause error, stdin io.Reader, stdout io.Writer) (int, error) {
	_, _ = api.Event(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "backend_error", Source: "zmx", Detail: cause.Error()})
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
	workspace, err := api.Event(context.Background(), Event{WorkspaceID: *workspaceID, PaneID: *paneID, Kind: "process_started", Source: "pane-host", PID: os.Getpid()})
	if err != nil {
		return 1, err
	}
	pane := workspace.Panes[*paneID]
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if pane != nil && pane.CWD != "" {
		cmd.Dir = pane.CWD
	}
	cmd.Env = append(os.Environ(), "ZKA_WORKSPACE_ID="+*workspaceID, "ZKA_PANE_ID="+*paneID)
	err = cmd.Run()
	exitCode := processExitCode(err)
	_, eventErr := api.Event(context.Background(), Event{WorkspaceID: *workspaceID, PaneID: *paneID, Kind: "process_exit", Source: "pane-host", ExitCode: &exitCode, Detail: fmt.Sprintf("exit code %d", exitCode)})
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
	prepared, err := api.PreparePane(ctx, *workspaceRef, *paneRef, "")
	if err != nil {
		return 1, err
	}
	workspace, pane := prepared.Workspace, prepared.Pane
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	if !prepared.Create {
		if pane.BackendDead {
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
	zmxArgs := []string{"attach", pane.Backend.Ref}
	if prepared.Create {
		zmxArgs = append(zmxArgs, "zka", "pane-host", "--workspace", workspace.ID, "--pane", pane.ID, "--")
		zmxArgs = append(zmxArgs, workspace.Shell...)
	}
	cmd := exec.Command(cfg.ZMX.Command, zmxArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = append(os.Environ(), "ZKA_WORKSPACE_ID="+workspace.ID, "ZKA_PANE_ID="+pane.ID)
	if prepared.Create && pane.CWD != "" {
		cmd.Dir = pane.CWD
	}
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
	heartbeat := attachmentPaneReadyRequest{Workspace: workspace.ID, Attachment: *attachmentID, Pane: pane.ID, Ready: true}
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
	if recorded := recordedBackendError(api, workspace.ID, pane.ID); recorded != nil {
		return runRemoteDeadPane(api, workspace, pane, attachmentID, recorded, stdin, stdout)
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
	_, _ = api.Event(context.Background(), Event{WorkspaceID: workspace.ID, PaneID: pane.ID, Kind: "backend_error", Source: "zmx", Detail: cause.Error()})
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
	var allocated allocatePaneResponse
	cwd, _ := os.Getwd()
	if err := api.RemoteCall(ctx, *host, "allocate_pane", allocatePaneRequest{
		Workspace: *workspaceID, Key: *attachment + ":" + allocationID, CWD: cwd,
	}, &allocated); err != nil {
		return 1, err
	}
	endpoint := os.Getenv("KITTY_LISTEN_ON")
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	kitty := KittyClient{Runner: ExecRunner{}, Command: cfg.Kitty.KittenCommand}
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
	return 0, runRemoteControl(ctx, paths, stdin, stdout)
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
