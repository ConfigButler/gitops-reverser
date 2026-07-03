// SPDX-License-Identifier: Apache-2.0

package v1alpha3

// PushStrategy defines how events are coalesced into commits before pushing.
type PushStrategy struct {
	// CommitWindow is the rolling silence window used to coalesce events into
	// a single commit per (author, gitTarget). The timer resets on every event
	// arrival and a flush is triggered after this many seconds of silence.
	// Setting "0s" opts into per-event commits in the steady-state.
	// Defaults to "5s".
	// +optional
	CommitWindow *string `json:"commitWindow,omitempty"`
}
