// SPDX-License-Identifier: Apache-2.0

package git

import "time"

// ObservationSource names what proved an observation. It separates "this revision is our own
// work, and the server took it" from "we went and looked, and this is what was there", which is
// the first question asked of a branch that moved unexpectedly.
type ObservationSource string

const (
	// ObservedByFetch is a fetch that was followed by a reset onto the fetched tip.
	ObservedByFetch ObservationSource = "Fetch"
	// ObservedByPush is a push session the server did not reject.
	ObservedByPush ObservationSource = "Push"
)

// RemoteObservation is what a branch worker last PROVED about the target branch on the remote,
// and when.
//
// It is not a cache of a fetch. A successful compare-and-swap push proves the same fact more
// strongly, because it names the hash the server accepted rather than one it advertised, so both
// producers write here. That is the whole point of collapsing the old trio: those fields were
// stamped by fetches alone, so on a busy target they were arbitrarily old while the worker was in
// fact in continuous contact with the remote.
//
// An observation is a fact about a MOMENT. Somebody can push a millisecond after the server acks
// ours, and that is equally true of a fetch — it is why the compare-and-swap exists on the write
// path. The cost of a stale belief is one rejected advertisement, never a bad write, and it is
// why anything published from this carries the timestamp beside the revision.
type RemoteObservation struct {
	// Revision is the hash the branch is at on the remote. EMPTY means the advertisement did not
	// carry the branch: a branch does not exist without a commit, so "no revision" and "no
	// branch" are the same fact and do not need two fields.
	Revision string
	// At is when the observation was made.
	At time.Time
	// By is what proved it.
	By ObservationSource
}

// Age is how long ago the observation was made, against now.
func (o RemoteObservation) Age(now time.Time) time.Duration { return now.Sub(o.At) }
