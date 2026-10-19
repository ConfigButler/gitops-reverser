// SPDX-License-Identifier: Apache-2.0

package telemetry

import "go.opentelemetry.io/otel/metric"

// ApplyWindowCollisionsTotal counts writes by an applier, inside its apply window (from the attempt
// being sent until its operation completed, plus a short grace), to an object the API side edited after the
// attempt was sent: the apply arriving late over a newer edit. Labelled by {provider_namespace,
// provider_name, branch, outcome}. outcome is a frozen enum:
//
//	withheld   — the write is proven to be the applier's (its field manager alone wrote last), so it
//	             was not mirrored. Git keeps the API's value, and a Repair is pending for the applier, an
//	             apply that puts it back on the cluster. The edit was reverted on the cluster until then.
//	ambiguous  — the applier and another field manager wrote in the same second, so the write may be
//	             the API edit itself. It was mirrored as ever, which cannot lose an edit; if it was the
//	             apply, the revert is in Git as the applier's commit.
//
// Any non-zero rate means edits are landing while an apply runs. A long quietPeriod makes the
// CatchUp windows rarer; the ForeignCommit and Refusal windows follow other people's pushes.
//
// Registered in registerCounters (exporter.go) with every other counter.
var ApplyWindowCollisionsTotal metric.Int64Counter
