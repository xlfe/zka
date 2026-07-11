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

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		printUsage(stderr)
		return 2, nil
	}
	// Managed hooks must never interfere with Codex. Non-zka sessions have no
	// session id and can return before XDG/runtime discovery; a broken runtime
	// environment is also treated as a dropped observation, not a hook failure.
	if args[0] == "hook" && os.Getenv("ZKA_SESSION_ID") == "" {
		return hookSuccess(stdout)
	}
	paths, err := DefaultPaths()
	if err != nil {
		if args[0] == "hook" {
			return hookSuccess(stdout)
		}
		return 1, err
	}
	switch args[0] {
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0, nil
	case "daemon":
		return runDaemon(args[1:], paths, stderr)
	case "launch":
		return runLaunch(args[1:], paths, stdout, stderr)
	case "attach":
		return runAttach(args[1:], paths, stdout, stderr)
	case "snapshot":
		return runSnapshot(args[1:], paths, stdout, stderr)
	case "restore":
		return runRestore(args[1:], paths, stdout, stderr)
	case "status":
		return runStatus(args[1:], paths, stdout, stderr)
	case "explain":
		return runExplain(args[1:], paths, stdout, stderr)
	case "seen":
		return runSeen(args[1:], paths, stdout, stderr)
	case "focus":
		return runFocus(args[1:], paths, stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], paths, stdout, stderr)
	case "view":
		return runView(args[1:], paths, stdin, stdout, stderr)
	case "agent-run":
		return runAgent(args[1:], paths, stdin, stdout, stderr)
	case "hook":
		return runHook(args[1:], paths, stdin, stdout)
	default:
		printUsage(stderr)
		return 2, fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `usage: zka COMMAND [OPTIONS]

Commands:
  launch    Create a persistent session and kitty view
  attach    Focus or create a view for an existing session
  snapshot  Save managed kitty views
  restore   Restore managed kitty views
  status    Show session state
  explain   Show state evidence
  focus     Focus a managed kitty view
  seen      Acknowledge a completed session
  doctor    Check runtime integration
  daemon    Run zkad (normally via systemd --user)`)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func runDaemon(args []string, paths Paths, stderr io.Writer) (int, error) {
	fs := newFlagSet("daemon", stderr)
	fs.StringVar(&paths.Socket, "socket", paths.Socket, "Unix socket path")
	fs.StringVar(&paths.StateDir, "state-dir", paths.StateDir, "state directory")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	paths.StateFile = filepath.Join(paths.StateDir, "state.json")
	paths.SnapshotDir = filepath.Join(paths.StateDir, "snapshots")
	d, err := NewDaemon(paths, ExecRunner{}, nil)
	if err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return 0, d.Serve(ctx)
}

