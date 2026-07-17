package zka

import (
	"context"
	"encoding/json"
	"time"
)

type API struct {
	client Client
}

func NewAPI(paths Paths) API {
	return API{client: Client{Socket: paths.Socket, Timeout: 5 * time.Second}}
}

func (a API) Ping(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := a.client.Call(ctx, "ping", nil, &out)
	return out, err
}

func (a API) Node(ctx context.Context) (Host, error) {
	var out Host
	err := a.client.Call(ctx, "node", nil, &out)
	return out, err
}

func (a API) CreateWorkspace(ctx context.Context, req createWorkspaceRequest) (*Workspace, error) {
	var out Workspace
	err := a.client.Call(ctx, "create_workspace", req, &out)
	return &out, err
}

func (a API) DeleteWorkspace(ctx context.Context, ref string) error {
	return a.client.Call(ctx, "delete_workspace", refRequest{Ref: ref}, nil)
}

func (a API) Workspace(ctx context.Context, ref string) (*Workspace, error) {
	var out Workspace
	err := a.client.Call(ctx, "get_workspace", refRequest{Ref: ref}, &out)
	return &out, err
}

func (a API) Workspaces(ctx context.Context) ([]*Workspace, error) {
	var out []*Workspace
	err := a.client.Call(ctx, "list_workspaces", nil, &out)
	return out, err
}

func (a API) Attention(ctx context.Context) (AttentionSnapshot, error) {
	var out AttentionSnapshot
	err := a.client.Call(ctx, "attention_snapshot", nil, &out)
	return out, err
}

func (a API) WatchAttention(ctx context.Context, yield func(AttentionSnapshot) error) error {
	return a.client.WatchAttention(ctx, yield)
}

func (a API) PauseAttention(ctx context.Context) (AttentionSnapshot, error) {
	var out AttentionSnapshot
	err := a.client.Call(ctx, "pause_attention", nil, &out)
	return out, err
}

func (a API) ResumeAttention(ctx context.Context) (AttentionSnapshot, error) {
	var out AttentionSnapshot
	err := a.client.Call(ctx, "resume_attention", nil, &out)
	return out, err
}

func (a API) ToggleAttention(ctx context.Context) (AttentionSnapshot, error) {
	var out AttentionSnapshot
	err := a.client.Call(ctx, "toggle_attention", nil, &out)
	return out, err
}

func (a API) PreparePane(ctx context.Context, workspace, pane, cwd string) (preparePaneResponse, error) {
	var out preparePaneResponse
	err := a.client.Call(ctx, "prepare_pane", workspacePaneRequest{Workspace: workspace, Pane: pane, CWD: cwd}, &out)
	return out, err
}

func (a API) AllocatePane(ctx context.Context, workspace, key, cwd string) (allocatePaneResponse, error) {
	var out allocatePaneResponse
	err := a.client.Call(ctx, "allocate_pane", allocatePaneRequest{Workspace: workspace, Key: key, CWD: cwd}, &out)
	return out, err
}

func (a API) ReconcileBackends(ctx context.Context, workspace string) (backendReconcileResponse, error) {
	var out backendReconcileResponse
	err := a.client.Call(ctx, "reconcile_backends", backendReconcileRequest{Workspace: workspace}, &out)
	return out, err
}

func (a API) RegisterAttachment(ctx context.Context, workspace string, attachment Attachment) (*Attachment, error) {
	var out Attachment
	err := a.client.Call(ctx, "register_attachment", attachmentRequest{Workspace: workspace, Attachment: attachment}, &out)
	return &out, err
}

func (a API) Attachment(ctx context.Context, workspace, attachment string) (*Attachment, error) {
	var out Attachment
	err := a.client.Call(ctx, "get_attachment", attachmentRefRequest{Workspace: workspace, Attachment: attachment}, &out)
	return &out, err
}

func (a API) UpdateAttachment(ctx context.Context, req attachmentUpdateRequest) (*Workspace, error) {
	var out Workspace
	err := a.client.Call(ctx, "update_attachment", req, &out)
	return &out, err
}

func (a API) SetAttachmentPaneReady(ctx context.Context, req attachmentPaneReadyRequest) (*Attachment, error) {
	var out Attachment
	err := a.client.Call(ctx, "set_attachment_pane_ready", req, &out)
	return &out, err
}

func (a API) UpdateManifest(ctx context.Context, req manifestUpdateRequest) (*Workspace, error) {
	var out Workspace
	err := a.client.Call(ctx, "update_manifest", req, &out)
	return &out, err
}

func (a API) RenameWorkspace(ctx context.Context, req renameWorkspaceRequest) (*Workspace, error) {
	var out Workspace
	err := a.client.Call(ctx, "rename_workspace", req, &out)
	return &out, err
}

func (a API) ClosePanes(ctx context.Context, req closePanesRequest) (*Workspace, error) {
	var out Workspace
	err := a.client.Call(ctx, "close_panes", req, &out)
	return &out, err
}

func (a API) KillWorkspace(ctx context.Context, workspaceID string) (workspaceDeletionResponse, error) {
	var out workspaceDeletionResponse
	err := a.client.Call(ctx, "kill_workspace", killWorkspaceRequest{WorkspaceID: workspaceID}, &out)
	return out, err
}

func (a API) CommitMove(ctx context.Context, req moveCommitRequest) (moveCommitResponse, error) {
	var out moveCommitResponse
	err := a.client.Call(ctx, "commit_move", req, &out)
	return out, err
}

func (a API) DetachAttachment(ctx context.Context, workspace, attachment string) (*Workspace, error) {
	var out Workspace
	err := a.client.Call(ctx, "detach_attachment", attachmentRefRequest{Workspace: workspace, Attachment: attachment}, &out)
	return &out, err
}

func (a API) Event(ctx context.Context, event Event) (*Workspace, error) {
	var out Workspace
	err := a.client.Call(ctx, "event", event, &out)
	return &out, err
}

func (a API) Seen(ctx context.Context, workspace, pane string) (*Workspace, error) {
	var out Workspace
	err := a.client.Call(ctx, "seen", workspacePaneRequest{Workspace: workspace, Pane: pane}, &out)
	return &out, err
}

func (a API) RemoteCall(ctx context.Context, host, op string, payload, out any) error {
	var raw json.RawMessage
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = encoded
	}
	var response json.RawMessage
	if err := a.client.Call(ctx, "remote_call", remoteDaemonRequest{Host: host, Op: op, Payload: raw}, &response); err != nil {
		return err
	}
	if out != nil && len(response) > 0 {
		return json.Unmarshal(response, out)
	}
	return nil
}
