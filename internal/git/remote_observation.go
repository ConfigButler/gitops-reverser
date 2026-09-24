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
// It is not a cache of a fetch: a successful compare-and-swap push proves the same fact more
// strongly, because it names the hash the server accepted rather than one it advertised, so both
// producers write here. The trio this replaced (branchExists, lastCommitSHA, lastFetchTime) was
// stamped by fetches alone, so it went arbitrarily stale on exactly the targets in continuous
// contact with the remote.
//
// An observation is a fact about a MOMENT — somebody can push a millisecond after the server acks
// ours, as they can after any fetch. The cost of a stale belief is one rejected advertisement,
// never a bad write, and it is why anything published from this carries the timestamp beside the
// revision.
type RemoteObservation struct {
	// Revision is the hash the branch is at on the remote. EMPTY means the advertisement did not
	// carry the branch: a branch does not exist without a commit, so "no revision" and "no
	// branch" are the same fact and do not need two fields.
	Revision string
	// At is when the observation was made.
	At time.Time
	// By is what proved it.
	By ObservationSource
	// Withdrawn marks the one report that is not an observation: "forget what was published".
	// A worker sends it when its GitProvider now names a different repository, because the
	// revision already published then describes a repository the target no longer points at, and
	// if the new one is unreachable nothing else would correct it. It carries no revision and
	// dates nothing — the projection turns it into an ABSENT status.remote, not a value.
	Withdrawn bool
}

// WithdrawnObservation is the report that takes back whatever was published about a branch.
func WithdrawnObservation() RemoteObservation { return RemoteObservation{Withdrawn: true} }

// Age is how long ago the observation was made, against now.
func (o RemoteObservation) Age(now time.Time) time.Duration { return now.Sub(o.At) }
