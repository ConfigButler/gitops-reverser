// SPDX-License-Identifier: Apache-2.0

package telemetry

import "go.opentelemetry.io/otel/attribute"

// git_branch_targets is the join between the two halves of the pipeline, and it exists because
// they do not share a label set.
//
// Everything up to the branch worker is labelled by the GitTarget that asked for the write —
// watch_events_total, git_documents_total, watch_recovery_total, the placement counters — because
// "which tenant stopped receiving events" is the question those answer. Everything from the branch
// worker on is labelled {gitbranch_namespace, gitbranch_name}, because a BranchWorker serves one
// GitBranch and several GitTargets can share one while writing different paths. So
// git_pushes_total{outcome="failed"} names a GitBranch that stopped advancing and CANNOT name the
// GitTargets now behind it, which is the question an operator asks during the incident.
//
// The fix is a join series rather than a label. Putting gittarget_* on git_commits_total would be
// worse than the gap: a commit genuinely spans every target sharing the branch, so a single owner
// would have to be invented. This publishes the mapping instead, always 1, bounded by GitTarget
// count, and PromQL does the join — the same shape attribution_transport_info already uses.
//
// The mapping is the GitTarget's spec.gitBranchRef, its namespace resolved, and nothing else: it
// holds whether or not that GitBranch exists, holds the branch, or has a running worker.
//
// # What it does NOT assert
//
// It says which GitTargets write through a GitBranch. It does not say any of them had pending
// work, so a failing GitBranch names POTENTIALLY affected targets and never "targets that are
// behind". Two consequences worth knowing before writing an alert on it:
//
//   - A GitBranch with no push series at all drops out of a multiplication entirely, which is the
//     silent case: the target an operator most wants to see is the one whose worker never ran.
//     Missing workers need their own query against this mapping and git_queue_depth.
//   - spec.suspend stops the WRITE and nothing else, so a suspended target keeps its worker, its
//     branch and this series while producing no commits by design. Nothing here separates
//     "suspended" from "stuck"; resource_condition does.
//
// # source_cluster
//
// It carries the cluster the GitTarget mirrors FROM, so the same join answers "which tenant's
// cluster" as well as "which GitTarget". The value is GitTarget.SourceCluster(), the referenced
// KubeProvider's namespace/name, so two providers with one name in two namespaces are two values.
// It is NOT the internal config-plane sentinel, which is deliberately the empty string and is a
// different identity.
//
// It rides this series rather than being added to the nine GitTarget-labelled instruments because
// spec.kubeProviderRef is CEL-immutable: a GitTarget's cluster is fixed for its life, so one
// join recovers the axis for all of them. The Git-side instruments cannot carry it at all — a
// branch is shared by GitTargets that may mirror different clusters, so a single value there would
// have to be invented, which is the same objection that keeps gittarget_* off git_commits_total.
// Joining a branch's failed pushes to several clusters means each is POTENTIALLY affected; it does
// not allocate the failures between them, and summing the expanded result double-counts the push.
//
// # Why it publishes configured targets rather than running workers
//
// The recording site is the GitTarget reconcile, ahead of every readiness gate, so a target that
// never got a worker still appears. Publishing from the live worker map would hide exactly the
// case the join is for. See docs/interpreting-metrics.md for the two queries this enables.

// branchTargets holds one series per GitTarget. One GitTarget has one destination —
// spec.gitBranchRef and spec.path are both CEL-immutable — so the key is the target and the
// GitBranch is the value.
var branchTargets = newObjectSeries("gittarget_namespace", "gittarget_name")

// RecordBranchTarget publishes one GitTarget's GitBranch, and the cluster it mirrors FROM,
// replacing any previous entry for it.
//
// Call it on every reconcile, including one that then fails a gate: a gauge is level rather than
// event, and the join has to carry the targets that are not working.
func RecordBranchTarget(namespace, name, gitBranchNamespace, gitBranchName, sourceCluster string) {
	branchTargets.record(namespace, name,
		attribute.String("gitbranch_namespace", gitBranchNamespace),
		attribute.String("gitbranch_name", gitBranchName),
		attribute.String("source_cluster", sourceCluster),
	)
}

// ForgetBranchTarget stops publishing the series for one GitTarget. It is the delete path, and it
// is the half that has to be right: a join series that outlives its GitTarget keeps attributing a
// live branch's failures to an object that no longer exists.
func ForgetBranchTarget(namespace, name string) {
	branchTargets.forget(namespace, name)
}