func runLaunch(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("launch", stderr)
	name := fs.String("name", "", "session name")
	typ := fs.String("type", "window", "kitty view type: window, tab, or os-window")
	cwd := fs.String("cwd", "", "working directory")
	endpoint := fs.String("to", os.Getenv("KITTY_LISTEN_ON"), "kitty remote-control endpoint")
	agent := fs.String("agent", "", "agent integration (default: infer from command)")
	backend := fs.String("backend", "zmx", "session backend")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	command := fs.Args()
	if *name == "" || len(command) == 0 {
		return 2, fmt.Errorf("launch requires --name and a command after --")
	}
	if err := validateViewType(*typ); err != nil {
		return 2, err
	}
	if *endpoint == "" {
		return 1, fmt.Errorf("kitty endpoint is required (--to or KITTY_LISTEN_ON)")
	}
	if *backend != "zmx" {
		return 1, fmt.Errorf("backend adapter %q is modeled but not implemented", *backend)
	}
	if *cwd == "" {
		var err error
		*cwd, err = os.Getwd()
		if err != nil {
			return 1, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	api := NewAPI(paths)
	session, err := api.CreateSession(ctx, createSessionRequest{Name: *name, BackendKind: *backend, Agent: *agent, Command: command, CWD: *cwd})
	if err != nil {
		return 1, err
	}
	kitty := KittyClient{Runner: ExecRunner{}, Command: os.Getenv("ZKA_KITTEN_COMMAND")}
	windowID, err := kitty.Launch(ctx, LaunchOptions{Endpoint: *endpoint, Type: *typ, CWD: *cwd, Title: *name, SessionID: session.ID, Backend: session.Backend.Kind, State: session.State})
	if err != nil {
		_ = api.DeleteSession(context.Background(), session.ID)
		return 1, err
	}
	_, err = api.RegisterView(ctx, session.ID, View{Endpoint: *endpoint, WindowID: windowID, Attached: true, LastSeen: time.Now().UTC()})
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "%s\t%s\n", session.ID, session.Name)
	return 0, nil
}

func runAttach(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("attach", stderr)
	typ := fs.String("type", "window", "kitty view type")
	endpoint := fs.String("to", os.Getenv("KITTY_LISTEN_ON"), "kitty remote-control endpoint")
	newView := fs.Bool("new-view", false, "create another view even if already attached")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("attach requires one session reference")
	}
	if err := validateViewType(*typ); err != nil {
		return 2, err
	}
	if *endpoint == "" {
		return 1, fmt.Errorf("kitty endpoint is required (--to or KITTY_LISTEN_ON)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	api := NewAPI(paths)
	session, err := api.Session(ctx, fs.Arg(0))
	if err != nil {
		return 1, err
	}
	if session.Backend.Kind != "zmx" {
		return 1, fmt.Errorf("backend adapter %q is modeled but not implemented", session.Backend.Kind)
	}
	kitty := KittyClient{Runner: ExecRunner{}, Command: os.Getenv("ZKA_KITTEN_COMMAND")}
	if !*newView {
		tree, listErr := kitty.List(ctx, *endpoint)
		if listErr == nil && len(findManagedViews(tree)[session.ID]) > 0 {
			if err := kitty.Focus(ctx, *endpoint, session.ID); err != nil {
				return 1, err
			}
			fmt.Fprintf(stdout, "focused existing view for %s\n", session.Name)
			return 0, nil
		}
	}
	windowID, err := kitty.Launch(ctx, LaunchOptions{Endpoint: *endpoint, Type: *typ, CWD: session.CWD, Title: session.Name, SessionID: session.ID, Backend: session.Backend.Kind, State: session.State})
	if err != nil {
		return 1, err
	}
	_, err = api.RegisterView(ctx, session.ID, View{Endpoint: *endpoint, WindowID: windowID, Attached: true, LastSeen: time.Now().UTC()})
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "%d\n", windowID)
	return 0, nil
}

func validateViewType(value string) error {
	switch value {
	case "window", "tab", "os-window":
		return nil
	default:
		return fmt.Errorf("invalid kitty view type %q", value)
	}
}

func runSnapshot(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("snapshot", stderr)
	endpoint := fs.String("to", os.Getenv("KITTY_LISTEN_ON"), "kitty remote-control endpoint")
	output := fs.String("output", "", "output path or - for stdout")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 || *endpoint == "" {
		return 2, fmt.Errorf("snapshot requires NAME and a kitty endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kitty := KittyClient{Runner: ExecRunner{}, Command: os.Getenv("ZKA_KITTEN_COMMAND")}
	snapshot, err := CaptureSnapshot(ctx, kitty, *endpoint, fs.Arg(0))
	if err != nil {
		return 1, err
	}
	result, err := NewStore(paths).SaveSnapshot(snapshot, *output)
	if err != nil {
		return 1, err
	}
	if *output == "-" {
		fmt.Fprint(stdout, result)
	} else {
		fmt.Fprintln(stdout, result)
	}
	return 0, nil
}

func runRestore(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("restore", stderr)
	endpoint := fs.String("to", os.Getenv("KITTY_LISTEN_ON"), "kitty remote-control endpoint")
	duplicate := fs.Bool("duplicate", false, "create duplicate views")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 || *endpoint == "" {
		return 2, fmt.Errorf("restore requires NAME|PATH and a kitty endpoint")
	}
	snapshot, _, err := NewStore(paths).LoadSnapshot(fs.Arg(0))
	if err != nil {
		return 1, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := NewAPI(paths)
	sessions, err := api.AllSessions(ctx)
	if err != nil {
		return 1, err
	}
	kitty := KittyClient{Runner: ExecRunner{}, Command: os.Getenv("ZKA_KITTEN_COMMAND")}
	skip := map[string]bool{}
	if !*duplicate {
		tree, err := kitty.List(ctx, *endpoint)
		if err != nil {
			return 1, err
		}
		for id := range findManagedViews(tree) {
			skip[id] = true
		}
	}
	for _, id := range SnapshotSessionIDs(snapshot) {
		if sessions[id] == nil {
			return 1, fmt.Errorf("snapshot references unknown session %s", id)
		}
	}
	content := ""
	if len(skip) == 0 && nativeSessionIsManaged(snapshot.NativeSession, SnapshotSessionIDs(snapshot)) {
		content = snapshot.NativeSession
	} else {
		content, err = GenerateKittySession(snapshot, sessions, skip)
	}
	if err != nil {
		if strings.Contains(err.Error(), "all snapshot views already exist") {
			fmt.Fprintln(stdout, err.Error())
			return 0, nil
		}
		return 1, err
	}
	path, err := writeRestoreSession(paths, snapshot, content)
	if err != nil {
		return 1, err
	}
	if err := kitty.LoadSession(ctx, *endpoint, path); err != nil {
		return 1, err
	}
	for id := range skip {
		fmt.Fprintf(stdout, "reused existing view %s\n", id)
	}
	fmt.Fprintln(stdout, path)
	return 0, nil
}

func runStatus(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("status", stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	api := NewAPI(paths)
	var sessions []*Session
	if fs.NArg() == 1 {
		session, err := api.Session(ctx, fs.Arg(0))
		if err != nil {
			return 1, err
		}
		sessions = []*Session{session}
	} else if fs.NArg() == 0 {
		var err error
		sessions, err = api.Sessions(ctx)
		if err != nil {
			return 1, err
		}
	} else {
		return 2, fmt.Errorf("status accepts at most one session reference")
	}
	if *jsonOut {
		return 0, writeJSON(stdout, sessions)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATE\tNAME\tID\tBACKEND\tVIEWS\tEVIDENCE")
	for _, session := range sessions {
		attached := 0
		for _, view := range session.Views {
			if view.Attached {
				attached++
			}
		}
		shortID := session.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s:%s\t%d\t%s/%s\n", session.State, session.Name, shortID, session.Backend.Kind, session.Backend.Ref, attached, session.Evidence.Source, session.Evidence.Event)
	}
	_ = tw.Flush()
	return 0, nil
}

func runExplain(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("explain", stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("explain requires one session reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := NewAPI(paths).Session(ctx, fs.Arg(0))
	if err != nil {
		return 1, err
	}
	if *jsonOut {
		return 0, writeJSON(stdout, session)
	}
	fmt.Fprintf(stdout, "state=%s\nsource=%s\nevent=%s\nevidence=%s\nsession=%s\nbackend=%s:%s\nturn=%s\n",
		session.State, session.Evidence.Source, session.Evidence.Event, session.Evidence.Detail, session.ID, session.Backend.Kind, session.Backend.Ref, session.LastTurnID)
	for _, record := range session.Notifications {
		if record.LastError != "" {
			fmt.Fprintf(stdout, "notification_error[%s]=%s\n", record.Channel, record.LastError)
		}
	}
	return 0, nil
}

func runSeen(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("seen", stderr)
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("seen requires one session reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := NewAPI(paths).Seen(ctx, fs.Arg(0))
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "%s\t%s\n", session.State, session.Name)
	return 0, nil
}

func runFocus(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("focus", stderr)
	endpoint := fs.String("to", "", "kitty remote-control endpoint")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, fmt.Errorf("focus requires one session reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	api := NewAPI(paths)
	session, err := api.Session(ctx, fs.Arg(0))
	if err != nil {
		return 1, err
	}
	target := *endpoint
	if target == "" {
		target = os.Getenv("KITTY_LISTEN_ON")
	}
	if target == "" {
		for _, view := range session.SortedViews() {
			if view.Attached {
				target = view.Endpoint
				break
			}
		}
	}
	if target == "" {
		return 1, fmt.Errorf("session has no attached kitty view")
	}
	kitty := KittyClient{Runner: ExecRunner{}, Command: os.Getenv("ZKA_KITTEN_COMMAND")}
	if err := kitty.Focus(ctx, target, session.ID); err != nil {
		return 1, err
	}
	_, _ = api.Seen(ctx, session.ID)
	fmt.Fprintln(stdout, session.Name)
	return 0, nil
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func runView(args []string, paths Paths, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) != 1 {
		return 2, fmt.Errorf("view requires one session id")
	}
	ctx := context.Background()
	api := NewAPI(paths)
	prepared, err := api.PrepareView(ctx, args[0])
	if err != nil {
		return 1, err
	}
	session := prepared.Session
	view := viewFromEnv()
	if view != nil {
		_, _ = api.RegisterView(ctx, session.ID, *view)
	}
	zmx := os.Getenv("ZKA_ZMX_COMMAND")
	if zmx == "" {
		zmx = "zmx"
	}
	if !prepared.Create {
		exists, err := zmxSessionExists(ctx, zmx, session.Backend.Ref)
		if err != nil {
			return reportBackendError(api, session, view, fmt.Errorf("query zmx sessions: %w", err))
		}
		if !exists {
			return reportBackendError(api, session, view, fmt.Errorf("zmx session %q no longer exists; refusing to restart it during attach", session.Backend.Ref))
		}
	}
	cmdArgs := []string{"attach", session.Backend.Ref}
	if prepared.Create {
		exe, err := os.Executable()
		if err != nil {
			return 1, err
		}
		cmdArgs = append(cmdArgs, exe, "agent-run", "--session", session.ID, "--")
		cmdArgs = append(cmdArgs, session.Command...)
	}
	cmd := exec.Command(zmx, cmdArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Dir = session.CWD
	cmd.Env = append(os.Environ(), "ZKA_SESSION_ID="+session.ID)
	if err := cmd.Run(); err != nil {
		return reportBackendError(api, session, view, err)
	}
	return 0, nil
}

func zmxSessionExists(ctx context.Context, command, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, _, err := (ExecRunner{}).Run(ctx, command, "list", "--short")
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

func reportBackendError(api API, session *Session, view *View, cause error) (int, error) {
	_, _ = api.Event(context.Background(), Event{SessionID: session.ID, Kind: "backend_error", Source: "zmx", Detail: cause.Error(), View: view})
	return 1, cause
}

func runAgent(args []string, paths Paths, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("agent-run", stderr)
	sessionID := fs.String("session", "", "zka session id")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	command := fs.Args()
	if *sessionID == "" || len(command) == 0 {
		return 2, fmt.Errorf("agent-run requires --session ID -- COMMAND")
	}
	api := NewAPI(paths)
	view := viewFromEnv()
	started, err := api.Event(context.Background(), Event{SessionID: *sessionID, Kind: "process_started", Source: "agent-run", PID: os.Getpid(), View: view})
	if err != nil {
		return 1, err
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Dir = started.CWD
	cmd.Env = append(os.Environ(), "ZKA_SESSION_ID="+started.ID)
	err = cmd.Run()
	exitCode := processExitCode(err)
	_, eventErr := api.Event(context.Background(), Event{SessionID: *sessionID, Kind: "process_exit", Source: "agent-run", ExitCode: &exitCode, Detail: fmt.Sprintf("exit code %d", exitCode), View: view})
	if eventErr != nil {
		fmt.Fprintf(stderr, "zka: report process exit: %v\n", eventErr)
	}
	if exitCode != 0 {
		return exitCode, nil
	}
	return 0, nil
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

func viewFromEnv() *View {
	endpoint := os.Getenv("KITTY_LISTEN_ON")
	id, err := strconv.ParseInt(os.Getenv("KITTY_WINDOW_ID"), 10, 64)
	if endpoint == "" || err != nil || id <= 0 {
		return nil
	}
	return &View{Endpoint: endpoint, WindowID: id, Attached: true, LastSeen: time.Now().UTC()}
}
