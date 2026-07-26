package zka

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A pane that has been allocated but not yet admitted must never be touched by
// reconciliation. The reconciler yields to admission instead, and reports a
// retryable condition rather than parking the attachment in an error state.
func TestReconcileYieldsToProposedPaneWithoutTouchingIt(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	desired := firstPane(workspace)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	allocated, err := testAllocatePane(d, workspace.ID, "concurrent:add", "")
	if err != nil {
		t.Fatal(err)
	}
	pending := allocated.Pane
	tree := kittyTreeForTabs(workspace.ID, [][]string{{desired.ID, pending.ID}})
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		return kittyResponse(t, args, tree, workspace)
	}}
	d.kitty.Runner = runner
	d.kitty.Command = "kitten-test"

	err = d.reconcileEndpointTopology(context.Background(), attachment.Endpoint)
	if !errors.Is(err, errPaneAdmissionPending) {
		t.Fatalf("reconcile error = %v, want errPaneAdmissionPending", err)
	}
	if !topologyReconcileErrorIsTransient(err) {
		t.Fatal("yielding to admission must be transient, not terminal")
	}
	for _, call := range runner.Calls() {
		joined := strings.Join(call.Args, " ")
		for _, destructive := range []string{"close-window", "goto_session", "detach-window", "detach-tab"} {
			if strings.Contains(joined, destructive) {
				t.Fatalf("reconcile ran %q against a workspace holding a proposed pane: %#v", destructive, call.Args)
			}
		}
	}
	got, err := d.getWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Panes[pending.ID].Proposed() {
		t.Fatalf("pending pane phase = %q, want proposed", got.Panes[pending.ID].Phase)
	}
	if got.Attachments[attachment.ID].ReconcileStatus == TopologyReconcileError {
		t.Fatal("yielding to admission must not mark the attachment as errored")
	}
}

// A converged workspace must be completely silent: no Kitty mutation of any
// kind. This is the property that makes the reconciler safe to run on a timer.
func TestConvergedTopologyIssuesNoKittyMutations(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 2)
	workspace, attachment := readyWorkspaceAttachment(t, d, workspace, "local")
	panes := workspace.SortedPanes()

	// manifestForPanes puts every pane in one tab, so the live tree must match.
	tree := kittyTreeForTabs(workspace.ID, [][]string{{panes[0].ID, panes[1].ID}})
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args ...string) (string, string, error) {
		return kittyResponse(t, args, tree, workspace)
	}}
	d.kitty.Runner = runner
	d.kitty.Command = "kitten-test"

	if err := d.reconcileEndpointTopology(context.Background(), attachment.Endpoint); err != nil {
		t.Fatalf("reconcile of a converged workspace failed: %v", err)
	}
	for _, call := range runner.Calls() {
		joined := strings.Join(call.Args, " ")
		for _, mutation := range []string{
			"close-window", "goto_session", "detach-window", "detach-tab",
			"set-tab-title", "goto-layout", "set-enabled-layouts", "focus-window", "launch",
		} {
			if strings.Contains(joined, mutation) {
				t.Fatalf("converged reconcile issued %q: %#v", mutation, call.Args)
			}
		}
	}
}

// Backoff must grow, stay bounded, and jitter within its band, so repeated
// failures cannot turn into a hot loop against Kitty.
func TestBackoffScheduleGrowsAndCaps(t *testing.T) {
	previousCeiling := time.Duration(0)
	for attempts := 1; attempts <= 12; attempts++ {
		ceiling := backoffBase << (attempts - 1)
		if ceiling > backoffCap || ceiling <= 0 {
			ceiling = backoffCap
		}
		for i := 0; i < 50; i++ {
			delay := backoffDelay(attempts)
			if delay < ceiling/2 || delay > ceiling {
				t.Fatalf("attempt %d delay %v outside [%v, %v]", attempts, delay, ceiling/2, ceiling)
			}
			if delay > backoffCap {
				t.Fatalf("attempt %d delay %v exceeds cap %v", attempts, delay, backoffCap)
			}
		}
		if ceiling < previousCeiling {
			t.Fatalf("attempt %d ceiling %v regressed from %v", attempts, ceiling, previousCeiling)
		}
		previousCeiling = ceiling
	}
}

// A repeatedly failing endpoint must be rate limited rather than retried on
// every trigger.
func TestFailingReconcileBacksOffInsteadOfSpinning(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "unix:/kitty"
	d.noteTopologyFailure(endpoint, 1)
	d.noteTopologyFailure(endpoint, 1)
	ready, wait := d.topologyBackoffReady(endpoint)
	if ready || wait <= 0 {
		t.Fatalf("expected an open backoff window, got ready=%v wait=%v", ready, wait)
	}
	d.clearTopologyBackoff(endpoint)
	if ready, _ := d.topologyBackoffReady(endpoint); !ready {
		t.Fatal("clearing backoff must allow an immediate retry")
	}
}

// Completing a reconcile must not shorten a settle window that something else
// deliberately installed.
func TestReconcileCompletionDoesNotShortenCaptureSuppression(t *testing.T) {
	d, err := newTestDaemon(t, t.TempDir(), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "unix:/kitty"
	d.extendTopologyCaptureSuppression(endpoint, 30*time.Second)
	d.topologyMu.Lock()
	d.captureHold[endpoint]++
	d.topologyMu.Unlock()
	d.topologyMu.Lock()
	d.captureHold[endpoint]--
	if d.captureHold[endpoint] == 0 {
		delete(d.captureHold, endpoint)
	}
	d.topologyMu.Unlock()
	if !d.topologyCaptureSuppressed(endpoint) {
		t.Fatal("finishing a reconcile discarded the explicit settle window")
	}
}
