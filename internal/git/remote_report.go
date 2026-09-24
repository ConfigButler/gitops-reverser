// SPDX-License-Identifier: Apache-2.0

package git

// RemoteReporter delivers a confirmed observation of a branch's remote state to the layer that
// keeps it for the whole branch. Every worker gets one, bound to its own BranchKey, because the
// observation is made on a branch-worker goroutine with no result channel back to the controller
// and would otherwise be learned and dropped.
//
// DELIVERY IS NOT PUBLICATION, and this is the delivery half. "Where is branch B" is ONE fact,
// proved once — by a push or by a fetch — and shared by every GitTarget on that branch at no cost:
// it is a map write, and distributing a fact is not re-proving it. What each GitTarget then WRITES
// to its status is a separate, bounded decision, made on its own reconcile. See publishRemote.
//
// This is why the worker keeps no list of targets. It used to report against whichever targets
// were in hand — the ones a push's writes named, or the one a refresh was serving — so a fact
// about the branch reached some of its targets and not others, and the layer above had to keep a
// copy per target of a value that was the same for all of them.
type RemoteReporter func(observed RemoteObservation)
