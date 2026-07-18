package zka

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type attentionOutputMode uint8

const (
	attentionOutputHuman attentionOutputMode = iota
	attentionOutputJSON
	attentionOutputWaybar
)

type waybarAttention struct {
	Text    string `json:"text"`
	Tooltip string `json:"tooltip"`
	Class   string `json:"class"`
}

func runAttention(args []string, paths Paths, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		printAttentionUsage(stderr)
		return 2, nil
	}
	switch args[0] {
	case "help", "--help", "-h":
		printAttentionUsage(stdout)
		return 0, nil
	case "show":
		if len(args) != 1 {
			return 2, fmt.Errorf("attention show accepts no arguments")
		}
		return runLauncherMode("attention", stdin, stdout, stderr)
	case "status":
		return runAttentionStatus(args[1:], paths, stdout, stderr)
	case "watch":
		return runAttentionWatch(args[1:], paths, stdout, stderr)
	case "focus-next":
		return runAttentionFocusNext(args[1:], paths, stdout, stderr)
	case "pause":
		return runAttentionModeChange(args[1:], paths, stdout, "pause")
	case "resume":
		return runAttentionModeChange(args[1:], paths, stdout, "resume")
	case "toggle":
		return runAttentionModeChange(args[1:], paths, stdout, "toggle")
	default:
		printAttentionUsage(stderr)
		return 2, fmt.Errorf("unknown attention command %q", args[0])
	}
}

func printAttentionUsage(w io.Writer) {
	fmt.Fprintln(w, `usage: zka attention COMMAND

  show                 Open the live attention popup
  status [--json|--waybar]
  watch [--json|--waybar]
  focus-next           Jump to the highest-priority pane
  pause                Silence attention notifications
  resume               Resume attention notifications
  toggle               Toggle attention notifications`)
}

func parseAttentionOutput(name string, args []string, stderr io.Writer) (attentionOutputMode, error) {
	fs := newFlagSet(name, stderr)
	jsonOutput := fs.Bool("json", false, "emit JSON")
	waybarOutput := fs.Bool("waybar", false, "emit Waybar custom-module JSON")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() != 0 {
		return 0, fmt.Errorf("%s accepts no positional arguments", name)
	}
	if *jsonOutput && *waybarOutput {
		return 0, fmt.Errorf("--json and --waybar are mutually exclusive")
	}
	if *jsonOutput {
		return attentionOutputJSON, nil
	}
	if *waybarOutput {
		return attentionOutputWaybar, nil
	}
	return attentionOutputHuman, nil
}

