// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"sync"

	"go.opentelemetry.io/otel/attribute"
)

// git_branch_targets is the join between the two halves of the pipeline, and it exists because
// they do not share a label set.
//
// Everything up to the branch worker is labelled by the GitTarget that asked for the write —
// watch_events_total, git_documents_total, watch_recovery_total, the placement counters — because
// "which tenant stopped receiving events" is the question those answer. Everything from the branch
// worker on is labelled {provider_namespace, provider_name, branch}, because a BranchWorker serves
// a (GitProvider, branch) and several GitTargets can share one while writing different paths. So
// git_pushes_total{outcome="failed"} names a branch that stopped advancing and CANNOT name the
// GitTargets now behind it, which is the question an operator asks during the incident.
//
// The fix is a join series rather than a label. Putting gittarget_* on git_commits_total would be
// worse than the gap: a commit genuinely spans every target sharing the branch, so a single owner
// would have to be invented. This publishes the mapping instead, always 1, bounded by GitTarget
// count, and PromQL does the join — the same shape attribution_transport_info already uses.
//
// # What it does NOT assert
//
// It says which GitTargets write to a branch. It does not say any of them had pending work, so a
// failing branch names POTENTIALLY affected targets and never "targets that are behind". Two
// consequences worth knowing before writing an alert on it:
//
//   - A branch with no push series at all drops out of a multiplication entirely, which is the
//     silent case: the target an operator most wants to see is the one whose worker never ran.
//     Missing workers need their own query against this mapping and git_queue_depth.
//   - spec.suspend stops the WRITE and nothing else, so a suspended target keeps its worker, its
//     branch and this series while producing no commits by design. Nothing here separates
//     "suspended" from "stuck"; resource_condition does.
//
// # Why it publishes configured targets rather than running workers
//
// The recording site is the GitTarget reconcile, ahead of every readiness gate, so a target that
// never got a worker still appears. Publishing from the live worker map would hide exactly the
// case the join is for. See docs/interpreting-metrics.md for the two queries this enables.

// branchTargetKey identifies the GitTarget one published series is about. One GitTarget has one
// destination — spec.branch, spec.path and spec.gitProviderRef are all CEL-immutable — so the key
// is the target and the destination is the value.
type branchTargetKey struct {
	namespace string
	name      string
}

// branchTargetDestination is where that GitTarget writes, as the Git-side instruments label it.
type branchTargetDestination struct {
	providerNamespace string
	providerName      string
	branch            string
}

var (
	branchTargetsMu sync.RWMutex
	branchTargets   = map[branchTargetKey]branchTargetDestination{}
)

// RecordBranchTarget publishes one GitTarget's destination, replacing any previous entry for it.
//
// Call it on every reconcile that resolves a destination, including one that then fails a gate: a
// gauge is level rather than event, and the join has to carry the targets that are not working.
func RecordBranchTarget(namespace, name, providerNamespace, providerName, branch string) {
	branchTargetsMu.Lock()
	defer branchTargetsMu.Unlock()
	branchTargets[branchTargetKey{namespace: namespace, name: name}] = branchTargetDestination{
		providerNamespace: providerNamespace,
		providerName:      providerName,
		branch:            branch,
	}
}

// ForgetBranchTarget stops publishing the series for one GitTarget. It is the delete path, and it
// is the half that has to be right: a join series that outlives its GitTarget keeps attributing a
// live branch's failures to an object that no longer exists.
func ForgetBranchTarget(namespace, name string) {
	branchTargetsMu.Lock()
	defer branchTargetsMu.Unlock()
	delete(branchTargets, branchTargetKey{namespace: namespace, name: name})
}

// branchTargetSamples renders the mapping as gauge samples, one per GitTarget, read at scrape time.
//
// Observable rather than pushed for the reason resource_condition is: this family needs series that
// STOP. A pushed gauge latches its last value, so a deleted GitTarget would stay in the join for
// the life of the process. Reading a map at scrape time makes the delete a map delete.
func branchTargetSamples() []GaugeSample {
	branchTargetsMu.RLock()
	defer branchTargetsMu.RUnlock()

	samples := make([]GaugeSample, 0, len(branchTargets))
	for key, dest := range branchTargets {
		samples = append(samples, GaugeSample{
			Value: 1,
			Attrs: []attribute.KeyValue{
				// Prefixed, never a bare namespace/name: a Prometheus pod scrape with
				// honor_labels=false overwrites those with the scraped pod's own.
				attribute.String("gittarget_namespace", key.namespace),
				attribute.String("gittarget_name", key.name),
				attribute.String("provider_namespace", dest.providerNamespace),
				attribute.String("provider_name", dest.providerName),
				attribute.String("branch", dest.branch),
			},
		})
	}
	return samples
}
