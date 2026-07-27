package zka

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeWorkspaceAttachOperations struct {
	readyFn        func(context.Context, string, *Workspace, *Attachment) (*Workspace, error)
	commitRemoteFn func(context.Context, string, *Workspace, *Attachment) (*Workspace, error)
	commitLocalFn  func(context.Context, *Workspace, *Attachment) (*Workspace, error)
	rollbackFn     func(context.Context, string, string, *Attachment) error

	readyCalls         int
	commitRemoteCalls  int
	commitLocalCalls   int
	rollbackCalls      int
	rollbackHost       string
	rollbackWorkspace  string
	rollbackAttachment string
}

func (o *fakeWorkspaceAttachOperations) readyRemote(ctx context.Context, host string, workspace *Workspace, attachment *Attachment) (*Workspace, error) {
	o.readyCalls++
	if o.readyFn != nil {
		return o.readyFn(ctx, host, workspace, attachment)
	}
	return workspace.Clone(), nil
}

func (o *fakeWorkspaceAttachOperations) commitRemote(ctx context.Context, host string, workspace *Workspace, attachment *Attachment) (*Workspace, error) {
	o.commitRemoteCalls++
	if o.commitRemoteFn != nil {
		return o.commitRemoteFn(ctx, host, workspace, attachment)
	}
	return workspace.Clone(), nil
}

func (o *fakeWorkspaceAttachOperations) commitLocal(ctx context.Context, workspace *Workspace, attachment *Attachment) (*Workspace, error) {
	o.commitLocalCalls++
	if o.commitLocalFn != nil {
		return o.commitLocalFn(ctx, workspace, attachment)
	}
	return workspace.Clone(), nil
}

func (o *fakeWorkspaceAttachOperations) rollback(ctx context.Context, host, workspaceID string, attachment *Attachment) error {
	o.rollbackCalls++
	o.rollbackHost = host
	o.rollbackWorkspace = workspaceID
	if attachment != nil {
		o.rollbackAttachment = attachment.ID
	}
	if o.rollbackFn != nil {
		return o.rollbackFn(ctx, host, workspaceID, attachment)
	}
	return nil
}

func workspaceAttachFixture() (*Workspace, *Attachment) {
	attachment := &Attachment{
		ID: "attachment", Endpoint: "unix:/attachment", Status: AttachmentReady,
	}
	workspace := &Workspace{
		ID: "workspace", PrimaryAttachmentID: "source",
		Attachments: map[string]*Attachment{attachment.ID: attachment},
	}
	return workspace, attachment
}

func assertWorkspaceAttachRollback(t *testing.T, operations *fakeWorkspaceAttachOperations) {
	t.Helper()
	if operations.rollbackCalls != 1 ||
		operations.rollbackHost != "origin.example" ||
		operations.rollbackWorkspace != "workspace" ||
		operations.rollbackAttachment != "attachment" {
		t.Fatalf("rollback calls=%d host=%q workspace=%q attachment=%q",
			operations.rollbackCalls,
			operations.rollbackHost,
			operations.rollbackWorkspace,
			operations.rollbackAttachment,
		)
	}
}

func TestFinalizeLaunchedWorkspaceAttachPreservesReadinessErrorAndRollsBack(t *testing.T) {
	workspace, attachment := workspaceAttachFixture()
	readinessErr := errors.New("remote readiness rejected")
	operations := &fakeWorkspaceAttachOperations{
		readyFn: func(context.Context, string, *Workspace, *Attachment) (*Workspace, error) {
			return nil, readinessErr
		},
	}

	got, err := finalizeLaunchedWorkspaceAttach(
		context.Background(), operations, "origin.example", false, workspace, attachment,
	)
	if got != nil || err != readinessErr {
		t.Fatalf("workspace=%#v error=%v", got, err)
	}
	if operations.readyCalls != 1 || operations.commitRemoteCalls != 0 || operations.commitLocalCalls != 0 {
		t.Fatalf("ready=%d remote move=%d local move=%d",
			operations.readyCalls, operations.commitRemoteCalls, operations.commitLocalCalls)
	}
	assertWorkspaceAttachRollback(t, operations)
}

