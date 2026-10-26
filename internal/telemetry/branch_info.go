// SPDX-License-Identifier: Apache-2.0

package telemetry

import "go.opentelemetry.io/otel/attribute"

// git_branch_info says what each GitBranch writes: its GitProvider and its spec.branch, always 1.
//
// The branch-worker instruments are labelled by the GitBranch alone, {gitbranch_namespace,
// gitbranch_name}, which is the object an operator reads conditions from and runs kubectl against.
// The provider and the branch name were labels there before, and they still answer a real question:
// a GitProvider whose credential expired fails every branch it writes, and "which provider" is the
// grouping that finds that. Both are immutable on a GitBranch, so they belong on one series per
// GitBranch rather than repeated on twenty instruments, and a join recovers them for any of those:
//
//	sum by (provider_namespace, provider_name) (
//	  rate(gitopsreverser_git_pushes_total{outcome="failed"}[15m])
//	  * on(gitbranch_namespace, gitbranch_name) group_left(provider_namespace, provider_name)
//	  gitopsreverser_git_branch_info)
//
// It is published by the GitBranch reconcile, for every GitBranch that exists, whether or not it
// holds its branch: a GitBranch reporting BranchConflict has no worker series to join, and still
// appears here so its provider can be read beside its conditions.

// branchInfos holds one series per GitBranch, from its immutable spec.
var branchInfos = newObjectSeries("gitbranch_namespace", "gitbranch_name")

// RecordGitBranchInfo publishes one GitBranch's GitProvider (namespace resolved) and branch,
// replacing any previous entry for it. Call it on every reconcile: a gauge is level, and a fresh pod
// has to publish before anything changes.
func RecordGitBranchInfo(namespace, name, providerNamespace, providerName, branch string) {
	branchInfos.record(namespace, name,
		attribute.String("provider_namespace", providerNamespace),
		attribute.String("provider_name", providerName),
		attribute.String("branch", branch),
	)
}

// ForgetGitBranchInfo stops publishing one GitBranch's series. It is the delete path: an info
// series that outlives its GitBranch would name a provider for a branch nobody writes.
func ForgetGitBranchInfo(namespace, name string) {
	branchInfos.forget(namespace, name)
}
