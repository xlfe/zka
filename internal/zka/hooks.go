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

const maxHookInputSize = 256 << 10

type agentHookInput struct {
	SessionID            string         `json:"session_id"`
	TurnID               string         `json:"turn_id"`
	PromptID             string         `json:"prompt_id"`
	HookEventName        string         `json:"hook_event_name"`
	Source               string         `json:"source"`
	ToolName             string         `json:"tool_name"`
	ToolInput            map[string]any `json:"tool_input"`
	LastAssistantMessage string         `json:"last_assistant_message"`
	PermissionMode       string         `json:"permission_mode"`
	AgentID              string         `json:"agent_id"`
	NotificationType     string         `json:"notification_type"`
	Message              string         `json:"message"`
	Error                string         `json:"error"`
	ErrorDetails         string         `json:"error_details"`
	Reason               string         `json:"reason"`
	MCPServerName        string         `json:"mcp_server_name"`
	Action               string         `json:"action"`
}

func runHook(args []string, paths Paths, stdin io.Reader, stdout io.Writer) (int, error) {
	if len(args) != 1 || (args[0] != "codex" && args[0] != "claude") {
		return 2, fmt.Errorf("hook supports only: zka hook codex|claude")
	}
	workspaceID := os.Getenv("ZKA_WORKSPACE_ID")
	paneID := os.Getenv("ZKA_PANE_ID")
	if workspaceID == "" || paneID == "" {
		return hookSuccess(stdout)
	}
	payload, err := io.ReadAll(io.LimitReader(stdin, maxHookInputSize+1))
	if err != nil || len(payload) > maxHookInputSize {
		return hookSuccess(stdout)
	}
	var input agentHookInput
	if err := json.Unmarshal(payload, &input); err != nil {
		return hookSuccess(stdout)
	}
	event, ok := mapHookEvent(args[0], workspaceID, paneID, input)
	if !ok {
		return hookSuccess(stdout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	api := NewAPI(paths)
	api.client.Timeout = 500 * time.Millisecond
	_, _ = api.Event(ctx, event)
	return hookSuccess(stdout)
}

func mapHookEvent(agent, workspaceID, paneID string, input agentHookInput) (Event, bool) {
	kind := ""
	switch agent {
	case "codex":
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
		}
	case "claude":
		switch input.HookEventName {
		case "SessionStart":
			kind = "session_start"
		case "UserPromptSubmit":
			kind = "user_prompt"
		case "PreToolUse":
			if input.ToolName == "AskUserQuestion" || input.ToolName == "ExitPlanMode" {
				kind = "permission_request"
			}
		case "PermissionRequest", "Elicitation":
			kind = "permission_request"
		case "PostToolUse", "PostToolUseFailure", "ElicitationResult":
			kind = "post_tool"
		case "Stop":
			kind = "stop"
		case "StopFailure":
			kind = "agent_error"
		case "SessionEnd":
			kind = "session_end"
		case "Notification":
			switch input.NotificationType {
			case "permission_prompt", "elicitation_dialog":
				kind = "permission_request"
			case "idle_prompt":
				kind = "stop"
			case "elicitation_complete", "elicitation_response":
				kind = "post_tool"
			}
		}
		// Background subagent activity must not overwrite the main pane's
		// lifecycle. A permission or question still needs the user in this
		// pane, so preserve only blocked transitions from subagents.
		if input.AgentID != "" && kind != "permission_request" {
			kind = ""
		}
	}
	if kind == "" {
		return Event{}, false
	}
	turnID := input.TurnID
	if agent == "claude" {
		turnID = input.PromptID
	}
	return Event{
		WorkspaceID: workspaceID,
		PaneID:      paneID,
		Kind:        kind,
		Source:      agent + "-hook",
		TurnID:      turnID,
		Detail:      hookDetail(input, kind),
	}, true
}

func hookDetail(input agentHookInput, kind string) string {
	detail := input.ToolName
	if description, ok := input.ToolInput["description"].(string); ok && description != "" {
		detail = strings.TrimSpace(detail + ": " + description)
	}
	switch kind {
	case "session_start":
		detail = input.Source
	case "stop":
		if input.LastAssistantMessage != "" {
			detail = input.LastAssistantMessage
		} else if input.Message != "" {
			detail = input.Message
		}
	case "agent_error":
		detail = strings.TrimSpace(strings.Join(nonEmptyStrings(input.Error, input.ErrorDetails), ": "))
		if detail == "" {
			detail = input.LastAssistantMessage
		}
	case "session_end":
		detail = input.Reason
	case "permission_request", "post_tool":
		if input.MCPServerName != "" || input.Message != "" {
			detail = strings.TrimSpace(strings.Join(nonEmptyStrings(input.MCPServerName, input.Message), ": "))
		}
	}
	return summarize(detail, 180)
}

func nonEmptyStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func hookSuccess(stdout io.Writer) (int, error) {
	// An empty JSON object is valid for every event zka observes and never
	// changes either agent's control flow or adds prompt context.
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
