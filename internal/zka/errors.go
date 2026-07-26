package zka

import (
	"context"
	"errors"
)

// Typed reconcile errors replace substring matching on error text. The old
// scheme defaulted an unrecognised error to *fatal*, which swallowed every
// inner context deadline, every raw `kitten @` failure, and several plain
// optimistic-concurrency retries into a terminal state that re-armed the
// destructive rebuild loop forever. The default here is the opposite: only an
// explicit allowlist is fatal.
var (
	// Retry immediately: the world moved under us and a fresh read will agree.
	errTopologyGenerationChanged = errors.New("topology generation changed")
	errPaneAdmissionPending      = errors.New("pane admission is pending")

	// Retry with backoff: a transient condition outside our control.
	errKittyNotQuiescent        = errors.New("kitty tree is not quiescent")
	errPanesNotReady            = errors.New("kitty panes did not become ready")
	errWorkspaceRevisionChanged = errors.New("workspace revision changed")
	errAttachmentTopologyStale  = errors.New("attachment topology is stale")
	errNoVerifiedBaseline       = errors.New("attachment has no verified topology baseline")
	errUnknownCapturedPane      = errors.New("kitty window references an unknown pane")
	errKittyCommand             = errors.New("kitty remote control command failed")
	errViewsNotReady            = errors.New("kitty views are not ready")
	errTopologyPaneSetMismatch  = errors.New("captured pane set does not match the workspace")

	// Adoption candidate: enforcement is not converging, so the observed tree
	// should become the desired one rather than being fought forever.
	errStructureNotConverged = errors.New("kitty structure did not converge")

	// Fatal: the desired state itself is malformed and retrying cannot help.
	errTopologyInvalid = errors.New("topology is structurally invalid")
)

type reconcileClass int

const (
	reconcileRetryFast reconcileClass = iota
	reconcileRetryBackoff
	reconcileAdopt
	reconcileFatal
)

// classifyReconcileError decides what to do with a failed reconcile pass.
// attempts is how many consecutive passes have already failed for the current
// endpoint and generation; it is what converts persistent non-convergence into
// adoption instead of an endless rebuild.
func classifyReconcileError(err error, attempts, maxEnforceAttempts int) reconcileClass {
	switch {
	case err == nil:
		return reconcileRetryFast
	case errors.Is(err, errTopologyInvalid):
		return reconcileFatal
	case errors.Is(err, errTopologyGenerationChanged), errors.Is(err, errPaneAdmissionPending):
		return reconcileRetryFast
	case errors.Is(err, errStructureNotConverged):
		if attempts >= maxEnforceAttempts {
			return reconcileAdopt
		}
		return reconcileRetryBackoff
	case errors.Is(err, context.DeadlineExceeded):
		return reconcileRetryBackoff
	default:
		return reconcileRetryBackoff
	}
}
