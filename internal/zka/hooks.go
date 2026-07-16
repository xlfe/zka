package zka

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type codexHookInput struct {
	SessionID            string         `json:"session_id"`
	TurnID               string         `json:"turn_id"`
	HookEventName        string         `json:"hook_event_name"`
	Source               string         `json:"source"`
	ToolName             string         `json:"tool_name"`
	ToolInput            map[string]any `json:"tool_input"`
	LastAssistantMessage string         `json:"last_assistant_message"`
	PermissionMode       string         `json:"permission_mode"`
}

func runHook(args []string, paths Paths, stdin io.Reader, stdout io.Writer) (int, error) {
	if len(args) != 1 || args[0] != "codex" {
		return 2, fmt.Errorf("hook supports only: zka hook codex")
	}
	workspaceID := os.Getenv("ZKA_WORKSPACE_ID")
	paneID := os.Getenv("ZKA_PANE_ID")
	if workspaceID == "" || paneID == "" {
		return hookSuccess(stdout)
	}
	var input codexHookInput
	if err := json.NewDecoder(io.LimitReader(stdin, 256<<10)).Decode(&input); err != nil {
		return hookSuccess(stdout)
	}
	kind := ""
	switch input.HookEventName {
	case "SessionStart":
		kind = "session_start"
	case "UserPromptSubmit":
		kind = "user_prompt"
	case "PermissionRequest":
		kind = "permission_request"
	case "PostToolUse":
		kind = "post_tool"
	case "Stop":
		kind = "stop"
	default:
		return hookSuccess(stdout)
	}
	detail := input.ToolName
	if description, ok := input.ToolInput["description"].(string); ok && description != "" {
		detail = strings.TrimSpace(detail + ": " + description)
	}
	if kind == "session_start" && input.Source != "" {
		detail = input.Source
	}
	if kind == "stop" && input.LastAssistantMessage != "" {
		detail = summarize(input.LastAssistantMessage, 180)
	}
	event := Event{WorkspaceID: workspaceID, PaneID: paneID, Kind: kind, Source: "codex-hook", TurnID: input.TurnID, Detail: detail}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	api := NewAPI(paths)
	api.client.Timeout = 500 * time.Millisecond
	_, _ = api.Event(ctx, event)
	return hookSuccess(stdout)
}

func hookSuccess(stdout io.Writer) (int, error) {
	// Stop hooks require JSON on stdout when they exit successfully. An empty
	// object is valid for every event zka observes and never changes Codex's
	// control flow.
	_, _ = io.WriteString(stdout, "{}\n")
	return 0, nil
}

func summarize(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= max {
		return value
	}
	return value[:max-1] + "…"
}
