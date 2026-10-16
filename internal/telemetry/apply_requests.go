// SPDX-License-Identifier: Apache-2.0

package telemetry

import "go.opentelemetry.io/otel/metric"

// ApplyRequestsTotal counts the apply requests GitOps Reverser decided, one per applier object per
// request, labelled by {applier_kind, reason, outcome}.
//
// applier_kind names the applier: "kustomization" (a Flux Kustomization). reason is the request's
// ApplyReason (ForeignCommit, Refusal, CatchUp). outcome is a frozen enum:
//
//	sent        — the applier was asked and the ask landed: a requestedAt stamp was patched, or, for
//	              a CatchUp, the applier's source already held the commit and nothing was owed.
//	failed      — every attempt failed and the request was given up on. An ApplyRequestFailed
//	              Event names the GitTargets it was for.
//	superseded  — a newer request for the same applier replaced this one before it was sent. The
//	              newer request carries the newer commit, so nothing is lost; a steady stream of
//	              these says requests are arriving faster than the applier is answering.
//
// It counts decisions, not attempts: a request that fails twice and then lands is one "sent". The
// retries are in the logs. It says nothing about whether the applier APPLIED: a request asks and
// never reports, and the applier's own status is the record of what it did.
//
// No GitTarget identity: one request is for one applier, which several targets can share, so a
// target label would have to invent an owner. The ApplyRequested and ApplyRequestFailed Events on
// each GitTarget carry that identity instead.
var ApplyRequestsTotal metric.Int64Counter