func runAttentionStatus(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	mode, err := parseAttentionOutput("attention status", args, stderr)
	if err != nil {
		return 2, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshot, err := NewAPI(paths).Attention(ctx)
	if err != nil {
		if mode != attentionOutputHuman {
			return 1, writeAttentionOutput(stdout, mode, AttentionSnapshot{}, err)
		}
		return 1, err
	}
	return 0, writeAttentionOutput(stdout, mode, snapshot, nil)
}

func runAttentionWatch(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	mode, err := parseAttentionOutput("attention watch", args, stderr)
	if err != nil {
		return 2, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	api := NewAPI(paths)
	backoff := 250 * time.Millisecond
	last := ""
	write := func(snapshot AttentionSnapshot, unavailable error) error {
		var encoded strings.Builder
		if err := writeAttentionOutput(&encoded, mode, snapshot, unavailable); err != nil {
			return err
		}
		line := encoded.String()
		if line == last {
			return nil
		}
		last = line
		_, err := io.WriteString(stdout, line)
		return err
	}
	for ctx.Err() == nil {
		received := false
		err := api.WatchAttention(ctx, func(snapshot AttentionSnapshot) error {
			received = true
			backoff = 250 * time.Millisecond
			return write(snapshot, nil)
		})
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			if writeErr := write(AttentionSnapshot{}, err); writeErr != nil {
				return 1, writeErr
			}
		}
		if received {
			backoff = 250 * time.Millisecond
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if backoff < 5*time.Second {
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
	}
	return 0, nil
}

func runAttentionFocusNext(args []string, paths Paths, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("attention focus-next", stderr)
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("attention focus-next accepts no arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	snapshot, err := NewAPI(paths).Attention(ctx)
	cancel()
	if err != nil {
		return 1, err
	}
	item, ok := nextAttentionItem(snapshot)
	if !ok {
		return 0, nil
	}
	if item.Attached {
		return runWorkspaceFocus([]string{item.WorkspaceID, "--pane", item.PaneID}, paths, stdout, stderr)
	}
	return runWorkspaceAttach([]string{item.WorkspaceRef(), "--pane", item.PaneID}, paths, false, stdout, stderr)
}

func runAttentionModeChange(args []string, paths Paths, stdout io.Writer, action string) (int, error) {
	fs := flag.NewFlagSet("attention "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("attention %s accepts no arguments", action)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	api := NewAPI(paths)
	var snapshot AttentionSnapshot
	var err error
	switch action {
	case "pause":
		snapshot, err = api.PauseAttention(ctx)
	case "resume":
		snapshot, err = api.ResumeAttention(ctx)
	case "toggle":
		snapshot, err = api.ToggleAttention(ctx)
	default:
		err = errors.New("invalid attention action")
	}
	if err != nil {
		return 1, err
	}
	state := "active"
	if snapshot.Paused {
		state = "paused"
	}
	fmt.Fprintf(stdout, "%s\t%d\n", state, snapshot.Counts.Total)
	return 0, nil
}

func writeAttentionOutput(w io.Writer, mode attentionOutputMode, snapshot AttentionSnapshot, unavailable error) error {
	switch mode {
	case attentionOutputJSON:
		if unavailable != nil {
			return json.NewEncoder(w).Encode(map[string]any{
				"version":     attentionSchemaVersion,
				"unavailable": true,
				"error":       unavailable.Error(),
			})
		}
		encoder := json.NewEncoder(w)
		return encoder.Encode(snapshot)
	case attentionOutputWaybar:
		output := attentionWaybar(snapshot, unavailable)
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(output)
	default:
		if unavailable != nil {
			_, err := fmt.Fprintf(w, "unavailable\t%s\n", unavailable)
			return err
		}
		state := "active"
		if snapshot.Paused {
			state = "paused"
		}
		_, err := fmt.Fprintf(w, "%s\t%d\tblocked=%d\terror=%d\tdone=%d\n", state, snapshot.Counts.Total, snapshot.Counts.Blocked, snapshot.Counts.Error, snapshot.Counts.Done)
		return err
	}
}

func attentionWaybar(snapshot AttentionSnapshot, unavailable error) waybarAttention {
	if unavailable != nil {
		return waybarAttention{Text: "?", Tooltip: "zka daemon unavailable: " + unavailable.Error(), Class: "unavailable"}
	}
	class := "clear"
	if snapshot.Paused {
		class = "paused"
	} else if snapshot.Counts.Total > 0 {
		class = string(snapshot.Highest)
	}
	lines := []string{fmt.Sprintf("zka: %d pane(s) need attention", snapshot.Counts.Total)}
	if snapshot.Paused {
		lines[0] = fmt.Sprintf("zka attention paused: %d pending", snapshot.Counts.Total)
	}
	for _, item := range snapshot.Items {
		label := item.WorkspaceName + " · " + item.PaneTitle
		if item.PaneTitle == "" {
			label = item.WorkspaceName + " · " + shortID(item.PaneID)
		}
		if item.Agent != "" {
			label += " (" + item.Agent + ")"
		}
		line := fmt.Sprintf("%s: %s", attentionStateLabel(item.State), label)
		if item.Detail != "" {
			line += " — " + item.Detail
		}
		lines = append(lines, line)
	}
	return waybarAttention{Text: fmt.Sprint(snapshot.Counts.Total), Tooltip: html.EscapeString(strings.Join(lines, "\n")), Class: class}
}

func attentionStateLabel(state AgentState) string {
	switch state {
	case StateBlocked:
		return "Waiting for you"
	case StateError:
		return "Failed"
	case StateDone:
		return "Finished"
	default:
		return string(state)
	}
}
