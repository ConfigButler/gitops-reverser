/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package git

import "errors"

// ErrFinalizeQueueFull is reported when a finalize signal cannot be enqueued
// because the worker's event queue is saturated.
var ErrFinalizeQueueFull = errors.New("branch worker event queue full; finalize signal dropped")

// FinalizeOutcome is the terminal result of processing a FinalizeSignal.
type FinalizeOutcome string

const (
	// FinalizeCommitted means an open commit window was finalized into a commit.
	FinalizeCommitted FinalizeOutcome = "Committed"
	// FinalizeNoOpenWindow means there was no open commit window to finalize.
	FinalizeNoOpenWindow FinalizeOutcome = "NoOpenWindow"
)

// FinalizeSignal is the "finalize the open commit window now" work item. It
// rides the same per-worker event queue as resource events, so by audit-stream
// ordering it is processed after every earlier write for that worker.
type FinalizeSignal struct {
	// CommitMessage overrides the generated commit message for the finalized
	// window. An empty string keeps the generated grouped-commit message.
	CommitMessage string

	// Author is the effective user that requested the finalize. The signal
	// only finalizes an open window whose author matches; this binds "the
	// open window" to "the requesting author's open window".
	Author string
	// GitTargetName is the name of the GitTarget the finalize is scoped to.
	GitTargetName string
	// GitTargetNamespace is the namespace of that GitTarget.
	GitTargetNamespace string

	// Result receives the outcome once the signal is processed. The channel is
	// expected to be buffered (capacity >= 1) so the worker never blocks.
	Result chan<- FinalizeResult
}

// matchesWindow reports whether the signal's author and target identify the
// given open window. A worker is keyed only by provider and branch, so the
// open window it holds may belong to a different author or GitTarget than the
// one this signal was issued for; finalizing it then would commit unrelated
// edits under the caller's message.
func (s *FinalizeSignal) matchesWindow(w *openWindow) bool {
	if s == nil || w == nil {
		return false
	}
	return s.Author == w.Author &&
		s.GitTargetName == w.GitTarget &&
		s.GitTargetNamespace == w.GitTargetNamespace
}

// reply delivers a result without blocking the worker loop.
func (s *FinalizeSignal) reply(result FinalizeResult) {
	if s == nil || s.Result == nil {
		return
	}
	select {
	case s.Result <- result:
	default:
	}
}

// FinalizeResult carries the outcome of a FinalizeSignal back to its caller.
type FinalizeResult struct {
	// Outcome is set when Err is nil.
	Outcome FinalizeOutcome
	// SHA is the resulting commit SHA when Outcome is FinalizeCommitted.
	SHA string
	// Branch is the branch the worker operates on.
	Branch string
	// Err is set when the finalize could not be completed.
	Err error
}
