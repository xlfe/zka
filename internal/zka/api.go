package zka

import (
	"context"
	"time"
)

type API struct {
	client Client
}

func NewAPI(paths Paths) API {
	return API{client: Client{Socket: paths.Socket, Timeout: 3 * time.Second}}
}

func (a API) Ping(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := a.client.Call(ctx, "ping", nil, &out)
	return out, err
}

func (a API) CreateSession(ctx context.Context, req createSessionRequest) (*Session, error) {
	var out Session
	err := a.client.Call(ctx, "create_session", req, &out)
	return &out, err
}

func (a API) DeleteSession(ctx context.Context, ref string) error {
	return a.client.Call(ctx, "delete_session", refRequest{Ref: ref}, nil)
}

func (a API) Session(ctx context.Context, ref string) (*Session, error) {
	var out Session
	err := a.client.Call(ctx, "get_session", refRequest{Ref: ref}, &out)
	return &out, err
}

func (a API) Sessions(ctx context.Context) ([]*Session, error) {
	var out []*Session
	err := a.client.Call(ctx, "list_sessions", nil, &out)
	return out, err
}

func (a API) AllSessions(ctx context.Context) (map[string]*Session, error) {
	var out map[string]*Session
	err := a.client.Call(ctx, "all_sessions", nil, &out)
	return out, err
}

func (a API) RegisterView(ctx context.Context, sessionID string, view View) (*Session, error) {
	var out Session
	err := a.client.Call(ctx, "register_view", registerViewRequest{SessionID: sessionID, View: view}, &out)
	return &out, err
}

func (a API) PrepareView(ctx context.Context, ref string) (prepareViewResponse, error) {
	var out prepareViewResponse
	err := a.client.Call(ctx, "prepare_view", refRequest{Ref: ref}, &out)
	return out, err
}

func (a API) Event(ctx context.Context, event Event) (*Session, error) {
	var out Session
	err := a.client.Call(ctx, "event", event, &out)
	return &out, err
}

func (a API) Seen(ctx context.Context, ref string) (*Session, error) {
	var out Session
	err := a.client.Call(ctx, "seen", refRequest{Ref: ref}, &out)
	return &out, err
}
