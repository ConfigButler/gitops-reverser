// SPDX-License-Identifier: Apache-2.0

package git

import (
	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// ApplyRequest is one request, decided by a branch worker, that the appliers of some GitTargets on
// its branch apply a commit. The worker decides WHEN; delivering it is the ApplyRequester's job, off
// the worker's queue, so a slow applier never holds the branch.
type ApplyRequest struct {
	// Branch is the branch the request is about.
	Branch BranchKey
	// Reason is why the appliers are asked.
	Reason v1alpha3.ApplyReason
	// Commit is the commit to apply: the branch tip when the worker decided, which the worker has
	// pushed or proved by a fetch. Empty when the remote does not carry the branch.
	Commit string
	// Targets are the GitTargets whose appliers are asked. Several may name one applier; the
	// requester asks it once.
	Targets []itypes.ResourceReference
}

// ApplyRequester delivers apply requests. RequestApply must not block: it is called on the branch
// worker's loop goroutine.
type ApplyRequester interface {
	RequestApply(req ApplyRequest)
}