func TestFinalizeLaunchedWorkspaceAttachRollsBackMoveFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		host string
		set  func(*fakeWorkspaceAttachOperations, error)
		want func(*testing.T, *fakeWorkspaceAttachOperations)
	}{
		{
			name: "remote",
			host: "origin.example",
			set: func(operations *fakeWorkspaceAttachOperations, moveErr error) {
				operations.commitRemoteFn = func(context.Context, string, *Workspace, *Attachment) (*Workspace, error) {
					return nil, moveErr
				}
			},
			want: func(t *testing.T, operations *fakeWorkspaceAttachOperations) {
				t.Helper()
				if operations.readyCalls != 1 || operations.commitRemoteCalls != 1 || operations.commitLocalCalls != 0 {
					t.Fatalf("ready=%d remote move=%d local move=%d",
						operations.readyCalls, operations.commitRemoteCalls, operations.commitLocalCalls)
				}
				assertWorkspaceAttachRollback(t, operations)
			},
		},
		{
			name: "local",
			set: func(operations *fakeWorkspaceAttachOperations, moveErr error) {
				operations.commitLocalFn = func(context.Context, *Workspace, *Attachment) (*Workspace, error) {
					return nil, moveErr
				}
			},
			want: func(t *testing.T, operations *fakeWorkspaceAttachOperations) {
				t.Helper()
				if operations.readyCalls != 0 || operations.commitRemoteCalls != 0 || operations.commitLocalCalls != 1 {
					t.Fatalf("ready=%d remote move=%d local move=%d",
						operations.readyCalls, operations.commitRemoteCalls, operations.commitLocalCalls)
				}
				if operations.rollbackCalls != 1 ||
					operations.rollbackHost != "" ||
					operations.rollbackWorkspace != "workspace" ||
					operations.rollbackAttachment != "attachment" {
					t.Fatalf("rollback calls=%d host=%q workspace=%q attachment=%q",
						operations.rollbackCalls,
						operations.rollbackHost,
						operations.rollbackWorkspace,
						operations.rollbackAttachment,
					)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, attachment := workspaceAttachFixture()
			moveErr := errors.New(test.name + " move failed")
			operations := &fakeWorkspaceAttachOperations{}
			test.set(operations, moveErr)

			got, err := finalizeLaunchedWorkspaceAttach(
				context.Background(), operations, test.host, true, workspace, attachment,
			)
			if got != nil || !errors.Is(err, moveErr) {
				t.Fatalf("workspace=%#v error=%v", got, err)
			}
			test.want(t, operations)
		})
	}
}

func TestFinalizeLaunchedWorkspaceAttachRejectsInvalidTransitionResults(t *testing.T) {
	for _, test := range []struct {
		name   string
		result *Workspace
		detail string
	}{
		{name: "nil", detail: "returned a nil workspace"},
		{name: "wrong workspace", result: &Workspace{ID: "other"}, detail: "returned workspace other"},
		{
			name: "missing destination attachment",
			result: &Workspace{
				ID: "workspace", PrimaryAttachmentID: "source",
				Attachments: map[string]*Attachment{},
			},
			detail: "lost destination attachment",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, attachment := workspaceAttachFixture()
			operations := &fakeWorkspaceAttachOperations{
				readyFn: func(context.Context, string, *Workspace, *Attachment) (*Workspace, error) {
					return test.result, nil
				},
			}

			got, err := finalizeLaunchedWorkspaceAttach(
				context.Background(), operations, "origin.example", test.name == "missing destination attachment", workspace, attachment,
			)
			if got != nil || err == nil || !strings.Contains(err.Error(), test.detail) {
				t.Fatalf("workspace=%#v error=%v", got, err)
			}
			assertWorkspaceAttachRollback(t, operations)
		})
	}
}

