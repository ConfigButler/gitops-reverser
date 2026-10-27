// SPDX-License-Identifier: Apache-2.0

package telemetry

import "go.opentelemetry.io/otel/metric"

// ApplyWindowCollisionsTotal counts writes by an applier, inside its apply window (from the attempt
// being sent until its operation completed, plus a short grace), to an object the API side edited after the
// attempt was sent: the apply arriving late over a newer edit. Labelled by {gitbranch_*,
// provider_*, outcome}. outcome is a frozen enum:
//
//	withheld   — the write is proven to be the applier's (its field manager alone wrote last), so it
//	             was not mirrored. Git keeps the API's value, and a Repair is pending for the applier, an
//	             apply that puts it back on the cluster. The edit was reverted on the cluster until then.
//	ambiguous  — the applier and another field manager wrote in the same second, so the write may be
//	             the API edit itself. It was mirrored as ever, which cannot lose an edit; if it was the
//	             apply, the revert is in Git as the applier's commit.
//
// Any non-zero rate means edits are landing while an apply runs: a withheld one is repaired, and the
// apply windows follow other people's pushes, refusals, repairs and API creates.
//
// Registered in registerCounters (exporter.go) with every other counter.
var ApplyWindowCollisionsTotal metric.Int64Counter

// ApplyWindowBarriersTotal counts the barriers apply windows took after their operation completed: a
// wait until the watch had delivered every write the source clusters held by then, so the applier's
// own writes are judged inside the window however far behind the watch was. Labelled by
// {gitbranch_*, provider_*, outcome}. outcome is a frozen enum:
//
//	met       — every stream delivered every such write; the window then closed at the end of its grace.
//	timed_out — the window closed at the limit (2 minutes) with a stream still behind: a write of the
//	            applier's arriving later is mirrored as an edit (apply_echoes_total{outcome="late"}).
//	failed    — the barrier could not be taken (the source cluster could not be listed, or its
//	            resourceVersions are not numbers); the window closed after its grace alone.
//
// A steady timed_out or failed rate means the window cannot see every apply it asked for land.
//
// Registered in registerCounters (exporter.go) with every other counter.
var ApplyWindowBarriersTotal metric.Int64Counter

// ApplyEchoesTotal counts the applier's own writes the apply window judged: an apply GitOps Reverser
// asked for landing, object by object. Labelled by {gitbranch_*, provider_*,
// outcome}. outcome is a frozen enum:
//
//	not_mirrored — the applier's managers alone wrote the object last, at or after the earliest send
//	               of the apply windows still open, and nobody edited it in the window: the apply
//	               itself. Its content is the commit's, which Git holds, so it is not mirrored.
//	late         — the applier's managers alone wrote the object last with no apply window open: an
//	               echo arriving after the window's grace, or an apply somebody else started. It is
//	               mirrored as an edit. A steady rate means applies keep landing outside the windows,
//	               and such a write can revert a newer Git-side change to the object.
//
// Registered in registerCounters (exporter.go) with every other counter.
var ApplyEchoesTotal metric.Int64Counter

// ApplyOwnershipConflictsTotal counts objects another applier owns that a branch worker kept out of a
// coordinated folder: a live write not mirrored, or a snapshot object neither upserted
// nor swept (docs/design/applier-echoes.md, F4). Labelled by {gitbranch_*,
// provider_*}. Any non-zero rate is a misconfiguration: another Kustomization or Application applies
// objects into a namespace the folder mirrors, and its log line names both.
//
// Registered in registerCounters (exporter.go) with every other counter.
var ApplyOwnershipConflictsTotal metric.Int64Counter

// StaleSnapshotsTotal counts resync snapshots a branch worker found stale: collected while Git was
// ahead of the cluster, across an apply, or arriving while Git is ahead, so an apply was about to make
// the picture they hold wrong (docs/design/applier-echoes.md, F3). Labelled by {gitbranch_*,
// provider_*, outcome}. outcome is a frozen enum:
//
//	discarded — the snapshot was not decided, and a fresh one of its GitTarget is asked for once Git
//	            is no longer ahead.
//	used      — the GitTarget's snapshots had been held for the limit (five minutes) while an apply
//	            kept failing, so this one was decided anyway. Whatever the unapplied commit changed
//	            in the objects it holds is reverted, as the API-wins rule says. A non-zero rate means
//	            an applier is failing for minutes at a time.
//
// Registered in registerCounters (exporter.go) with every other counter.
var StaleSnapshotsTotal metric.Int64Counter
