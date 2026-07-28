package zka

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeWorkspaceDetachOperations struct {
	detachLocalFn  func(context.Context, string, *Attachment) error
	detachRemoteFn func(context.Context, string, string, *Attachment) error
	calls          []string
}

func (o *fakeWorkspaceDetachOperations) detachLocal(ctx context.Context, workspaceID string, attachment *Attachment) error {
	o.calls = append(o.calls, "local")
	if o.detachLocalFn != nil {
		return o.detachLocalFn(ctx, workspaceID, attachment)
	}
	return nil
}

func (o *fakeWorkspaceDetachOperations) detachRemote(ctx context.Context, host, workspaceID string, attachment *Attachment) error {
	o.calls = append(o.calls, "remote")
	if o.detachRemoteFn != nil {
		return o.detachRemoteFn(ctx, host, workspaceID, attachment)
	}
	return nil
}

func TestDetachWorkspaceAttachmentClosesLocalKittyBeforePublishingRemoteDetach(t *testing.T) {
	kittyReachable := true
	operations := &fakeWorkspaceDetachOperations{
		detachLocalFn: func(context.Context, string, *Attachment) error {
			if !kittyReachable {
				return &kittyCloseError{err: errors.New("connection refused")}
			}
			return nil
		},
		detachRemoteFn: func(context.Context, string, string, *Attachment) error {
			// Caching the remote response revokes the local attachment and may
			// close its Kitty process immediately.
			kittyReachable = false
			return nil
		},
	}

	err := detachWorkspaceAttachment(
		context.Background(),
		operations,
		"origin.example",
		"workspace",
		&Attachment{ID: "attachment"},
	)
	if err != nil {
		t.Fatalf("detach workspace attachment = %v", err)
	}
	if got := strings.Join(operations.calls, ","); got != "local,remote" {
		t.Fatalf("detach order = %s", got)
	}
}

func TestDetachWorkspaceAttachmentStillPublishesRemoteDetachAfterKittyCloseFailure(t *testing.T) {
	closeErr := &kittyCloseError{err: errors.New("connection refused")}
	operations := &fakeWorkspaceDetachOperations{
		detachLocalFn: func(context.Context, string, *Attachment) error {
			return closeErr
		},
	}

	err := detachWorkspaceAttachment(
		context.Background(),
		operations,
		"origin.example",
		"workspace",
		&Attachment{ID: "attachment"},
	)
	if !errors.Is(err, closeErr) {
		t.Fatalf("detach workspace attachment error = %v", err)
	}
	if got := strings.Join(operations.calls, ","); got != "local,remote" {
		t.Fatalf("detach order = %s", got)
	}
}

func TestDetachWorkspaceAttachmentStopsWhenLocalStateCannotDetach(t *testing.T) {
	stateErr := errors.New("local detach failed")
	operations := &fakeWorkspaceDetachOperations{
		detachLocalFn: func(context.Context, string, *Attachment) error {
			return stateErr
		},
	}

	err := detachWorkspaceAttachment(
		context.Background(),
		operations,
		"origin.example",
		"workspace",
		&Attachment{ID: "attachment"},
	)
	if !errors.Is(err, stateErr) {
		t.Fatalf("detach workspace attachment error = %v", err)
	}
	if got := strings.Join(operations.calls, ","); got != "local" {
		t.Fatalf("detach order = %s", got)
	}
}