func TestFinalizeLaunchedWorkspaceAttachJoinsRollbackFailureUsingFreshDeadline(t *testing.T) {
	workspace, attachment := workspaceAttachFixture()
	readinessErr := errors.New("remote readiness rejected")
	rollbackErr := errors.New("origin detach failed")
	operations := &fakeWorkspaceAttachOperations{
		readyFn: func(context.Context, string, *Workspace, *Attachment) (*Workspace, error) {
			return nil, readinessErr
		},
		rollbackFn: func(ctx context.Context, _ string, _ string, _ *Attachment) error {
			if ctx.Err() != nil {
				t.Fatalf("rollback inherited canceled context: %v", ctx.Err())
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > workspaceAttachRollbackTimeout {
				t.Fatalf("rollback deadline = %v, ok=%v", deadline, ok)
			}
			return rollbackErr
		},
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := finalizeLaunchedWorkspaceAttach(
		canceled, operations, "origin.example", false, workspace, attachment,
	)
	if got != nil || !errors.Is(err, readinessErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("workspace=%#v error=%v", got, err)
	}
	if !strings.Contains(err.Error(), "workspace attach rollback failed") {
		t.Fatalf("rollback context missing from error: %v", err)
	}
	assertWorkspaceAttachRollback(t, operations)
}

func TestFinalizeLaunchedWorkspaceAttachSuccessDoesNotRollBack(t *testing.T) {
	for _, test := range []struct {
		name             string
		host             string
		move             bool
		wantReady        int
		wantRemoteCommit int
		wantLocalCommit  int
	}{
		{name: "remote mirror", host: "origin.example", wantReady: 1},
		{name: "remote move", host: "origin.example", move: true, wantReady: 1, wantRemoteCommit: 1},
		{name: "local move", move: true, wantLocalCommit: 1},
		{name: "local attach"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, attachment := workspaceAttachFixture()
			operations := &fakeWorkspaceAttachOperations{}
			got, err := finalizeLaunchedWorkspaceAttach(
				context.Background(), operations, test.host, test.move, workspace, attachment,
			)
			if err != nil || got == nil || got.ID != workspace.ID {
				t.Fatalf("workspace=%#v error=%v", got, err)
			}
			if operations.readyCalls != test.wantReady ||
				operations.commitRemoteCalls != test.wantRemoteCommit ||
				operations.commitLocalCalls != test.wantLocalCommit ||
				operations.rollbackCalls != 0 {
				t.Fatalf("ready=%d remote move=%d local move=%d rollback=%d",
					operations.readyCalls,
					operations.commitRemoteCalls,
					operations.commitLocalCalls,
					operations.rollbackCalls,
				)
			}
		})
	}
}

func TestWorkspaceAttachRollbackStepsAttemptEveryCleanup(t *testing.T) {
	localErr := errors.New("local detach failed")
	remoteErr := errors.New("origin detach failed")
	var calls []string
	err := runWorkspaceAttachRollbackSteps(
		context.Background(),
		workspaceAttachRollbackStep{
			name: "local",
			run: func(context.Context) error {
				calls = append(calls, "local")
				return localErr
			},
		},
		workspaceAttachRollbackStep{
			name: "kitty",
			run: func(context.Context) error {
				calls = append(calls, "kitty")
				return nil
			},
		},
		workspaceAttachRollbackStep{
			name: "remote",
			run: func(context.Context) error {
				calls = append(calls, "remote")
				return remoteErr
			},
		},
	)
	if strings.Join(calls, ",") != "local,kitty,remote" {
		t.Fatalf("cleanup calls = %v", calls)
	}
	if !errors.Is(err, localErr) || !errors.Is(err, remoteErr) ||
		!strings.Contains(err.Error(), "local:") || !strings.Contains(err.Error(), "remote:") {
		t.Fatalf("cleanup error = %v", err)
	}
}
