// SPDX-License-Identifier: Apache-2.0

package telemetry

import "go.opentelemetry.io/otel/metric"

// ApplyRequestsTotal counts the apply attempts GitOps Reverser made, one per attempt to one applier
// object, labelled by {applier_kind, reason, outcome}.
//
// applier_kind names the applier: "kustomization" (a Flux Kustomization). reason is the attempt's
// ApplyReason (ForeignCommit, Refusal, CatchUp). outcome is a frozen enum:
//
//	sent     — the applier was asked and the ask landed: a requestedAt stamp was patched, or, for a
//	           CatchUp, the applier's source already held the commit and nothing was owed.
//	failed   — the attempt could not ask, including an applier that applies another repository or
//	           branch. The branch worker owns the obligation and decides a retry on a quiet branch,
//	           with a backoff of its own, so a failure that persists keeps counting. An
//	           ApplyRequestFailed Event marks the first failure of a run.
//	dropped  — the attempt asked nothing, because no GitTarget may still ask for its reason: each
//	           was deleted or recreated, moved to another branch or applier, is no longer admitted by
//	           its ClusterProvider, or withdrew its consent to a Refusal.
//	stale    — the attempt asked nothing, because the decision no longer held when the patch was due:
//	           the branch had moved on or become busy. The worker decides again at the next quiet
//	           moment. Frequent stale attempts mean edits keep arriving within an attempt's lifetime.
//
// It counts attempts, so a request that fails twice and then lands is two "failed" and one "sent".
// It says nothing about whether the applier APPLIED: a request asks and never reports, and the
// applier's own status is the record of what it did.
//
// No GitTarget identity: one attempt is for one applier, which several targets can share, so a
// target label would have to invent an owner. The ApplyRequested and ApplyRequestFailed Events on
// each GitTarget carry that identity instead.
var ApplyRequestsTotal metric.Int64Counter
