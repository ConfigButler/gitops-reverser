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
	// Commit is the hash the branch is at on the remote. EMPTY means the advertisement did not
	// carry the branch: a branch does not exist without a commit, so "no revision" and "no
	// branch" are the same fact and do not need two fields.
	Commit string
	// At is when the observation was made.
	At time.Time
	// By is what proved it.
	By ObservationSource
	// Repo is the repository the observation is ABOUT: the identity of the worker that proved it.
	//
	// A revision means nothing without it. Workers are keyed by (provider, branch) while a
	// GitProvider can be repointed at another repository, so the layer that publishes this can be
	// holding a revision from a repository its GitTarget no longer uses — and if the new one is
	// unreachable, nothing else would ever correct it. Carrying the identity lets that be DERIVED
	// at the moment of publication, by comparing it against what the GitProvider names now,
	// instead of latched by whoever happened to notice the change.
	//
	// It is zero for a worker that has no identity of its own: the CLI, and tests that never reach
	// a remote. A comparison against an unknown identity proves nothing, so it is not made.
	Repo RepoIdentity
	// ParentState is what the remote offers an absent branch to be created from, observed
	// together with its absence. Empty while the branch exists.
	ParentState ParentState
	// ParentBranch and ParentCommit name that parent and where it is. ParentCommit is empty
	// unless ParentState is ParentFound; ParentBranch is empty for ParentUnborn.
	ParentBranch string
	ParentCommit string
}

// ParentState is what a branch the remote does not carry would be created from.
type ParentState string

const (
	// ParentFound means the parent branch exists, and the branch's first commit builds on its tip.
	ParentFound ParentState = "Found"
	// ParentMissing means the configured parent branch is not on the remote either, so the branch
	// cannot be created and nothing is written until one of them exists.
	ParentMissing ParentState = "Missing"
	// ParentUnborn means the repository offers no branch to start from (it is empty) and none was
	// configured, so the first commit starts the branch with no history.
	ParentUnborn ParentState = "Unborn"
)

// Age is how long ago the observation was made, against now.
func (o RemoteObservation) Age(now time.Time) time.Duration { return now.Sub(o.At) }
