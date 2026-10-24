// SPDX-License-Identifier: Apache-2.0

package telemetry

import "go.opentelemetry.io/otel/metric"

// ApplyRequestsTotal counts the apply attempts GitOps Reverser made, one per attempt to one applier
// object, labelled by {applier_kind, reason, outcome}.
//
// applier_kind names the applier: "kustomization" (a Flux Kustomization) or "application" (an Argo
// CD Application). reason is the attempt's ApplyReason (ForeignCommit, Refusal, Repair, CatchUp),
// for either kind. outcome is a frozen enum:
//
//	sent     — the applier was asked and the ask landed: a Kustomization was resumed with a
//	           requestedAt token or a sync operation set. What needs no apply (a converge of the commit
//	           the applier last applied, an empty tip) the branch worker never sends, so it is not
//	           counted.
//	failed   — the attempt could not ask, including an applier that applies another repository or
//	           branch. The branch worker keeps the reason pending and decides a retry after a flush,
//	           with a backoff of its own, so a failure that persists keeps counting. An
//	           ApplyRequestFailed Event marks the first failure of a run.
//	dropped  — the attempt asked nothing, because no GitTarget may still ask for its reason: each
//	           was deleted or recreated, moved to another branch or applier, is no longer admitted by
//	           its KubeProvider, or withdrew its consent to a Refusal.
//	stale    — the attempt asked nothing, because the decision no longer held when the write was due:
//	           the branch had moved on. The worker decides again at once, at the new tip. Frequent
//	           stale attempts mean the branch keeps moving within an attempt's lifetime.
//
// It counts attempts, so a request that fails twice and then lands is two "failed" and one "sent".
// It says nothing about whether the applier APPLIED: the operation a sent request starts is
// followed by the branch worker, and an ApplyFailed or ApplyUnresolved Event marks one that did not
// complete, or not in time (apply_operations_unresolved counts the latter).
//
// No GitTarget identity: one attempt is for one applier, which several targets can share, so a
// target label would have to invent an owner. The ApplyRequested and ApplyRequestFailed Events on
// each GitTarget carry that identity instead.
var ApplyRequestsTotal metric.Int64Counter

// ClaimedResourceReconciledTotal counts the applies an applier claim did not start, labelled by
// {applier_kind, claim_namespace, claim_name}: a held Kustomization resumed, or a held Application's
// automated sync switched on, by somebody else. The applier may apply unchecked from then until the
// claim takes it back, which it does on its next look, within a minute. The ClaimedResourceReconciled
// Event on the claim says the same, but the API server aggregates identical Events and expires them,
// so alert on this instead. A Kustomization resumed so also shows its apply as an Unresolved operation.
//
// The claim is named: it holds exactly one applier, so the label is its identity and stays bounded
// by the claims that exist.
var ClaimedResourceReconciledTotal metric.Int64Counter

// ApplyRevisionMismatchesTotal counts apply operations that succeeded on another revision than the
// commit GitOps Reverser checked, labelled by {applier_kind}. A Flux Kustomization applies whatever
// its GitRepository's artifact holds when it reconciles; the artifact is read again just before the
// resume, so this is somebody stamping the GitRepository between that read and the reconcile (a
// person, or the source's own interval). The apply window still withholds a late write over a
// newer API edit; a steady rate means something else keeps fetching the source.
var ApplyRevisionMismatchesTotal metric.Int64Counter
